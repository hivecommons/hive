package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Spoke-side wrapping-key lifecycle: generate, persist, load, rotate.
//
// STORAGE MIRRORS AN ESTABLISHED PATTERN rather than inventing one. The spoke
// already persists private key material on the same PVC at the same mode:
// spokeAppKeyPath = "/data/gh-app-key.pem" with spokeAppKeyFileMode = 0o600 and
// the comment "signing material must never be readable by anything else sharing
// the PVC or the pod" (src/cmd/hive/main.go:99,114-116). /data is the PVC mount
// in the spoke template, and /data/hive-id already establishes that
// identity-critical state persists there across restarts.
//
// PERSIST BEFORE USE. Step 2 of the lifecycle writes the key to disk BEFORE
// returning it to any caller, mirroring rotateMasterSecret's persist-before-
// install ordering and its stated rationale: never act on key material you
// might forget at the next roll. A key used but not persisted would be
// republished as a DIFFERENT key after the next pod roll, which the hub would
// correctly refuse as a pin mismatch — turning a write failure into an
// operator-intervention event on a cluster the hub cannot reach into.

// spokeWrapKeyPath is where the spoke's X25519 private wrapping key lives on
// the PVC. A var rather than a const so tests can redirect it and exercise the
// real resolution order — the reason given at src/cmd/hive/main.go:95-97.
// Production never reassigns it.
//
// NOT YET READ BY ANY CALLER, deliberately kept. Every function in this file
// takes the path as an argument and the spoke-side lifecycle has no production
// entry point yet, so this declaration is the only record of where the key is
// supposed to land once it does. Deleting it to satisfy the linter would delete
// the answer, not the dead code — the first caller would then have to re-derive
// a path for private key material, which is exactly the kind of decision that
// should not be made twice.
//
//nolint:unused // staged lifecycle: documented production location, no caller yet (#4903)
var spokeWrapKeyPath = "/data/hive-wrap-key"

// spokeWrapKeyFileMode is rw------- , identical to spokeAppKeyFileMode and for
// the identical reason: the wrapping private key is the ONLY thing standing
// between a sealed master and anything else sharing the PVC or the pod.
const spokeWrapKeyFileMode = 0o600

// spokeWrapKeyDirMode is rwx------ for the containing directory when it must be
// created. /data normally exists as the PVC mount; this covers the test and
// first-boot-before-mount cases without ever widening the directory.
const spokeWrapKeyDirMode = 0o700

// wrapKeyFile is the on-disk form. It is versioned and carries explicit
// timestamps because the overlap window is a SECOND dual-acceptance window and
// inherits the discipline the master generations carry: explicitly versioned
// (the previous key is a numbered entry with a fingerprint, not an unnamed `if`
// branch) and explicitly finite (it carries an expiry, and an expired key is
// EXCLUDED, not warned about).
type wrapKeyFile struct {
	// Version guards the format. An unrecognised version is MALFORMED, not
	// "probably fine" — see loadSpokeWrapKeys.
	Version int `json:"version"`
	// Current is the hex private key currently published and sealed to.
	Current string `json:"current"`
	// CurrentCreated is when Current was generated. Drives wrapKeyMaxAge.
	CurrentCreated time.Time `json:"current_created"`
	// Previous is the hex private key retained across a rotation so a master
	// sealed to the old key and still in flight can be opened. Empty when there
	// is no overlap in progress.
	//
	// NOTE THE ASYMMETRY THAT MAKES THIS SAFE, AND DO NOT REMOVE IT: retaining
	// the old PRIVATE key spoke-side is what lets the spoke rotate WITHOUT
	// asking the hub to accept a new public key. The hub-side pin is untouched
	// by spoke-side rotation. If a future change instead makes the hub accept
	// the new public key automatically, that is an auto-re-pin path and it
	// silently degrades Option D into the rejected Option B. The adversarial
	// review hunted specifically for such a path and found none; keep it that
	// way. Spoke-side wrap-key rotation still requires a hub-side OPERATOR
	// re-pin before the new key is sealed to.
	Previous string `json:"previous,omitempty"`
	// PreviousExpires is the hard expiry of Previous's acceptance. A ZERO value
	// means ALREADY EXPIRED, never "never expires" — the same reading
	// acceptableGenerations gives VerifyUntil (hub_generations.go:232-238).
	// Reading it the other way would pin a superseded key live forever, which is
	// the F1/F2 failure mode this project has spent five audits removing.
	PreviousExpires time.Time `json:"previous_expires,omitempty"`
}

const wrapKeyFileVersion = 1

// spokeWrapKeys is the in-memory view a spoke uses.
type spokeWrapKeys struct {
	current         wrapPrivateKey
	currentCreated  time.Time
	previous        wrapPrivateKey
	previousExpires time.Time
}

// errWrapKeyFileMalformed is returned when the on-disk key file cannot be
// trusted. Callers must treat it EXACTLY as they treat an absent file: generate
// fresh. There is deliberately no partial-recovery path — a half-readable key
// file is not evidence of anything.
var errWrapKeyFileMalformed = errors.New("wrap key file: malformed")

// loadSpokeWrapKeys reads and validates the spoke's key file.
//
// Returns (keys, nil) on success, (zero, os.ErrNotExist) when absent, and
// (zero, errWrapKeyFileMalformed) for anything unparseable. The caller
// distinguishes absent from unreadable only for LOGGING — both lead to
// generate-fresh, because a spoke with no usable private key cannot open a
// sealed master either way.
//
// AN EXPIRED PREVIOUS KEY IS DROPPED, NOT WARNED ABOUT. That is the finiteness
// promise made real: the overlap window closes on its own without operator
// action, and a spoke cannot extend its own acceptance window.
func loadSpokeWrapKeys(path string, now time.Time) (spokeWrapKeys, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return spokeWrapKeys{}, os.ErrNotExist
		}
		// Present but unreadable. NOT the same as absent, and it must not be
		// silently treated as "no key, all good" — this is the same
		// absent-versus-unreadable distinction hub_generations_store.go makes.
		// It still routes to generate-fresh, but the caller logs it loudly.
		return spokeWrapKeys{}, fmt.Errorf("wrap key file: read: %w", err)
	}
	var f wrapKeyFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return spokeWrapKeys{}, errWrapKeyFileMalformed
	}
	if f.Version != wrapKeyFileVersion {
		return spokeWrapKeys{}, errWrapKeyFileMalformed
	}
	cur, err := parseWrapPrivateKey(strings.TrimSpace(f.Current))
	if err != nil {
		return spokeWrapKeys{}, errWrapKeyFileMalformed
	}
	out := spokeWrapKeys{current: cur, currentCreated: f.CurrentCreated}
	if strings.TrimSpace(f.Previous) != "" {
		// A ZERO PreviousExpires means ALREADY EXPIRED. Same rule as
		// acceptableGenerations. Dropping the key here rather than accepting it
		// is what stops a hand-edited file with the field stripped from keeping
		// a superseded wrapping key acceptable forever.
		if !f.PreviousExpires.IsZero() && now.Before(f.PreviousExpires) {
			prev, perr := parseWrapPrivateKey(strings.TrimSpace(f.Previous))
			if perr == nil {
				out.previous = prev
				out.previousExpires = f.PreviousExpires
			}
			// A malformed PREVIOUS key is dropped without failing the load: the
			// current key is independently valid and the spoke can still
			// publish and receive. Failing the whole load here would discard a
			// good current key over a stale overlap entry.
		}
	}
	return out, nil
}

// persistSpokeWrapKeys writes the key file at 0600.
//
// Writes to a temp file in the same directory and renames, so a crash mid-write
// never leaves a truncated key file that the next boot would read as malformed
// and replace — which would produce a new public key, a pin mismatch, and an
// operator-intervention event on an unreachable cluster.
func persistSpokeWrapKeys(path string, keys spokeWrapKeys) error {
	if !keys.current.valid() {
		return errWrapKeyMalformed
	}
	f := wrapKeyFile{
		Version:        wrapKeyFileVersion,
		Current:        keys.current.hex(),
		CurrentCreated: keys.currentCreated.UTC(),
	}
	if keys.previous.valid() && !keys.previousExpires.IsZero() {
		f.Previous = keys.previous.hex()
		f.PreviousExpires = keys.previousExpires.UTC()
	}
	blob, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("wrap key file: marshal: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, spokeWrapKeyDirMode); err != nil {
		return fmt.Errorf("wrap key file: mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".hive-wrap-key-*")
	if err != nil {
		return fmt.Errorf("wrap key file: temp: %w", err)
	}
	tmpName := tmp.Name()
	// Chmod BEFORE writing: the key bytes must never exist on disk at a wider
	// mode, not even for the microseconds between write and chmod.
	if err := tmp.Chmod(spokeWrapKeyFileMode); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("wrap key file: chmod: %w", err)
	}
	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("wrap key file: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("wrap key file: close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("wrap key file: rename: %w", err)
	}
	return nil
}

// ensureSpokeWrapKeys is the ONE mechanism covering first boot, pod roll and
// PVC loss — they are one mechanism because they must be.
//
//  1. Read the file. If present and well-formed, use it.
//  2. If absent or malformed, generate a fresh keypair and PERSIST IT BEFORE
//     returning it.
//
// A pod roll with the PVC intact is therefore a no-op: same key, same
// publication. PVC LOSS IS INDISTINGUISHABLE FROM FIRST BOOT by construction —
// the spoke generates a new keypair and publishes it. Whether the HUB should
// accept that new key is not decided here and must not be: it is refused as a
// pin mismatch and requires an operator re-pin. That availability cost is
// accepted DELIBERATELY, because the alternative is a design where losing a PVC
// and being attacked are indistinguishable and both silently succeed.
//
// Returns the keys and whether a fresh keypair was generated, so the caller can
// log the (operationally significant) difference.
func ensureSpokeWrapKeys(path string, now time.Time) (spokeWrapKeys, bool, error) {
	keys, err := loadSpokeWrapKeys(path, now)
	if err == nil && keys.current.valid() {
		return keys, false, nil
	}
	priv, _, gerr := generateWrapKeypair()
	if gerr != nil {
		return spokeWrapKeys{}, false, gerr
	}
	fresh := spokeWrapKeys{current: priv, currentCreated: now}
	if perr := persistSpokeWrapKeys(path, fresh); perr != nil {
		// Persist-before-use: refuse to return a key we could not durably
		// record. Returning it anyway would mean publishing a key that
		// disappears at the next roll, producing a pin mismatch the operator
		// then has to resolve on an unreachable cluster.
		return spokeWrapKeys{}, false, perr
	}
	return fresh, true, nil
}

// wrapKeyNeedsRotation reports whether the current key has exceeded
// wrapKeyMaxAge.
//
// A ZERO currentCreated reads as NEEDS ROTATION, not as "brand new". Same
// fail-closed direction as VerifyUntil.IsZero(): an unknown age must not read
// as a safe age. A hand-edited or migrated file missing the timestamp gets
// rotated rather than kept indefinitely.
func wrapKeyNeedsRotation(keys spokeWrapKeys, now time.Time) bool {
	if !keys.current.valid() {
		return true
	}
	if keys.currentCreated.IsZero() {
		return true
	}
	return !now.Before(keys.currentCreated.Add(wrapKeyMaxAge))
}

// rotateSpokeWrapKeys generates a new current key and retains the OLD PRIVATE
// key for wrapKeyOverlap, so a master sealed to the old key and still in flight
// can be opened.
//
// SPOKE-LOCAL ONLY. This does not, and must not, cause the hub to accept the
// new public key. The hub keeps sealing to the PINNED key until an operator
// re-pins. That is why the overlap retains the old private half rather than
// asking for acceptance of a new public half — it is the construction that
// keeps the never-auto-re-pin guarantee intact through the one place the design
// itself introduces a legitimate key change.
func rotateSpokeWrapKeys(path string, keys spokeWrapKeys, now time.Time) (spokeWrapKeys, error) {
	priv, _, err := generateWrapKeypair()
	if err != nil {
		return keys, err
	}
	next := spokeWrapKeys{
		current:         priv,
		currentCreated:  now,
		previous:        keys.current,
		previousExpires: now.Add(wrapKeyOverlap),
	}
	if perr := persistSpokeWrapKeys(path, next); perr != nil {
		// Keep the existing keys on a persist failure. Rotating in memory only
		// would publish a key that vanishes at the next roll.
		return keys, perr
	}
	return next, nil
}

// openWithSpokeKeys tries the current key, then the unexpired previous key.
//
// Order matters only for efficiency; correctness comes from the AEAD tag, which
// fails for the wrong key regardless of order. An EXPIRED previous key is not
// tried at all — loadSpokeWrapKeys never puts one in the struct.
func openWithSpokeKeys(keys spokeWrapKeys, hiveID string, sp sealedPayload) ([]byte, error) {
	if keys.current.valid() {
		if pt, err := openFromHub(keys.current, hiveID, sp); err == nil {
			return pt, nil
		}
	}
	if keys.previous.valid() {
		if pt, err := openFromHub(keys.previous, hiveID, sp); err == nil {
			return pt, nil
		}
	}
	return nil, errWrapOpenFailed
}
