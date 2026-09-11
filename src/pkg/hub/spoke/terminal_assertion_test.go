package spoke

import (
	"testing"
	"time"
)

// The terminal-assertion unit and N3 regression tests live with the
// implementation in pkg/terminalassert. This test stays here because it needs
// the SSO verifier, which is hub-only.
//
// A terminal assertion must NOT verify as an SSO handoff token: the two are
// signed by different keys and carry different version strings, so cross-family
// replay fails. (The reverse — an SSO token verifying as a terminal assertion —
// cannot even be expressed once SSO is Ed25519, since MintSSOToken then takes a
// seed, not the terminal's HMAC key; the version-string mismatch is the durable
// guarantee, exercised by terminalassert's TestTerminalAssertionRejectsForeignVersion.)
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

func TestSpokeTerminalSigningKeyDelegatesToTerminalassert(t *testing.T) {
	t.Setenv(EnvTerminalKey, "explicit-terminal-key")
	t.Setenv("HIVE_HUB_SECRET", "master-secret")
	t.Setenv(EnvHiveID, "hive-1")

	if got := TerminalSigningKey(); got != "explicit-terminal-key" {
		t.Fatalf("TerminalSigningKey() = %q, want explicit terminal key", got)
	}
}

func TestSpokeTerminalAssertionWrappersMintAndVerify(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	token := MintTerminalAssertion("terminal-key", "alice", "owner", "hive-1", now)
	if token == "" {
		t.Fatal("MintTerminalAssertion returned empty token")
	}

	user, role, err := VerifyTerminalAssertion("terminal-key", token, "hive-1", now)
	if err != nil {
		t.Fatalf("VerifyTerminalAssertion returned error: %v", err)
	}
	if user != "alice" || role != "owner" {
		t.Fatalf("VerifyTerminalAssertion = user %q role %q, want alice/owner", user, role)
	}
}

func TestSpokeTerminalAssertionWrappersFailClosed(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	if got := MintTerminalAssertion("", "alice", "owner", "hive-1", now); got != "" {
		t.Fatalf("MintTerminalAssertion with empty key = %q, want empty", got)
	}

	token := MintTerminalAssertion("terminal-key", "alice", "owner", "hive-1", now)
	if _, _, err := VerifyTerminalAssertion("", token, "hive-1", now); err == nil {
		t.Fatal("VerifyTerminalAssertion with empty key succeeded")
	}
	if _, _, err := VerifyTerminalAssertion("terminal-key", token, "other-hive", now); err == nil {
		t.Fatal("VerifyTerminalAssertion accepted token for another hive")
	}
}
