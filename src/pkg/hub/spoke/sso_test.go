package spoke

import (
	"testing"
	"time"
)

// ssoTestKeypair derives a matching {signing seed, public key} pair the way the
// hub does from a master. mint uses the seed (private material, hub-only); verify
// uses the public key (what a spoke is injected).
func ssoTestKeypair(master string) (seedHex, pubHex string) {
	seedHex = SSOSigningSeedFromMaster(master)
	pubHex = ssoPublicKeyFromSeed(seedHex)
	return seedHex, pubHex
}

func TestSSOTokenRoundTrip(t *testing.T) {
	seed, pub := ssoTestKeypair("shared-hub-secret-abc123")
	now := time.Unix(1_700_000_000, 0)
	tok := MintSSOToken(seed, "alice", "owner", "hosted-hive-1", now)
	if tok == "" {
		t.Fatal("expected a token")
	}
	user, role, err := VerifySSOToken(pub, tok, "hosted-hive-1", now)
	if err != nil {
		t.Fatalf("verify failed: %v", err)
	}
	if user != "alice" || role != "owner" {
		t.Fatalf("got user=%q role=%q, want alice/owner", user, role)
	}
}

// TestSSOSpokeWithPublicKeyCannotMint is the core C2 follow-up guarantee: a spoke
// holds ONLY the Ed25519 public key. Handing that public key to the mint path must
// NOT produce a token that verifies — the spoke has no private material to sign
// with, so it cannot forge an SSO-as-any-owner handoff for its own hive.
func TestSSOSpokeWithPublicKeyCannotMint(t *testing.T) {
	seed, pub := ssoTestKeypair("the-hub-master")
	now := time.Unix(1_700_000_000, 0)

	// A spoke, holding only `pub`, attempts to mint. Whatever comes out must not
	// verify against the real public key.
	forged := MintSSOToken(pub, "attacker", "owner", "hive-x", now)
	if _, _, err := VerifySSOToken(pub, forged, "hive-x", now); err == nil {
		t.Fatal("a token minted with only the public key must NOT verify")
	}

	// Sanity: the real private seed DOES mint a token the public key verifies, so
	// the negative above is about lacking the private key, not a broken primitive.
	good := MintSSOToken(seed, "alice", "owner", "hive-x", now)
	if _, _, err := VerifySSOToken(pub, good, "hive-x", now); err != nil {
		t.Fatalf("hub-minted token failed to verify with the public key: %v", err)
	}
}

func TestSSOTokenRejectsWrongHive(t *testing.T) {
	seed, pub := ssoTestKeypair("s")
	now := time.Unix(1_700_000_000, 0)
	tok := MintSSOToken(seed, "alice", "owner", "hive-A", now)
	if _, _, err := VerifySSOToken(pub, tok, "hive-B", now); err == nil {
		t.Fatal("expected rejection for a token minted for a different hive")
	}
}

func TestSSOTokenRejectsWrongKey(t *testing.T) {
	seed1, _ := ssoTestKeypair("secret-1")
	_, pub2 := ssoTestKeypair("secret-2")
	now := time.Unix(1_700_000_000, 0)
	tok := MintSSOToken(seed1, "alice", "owner", "hive-1", now)
	if _, _, err := VerifySSOToken(pub2, tok, "hive-1", now); err == nil {
		t.Fatal("expected rejection under a different keypair's public key")
	}
}

func TestSSOTokenRejectsExpired(t *testing.T) {
	seed, pub := ssoTestKeypair("s")
	now := time.Unix(1_700_000_000, 0)
	tok := MintSSOToken(seed, "alice", "owner", "hive-1", now)
	// Verify well past expiry (TTL + skew).
	later := now.Add(ssoTokenTTL + ssoClockSkew + time.Second)
	if _, _, err := VerifySSOToken(pub, tok, "hive-1", later); err == nil {
		t.Fatal("expected rejection for an expired token")
	}
}

func TestSSOTokenRejectsTamper(t *testing.T) {
	seed, pub := ssoTestKeypair("s")
	now := time.Unix(1_700_000_000, 0)
	tok := MintSSOToken(seed, "alice", "read", "hive-1", now)
	// Flip a byte in the payload; signature must no longer match.
	b := []byte(tok)
	// mutate an early char that is part of the payload, not the '.' separator
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	if _, _, err := VerifySSOToken(pub, string(b), "hive-1", now); err == nil {
		t.Fatal("expected rejection for a tampered token")
	}
}

func TestSSOTokenEmptyInputs(t *testing.T) {
	seed, pub := ssoTestKeypair("s")
	now := time.Unix(1_700_000_000, 0)
	if MintSSOToken("", "alice", "owner", "hive-1", now) != "" {
		t.Fatal("empty seed must not mint a token")
	}
	if MintSSOToken(seed, "", "owner", "hive-1", now) != "" {
		t.Fatal("empty username must not mint a token")
	}
	// A non-seed value (not 32-byte hex) must fail closed on mint.
	if MintSSOToken("not-a-valid-seed", "alice", "owner", "hive-1", now) != "" {
		t.Fatal("a non-seed value must not mint a token")
	}
	if _, _, err := VerifySSOToken("", "x.y", "hive-1", now); err == nil {
		t.Fatal("empty public key must not verify")
	}
	if _, _, err := VerifySSOToken(pub, "malformed", "hive-1", now); err == nil {
		t.Fatal("malformed token must not verify")
	}
}
