package mint

import (
	"testing"
	"time"
)

// testMinter builds a minter with a fresh key for agent-token tests.
func testMinter(t *testing.T) *Minter {
	t.Helper()
	key, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	m, err := NewMinter(key, "https://hive.example/mint", WithHiveID("hive-test"))
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	return m
}

func TestScopesForTier(t *testing.T) {
	cases := map[string][]string{
		"advisor":     {ScopeIssuesRead},
		"newcomer":    {ScopeIssuesRead, ScopeIssuesWrite},
		"contributor": {ScopeIssuesRead, ScopeIssuesWrite, ScopeContentsWrite, ScopePullsWrite},
		"trusted":     {ScopeIssuesRead, ScopeIssuesWrite, ScopeContentsWrite, ScopePullsWrite, ScopeMerge},
	}
	for tier, want := range cases {
		got := ScopesForTier(tier)
		if len(got) != len(want) {
			t.Fatalf("tier %q: got %v want %v", tier, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("tier %q scope %d: got %q want %q", tier, i, got[i], want[i])
			}
		}
	}
}

func TestScopesForTier_UnknownFailsClosed(t *testing.T) {
	got := ScopesForTier("totally-unknown-tier")
	if len(got) != 1 || got[0] != ScopeIssuesRead {
		t.Fatalf("unknown tier should fail closed to read-only, got %v", got)
	}
}

func TestScopesForTier_ReturnsFreshCopy(t *testing.T) {
	a := ScopesForTier("trusted")
	a[0] = "mutated"
	b := ScopesForTier("trusted")
	if b[0] == "mutated" {
		t.Fatalf("ScopesForTier must return an independent copy")
	}
}

// TestMintAgentToken_EnabledMintsVerifiableScopedToken is the core "enabled →
// mints a verifiable scoped token for an agent" case.
func TestMintAgentToken_EnabledMintsVerifiableScopedToken(t *testing.T) {
	m := testMinter(t)
	am := NewAgentMinter(m, 5*time.Minute)
	if !am.Enabled() {
		t.Fatalf("AgentMinter with a real minter should be Enabled")
	}

	tok, err := am.MintAgentToken("scanner", "contributor")
	if err != nil {
		t.Fatalf("MintAgentToken: %v", err)
	}
	if tok == "" {
		t.Fatalf("expected a non-empty token")
	}

	claims, err := m.Verify(tok)
	if err != nil {
		t.Fatalf("minted token failed verification: %v", err)
	}
	if claims.Subject != "scanner" {
		t.Fatalf("subject = %q, want scanner", claims.Subject)
	}
	if claims.HiveID != "hive-test" {
		t.Fatalf("hive_id = %q, want hive-test", claims.HiveID)
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != agentTokenAudience {
		t.Fatalf("audience = %v, want [%s]", claims.Audience, agentTokenAudience)
	}
	want := ScopesForTier("contributor")
	if len(claims.Scopes) != len(want) {
		t.Fatalf("scopes = %v, want %v", claims.Scopes, want)
	}
	for i := range want {
		if claims.Scopes[i] != want[i] {
			t.Fatalf("scope %d = %q, want %q", i, claims.Scopes[i], want[i])
		}
	}
}

// TestMintAgentToken_DisabledReturnsNoTokenNoError is the "disabled → no token,
// no error" case.
func TestMintAgentToken_DisabledReturnsNoTokenNoError(t *testing.T) {
	am := NewAgentMinter(nil, 5*time.Minute)
	if am.Enabled() {
		t.Fatalf("AgentMinter with nil minter must be disabled")
	}
	tok, err := am.MintAgentToken("scanner", "trusted")
	if err != nil {
		t.Fatalf("disabled mint should not error, got %v", err)
	}
	if tok != "" {
		t.Fatalf("disabled mint should return no token, got %q", tok)
	}
}

func TestMintAgentToken_EmptyAgentFailsClosed(t *testing.T) {
	am := NewAgentMinter(testMinter(t), time.Minute)
	if _, err := am.MintAgentToken("", "trusted"); err == nil {
		t.Fatalf("empty agent name should fail closed with an error")
	}
}

func TestAgentMinter_NilReceiverDisabled(t *testing.T) {
	var am *AgentMinter
	if am.Enabled() {
		t.Fatalf("nil AgentMinter must report disabled")
	}
	tok, err := am.MintAgentToken("scanner", "trusted")
	if err != nil || tok != "" {
		t.Fatalf("nil AgentMinter should no-op, got tok=%q err=%v", tok, err)
	}
}

func TestMintAgentToken_TTLClampedToCap(t *testing.T) {
	m := testMinter(t)
	// Request a TTL well above the hard cap; the underlying minter clamps it.
	am := NewAgentMinter(m, 10*HardCapTTL)
	tok, err := am.MintAgentToken("worker", "advisor")
	if err != nil {
		t.Fatalf("MintAgentToken: %v", err)
	}
	claims, err := m.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	life := claims.ExpiresAt.Sub(claims.IssuedAt.Time)
	if life > HardCapTTL+clockSkewLeeway {
		t.Fatalf("token life %v exceeds hard cap %v", life, HardCapTTL)
	}
}
