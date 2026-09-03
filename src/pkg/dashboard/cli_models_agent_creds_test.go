package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claude"
)

// cli_models_agent_creds_test.go pins the #4699 fix on the dashboard side:
// model discovery must find a credential that exists ONLY in a per-agent home.
//
// The operator-visible symptom was a self-contradicting dashboard — the agent
// card showing "✓ logged in" (from pkg/agent's per-UID-aware probe) beside a
// model dropdown where every entry read "(common alias, unverified)" (from this
// probe, which knew only the shared path).

// writeCredsAt writes a credentials file in the CLI's on-disk shape at path.
func writeCredsAt(t *testing.T, path, token string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(claude.Credentials{ClaudeAIOAuth: &claude.OAuthTokens{
		AccessToken: token,
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestClaudeAPICredentialFindsPerAgentToken is the regression itself. The token
// lives only where a per-UID agent's own CLI would have written it: nothing is
// at the shared path, and nothing is under the DASHBOARD process's $HOME.
//
// Before the fix the probe consulted only those two, found nothing, and
// returned "no credential" — the all-unverified dropdown — on a hive that was
// fully authenticated.
func TestClaudeAPICredentialFindsPerAgentToken(t *testing.T) {
	withoutClaudeCredentials(t) // shared path and $HOME both empty

	agentHome := t.TempDir()
	agentCreds := filepath.Join(agentHome, ".claude", ".credentials.json")
	writeCredsAt(t, agentCreds, "per-agent-token")

	// The pre-fix probe: shared paths only.
	if _, _, ok := claudeAPICredential(nil); ok {
		t.Fatal("precondition: no credential should be reachable without the agent paths")
	}

	header, value, ok := claudeAPICredential([]string{agentCreds})
	if !ok {
		t.Fatal("per-agent credential not found — #4699 has regressed")
	}
	if header != "Authorization" {
		t.Errorf("header = %q, want Authorization (subscription OAuth bearer, not an API key)", header)
	}
	if value != "Bearer per-agent-token" {
		t.Errorf("value = %q, want the per-agent token as a bearer", value)
	}
}

// TestClaudeCredentialCandidatePathsOrdersAgentPathsFirst: the per-UID file is
// the one the agent's CLI actually writes, so it must be consulted before the
// shared legacy path — the same ordering pkg/agent's probe uses.
func TestClaudeCredentialCandidatePathsOrdersAgentPathsFirst(t *testing.T) {
	withoutClaudeCredentials(t)
	agentPath := filepath.Join(t.TempDir(), ".claude", ".credentials.json")

	got := claudeCredentialCandidatePaths([]string{agentPath})

	if len(got) == 0 || got[0] != agentPath {
		t.Fatalf("got %v, want the agent path first", got)
	}
	var sawShared bool
	for _, p := range got {
		if p == claudePodCredentialsPath {
			sawShared = true
		}
	}
	if !sawShared {
		t.Errorf("shared path dropped from %v — a correctly-bridged hive must still work", got)
	}
}

// TestClaudeCredentialCandidatePathsNilAgentPathsUnchanged: a manager-less
// embedding keeps exactly the pre-#4699 behavior.
func TestClaudeCredentialCandidatePathsNilAgentPathsUnchanged(t *testing.T) {
	withoutClaudeCredentials(t)

	got := claudeCredentialCandidatePaths(nil)
	if len(got) == 0 || got[0] != claudePodCredentialsPath {
		t.Fatalf("got %v, want the shared pod path first", got)
	}
}

// TestClaudeCredentialCandidatePathsDedupes: the agent manager may hand back a
// path that equals the shared one (a shared-home hive), and stat'ing it twice
// is pure waste.
func TestClaudeCredentialCandidatePathsDedupes(t *testing.T) {
	withoutClaudeCredentials(t)

	got := claudeCredentialCandidatePaths([]string{claudePodCredentialsPath, claudePodCredentialsPath})
	seen := map[string]bool{}
	for _, p := range got {
		if seen[p] {
			t.Errorf("duplicate %q in %v", p, got)
		}
		seen[p] = true
	}
}

// TestClaudeAPICredentialPrefersAPIKey: ANTHROPIC_API_KEY still wins over any
// file. The per-UID search must not have reordered the documented precedence.
func TestClaudeAPICredentialPrefersAPIKey(t *testing.T) {
	withoutClaudeCredentials(t)
	agentCreds := filepath.Join(t.TempDir(), ".claude", ".credentials.json")
	writeCredsAt(t, agentCreds, "per-agent-token")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")

	header, value, ok := claudeAPICredential([]string{agentCreds})
	if !ok || header != "x-api-key" || value != "sk-test" {
		t.Errorf("got (%q, %q, %v), want the API key to win", header, value, ok)
	}
}
