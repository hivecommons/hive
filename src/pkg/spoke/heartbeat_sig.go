// Package spoke holds the shared hub<->spoke heartbeat-response signing
// contract (issue #7082) plus the spoke-side verifier that enforces it.
//
// BACKGROUND. The spoke->hub direction is authenticated per-hive
// (keyderive.PerHiveKey) and hub-minted SSO/delegation tokens are
// Ed25519-signed (pkg/delegation/token.go). The hub->spoke direction — the
// heartbeat RESPONSE, which carries config and, for hosted spokes, GitHub App
// credentials — was authenticated by the TLS channel ALONE. A TLS-terminating
// middlebox or a misconfigured HIVE_HUB_URL could therefore deliver arbitrary
// config/credentials to a spoke, and a response captured for hive A could be
// replayed to hive B or an old response replayed to roll config back. This was
// raised in the CNCF TAG-Security second review (cncf/toc#2286).
//
// FIX. The hub signs the heartbeat response body with the SAME Ed25519 key it
// already derives from the master seed for SSO/session tokens
// (infoSSOEd25519Seed); spokes already hold the matching public key as
// HIVE_SSO_PUBLIC_KEY. NO new key, NO new distribution, NO new primitive — the
// construction is copied deliberately from pkg/delegation/token.go: the
// signature is checked before any payload byte is trusted, and the binding
// fields (hive_id, seq, timestamp) live INSIDE the signed body so they are
// authenticated by the same signature that authenticates the body.
package spoke

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"time"
)

// SigVersion versions the signed-heartbeat DOMAIN so the format can evolve
// without a verifier ever silently accepting a foreign-domain artifact. It is
// carried in the signed body (HeartbeatSigMeta.Version) and checked on verify.
const SigVersion = 1

// SigHeader is the HTTP response header that carries the detached Ed25519
// signature over the heartbeat response BODY. A detached signature (rather than
// a field inside the body) avoids the circularity of signing a body that
// contains its own signature: the signed message is exactly the bytes on the
// wire, and the verifier re-hashes the EXACT bytes it received rather than
// re-encoding JSON — the same reason pkg/delegation/token.go signs over a fixed
// string instead of re-marshalled JSON.
const SigHeader = "X-Hive-Heartbeat-Signature"

// HeartbeatSigMeta is the binding carried INSIDE the signed response body (as
// the sig_* fields on hub.HeartbeatResponse). Because these fields are part of
// the body bytes the signature covers, a valid signature authenticates them:
//
//   - HiveID defeats cross-hive replay — a response signed for hive A carries
//     HiveID "A", so a spoke serving hive B rejects it (A != B) even though the
//     signature itself is a genuine hub signature.
//   - Seq (monotonic per hive) and SignedAt (unix seconds) defeat rollback
//     replay — a spoke rejects a Seq at or below the highest it has already
//     accepted, so an old captured response cannot roll config back.
//
// This type documents the contract; the fields are declared on
// hub.HeartbeatResponse itself so they marshal into the one body that gets
// signed. Verify below reads them back off the parsed response.
type HeartbeatSigMeta struct {
	HiveID   string
	Seq      int64
	SignedAt int64
	Version  int
}

// sigB64 encodes the signature without padding, matching pkg/delegation's
// chainB64 / pkg/hub's ssoB64 so a signature is header-safe.
var sigB64 = base64.RawURLEncoding

// SignBody returns the detached, base64url-encoded Ed25519 signature over the
// heartbeat response body bytes, for the hub's hex SSO signing seed.
//
// PRIVATE MATERIAL IN, PUBLIC ARTIFACT OUT. seedHex is the hub-only signing
// seed (hub.HubServer.ssoSigningSeed()); it never leaves the hub. The returned
// signature is safe to place in a response header.
//
// Returns "" — never a partial or bogus signature — for an empty/invalid seed,
// so a keyless hub emits an UNSIGNED response (which spokes accept under the
// staged rollout until they have seen a signed one). A caller that gets ""
// simply sets no header. There is deliberately no way to sign with junk that
// would never verify: a PUBLIC key mistakenly passed here (also 64 hex chars)
// is rejected as not-a-seed rather than used.
func SignBody(seedHex string, body []byte) string {
	seedHex = strings.TrimSpace(seedHex)
	if seedHex == "" {
		return ""
	}
	seed, err := hex.DecodeString(seedHex)
	if err != nil || len(seed) != ed25519.SeedSize {
		return ""
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return sigB64.EncodeToString(ed25519.Sign(priv, body))
}

// verifyBodySignature checks a detached signature over body against ONE hex
// Ed25519 public key. Fails closed on every mismatch: unusable key, malformed
// signature, or bad signature.
//
// Length of the key is checked BEFORE use because ed25519.Verify PANICS on a
// public key of the wrong size — the same crash-guard rationale as
// pkg/hub/validPublicKeys and pkg/delegation.ValidPublicKeys.
func verifyBodySignature(pubHex, sig string, body []byte) bool {
	pubHex = strings.TrimSpace(pubHex)
	if pubHex == "" {
		return false
	}
	pub, err := hex.DecodeString(pubHex)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	raw, err := sigB64.DecodeString(strings.TrimSpace(sig))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), body, raw)
}

// verifyBodyAcrossKeys is bounded trial verification against every published
// key a spoke holds (current-then-previous), mirroring
// hub.VerifySSOTokenAcrossKeys / pkg/delegation.VerifyTokenAcrossKeys.
//
// The loop is bounded by the number of published keys (2 during a rotation
// window). A malformed key costs one wasted candidate and cannot prevent a good
// one from verifying.
func verifyBodyAcrossKeys(pubHexes []string, sig string, body []byte) bool {
	for _, k := range pubHexes {
		if verifyBodySignature(k, sig, body) {
			return true
		}
	}
	return false
}

// nowOrDefault keeps callers terse in tests while production passes a real
// clock, matching the now-injection style of pkg/delegation.MintToken.
func nowOrDefault(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now()
	}
	return now
}
