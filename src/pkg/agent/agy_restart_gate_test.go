package agent

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/crux/config"
)

// TestTokenRestartHealIsBackendScoped pins the gate that stopped agents being
// restart-looped out of a sign-in.
//
// configHasTokens() is backend-blind: it answers true when EITHER the shared
// claude or copilot credential is usable, regardless of which backend the agent
// in front of it runs. Gating the token-restart heal on it meant an agy agent
// sitting at its Google OAuth prompt — or a Copilot agent with only an unrelated
// Claude login present — was restarted because the shared CLAUDE login was
// valid. A restart cannot mint those sessions, so it looped; for agy every
// relaunch minted a fresh PKCE challenge, invalidating the code the operator was
// mid-way through pasting. Measured live: agy 15 restarts/hour and Copilot
// hives repeatedly re-badged after an operator reset.
func TestTokenRestartHealIsBackendScoped(t *testing.T) {
	// A live, usable shared claude credential — the thing that used to make the
	// backend-blind gate answer true for every agent on the hive.
	stageSharedClaudeCredential(t, map[string]any{
		"accessToken": "sk-ant-oat-live",
		"expiresAt":   time.Now().Add(6 * time.Hour).UnixMilli(),
	})

	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "agy"},
		"guide":   {Backend: "copilot"},
		"quality": {Backend: "claude"},
	}, discardLogger(), ProjectContext{ACMMLevel: 5})

	if configHasTokens() != true {
		t.Fatal("precondition: the backend-blind check must be true here — otherwise this test proves nothing")
	}

	if m.AgentHasValidCredential("scanner") {
		t.Fatal("an agy agent must NOT read as credentialed off a claude login: restarting it cannot mint a Google session, and each relaunch rotates the PKCE challenge the operator is using")
	}
	if m.AgentHasValidCredential("guide") {
		t.Fatal("a copilot agent must NOT read as credentialed off a claude login: restarting it cannot mint a Copilot session, so it must be left for the auth-required hold")
	}
	if !m.AgentHasValidCredential("quality") {
		t.Fatal("a claude agent with a usable shared credential must still qualify — the heal's real use case (#4596) must keep working")
	}
}
