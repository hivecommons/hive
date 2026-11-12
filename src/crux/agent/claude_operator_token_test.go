package agent

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/claude"
	"github.com/hivecommons/hive/crux/claude/claudetest"
	"github.com/hivecommons/hive/crux/config"
)

const operatorTokenSentinel = "sk-ant-oat01-OPERATOR-SENTINEL-7f3c9a1e5b"

// stageOperatorToken makes claude.OperatorToken hermetic: the env var holds
// token ("" for unset) and the default secret file points at a missing path.
func stageOperatorToken(t *testing.T, token string) {
	t.Helper()
	claudetest.SetOperatorToken(t, token)
}

// With a long-lived operator token set, an agent whose credential file is
// readable and fresh still runs on the operator token: that file is the
// 8-hour login that logs the fleet out (#17753).
func TestClaudeOperatorToken_InjectedOverAReadableCredential(t *testing.T) {
	stageSharedClaudeCredential(t, map[string]any{
		"accessToken": "sk-ant-oat-live",
		"expiresAt":   time.Now().Add(6 * time.Hour).UnixMilli(),
	})
	stageOperatorToken(t, operatorTokenSentinel)
	m, agent := claudeAgentFixture(t, "sk-ant-oat-snapshot-from-boot")

	pair, ok := envPairsByKey(m, agent)["CLAUDE_CODE_OAUTH_TOKEN"]
	if !ok || pair.Value != operatorTokenSentinel {
		t.Fatalf("pair = %+v, %v; want the operator token over a readable credential", pair, ok)
	}
	if !pair.Secret {
		t.Fatal("the operator token must be a secret pair (tmux set-environment, never a command line)")
	}
}

// With no credential file at all, the operator token is still injected, and it
// beats the dashboard snapshot the last-resort fallback would otherwise use.
func TestClaudeOperatorToken_InjectedWithNoCredentialFile(t *testing.T) {
	stageSharedClaudeCredential(t, nil)
	stageOperatorToken(t, operatorTokenSentinel)
	m, agent := claudeAgentFixture(t, "sk-ant-oat-from-dashboard-login")

	pair, ok := envPairsByKey(m, agent)["CLAUDE_CODE_OAUTH_TOKEN"]
	if !ok || pair.Value != operatorTokenSentinel || !pair.Secret {
		t.Fatalf("pair = %+v, %v; want the operator token as a secret pair", pair, ok)
	}
}

// A dashboard sign-in after boot only replaces claudeAuthToken; it must not
// switch agents back to the 8-hour token.
func TestClaudeOperatorToken_WinsOverALaterDashboardSignIn(t *testing.T) {
	stageSharedClaudeCredential(t, nil)
	stageOperatorToken(t, operatorTokenSentinel)
	m, agent := claudeAgentFixture(t, "")
	m.mu.Lock()
	m.claudeAuthToken = "sk-ant-oat-fresh-dashboard-login"
	m.mu.Unlock()

	pairs := m.agentEnvPairs(agent)
	var got []string
	for _, p := range pairs {
		if p.Key == "CLAUDE_CODE_OAUTH_TOKEN" {
			got = append(got, p.Value)
		}
	}
	if len(got) != 1 || got[0] != operatorTokenSentinel {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN values = %q; want exactly the operator token", got)
	}
}

// Unset: behaviour is exactly today's — no override over a readable
// credential, the dashboard snapshot when nothing is readable.
func TestClaudeOperatorToken_UnsetKeepsTodaysBehaviour(t *testing.T) {
	stageSharedClaudeCredential(t, map[string]any{
		"accessToken": "sk-ant-oat-live",
		"expiresAt":   time.Now().Add(6 * time.Hour).UnixMilli(),
	})
	stageOperatorToken(t, "")
	m, agent := claudeAgentFixture(t, "sk-ant-oat-snapshot-from-boot")
	if _, ok := envPairsByKey(m, agent)["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Fatal("with the operator token unset, nothing may be injected over a readable credential")
	}

	stageSharedClaudeCredential(t, nil)
	m, agent = claudeAgentFixture(t, "sk-ant-oat-from-dashboard-login")
	if pair := envPairsByKey(m, agent)["CLAUDE_CODE_OAUTH_TOKEN"]; pair.Value != "sk-ant-oat-from-dashboard-login" {
		t.Fatalf("with the operator token unset, the last-resort fallback must be unchanged: %+v", pair)
	}
}

// The token can come from the secret-file form and is never sent to an agent
// on another backend.
func TestClaudeOperatorToken_FileFormAndOtherBackends(t *testing.T) {
	stageSharedClaudeCredential(t, nil)
	stageOperatorToken(t, "")
	path := filepath.Join(t.TempDir(), "claude-oauth-token")
	if err := os.WriteFile(path, []byte(operatorTokenSentinel+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(claude.OperatorTokenFileEnvVar, path)

	m, agent := claudeAgentFixture(t, "")
	if pair := envPairsByKey(m, agent)["CLAUDE_CODE_OAUTH_TOKEN"]; pair.Value != operatorTokenSentinel {
		t.Fatalf("file form not injected: %+v", pair)
	}

	other := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "copilot"},
	}, discardLogger(), ProjectContext{ACMMLevel: 5})
	other.mu.RLock()
	otherAgent := other.agents["scanner"]
	other.mu.RUnlock()
	if _, ok := envPairsByKey(other, otherAgent)["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Fatal("a copilot agent must not receive the operator Claude token")
	}
}

// The manager never writes the operator token to its log while loading it
// and building an agent's environment.
func TestClaudeOperatorToken_NeverLogged(t *testing.T) {
	stageSharedClaudeCredential(t, nil)
	stageOperatorToken(t, operatorTokenSentinel)
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Model: "claude-sonnet-5"},
	}, logger, ProjectContext{ACMMLevel: 5})
	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()
	_ = m.agentEnvPairs(agent)
	_ = m.buildEnvPrefix(agent)

	if strings.Contains(buf.String(), operatorTokenSentinel) {
		t.Fatalf("operator token written to the hive log: %s", buf.String())
	}
	if strings.Contains(m.buildEnvPrefix(agent), operatorTokenSentinel) {
		t.Fatal("operator token reached the shell command-line env prefix")
	}
}

// The hive process holds HIVE_CLAUDE_OAUTH_TOKEN in its own environment, and
// every tmux server it starts inherits that globally. applySessionEnv must
// strip it (with -r, so the global value is hidden too) or every pane of every
// backend — copilot, codex, bob — could read the year-long token under its
// original name. The claude backend receives it only as CLAUDE_CODE_OAUTH_TOKEN.
func TestClaudeOperatorToken_StrippedFromInheritedPaneEnv(t *testing.T) {
	for _, k := range inheritedCredentialEnvVars {
		if k == claude.OperatorTokenEnvVar {
			return
		}
	}
	t.Fatalf("%s is not in inheritedCredentialEnvVars; it would reach every agent pane via the tmux global environment", claude.OperatorTokenEnvVar)
}

// On the long-lived token a fresh dashboard sign-in on disk proves nothing
// about the credential the agent uses, so login text on its pane is the token
// being rejected and must reach the operator, not the restart heal (#17759).
func TestClaudeOperatorToken_CredentialFileProvesNothing(t *testing.T) {
	stageSharedClaudeCredential(t, map[string]any{
		"accessToken": "sk-ant-oat-live",
		"expiresAt":   time.Now().Add(6 * time.Hour).UnixMilli(),
	})
	stageOperatorToken(t, "")
	m, agent := claudeAgentFixture(t, "")
	if !m.AgentHasValidCredential(agent.Name) {
		t.Fatal("precondition: without the operator token a usable shared credential must qualify")
	}

	stageOperatorToken(t, operatorTokenSentinel)
	m, agent = claudeAgentFixture(t, "")
	if m.AgentHasValidCredential(agent.Name) {
		t.Fatal("on the long-lived token the dashboard credential file must not read as proof")
	}
}
