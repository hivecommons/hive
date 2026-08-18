package hub

import (
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestEd25519SSOKeysAndToken(t *testing.T) {
	master := "fleet-master-secret"
	seed := SSOSigningSeedFromMaster(master)
	if seed == "" || seed != (&HubServer{hubSecret: master}).ssoSigningSeed() {
		t.Fatalf("derived seed mismatch: %q", seed)
	}
	if seed == deriveDomainKey(master, infoSSOKey) {
		t.Fatal("ed25519 SSO seed must be domain-separated from legacy HMAC SSO key")
	}
	pubHex := ssoPublicKeyFromSeed(seed)
	if pubHex == "" {
		t.Fatal("expected public key from derived seed")
	}
	pubBytes, err := hex.DecodeString(pubHex)
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		t.Fatalf("invalid public key hex %q", pubHex)
	}
	now := time.Unix(1_700_000_000, 0)
	tok := MintSSOToken(seed, "alice", "owner", "hosted-hive", now)
	parts := strings.Split(tok, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Fatalf("malformed ed25519 token %q", tok)
	}
	sig, err := ssoB64.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("signature is not base64url: %v", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pubBytes), []byte(parts[0]), sig) {
		t.Fatal("ed25519 token signature does not verify with derived public key")
	}
	if _, _, err := VerifySSOToken(deriveDomainKey(master, infoSSOKey), tok, "hosted-hive", now); err == nil {
		t.Fatal("legacy HMAC verifier must not accept an Ed25519 token")
	}
}

func TestEd25519SSOGuardsAndHiveSelection(t *testing.T) {
	seed := SSOSigningSeedFromMaster("master")
	now := time.Unix(1_700_000_000, 0)
	for _, tc := range []struct {
		name string
		seed string
		user string
		hive string
	}{
		{"empty seed", "", "alice", "h"},
		{"garbage seed", "not-hex", "alice", "h"},
		{"short seed", "abcd", "alice", "h"},
		{"empty user", seed, "", "h"},
		{"empty hive", seed, "alice", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MintSSOToken(tc.seed, tc.user, "owner", tc.hive, now); got != "" {
				t.Fatalf("MintSSOToken = %q, want empty", got)
			}
		})
	}
	if ssoPublicKeyFromSeed("") != "" || ssoPublicKeyFromSeed("abcd") != "" || SSOSigningSeedFromMaster("") != "" {
		t.Fatal("invalid or empty key material should fail closed")
	}
}
