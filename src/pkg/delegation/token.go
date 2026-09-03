package delegation

import (
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"strings"
)

// EnvChainsEnabled gates whether the hub publishes delegation verification keys.
const EnvChainsEnabled = "HIVE_DELEGATION_CHAIN_ENABLED"

// InfoChainEd25519Seed is the domain-separation label for the published
// delegation Ed25519 key derived from the hub master secret.
const InfoChainEd25519Seed = "hive-delegation-ed25519-v1"

// Enabled reports whether delegation key publication is enabled.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvChainsEnabled))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// SeedFromMaster derives the hex Ed25519 seed associated with delegation key
// publication. Returns "" for an empty master so keyless callers fail closed.
func SeedFromMaster(master string) string {
	return deriveDomainKey(master, InfoChainEd25519Seed)
}

// PublicKeyFromSeed expands a hex Ed25519 seed and returns only the hex public
// half. Returns "" for anything that is not a valid 32-byte seed.
func PublicKeyFromSeed(seedHex string) string {
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

// ValidPublicKeys filters candidates down to well-formed hex Ed25519 public
// keys, dropping empty or malformed entries.
func ValidPublicKeys(keys ...string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		raw, err := hex.DecodeString(k)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			continue
		}
		out = append(out, k)
	}
	return out
}
