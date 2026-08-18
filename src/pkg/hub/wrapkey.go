package hub

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Wrapped master delivery to pull-only spokes — the sealing primitive.
//
// WHY THIS EXISTS. The master (HIVE_HUB_SECRET) is the ONE value a spoke cannot
// self-derive; every other per-hive value is a pure function of it plus HIVE_ID
// (SpokeHeartbeatKey, SpokeInviteKey, spokeDomainKey, SpokeSSOPublicKey — see
// hub_keys.go). 44 of 66 spokes sit on pull_only clusters the hub cannot write
// to BY DESIGN (saas_provision.go KubectlReachable), so rotation cannot deliver
// a new master to them and RESIDUAL-1 (strip the plaintext master) is blocked
// behind the same boundary.
//
// Option D closes it without weakening pull_only: the spoke generates a
// keypair, keeps the private half on its PVC, publishes the public half over
// its own OUTBOUND heartbeat, and the hub seals each new master to that key.
// The delivery channel need not be confidential, because the payload is already
// sealed to a key only the recipient holds. Nothing here asks the hub to
// initiate a connection to a spoke.
//
// WHY X25519 AND NOT Ed25519. The repo's existing asymmetric material is
// Ed25519 (sso.go, hub_cookie.go, hub_pubkey_generations.go). Ed25519 is a
// SIGNING key and CANNOT encrypt. Reusing it here would require either the
// Ed25519->X25519 birational map — sharp-edged, easy to get wrong, and it
// creates one key used under two algorithms — or an ad-hoc scheme. Neither is
// acceptable for the one payload whose compromise is fleet-wide. This is a new
// key type, deliberately. See TestWrapKeyNeverAcceptsEd25519Material, which
// asserts no Ed25519-sized-or-shaped material can enter this path, because
// "unify the key types" is exactly the plausible future refactor and a
// behavioural test would not notice the crypto silently weakening.
//
// WHY NO HKDF. hub_keys.go:31-33 records the deliberate choice to avoid
// x/crypto/hkdf as a direct module dependency and to use single-block
// HMAC-SHA256 expansion instead. That reasoning holds identically here — fixed,
// unique context labels and one 32-byte output — and OQ-3 was decided to follow
// the precedent rather than reverse it. Adding a module to the one payload
// whose compromise is fleet-wide is the wrong place for new supply-chain
// surface.

const (
	// wrapKeyLen is the X25519 key size in bytes, for both halves. Named rather
	// than inlined so a reader does not have to know that 32 here means "an
	// X25519 scalar" and 32 elsewhere in this file means "an AES-256 key".
	wrapKeyLen = 32

	// wrapAEADKeyLen is the AES-256 key size. AES-256-GCM has precedent in this
	// repo (pkg/hubbackup) — precedent, not novelty.
	wrapAEADKeyLen = 32

	// wrapNonceLen is the GCM standard nonce size (96 bits). Every seal draws a
	// fresh random nonce; nonces are never derived or counted, so there is no
	// reuse hazard to reason about across restarts.
	wrapNonceLen = 12

	// infoWrapSharedSecret is the domain-separation label for the single-block
	// HMAC-SHA256 expansion of the X25519 shared secret into the AEAD key.
	// Versioned ("-v1") for the same reason every info string in hub_keys.go is:
	// so the format can evolve without a verifier ever silently accepting a
	// foreign-domain key.
	infoWrapSharedSecret = "hive-wrap-master-v1"

	// infoWrapFingerprint is the domain-separation label for the public-key
	// fingerprint. A fingerprint is a PUBLIC identifier that names a pinned key
	// in logs, alerts and operator re-pin commands; it is separated from the
	// AEAD-key label so a fingerprint can never collide with key material.
	infoWrapFingerprint = "hive-wrap-fingerprint-v1"
)

// wrapKeyMaxAge and wrapKeyOverlap are POLICY CHOICES, not derived values.
//
// There is no calculation behind either number and a future reader must not
// assume there is. They are recorded here with what they trade off so they can
// be changed DELIBERATELY rather than either treated as sacred or adjusted
// blindly. (OQ-4, decided by the operator 2026-08-14.)
//
//   - wrapKeyMaxAge = 90 days bounds how long any single wrapping key is
//     exposed. Shorter is more conservative but generates more republication
//     churn; the constraint on the upper end of churn is the reconcile lane's
//     perHiveEnvMaxPatchesPerCycle = 3 budget, since each patch rolls a tenant
//     pod. 90 days is comfortably clear of making that a bottleneck.
//   - wrapKeyOverlap = 24 hours is how long a spoke retains its PREVIOUS
//     private key after rotating, so a master sealed to the old key and still
//     in flight can be opened. It must exceed the time a full fleet
//     convergence takes (~6 hours), with margin for a spoke that misses a
//     cycle. 24 hours gives roughly 4x that.
//
// Safe to change deliberately. Not safe to change because a number "looked
// arbitrary".
const (
	wrapKeyMaxAge  = 90 * 24 * time.Hour
	wrapKeyOverlap = 24 * time.Hour
)

var (
	// errWrapKeyMalformed is returned for any public or private key that is not
	// exactly wrapKeyLen bytes of valid X25519 material. Callers must treat this
	// as "no usable key", never as "accept something else" — an unparseable key
	// must not silently unpin a good one (§8 row 1).
	errWrapKeyMalformed = errors.New("wrap key: malformed X25519 key material")

	// errWrapOpenFailed is returned for every unseal failure — bad AAD, wrong
	// recipient, tampered ciphertext, wrong generation. Deliberately ONE error
	// with no detail: distinguishing "wrong hive" from "bad tag" would hand an
	// attacker an oracle, and the caller's correct response is identical in
	// every case (do not apply, do not ack, keep the current master).
	errWrapOpenFailed = errors.New("wrap key: sealed payload did not open")
)

// wrapPublicKey is a spoke's published X25519 public half. It is a PUBLIC
// value: it appears on the heartbeat, in the pin store, and in alerts.
type wrapPublicKey struct {
	raw []byte
}

// wrapPrivateKey is a spoke's X25519 private half. It NEVER leaves the pod and
// is never logged, never serialised into a heartbeat, and never returned by any
// hub API. Only the spoke holds one.
type wrapPrivateKey struct {
	key *ecdh.PrivateKey
}

// generateWrapKeypair produces a fresh X25519 keypair from crypto/rand.
//
// Spoke-side only. The hub never generates a wrapping keypair — if it did, it
// would hold the private half and the entire point of Option D (the hub cannot
// read what it seals) would be lost.
func generateWrapKeypair() (wrapPrivateKey, wrapPublicKey, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return wrapPrivateKey{}, wrapPublicKey{}, fmt.Errorf("wrap key: generate: %w", err)
	}
	return wrapPrivateKey{key: priv}, wrapPublicKey{raw: priv.PublicKey().Bytes()}, nil
}

// parseWrapPublicKey decodes a hex-encoded published public key.
//
// FAILS CLOSED. Anything that is not exactly wrapKeyLen bytes of valid X25519
// material returns errWrapKeyMalformed and NO key. There is deliberately no
// lenient path, no truncation, and no padding: a caller that cannot parse a
// publication must count the hive as NOT having a usable key (which blocks
// retirement) rather than falling back to any other key.
func parseWrapPublicKey(hexEncoded string) (wrapPublicKey, error) {
	raw, err := hex.DecodeString(hexEncoded)
	if err != nil {
		return wrapPublicKey{}, errWrapKeyMalformed
	}
	if len(raw) != wrapKeyLen {
		return wrapPublicKey{}, errWrapKeyMalformed
	}
	// Round-trip through crypto/ecdh so the point is actually validated rather
	// than merely length-checked. A 32-byte string is not necessarily a valid
	// X25519 public key.
	if _, err := ecdh.X25519().NewPublicKey(raw); err != nil {
		return wrapPublicKey{}, errWrapKeyMalformed
	}
	return wrapPublicKey{raw: raw}, nil
}

// parseWrapPrivateKey decodes a hex-encoded private key read from the spoke's
// PVC. Same fail-closed discipline as parseWrapPublicKey: a malformed key file
// yields no key, and the caller's correct response is to generate a fresh
// keypair (which then requires an operator re-pin — §8 row 7, deliberate).
func parseWrapPrivateKey(hexEncoded string) (wrapPrivateKey, error) {
	raw, err := hex.DecodeString(hexEncoded)
	if err != nil {
		return wrapPrivateKey{}, errWrapKeyMalformed
	}
	if len(raw) != wrapKeyLen {
		return wrapPrivateKey{}, errWrapKeyMalformed
	}
	k, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return wrapPrivateKey{}, errWrapKeyMalformed
	}
	return wrapPrivateKey{key: k}, nil
}

// Hex renders the public key for publication on the heartbeat and for storage
// in the pin store. Hex for the same reason every derived key in hub_keys.go is
// hex: it is a safe ASCII value for an env var, a JSON field and a log line.
func (p wrapPublicKey) Hex() string {
	if len(p.raw) != wrapKeyLen {
		return ""
	}
	return hex.EncodeToString(p.raw)
}

// hex renders the private key for persistence to the spoke PVC at 0600.
// Unexported and lowercase on purpose: this must be impossible to reach from
// any package that could serialise it outward.
func (p wrapPrivateKey) hex() string {
	if p.key == nil {
		return ""
	}
	return hex.EncodeToString(p.key.Bytes())
}

// publicKey returns the public half of a private key, so a spoke that loaded a
// key from disk can publish without storing the public half separately.
func (p wrapPrivateKey) publicKey() wrapPublicKey {
	if p.key == nil {
		return wrapPublicKey{}
	}
	return wrapPublicKey{raw: p.key.PublicKey().Bytes()}
}

func (p wrapPublicKey) valid() bool  { return len(p.raw) == wrapKeyLen }
func (p wrapPrivateKey) valid() bool { return p.key != nil }

// wrapKeyFingerprint is the short PUBLIC identifier for a pinned key. It names
// a key in alerts, in the pin store, and in the operator's re-pin command —
// which is why it must be domain-separated from the AEAD key derivation: a
// value an operator reads off a dashboard must never be usable as key material.
func wrapKeyFingerprint(pub wrapPublicKey) string {
	if !pub.valid() {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(infoWrapFingerprint))
	mac.Write(pub.raw)
	return hex.EncodeToString(mac.Sum(nil))
}

// wrapAAD builds the additional authenticated data that binds a sealed payload
// to exactly one recipient and one generation.
//
//	hiveID || 0x00 || generationID || 0x00 || fingerprint
//
// The 0x00 separators match derivePerHiveKey's convention (hub_keys.go:118-122)
// and exist for the same stated reason: hive IDs are attacker-influenced and
// plain concatenation is ambiguous, so ("ab","c") and ("a","bc") must not
// produce the same bytes.
//
// Each component closes a specific attack:
//   - hiveID: a ciphertext sealed for X cannot be opened as Y even if misrouted
//     or deliberately replayed cross-hive.
//   - generationID: an attacker who captured a generation-N ciphertext cannot
//     roll a spoke back to a retired master after N+1 has shipped. The channel
//     is assumed NON-confidential, so replay resistance cannot come from it.
//   - fingerprint: binds to the specific pinned key, so a payload sealed under
//     a superseded wrapping key cannot be presented as current.
func wrapAAD(hiveID string, generationID int, fingerprint string) []byte {
	aad := make([]byte, 0, len(hiveID)+len(fingerprint)+16)
	aad = append(aad, []byte(hiveID)...)
	aad = append(aad, 0)
	aad = append(aad, []byte(fmt.Sprintf("%d", generationID))...)
	aad = append(aad, 0)
	aad = append(aad, []byte(fingerprint)...)
	return aad
}

// deriveWrapAEADKey expands an X25519 shared secret into the AES-256 key.
//
// Single-block HMAC-SHA256, following hub_keys.go's deliberate no-HKDF
// precedent (OQ-3). Both public keys are mixed in so the derived key is bound
// to BOTH parties — without that, a shared secret alone would not distinguish
// which ephemeral produced it.
func deriveWrapAEADKey(shared []byte, ephemeralPub, recipientPub wrapPublicKey) []byte {
	mac := hmac.New(sha256.New, shared)
	mac.Write([]byte(infoWrapSharedSecret))
	mac.Write([]byte{0})
	mac.Write(ephemeralPub.raw)
	mac.Write([]byte{0})
	mac.Write(recipientPub.raw)
	return mac.Sum(nil)[:wrapAEADKeyLen]
}

// sealedPayload is the wire form of a wrapped master.
//
// Everything here is PUBLIC except by construction: the ephemeral public key is
// public, and the ciphertext is opaque to anyone without the recipient's
// private key. There is deliberately NO field carrying a plaintext secret —
// that would be Option B, which was rejected because it makes the heartbeat
// bearer a master-exfiltration primitive.
type sealedPayload struct {
	// EphemeralPub is the hex X25519 public half of the fresh sender keypair
	// generated for THIS seal. Fresh per wrap, which gives forward secrecy
	// against later compromise of hub state and makes every ciphertext
	// independently sealed.
	EphemeralPub string `json:"ephemeral_pub"`
	// Nonce is the hex 96-bit GCM nonce, fresh random per seal.
	Nonce string `json:"nonce"`
	// Ciphertext is the hex AES-256-GCM output (with tag).
	Ciphertext string `json:"ciphertext"`
	// GenerationID names the master generation sealed inside. It is REPEATED in
	// the AAD; this copy exists only so the spoke can decide whether to attempt
	// the open at all (refuse generation <= current) without first decrypting.
	// The AAD copy is the authoritative one — a tampered GenerationID here fails
	// the AEAD tag rather than being trusted.
	GenerationID int `json:"generation_id"`
	// Fingerprint names the wrapping key this was sealed to, so a spoke holding
	// both a current and an overlap key knows which private half to try first.
	// Also repeated in the AAD, and authoritative there for the same reason.
	Fingerprint string `json:"fingerprint"`
}

// sealForSpoke encrypts payload to a spoke's pinned wrapping key.
//
// HUB-SIDE. The hub holds no private half, generates a fresh ephemeral keypair
// per call, and discards it immediately — so the hub cannot re-open what it
// sealed, which is the property that makes a non-confidential delivery channel
// acceptable.
//
// payload is the caller's plaintext. Under OQ-2(c) it carries BOTH the new
// master AND the spoke's freshly derived heartbeat key for the incoming
// generation, sealed together — never as separate plaintext fields on the
// heartbeat response, which is what would turn Option D into the rejected
// Option B.
func sealForSpoke(recipient wrapPublicKey, hiveID string, generationID int, payload []byte) (sealedPayload, error) {
	if !recipient.valid() {
		return sealedPayload{}, errWrapKeyMalformed
	}
	if hiveID == "" {
		// No identity means no AAD binding means a ciphertext openable by
		// anyone holding any wrapping key. Refuse rather than seal something
		// unbound. Mirrors derivePerHiveKey's refusal to derive without a hive
		// ID (hub_keys.go:115-117), which the F2 banner names as the invariant
		// that must never regain a bypass.
		return sealedPayload{}, errors.New("wrap key: refusing to seal without a hive ID")
	}
	recipientPub, err := ecdh.X25519().NewPublicKey(recipient.raw)
	if err != nil {
		return sealedPayload{}, errWrapKeyMalformed
	}
	ephPriv, ephPub, err := generateWrapKeypair()
	if err != nil {
		return sealedPayload{}, err
	}
	shared, err := ephPriv.key.ECDH(recipientPub)
	if err != nil {
		return sealedPayload{}, fmt.Errorf("wrap key: ecdh: %w", err)
	}
	aeadKey := deriveWrapAEADKey(shared, ephPub, recipient)
	block, err := aes.NewCipher(aeadKey)
	if err != nil {
		return sealedPayload{}, fmt.Errorf("wrap key: aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return sealedPayload{}, fmt.Errorf("wrap key: gcm: %w", err)
	}
	nonce := make([]byte, wrapNonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return sealedPayload{}, fmt.Errorf("wrap key: nonce: %w", err)
	}
	fp := wrapKeyFingerprint(recipient)
	ct := gcm.Seal(nil, nonce, payload, wrapAAD(hiveID, generationID, fp))
	return sealedPayload{
		EphemeralPub: ephPub.Hex(),
		Nonce:        hex.EncodeToString(nonce),
		Ciphertext:   hex.EncodeToString(ct),
		GenerationID: generationID,
		Fingerprint:  fp,
	}, nil
}

// openFromHub decrypts a sealed payload with the spoke's private wrapping key.
//
// SPOKE-SIDE. hiveID is supplied by the CALLER from its own local identity
// (/data/hive-id), never from the payload — taking it from the payload would
// let an attacker choose the AAD and defeat the cross-hive binding entirely.
//
// Every failure returns errWrapOpenFailed with no detail. On failure the spoke
// keeps its current master, does NOT apply, and does NOT ack, which is what
// makes the hub retry (§8 row 3). Applying a partially decoded master would
// brick the spoke.
func openFromHub(priv wrapPrivateKey, hiveID string, sp sealedPayload) ([]byte, error) {
	if !priv.valid() || hiveID == "" {
		return nil, errWrapOpenFailed
	}
	ephRaw, err := hex.DecodeString(sp.EphemeralPub)
	if err != nil || len(ephRaw) != wrapKeyLen {
		return nil, errWrapOpenFailed
	}
	ephPub, err := ecdh.X25519().NewPublicKey(ephRaw)
	if err != nil {
		return nil, errWrapOpenFailed
	}
	nonce, err := hex.DecodeString(sp.Nonce)
	if err != nil || len(nonce) != wrapNonceLen {
		return nil, errWrapOpenFailed
	}
	ct, err := hex.DecodeString(sp.Ciphertext)
	if err != nil {
		return nil, errWrapOpenFailed
	}
	shared, err := priv.key.ECDH(ephPub)
	if err != nil {
		return nil, errWrapOpenFailed
	}
	ownPub := priv.publicKey()
	aeadKey := deriveWrapAEADKey(shared, wrapPublicKey{raw: ephRaw}, ownPub)
	block, err := aes.NewCipher(aeadKey)
	if err != nil {
		return nil, errWrapOpenFailed
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errWrapOpenFailed
	}
	// The AAD is rebuilt from the SPOKE's own identity and its OWN key's
	// fingerprint — not from sp.Fingerprint. If the hub sealed to a different
	// key, or to a different hive, the tag fails here. This is the line that
	// makes cross-hive misdelivery and stale-key replay fail closed, so it must
	// never be "fixed" to read the fingerprint off the payload.
	aad := wrapAAD(hiveID, sp.GenerationID, wrapKeyFingerprint(ownPub))
	pt, err := gcm.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, errWrapOpenFailed
	}
	return pt, nil
}
