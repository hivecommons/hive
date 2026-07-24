package hub

import (
	"testing"
	"time"
)

func TestSSOTokenRoundTrip(t *testing.T) {
	secret := "shared-hub-secret-abc123"
	now := time.Unix(1_700_000_000, 0)
	tok := MintSSOToken(secret, "alice", "owner", "hosted-hive-1", now)
	if tok == "" {
		t.Fatal("expected a token")
	}
	user, role, err := VerifySSOToken(secret, tok, "hosted-hive-1", now)
	if err != nil {
		t.Fatalf("verify failed: %v", err)
	}
	if user != "alice" || role != "owner" {
		t.Fatalf("got user=%q role=%q, want alice/owner", user, role)
	}
}

func TestSSOTokenRejectsWrongHive(t *testing.T) {
	secret := "s"
	now := time.Unix(1_700_000_000, 0)
	tok := MintSSOToken(secret, "alice", "owner", "hive-A", now)
	if _, _, err := VerifySSOToken(secret, tok, "hive-B", now); err == nil {
		t.Fatal("expected rejection for a token minted for a different hive")
	}
}

func TestSSOTokenRejectsWrongSecret(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tok := MintSSOToken("secret-1", "alice", "owner", "hive-1", now)
	if _, _, err := VerifySSOToken("secret-2", tok, "hive-1", now); err == nil {
		t.Fatal("expected rejection under a different secret")
	}
}

func TestSSOTokenRejectsExpired(t *testing.T) {
	secret := "s"
	now := time.Unix(1_700_000_000, 0)
	tok := MintSSOToken(secret, "alice", "owner", "hive-1", now)
	// Verify well past expiry (TTL + skew).
	later := now.Add(ssoTokenTTL + ssoClockSkew + time.Second)
	if _, _, err := VerifySSOToken(secret, tok, "hive-1", later); err == nil {
		t.Fatal("expected rejection for an expired token")
	}
}

func TestSSOTokenRejectsTamper(t *testing.T) {
	secret := "s"
	now := time.Unix(1_700_000_000, 0)
	tok := MintSSOToken(secret, "alice", "read", "hive-1", now)
	// Flip a byte in the payload; signature must no longer match.
	b := []byte(tok)
	// mutate an early char that is part of the payload, not the '.' separator
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	if _, _, err := VerifySSOToken(secret, string(b), "hive-1", now); err == nil {
		t.Fatal("expected rejection for a tampered token")
	}
}

func TestSSOTokenEmptyInputs(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	if MintSSOToken("", "alice", "owner", "hive-1", now) != "" {
		t.Fatal("empty secret must not mint a token")
	}
	if MintSSOToken("s", "", "owner", "hive-1", now) != "" {
		t.Fatal("empty username must not mint a token")
	}
	if _, _, err := VerifySSOToken("", "x.y", "hive-1", now); err == nil {
		t.Fatal("empty secret must not verify")
	}
	if _, _, err := VerifySSOToken("s", "malformed", "hive-1", now); err == nil {
		t.Fatal("malformed token must not verify")
	}
}
