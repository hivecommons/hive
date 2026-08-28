package hub

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Tests for the Option D sealing primitive and the spoke-side wrapping-key
// lifecycle (src/docs/design/master-delivery-wrapped.md, PR #3833).
//
// THE HOUSE STANDARD THIS FILE IS WRITTEN TO. Fifteen tests in this repo have
// been found ENCODING vulnerabilities or passing for the wrong reason —
// heartbeatBearerOK's test literally asserting the hub "must accept raw master
// secret" (F24) is the canonical example, and F19's
// TestDesiredPerHiveEnvUsesCurrentNotPrevious passed while validating a code
// path production never takes. So:
//
//   - Every negative assertion carries a POSITIVE CONTROL in the same test. A
//     test asserting "decryption fails" passes trivially when decryption is
//     broken outright; without the paired success case it asserts nothing.
//   - Properties where a behavioural test would pass for the wrong reason get a
//     SOURCE-ASSERTING test as well — with the subject chosen carefully, since
//     the design's own §10 spec was found to assert a true property of the
//     WRONG variable.

// --- helpers -----------------------------------------------------------------

func mustWrapKeypair(t *testing.T) (wrapPrivateKey, wrapPublicKey) {
	t.Helper()
	priv, pub, err := generateWrapKeypair()
	if err != nil {
		t.Fatalf("generateWrapKeypair: %v", err)
	}
	return priv, pub
}

// --- AAD BINDING: cross-hive, cross-generation, cross-key ---------------------

// TestSealOpenRoundTrip is the POSITIVE CONTROL for every negative test below.
// Without it, an implementation whose Open always fails would pass the entire
// rest of this file.
func TestSealOpenRoundTrip(t *testing.T) {
	priv, pub := mustWrapKeypair(t)
	payload := []byte("master-generation-7-plus-heartbeat-key")

	sp, err := sealForSpoke(pub, "hive-alpha", 7, payload)
	if err != nil {
		t.Fatalf("sealForSpoke: %v", err)
	}
	got, err := openFromHub(priv, "hive-alpha", sp)
	if err != nil {
		t.Fatalf("openFromHub on the correctly-bound case failed: %v — every "+
			"negative assertion in this file is meaningless without this passing", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("round trip corrupted the payload: got %d bytes, want %d", len(got), len(payload))
	}
	if sp.Ciphertext == hex.EncodeToString(payload) {
		t.Fatal("ciphertext equals hex(plaintext) — the payload was not encrypted at all")
	}
	if strings.Contains(sp.Ciphertext, hex.EncodeToString(payload)) {
		t.Fatal("plaintext appears inside the ciphertext")
	}
}

// TestAADBindsHiveID: a ciphertext sealed for hive X must not open as hive Y,
// even with the correct private key. This is what makes cross-hive
// misdelivery — accidental or deliberate — fail closed.
func TestAADBindsHiveID(t *testing.T) {
	priv, pub := mustWrapKeypair(t)
	sp, err := sealForSpoke(pub, "hive-alpha", 3, []byte("secret"))
	if err != nil {
		t.Fatalf("sealForSpoke: %v", err)
	}

	// Positive control FIRST, so a broken-open implementation cannot pass.
	if _, err := openFromHub(priv, "hive-alpha", sp); err != nil {
		t.Fatalf("positive control failed: correctly-bound open errored: %v", err)
	}

	if _, err := openFromHub(priv, "hive-beta", sp); err == nil {
		t.Error("a ciphertext sealed for hive-alpha opened as hive-beta — the AAD hiveID " +
			"binding is not in force, so a misrouted or replayed payload crosses hives")
	}
}

// TestAADBindsGeneration: an attacker who captured a generation-N ciphertext
// must not be able to present it as any other generation. The channel is
// assumed NON-confidential, so replay resistance cannot come from the channel.
func TestAADBindsGeneration(t *testing.T) {
	priv, pub := mustWrapKeypair(t)
	sp, err := sealForSpoke(pub, "hive-alpha", 5, []byte("master-5"))
	if err != nil {
		t.Fatalf("sealForSpoke: %v", err)
	}

	if _, err := openFromHub(priv, "hive-alpha", sp); err != nil {
		t.Fatalf("positive control failed: correctly-bound open errored: %v", err)
	}

	// Rewrite the advertised generation, as a replaying attacker would.
	replayed := sp
	replayed.GenerationID = 4
	if _, err := openFromHub(priv, "hive-alpha", replayed); err == nil {
		t.Error("a generation-5 ciphertext opened when presented as generation 4 — an attacker " +
			"who captured an old ciphertext could roll a spoke back to a retired master")
	}
	forward := sp
	forward.GenerationID = 6
	if _, err := openFromHub(priv, "hive-alpha", forward); err == nil {
		t.Error("a generation-5 ciphertext opened when presented as generation 6")
	}
}

// TestAADBindsWrappingKey: a payload sealed to key A must not open with key B,
// and — critically — the AAD is rebuilt from the SPOKE's own key fingerprint,
// not from the fingerprint field on the payload. If openFromHub ever reads
// sp.Fingerprint into the AAD, an attacker chooses the AAD and the binding is
// gone.
func TestAADBindsWrappingKey(t *testing.T) {
	privA, pubA := mustWrapKeypair(t)
	privB, _ := mustWrapKeypair(t)

	sp, err := sealForSpoke(pubA, "hive-alpha", 2, []byte("secret"))
	if err != nil {
		t.Fatalf("sealForSpoke: %v", err)
	}
	if _, err := openFromHub(privA, "hive-alpha", sp); err != nil {
		t.Fatalf("positive control failed: open with the sealed-to key errored: %v", err)
	}
	if _, err := openFromHub(privB, "hive-alpha", sp); err == nil {
		t.Error("a payload sealed to key A opened with key B's private half")
	}

	// THE FINGERPRINT-TRUST CASE, AND WHY IT IS SHAPED LIKE THIS.
	//
	// The obvious construction — rewrite sp.Fingerprint to key B's and open with
	// B — does NOT isolate the property. It fails whether or not openFromHub
	// trusts sp.Fingerprint, because opening with a different key also changes
	// the ECDH shared secret and therefore the AEAD key. The tag fails for an
	// unrelated reason and the test passes for the wrong reason. (Verified: the
	// regression replay for this property PASSED while neutered when written
	// that way.)
	//
	// The isolating construction keeps the recipient key FIXED, so the ECDH
	// shared secret is unchanged and the ONLY varying input is the fingerprint
	// that goes into the AAD.
	spoofed := sp
	spoofed.Fingerprint = wrapKeyFingerprint(privB.publicKey())
	if _, err := openFromHub(privA, "hive-alpha", spoofed); err != nil {
		t.Error("rewriting sp.Fingerprint broke the open for the CORRECT recipient — openFromHub " +
			"is reading the fingerprint off the payload instead of rebuilding it from the " +
			"spoke's own key, so an attacker who controls the payload controls the AAD")
	}
}

// TestOpenRebuildsAADFromOwnKeyNotPayload is the source-asserting half of the
// fingerprint-trust property, because the behavioural test above can only prove
// the negative direction (a rewritten fingerprint does not BREAK a good open).
// It cannot prove the positive one — that a payload whose fingerprint names a
// key the spoke does not hold is still rejected on the fingerprint's account —
// since any such payload also fails on the ECDH secret.
//
// The subject is chosen deliberately, per §10's worked example that a
// source-assertion is only as good as its choice of subject: assert that the
// AAD construction inside openFromHub passes wrapKeyFingerprint(ownPub) and NOT
// sp.Fingerprint. That is the property actually needed.
func TestOpenRebuildsAADFromOwnKeyNotPayload(t *testing.T) {
	raw, err := os.ReadFile("wrapkey.go")
	if err != nil {
		t.Fatalf("read wrapkey.go: %v", err)
	}
	src := stripGoComments(string(raw))
	i := strings.Index(src, "func openFromHub(")
	if i < 0 {
		t.Fatal("openFromHub not found — this test is not reading the implementation it thinks it is")
	}
	body := src[i:]
	if j := strings.Index(body[1:], "\nfunc "); j >= 0 {
		body = body[:j+1]
	}
	if !strings.Contains(body, "wrapAAD(hiveID, sp.GenerationID, wrapKeyFingerprint(ownPub))") {
		t.Error("openFromHub does not rebuild the AAD from the spoke's OWN key fingerprint. " +
			"The AAD must be wrapAAD(hiveID, sp.GenerationID, wrapKeyFingerprint(ownPub)) — " +
			"reading sp.Fingerprint instead lets an attacker who controls the payload choose " +
			"the AAD, and cross-key replay stops failing closed")
	}
	if regexp.MustCompile(`wrapAAD\([^)]*sp\.Fingerprint`).MatchString(body) {
		t.Error("openFromHub passes sp.Fingerprint into wrapAAD — the payload's own copy is " +
			"attacker-controlled and must never be an AAD input")
	}
	// Positive control on the subject: hiveID IS supposed to come from the
	// caller, so confirm the matcher can see real content and is not matching an
	// empty body.
	if !strings.Contains(body, "priv.key.ECDH(ephPub)") {
		t.Fatal("the extracted openFromHub body does not contain its ECDH call — the extraction " +
			"is wrong and every assertion above is vacuous")
	}
}

// TestSealRefusesWithoutHiveID mirrors derivePerHiveKey's refusal to derive
// without a hive ID (hub_keys.go:115-117). An unbound ciphertext is openable by
// anyone holding any wrapping key, so sealing one must be impossible.
func TestSealRefusesWithoutHiveID(t *testing.T) {
	_, pub := mustWrapKeypair(t)
	if _, err := sealForSpoke(pub, "", 1, []byte("secret")); err == nil {
		t.Error("sealForSpoke produced a ciphertext with no hive ID in the AAD — F2's " +
			"identity-binding invariant is re-opened through a new door")
	}
	// Positive control: with an ID it succeeds.
	if _, err := sealForSpoke(pub, "hive-alpha", 1, []byte("secret")); err != nil {
		t.Fatalf("positive control failed: sealForSpoke with a hive ID errored: %v", err)
	}
}

// TestOpenRefusesWithoutHiveID: the spoke supplies the hive ID from its own
// local identity, never from the payload. An empty one must fail closed.
func TestOpenRefusesWithoutHiveID(t *testing.T) {
	priv, pub := mustWrapKeypair(t)
	sp, err := sealForSpoke(pub, "hive-alpha", 1, []byte("secret"))
	if err != nil {
		t.Fatalf("sealForSpoke: %v", err)
	}
	if _, err := openFromHub(priv, "", sp); err == nil {
		t.Error("openFromHub opened a payload with an empty hive ID")
	}
	if _, err := openFromHub(priv, "hive-alpha", sp); err != nil {
		t.Fatalf("positive control failed: %v", err)
	}
}

// TestCiphertextTamperingFailsClosed covers the ordinary AEAD property, with a
// positive control, so "GCM is wired up at all" is actually asserted.
func TestCiphertextTamperingFailsClosed(t *testing.T) {
	priv, pub := mustWrapKeypair(t)
	sp, err := sealForSpoke(pub, "hive-alpha", 1, []byte("a-master-secret-value"))
	if err != nil {
		t.Fatalf("sealForSpoke: %v", err)
	}
	if _, err := openFromHub(priv, "hive-alpha", sp); err != nil {
		t.Fatalf("positive control failed: %v", err)
	}

	raw, err := hex.DecodeString(sp.Ciphertext)
	if err != nil || len(raw) == 0 {
		t.Fatalf("ciphertext not hex-decodable: %v", err)
	}
	raw[0] ^= 0xFF
	tampered := sp
	tampered.Ciphertext = hex.EncodeToString(raw)
	if _, err := openFromHub(priv, "hive-alpha", tampered); err == nil {
		t.Error("a tampered ciphertext opened — the GCM tag is not being verified")
	}
}

// TestEphemeralIsFreshPerSeal: sealing the same payload to the same key twice
// must produce different ciphertexts. Otherwise the "fresh sender keypair per
// wrap" forward-secrecy claim is false and an observer can correlate deliveries.
func TestEphemeralIsFreshPerSeal(t *testing.T) {
	_, pub := mustWrapKeypair(t)
	a, err := sealForSpoke(pub, "hive-alpha", 1, []byte("same"))
	if err != nil {
		t.Fatalf("sealForSpoke: %v", err)
	}
	b, err := sealForSpoke(pub, "hive-alpha", 1, []byte("same"))
	if err != nil {
		t.Fatalf("sealForSpoke: %v", err)
	}
	if a.EphemeralPub == b.EphemeralPub {
		t.Error("two seals reused the same ephemeral public key — forward secrecy claim is false")
	}
	if a.Ciphertext == b.Ciphertext {
		t.Error("two seals of the same payload produced identical ciphertext")
	}
	if a.Nonce == b.Nonce {
		t.Error("two seals reused the same GCM nonce")
	}
}

// --- MALFORMED KEY HANDLING: fail closed, never fall back ---------------------

func TestParseWrapPublicKeyFailsClosed(t *testing.T) {
	_, pub := mustWrapKeypair(t)
	// Positive control first.
	if _, err := parseWrapPublicKey(pub.Hex()); err != nil {
		t.Fatalf("positive control failed: a valid key did not parse: %v", err)
	}
	for _, bad := range []struct{ name, in string }{
		{"empty", ""},
		{"not hex", "zzzz"},
		{"too short", hex.EncodeToString(make([]byte, wrapKeyLen-1))},
		{"too long", hex.EncodeToString(make([]byte, wrapKeyLen+1))},
		{"ed25519 public key length", hex.EncodeToString(make([]byte, ed25519.PublicKeySize+1))},
	} {
		if _, err := parseWrapPublicKey(bad.in); err == nil {
			t.Errorf("parseWrapPublicKey accepted %s — a malformed publication must count as "+
				"NO usable key, never as something to fall back on", bad.name)
		}
	}
}

// TestWrapKeyNeverAcceptsEd25519Material is the source+behaviour assertion for
// the Ed25519-is-not-an-encryption-key invariant.
//
// WHY THIS IS SOURCE-ASSERTING. "Unify the key types" is exactly the plausible
// future refactor, and a behavioural test would not notice the crypto silently
// weakening: an Ed25519 seed is 32 bytes, the same as an X25519 scalar, so a
// refactor feeding Ed25519 material into this path would still produce
// working-looking round trips. The subject is chosen deliberately: assert that
// wrapkey.go imports no ed25519 and names no ed25519 symbol, because that is
// the property actually needed — not the weaker "the happy path still works".
func TestWrapKeyNeverAcceptsEd25519Material(t *testing.T) {
	for _, file := range []string{"wrapkey.go", "wrapkey_store.go"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		src := string(raw)
		// Strip comments before matching, so the explanatory prose in these
		// files (which necessarily says "Ed25519") does not trip the assertion.
		src = stripGoComments(src)
		if strings.Contains(src, "crypto/ed25519") {
			t.Errorf("%s imports crypto/ed25519 — Ed25519 is a SIGNING key and cannot encrypt; "+
				"routing it into the wrapping path is the refactor this test exists to block", file)
		}
		if regexp.MustCompile(`\bed25519\.`).MatchString(src) {
			t.Errorf("%s references an ed25519 symbol in code", file)
		}
	}
	// Positive control on the subject choice: the file DOES use crypto/ecdh, so
	// this test is reading the file it thinks it is reading and a typo'd
	// filename would not silently pass.
	raw, err := os.ReadFile("wrapkey.go")
	if err != nil {
		t.Fatalf("read wrapkey.go: %v", err)
	}
	if !strings.Contains(string(raw), "crypto/ecdh") {
		t.Fatal("wrapkey.go does not import crypto/ecdh — this test is not reading the wrapping " +
			"implementation, so its negative assertions above prove nothing")
	}
}

// TestNoHKDFModuleDependency asserts OQ-3: single-block HMAC-SHA256 expansion,
// following hub_keys.go:31-33's deliberate choice, not x/crypto/hkdf.
func TestNoHKDFModuleDependency(t *testing.T) {
	raw, err := os.ReadFile("wrapkey.go")
	if err != nil {
		t.Fatalf("read wrapkey.go: %v", err)
	}
	src := stripGoComments(string(raw))
	if strings.Contains(src, "hkdf") {
		t.Error("wrapkey.go references hkdf — OQ-3 decided to follow hub_keys.go's no-HKDF " +
			"precedent; adding a module to the one payload whose compromise is fleet-wide " +
			"is a deliberate operator decision, not a side effect")
	}
	if !strings.Contains(src, "hmac.New(sha256.New") {
		t.Error("wrapkey.go does not use hmac.New(sha256.New) — the expansion is not the " +
			"single-block HMAC-SHA256 shape OQ-3 selected")
	}
}

// --- POLICY CONSTANTS: recorded as policy, not derivation --------------------

// TestWrapKeyPolicyConstants asserts OQ-4's decided values AND that they carry
// the rationale comment the operator required. "A bare 90 is how the next
// reader assumes it was computed."
func TestWrapKeyPolicyConstants(t *testing.T) {
	if wrapKeyMaxAge != 90*24*time.Hour {
		t.Errorf("wrapKeyMaxAge = %v, want 90 days (OQ-4)", wrapKeyMaxAge)
	}
	if wrapKeyOverlap != 24*time.Hour {
		t.Errorf("wrapKeyOverlap = %v, want 24h (OQ-4)", wrapKeyOverlap)
	}
	if wrapKeyOverlap >= wrapKeyMaxAge {
		t.Error("the overlap window is not shorter than the key max age — a key would be " +
			"retained past the life of its successor")
	}
	raw, err := os.ReadFile("wrapkey.go")
	if err != nil {
		t.Fatalf("read wrapkey.go: %v", err)
	}
	src := string(raw)
	idx := strings.Index(src, "wrapKeyMaxAge  = 90")
	if idx < 0 {
		t.Fatal("could not locate the wrapKeyMaxAge declaration — this test is not reading " +
			"what it thinks it is")
	}
	preamble := src[:idx]
	for _, want := range []string{"POLICY CHOICES", "not derived", "changed"} {
		if !strings.Contains(preamble, want) {
			t.Errorf("the comment above wrapKeyMaxAge does not record %q — OQ-4 requires these "+
				"constants carry a comment saying they are policy choices with no derivation "+
				"behind them, what they trade off, and that they are safe to change deliberately", want)
		}
	}
}

// --- SPOKE KEY LIFECYCLE ------------------------------------------------------

// TestEnsureWrapKeysPersistsBeforeUse: the key returned on first boot must
// already be on disk. A key used but not persisted republishes as a DIFFERENT
// key after the next pod roll, which the hub correctly refuses as a pin
// mismatch — turning a write failure into an operator-intervention event on a
// cluster the hub cannot reach into.
func TestEnsureWrapKeysPersistsBeforeUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hive-wrap-key")
	now := time.Now()

	keys, fresh, err := ensureSpokeWrapKeys(path, now)
	if err != nil {
		t.Fatalf("ensureSpokeWrapKeys: %v", err)
	}
	if !fresh {
		t.Error("first boot did not report generating a fresh keypair")
	}
	if !keys.current.valid() {
		t.Fatal("no current key returned")
	}

	reloaded, err := loadSpokeWrapKeys(path, now)
	if err != nil {
		t.Fatalf("the key ensure() returned was not on disk: %v", err)
	}
	if reloaded.current.publicKey().Hex() != keys.current.publicKey().Hex() {
		t.Error("the persisted key differs from the one returned — the spoke would publish one " +
			"key and republish a different one after a roll")
	}
}

// TestWrapKeyFileMode: the private key must be 0600, matching spokeAppKeyPath's
// stated rationale.
func TestWrapKeyFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hive-wrap-key")
	if _, _, err := ensureSpokeWrapKeys(path, time.Now()); err != nil {
		t.Fatalf("ensureSpokeWrapKeys: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// ASSERT THE LITERAL, NOT THE CONSTANT. Comparing fi.Mode() against
	// spokeWrapKeyFileMode is a tautology: widening the constant to 0644 moves
	// both sides of the comparison and the test passes. (Verified: the
	// regression replay for this property PASSED while neutered when written
	// that way.) The invariant is 0600 specifically, so 0600 is what is written
	// here.
	const wantMode os.FileMode = 0o600
	if got := fi.Mode().Perm(); got != wantMode {
		t.Errorf("wrap key file mode = %04o, want %04o — the wrapping private key must never be "+
			"readable by anything else sharing the PVC or the pod", got, wantMode)
	}
	if spokeWrapKeyFileMode != wantMode {
		t.Errorf("spokeWrapKeyFileMode = %04o, want %04o — the constant itself was widened",
			spokeWrapKeyFileMode, wantMode)
	}
	// NOT ASSERTED HERE: the mode of the containing directory. In production
	// that is /data, the PVC mount, whose mode the spoke does not own — and in
	// this test it is t.TempDir(), which pre-exists at 0755 so MkdirAll's mode
	// never applies. Asserting on it would test the harness, not the code. The
	// 0600 file mode is what actually protects the key from anything else
	// sharing the PVC or the pod, exactly as for spokeAppKeyPath.
}

// TestPodRollWithIntactPVCIsNoOp: same key, same publication. This is the
// property that makes ordinary restarts invisible to the hub-side pin.
func TestPodRollWithIntactPVCIsNoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hive-wrap-key")
	now := time.Now()
	first, _, err := ensureSpokeWrapKeys(path, now)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	second, fresh, err := ensureSpokeWrapKeys(path, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("after roll: %v", err)
	}
	if fresh {
		t.Error("a pod roll with the PVC intact regenerated the keypair — every restart would " +
			"then require an operator re-pin")
	}
	if first.current.publicKey().Hex() != second.current.publicKey().Hex() {
		t.Error("the published public key changed across a pod roll")
	}
}

// TestPVCLossGeneratesFreshKey: PVC loss is indistinguishable from first boot BY
// CONSTRUCTION. The spoke generates and publishes a new key; whether the hub
// accepts it is emphatically not decided here.
func TestPVCLossGeneratesFreshKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive-wrap-key")
	now := time.Now()
	first, _, err := ensureSpokeWrapKeys(path, now)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("simulating PVC loss: %v", err)
	}
	second, fresh, err := ensureSpokeWrapKeys(path, now)
	if err != nil {
		t.Fatalf("after PVC loss: %v", err)
	}
	if !fresh {
		t.Error("PVC loss did not produce a fresh keypair")
	}
	if first.current.publicKey().Hex() == second.current.publicKey().Hex() {
		t.Error("PVC loss reproduced the same key — impossible unless generation is deterministic, " +
			"which would mean the keypair is derived rather than random")
	}
}

// TestMalformedKeyFileIsReplacedNotTrusted: a malformed file must route to
// generate-fresh, never to a partial-recovery path. A half-readable key file is
// not evidence of anything.
func TestMalformedKeyFileIsReplacedNotTrusted(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"not json", "definitely not json"},
		{"wrong version", `{"version":99,"current":"` + hex.EncodeToString(make([]byte, wrapKeyLen)) + `"}`},
		{"truncated key", `{"version":1,"current":"abcd"}`},
		{"empty object", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hive-wrap-key")
			if err := os.WriteFile(path, []byte(tc.body), spokeWrapKeyFileMode); err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, err := loadSpokeWrapKeys(path, time.Now()); err == nil {
				t.Fatal("loadSpokeWrapKeys accepted a malformed file")
			}
			keys, fresh, err := ensureSpokeWrapKeys(path, time.Now())
			if err != nil {
				t.Fatalf("ensureSpokeWrapKeys: %v", err)
			}
			if !fresh || !keys.current.valid() {
				t.Error("a malformed key file did not route to generate-fresh")
			}
		})
	}
}

// --- ROTATION OVERLAP: versioned, finite, and NOT an auto-re-pin -------------

// TestRotationRetainsOldPrivateKeyForOverlap is the behavioural half of
// non-weakenable property 1. The wrap-key rotation path retains the old PRIVATE
// key spoke-side rather than asking the hub to accept a new PUBLIC key — that
// construction is what keeps "a different key is never accepted on bearer
// authority" intact through the one place the design itself introduces a
// legitimate key change.
func TestRotationRetainsOldPrivateKeyForOverlap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hive-wrap-key")
	now := time.Now()
	keys, _, err := ensureSpokeWrapKeys(path, now)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	oldPub := keys.current.publicKey()

	// The hub sealed to the OLD (still pinned) key while this was in flight.
	inFlight, err := sealForSpoke(oldPub, "hive-alpha", 4, []byte("master-4"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	rotated, err := rotateSpokeWrapKeys(path, keys, now)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.current.publicKey().Hex() == oldPub.Hex() {
		t.Fatal("rotation did not produce a new current key")
	}
	if !rotated.previous.valid() {
		t.Fatal("rotation discarded the old private key — a master sealed to the pinned key and " +
			"still in flight becomes unopenable, stranding the spoke")
	}
	if got, err := openWithSpokeKeys(rotated, "hive-alpha", inFlight); err != nil {
		t.Errorf("an in-flight payload sealed to the pinned key did not open after rotation: %v", err)
	} else if string(got) != "master-4" {
		t.Errorf("overlap open returned %q", got)
	}
}

// TestOverlapKeyIsFiniteAndZeroMeansExpired: a ZERO PreviousExpires reads as
// ALREADY EXPIRED, never "never expires" — the same rule acceptableGenerations
// gives VerifyUntil. Reading it the other way would keep a superseded wrapping
// key acceptable forever, which is the F1/F2 failure mode.
func TestOverlapKeyIsFiniteAndZeroMeansExpired(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	privCur, _ := mustWrapKeypair(t)
	privPrev, prevPub := mustWrapKeypair(t)

	write := func(t *testing.T, expires time.Time) string {
		t.Helper()
		path := filepath.Join(dir, "k-"+strings.ReplaceAll(t.Name(), "/", "_"))
		f := wrapKeyFile{
			Version:         wrapKeyFileVersion,
			Current:         privCur.hex(),
			CurrentCreated:  now,
			Previous:        privPrev.hex(),
			PreviousExpires: expires,
		}
		blob, err := json.Marshal(f)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(path, blob, spokeWrapKeyFileMode); err != nil {
			t.Fatalf("write: %v", err)
		}
		return path
	}

	sealed, err := sealForSpoke(prevPub, "hive-alpha", 1, []byte("old-gen"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	// POSITIVE CONTROL: an unexpired overlap key IS retained and DOES open.
	t.Run("unexpired overlap opens", func(t *testing.T) {
		path := write(t, now.Add(time.Hour))
		keys, err := loadSpokeWrapKeys(path, now)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if !keys.previous.valid() {
			t.Fatal("an unexpired previous key was dropped")
		}
		if _, err := openWithSpokeKeys(keys, "hive-alpha", sealed); err != nil {
			t.Errorf("unexpired overlap key did not open an in-flight payload: %v", err)
		}
	})

	t.Run("zero expiry means already expired", func(t *testing.T) {
		path := write(t, time.Time{})
		keys, err := loadSpokeWrapKeys(path, now)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if keys.previous.valid() {
			t.Error("a previous key with a ZERO expiry was retained — a hand-edited file with " +
				"the field stripped would keep a superseded wrapping key acceptable forever")
		}
		if _, err := openWithSpokeKeys(keys, "hive-alpha", sealed); err == nil {
			t.Error("a payload sealed to a zero-expiry previous key still opened")
		}
	})

	t.Run("past expiry is excluded not warned about", func(t *testing.T) {
		path := write(t, now.Add(-time.Minute))
		keys, err := loadSpokeWrapKeys(path, now)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if keys.previous.valid() {
			t.Error("an EXPIRED previous key was retained")
		}
		if _, err := openWithSpokeKeys(keys, "hive-alpha", sealed); err == nil {
			t.Error("a payload sealed to an expired previous key still opened")
		}
	})
}

// TestWrapKeyRotationDueOnUnknownAge: a ZERO currentCreated reads as NEEDS
// ROTATION, not "brand new" — the same fail-closed direction as
// VerifyUntil.IsZero().
func TestWrapKeyRotationDueOnUnknownAge(t *testing.T) {
	priv, _ := mustWrapKeypair(t)
	now := time.Now()

	if wrapKeyNeedsRotation(spokeWrapKeys{current: priv, currentCreated: now}, now) {
		t.Error("positive control failed: a brand-new key was reported as needing rotation")
	}
	if !wrapKeyNeedsRotation(spokeWrapKeys{current: priv}, now) {
		t.Error("a key with a ZERO creation time was NOT reported as needing rotation — an " +
			"unknown age must not read as a safe age")
	}
	old := now.Add(-wrapKeyMaxAge - time.Minute)
	if !wrapKeyNeedsRotation(spokeWrapKeys{current: priv, currentCreated: old}, now) {
		t.Error("a key past wrapKeyMaxAge was not reported as needing rotation")
	}
	if !wrapKeyNeedsRotation(spokeWrapKeys{}, now) {
		t.Error("an absent key was not reported as needing rotation")
	}
}

// --- OPTION B REPLAY ----------------------------------------------------------

// TestSealedPayloadCarriesNoPlaintextSecret is the Option B replay for this
// stage. The invariant separating D from the rejected B is that no plaintext
// secret ever rides the delivery structure. sealedPayload must have no field
// that could carry one.
//
// This is checked against the STRUCT rather than an instance, because an
// instance test would pass simply by not populating the field. The failure mode
// being blocked is a future field being ADDED — e.g. a heartbeat key delivered
// alongside the master as a separate plaintext field, which OQ-2's
// implementation note calls out by name as the way (c) becomes Option B by the
// back door.
func TestSealedPayloadCarriesNoPlaintextSecret(t *testing.T) {
	// REFLECT OVER THE STRUCT, DO NOT MARSHAL AN INSTANCE. Marshalling a
	// zero-valued instance hides any field tagged `omitempty` — and a new
	// plaintext secret field would almost certainly be tagged that way, since
	// every optional field on HeartbeatResponse is. (Verified: the regression
	// replay for this property PASSED while neutered when written as a marshal
	// of an instance.) reflect.Type sees the field regardless of its tag.
	rt := reflect.TypeOf(sealedPayload{})
	allowed := map[string]bool{
		"EphemeralPub": true, "Nonce": true, "Ciphertext": true,
		"GenerationID": true, "Fingerprint": true,
	}
	seen := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		seen[name] = true
		if !allowed[name] {
			t.Errorf("sealedPayload gained an unreviewed field %q (json tag %q). Under OQ-2(c) "+
				"the new heartbeat key ships SEALED INSIDE the ciphertext, never as a separate "+
				"plaintext field on the delivery structure — a plaintext secret field here is "+
				"Option B by the back door, and Option B was rejected because it makes the "+
				"heartbeat bearer a master-exfiltration primitive",
				name, rt.Field(i).Tag.Get("json"))
		}
	}
	// Positive control on the subject: the fields this test knows about are
	// actually present, so a renamed or emptied struct would not silently pass.
	for k := range allowed {
		if !seen[k] {
			t.Errorf("expected field %q missing — this test is not inspecting the payload it "+
				"thinks it is, so its negative assertions above prove nothing", k)
		}
	}
	// And the marshalled form must carry no unexpected key either, which catches
	// a field renamed only at the json-tag level.
	blob, err := json.Marshal(sealedPayload{
		EphemeralPub: "aa", Nonce: "bb", Ciphertext: "cc", GenerationID: 1, Fingerprint: "dd",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(blob, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	allowedJSON := map[string]bool{
		"ephemeral_pub": true, "nonce": true, "ciphertext": true,
		"generation_id": true, "fingerprint": true,
	}
	for k := range fields {
		if !allowedJSON[k] {
			t.Errorf("sealedPayload marshals an unreviewed json key %q", k)
		}
	}
}

// TestHubCannotReopenWhatItSealed: the hub generates a fresh ephemeral per seal
// and discards it. If the hub retained any means of reopening, the entire
// premise (a non-confidential delivery channel is acceptable because only the
// recipient can open) would be false.
func TestHubCannotReopenWhatItSealed(t *testing.T) {
	priv, pub := mustWrapKeypair(t)
	sp, err := sealForSpoke(pub, "hive-alpha", 1, []byte("master"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	// The only public material the hub retains is the ephemeral public key and
	// the recipient public key. Neither yields the shared secret.
	ephPub, err := parseWrapPublicKey(sp.EphemeralPub)
	if err != nil {
		t.Fatalf("ephemeral pub did not parse: %v", err)
	}
	if ephPub.Hex() == pub.Hex() {
		t.Error("the ephemeral public key equals the recipient's — no ephemeral was generated")
	}
	// Positive control: the recipient CAN open.
	if _, err := openFromHub(priv, "hive-alpha", sp); err != nil {
		t.Fatalf("positive control failed: the recipient could not open: %v", err)
	}
}

// --- fingerprint --------------------------------------------------------------

// TestFingerprintIsStableDistinctAndNotKeyMaterial. A fingerprint is a PUBLIC
// identifier that appears on dashboards and in operator re-pin commands, so it
// must be domain-separated from the AEAD key derivation: a value an operator
// reads off a screen must never be usable as key material.
func TestFingerprintIsStableDistinctAndNotKeyMaterial(t *testing.T) {
	privA, pubA := mustWrapKeypair(t)
	_, pubB := mustWrapKeypair(t)

	if wrapKeyFingerprint(pubA) == "" {
		t.Fatal("fingerprint of a valid key was empty")
	}
	fpA1 := wrapKeyFingerprint(pubA)
	fpA2 := wrapKeyFingerprint(pubA)
	if fpA1 != fpA2 {
		t.Error("fingerprint is not stable")
	}
	if wrapKeyFingerprint(pubA) == wrapKeyFingerprint(pubB) {
		t.Error("two distinct keys share a fingerprint")
	}
	if wrapKeyFingerprint(wrapPublicKey{}) != "" {
		t.Error("an invalid key produced a non-empty fingerprint")
	}
	if strings.Contains(wrapKeyFingerprint(pubA), pubA.Hex()) {
		t.Error("the fingerprint contains the raw public key")
	}
	// It must not equal the AEAD key derived from any shared secret involving
	// this key. Different info labels guarantee this; assert it so a future
	// "simplify the labels" change is caught.
	_, ephPub := mustWrapKeypair(t)
	shared := make([]byte, wrapKeyLen)
	aeadKey := hex.EncodeToString(deriveWrapAEADKey(shared, ephPub, pubA))
	if wrapKeyFingerprint(pubA) == aeadKey {
		t.Error("the fingerprint collides with a derived AEAD key — the domain separation " +
			"between a public identifier and key material has been lost")
	}
	_ = privA
}

// stripGoComments removes // and /* */ comments so source assertions match on
// CODE rather than on the explanatory prose that necessarily names the very
// things being forbidden.
func stripGoComments(src string) string {
	src = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(src, "")
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
