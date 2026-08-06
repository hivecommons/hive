package hub

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"
)

func TestTerminalAssertionRoundTrip(t *testing.T) {
	secret := "shared-hub-secret-abc123"
	now := time.Unix(1_700_000_000, 0)
	tok := MintTerminalAssertion(secret, "alice", "owner", "hosted-hive-1", now)
	if tok == "" {
		t.Fatal("expected a token")
	}
	user, role, err := VerifyTerminalAssertion(secret, tok, "hosted-hive-1", now)
	if err != nil {
		t.Fatalf("verify failed: %v", err)
	}
	if user != "alice" || role != "owner" {
		t.Fatalf("got user=%q role=%q, want alice/owner", user, role)
	}
}

func TestTerminalAssertionRejectsWrongHive(t *testing.T) {
	secret := "s"
	now := time.Unix(1_700_000_000, 0)
	tok := MintTerminalAssertion(secret, "alice", "owner", "hive-A", now)
	if _, _, err := VerifyTerminalAssertion(secret, tok, "hive-B", now); err == nil {
		t.Fatal("expected rejection for an assertion minted for a different hive")
	}
}

func TestTerminalAssertionRejectsWrongSecret(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tok := MintTerminalAssertion("secret-1", "alice", "owner", "hive-1", now)
	if _, _, err := VerifyTerminalAssertion("secret-2", tok, "hive-1", now); err == nil {
		t.Fatal("expected rejection under a different secret")
	}
}

func TestTerminalAssertionRejectsExpired(t *testing.T) {
	secret := "s"
	now := time.Unix(1_700_000_000, 0)
	tok := MintTerminalAssertion(secret, "alice", "owner", "hive-1", now)
	later := now.Add(terminalAssertionTTL + ssoClockSkew + time.Second)
	if _, _, err := VerifyTerminalAssertion(secret, tok, "hive-1", later); err == nil {
		t.Fatal("expected rejection for an expired assertion")
	}
}

func TestTerminalAssertionRejectsNotYetValid(t *testing.T) {
	secret := "s"
	now := time.Unix(1_700_000_000, 0)
	// Minted "in the future" relative to the verifier's earlier clock.
	tok := MintTerminalAssertion(secret, "alice", "owner", "hive-1", now)
	earlier := now.Add(-(ssoClockSkew + time.Minute))
	if _, _, err := VerifyTerminalAssertion(secret, tok, "hive-1", earlier); err == nil {
		t.Fatal("expected rejection for a not-yet-valid assertion")
	}
}

func TestTerminalAssertionRejectsTamper(t *testing.T) {
	secret := "s"
	now := time.Unix(1_700_000_000, 0)
	tok := MintTerminalAssertion(secret, "alice", "read-write", "hive-1", now)
	b := []byte(tok)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	if _, _, err := VerifyTerminalAssertion(secret, string(b), "hive-1", now); err == nil {
		t.Fatal("expected rejection for a tampered assertion")
	}
}

func TestTerminalAssertionEmptyInputs(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	if MintTerminalAssertion("", "alice", "owner", "hive-1", now) != "" {
		t.Fatal("empty secret must not mint an assertion")
	}
	if MintTerminalAssertion("s", "", "owner", "hive-1", now) != "" {
		t.Fatal("empty username must not mint an assertion")
	}
	if MintTerminalAssertion("s", "alice", "owner", "", now) != "" {
		t.Fatal("empty hiveID must not mint an assertion")
	}
	if _, _, err := VerifyTerminalAssertion("", "x.y", "hive-1", now); err == nil {
		t.Fatal("empty secret must not verify")
	}
	if _, _, err := VerifyTerminalAssertion("s", "malformed", "hive-1", now); err == nil {
		t.Fatal("malformed assertion must not verify")
	}
}

// A terminal assertion must NOT verify as an SSO handoff token: the two are
// signed by different keys and carry different version strings, so cross-family
// replay fails. (The reverse — an SSO token verifying as a terminal assertion —
// cannot even be expressed once SSO is Ed25519, since MintSSOToken then takes a
// seed, not the terminal's HMAC key; the version-string mismatch is the durable
// guarantee, exercised by TestTerminalAssertionRejectsForeignVersion.)
func TestTerminalAssertionNotConfusableWithSSO(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// A terminal assertion, verified under the SSO path, must be rejected.
	term := MintTerminalAssertion("terminal-key", "alice", "owner", "hive-1", now)
	// The SSO verifier on this base takes a secret; even if the same string is
	// tried, the version ("hive-terminal-v1") is not an SSO version, so it fails.
	if _, _, err := VerifySSOToken("terminal-key", term, "hive-1", now); err == nil {
		t.Fatal("a terminal assertion must NOT verify as an SSO handoff token")
	}
}

// A token carrying any non-terminal version string is rejected even when signed
// with the correct terminal key — the version namespace is a hard gate.
func TestTerminalAssertionRejectsForeignVersion(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	key := "terminal-key"
	// Hand-mint a well-signed token whose version is an SSO version.
	forged := forgeTerminalToken(key, ssoClaims{
		Version: "hive-sso-v2", Username: "alice", Role: "owner", HiveID: "hive-1",
		IssuedAt: now.Unix(), Expiry: now.Add(terminalAssertionTTL).Unix(),
	})
	if _, _, err := VerifyTerminalAssertion(key, forged, "hive-1", now); err == nil {
		t.Fatal("a correctly-signed token with a foreign version must be rejected")
	}
}

// The terminal signing key is a DEDICATED symmetric sub-key, distinct from the
// master and from the SSO material.
func TestDeriveTerminalKeyIsDomainSeparated(t *testing.T) {
	master := "master-secret"
	got := deriveTerminalKeyFrom(master)
	if got == "" {
		t.Fatal("expected a derived key")
	}
	if got == master {
		t.Fatal("derived key must not equal the master")
	}
	// Deterministic and equal to the documented formula: HMAC-SHA256(master, info) hex.
	mac := hmac.New(sha256.New, []byte(master))
	mac.Write([]byte(infoTerminalKey))
	if want := hex.EncodeToString(mac.Sum(nil)); got != want {
		t.Fatalf("derived key = %q, want %q", got, want)
	}
	if deriveTerminalKeyFrom("") != "" {
		t.Fatal("empty master must derive to empty (fail closed)")
	}
}

// TerminalSigningKey resolution order: HIVE_TERMINAL_KEY > HIVE_SESSION_KEY >
// derive(HIVE_HUB_SECRET). Ensures the spoke picks the least-privilege key
// available and still works pre-#2761 (master only) and post-#2761 (session key).
func TestTerminalSigningKeyResolutionOrder(t *testing.T) {
	t.Setenv(EnvTerminalKey, "")
	t.Setenv(envSessionKey, "")
	t.Setenv(envHubSecret, "")
	if TerminalSigningKey() != "" {
		t.Fatal("no key sources → empty (fail closed)")
	}

	t.Setenv(envHubSecret, "master")
	if got, want := TerminalSigningKey(), deriveTerminalKeyFrom("master"); got != want {
		t.Fatalf("hub-secret fallback: got %q want derived %q", got, want)
	}

	t.Setenv(envSessionKey, "session-subkey")
	if TerminalSigningKey() != "session-subkey" {
		t.Fatal("session key must win over hub-secret derivation")
	}

	t.Setenv(EnvTerminalKey, "dedicated-terminal-key")
	if TerminalSigningKey() != "dedicated-terminal-key" {
		t.Fatal("dedicated terminal key must win over all")
	}
}

// forgeTerminalToken signs an arbitrary claims payload with the terminal key —
// a test-only helper to construct otherwise-valid tokens with a bad version.
func forgeTerminalToken(key string, claims ssoClaims) string {
	payload, _ := json.Marshal(claims)
	body := ssoB64.EncodeToString(payload)
	return body + "." + terminalSign(key, body)
}
