package agent

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
)

func TestSharedUIDReadTokenLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name         string
		enabled, app bool
		uid          int
		wantCache    bool
	}{
		{"default off", false, true, 0, false},
		{"no App", true, false, 0, false},
		{"shared opt in", true, true, 0, true},
		{"dedicated UID unchanged", false, true, 2001, true},
		{"dedicated UID with opt in", true, true, 2001, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(map[string]config.AgentConfig{
				"worker": {Backend: "claude", Mode: "ISSUES_AND_PRS", Role: "governor"},
			}, discardLogger(), ProjectContext{ACMMLevel: 5, SharedUIDReadToken: tc.enabled, AppAuthoredPRs: true})
			fm := &fakeMinter{}
			if tc.app {
				m.SetAppAuth(fm)
			}
			a := m.agents["worker"]
			a.UID = tc.uid
			a.tmuxSession = "" // no real tmux processes in this lifecycle unit test
			a.State = StateRunning
			var cache string
			for _, pair := range m.agentEnvPairs(a) {
				if pair.Key == "HIVE_AGENT_TOKEN_CACHE" {
					cache = pair.Value
				}
				if tc.uid == 0 && pair.Key == "GITHUB_TOKEN" {
					t.Fatal("shared agent received MCP credential")
				}
			}
			if (cache != "") != tc.wantCache {
				t.Fatalf("cache path = %q, want enabled %v", cache, tc.wantCache)
			}
			if tc.wantCache && cache != crux.AgentTokenCachePath(crux.DefaultAgentTokenCacheDir, a.Name) {
				t.Fatalf("wrong cache path %q", cache)
			}
			wantTier := TokenTierForRole(m.agentMode(a), a.Config.Role)
			if tc.uid == 0 {
				wantTier = crux.SharedUIDReadTokenTier
			}
			checkMint := func(stage string, wantCalls int) {
				t.Helper()
				if !tc.wantCache {
					wantCalls = 0
				}
				if fm.calls != wantCalls || (wantCalls > 0 && fm.lastTier != wantTier) {
					t.Fatalf("%s: calls %d tier %q, want calls %d tier %q", stage, fm.calls, fm.lastTier, wantCalls, wantTier)
				}
			}
			m.mintAgentTokenUnlocked(context.Background(), a)
			checkMint("launch", 1)
			m.RefreshAgentTokens(context.Background())
			checkMint("periodic refresh", 2)
			err := m.RefreshAgentTokenFor(context.Background(), a.Name)
			if (err == nil) != tc.wantCache {
				t.Fatalf("recovery error = %v, want enabled %v", err, tc.wantCache)
			}
			checkMint("recovery", 3)
		})
	}
}
