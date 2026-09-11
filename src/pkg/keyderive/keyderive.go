// Package keyderive holds hive's wire-compatibility-critical key derivation
// primitives in one stdlib-only leaf package.
//
// The hub (pkg/hub), the spoke (pkg/hub/spoke), and the delegation minting
// path (pkg/delegation) must all derive byte-identical keys from the same
// master secret: the heartbeat bearer, the invite key, and the SSO/session/
// delegation Ed25519 seeds are minted on one side of the wire and verified on
// the other. Historically each package carried its own copy of these
// functions because pkg/hub imports most of the tree and the near-leaf
// packages could not import it without a cycle; the copies were pinned
// against each other only by tests, and only on one of the pairs. This
// package removes the cycle rationale: it imports nothing but the standard
// library, so every deriver can share the single implementation and drift is
// structurally impossible rather than merely tested for.
package keyderive

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// DomainKey returns a domain-separated sub-key of master for the given info
// label, as lowercase hex: HMAC-SHA256(master, info).
//
// Returns "" for an empty master so callers keep the fail-closed contract
// every derivation site in hive relies on: no secret configured means no key,
// which means nothing is minted and no public key is emitted.
func DomainKey(master, info string) string {
	if master == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(master))
	mac.Write([]byte(info))
	return hex.EncodeToString(mac.Sum(nil))
}

// PerHiveKey returns a per-hive domain-separated sub-key of master, as
// lowercase hex: HMAC-SHA256(master, info || 0x00 || hiveID). The 0x00
// separator keeps (info, hiveID) pairs unambiguous.
//
// Returns "" if master or hiveID is empty, preserving the same fail-closed
// contract as DomainKey.
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

// Ed25519PublicKeyFromSeed expands a hex-encoded Ed25519 seed and returns only
// the hex-encoded public half. Returns "" for anything that is not a valid
// 32-byte seed, so a missing or malformed seed publishes no key rather than a
// wrong one.
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
