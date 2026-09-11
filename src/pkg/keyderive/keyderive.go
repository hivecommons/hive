// Package keyderive is the stdlib-only leaf holding hive's wire-compat
// symmetric key derivation and Ed25519 seed-to-public-key expansion.
//
// Before this package existed, the same nine-line HMAC-SHA256 construction was
// hand-copied byte-identically into pkg/delegation (deriveDomainKey) and
// pkg/hub (deriveDomainKey), and the Ed25519 seed expansion was similarly
// duplicated as pkg/delegation.PublicKeyFromSeed and pkg/hub's
// ssoPublicKeyFromSeed. pkg/hub's per-hive derivation was already
// single-sourced in pkg/terminalassert (DerivePerHiveKey); that
// implementation now lives here too, with pkg/terminalassert reduced to a
// thin wrapper, so there is exactly ONE stdlib-only implementation of each
// primitive and every consumer (hub, delegation, terminalassert) delegates to
// it (#6643).
//
// Byte-identical derivation across independent processes matters here in a
// way it would not for an ordinary helper: the hub derives a domain's signing
// key with one call site and a spoke — or a third-party verifier holding only
// the hub's published public key — must derive (or verify against) the exact
// same bytes with a different call site. If any two copies of this logic ever
// disagreed, the failure would not be a compile error or a panic; it would be
// a hub that publishes a public key that verifies nothing, surfacing only as
// "every chain is unverifiable" fleet-wide, with no error anywhere near the
// cause. Consolidating into one leaf package makes that class of drift
// impossible rather than merely tested against.
//
// This package is deliberately stdlib-only (crypto/ed25519, crypto/hmac,
// crypto/sha256, encoding/hex) and imports nothing else in the module, so any
// package — no matter how close to the leaves of the import graph it must
// stay — can depend on it without risking an import cycle.
package keyderive

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// DomainKey returns a domain-separated sub-key of master for the given info
// label, as lowercase hex: hex(HMAC-SHA256(master, info)).
//
// Returns "" for an empty master so callers keep the fail-closed contract
// every derivation site in hive relies on: no secret configured means no key.
func DomainKey(master, info string) string {
	if master == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(master))
	mac.Write([]byte(info))
	return hex.EncodeToString(mac.Sum(nil))
}

// PerHiveKey returns a sub-key bound to BOTH a trust domain and a single
// hive: hex(HMAC-SHA256(master, info || 0x00 || hiveID)).
//
// The 0x00 separator matters. Plain concatenation is ambiguous — ("a", "b|c")
// and ("a|b", "c") would hash identically — and hive IDs are
// operator-influenced, so an ambiguous encoding is a real (if narrow)
// collision lane. A NUL byte cannot appear in either input: info strings are
// compile-time constants and hive IDs are validated before reaching here.
//
// Returns "" for an empty master OR an empty hiveID: a keyless or
// identity-less caller must fail closed rather than silently sharing one key
// across every hive.
func PerHiveKey(master, info, hiveID string) string {
	if master == "" || hiveID == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(master))
	mac.Write([]byte(info))
	mac.Write([]byte{0})
	mac.Write([]byte(hiveID))
	return hex.EncodeToString(mac.Sum(nil))
}

// Ed25519PublicKeyFromSeed expands a hex Ed25519 seed and returns ONLY the
// hex-encoded 32-byte public half. Returns "" if seedHex is not a valid
// 32-byte seed.
//
// The private half is a local that goes out of scope; nothing in this
// function can return it. Callers deriving a signing seed with DomainKey (or
// PerHiveKey) and expanding it here get a deterministic keypair with no new
// secret to store, escrow, or rotate.
func Ed25519PublicKeyFromSeed(seedHex string) string {
	seed, err := hex.DecodeString(strings.TrimSpace(seedHex))
	if err != nil || len(seed) != ed25519.SeedSize {
		return ""
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return ""
	}
	return hex.EncodeToString(pub)
}
