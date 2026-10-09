package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestApplyPackHonorsSpokeBackendsList covers #11310: a backend disabled by
// the spoke's backends allow/deny list is never placed by a pack apply, and a
// still pack-owned agent already on it is moved to the fallback.
func TestApplyPackHonorsSpokeBackendsList(t *testing.T) {
	tests := []struct {
		name        string
		backends    config.BackendsConfig
		wantBackend string
	}{
		{"deny falls back to hive default", config.BackendsConfig{Deny: []string{"copilot"}}, ""},
		{"allow falls back to first allowed", config.BackendsConfig{Allow: []string{"codex"}}, "codex"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newFullServer(t)
			enableScanner(t, srv)
			srv.deps.Config.Backends = tt.backends
			sc := srv.deps.Config.Agents["scanner"]
			sc.Backend = "copilot"
			sc.BackendOwner = config.FieldOwnerPack
			sc.Model = "claude-sonnet-4-6"
			sc.ModelOwner = config.FieldOwnerPack
			srv.deps.Config.Agents["scanner"] = sc

			// Twice: the second apply must be a stable no-op, not a flap back.
			for i := 0; i < 2; i++ {
				if _, err := srv.ApplyPack(3); err != nil {
					t.Fatalf("ApplyPack(3) #%d: %v", i, err)
				}
			}

			got := srv.deps.Config.Agents["scanner"]
			if got.Backend != tt.wantBackend {
				t.Errorf("scanner backend = %q, want %q", got.Backend, tt.wantBackend)
			}
			if got.Model != "" {
				t.Errorf("scanner model = %q, want \"\" (pack model belongs to the denied backend)", got.Model)
			}
			for name, ac := range srv.deps.Config.Agents {
				if !srv.deps.Config.BackendAllowed(ac.Backend) {
					t.Errorf("agent %q placed on disallowed backend %q", name, ac.Backend)
				}
			}
		})
	}
}

// TestApplyPackKeepsOperatorBackendDespiteBackendsList: an explicit operator
// placement is the operator's call; the pack never rewrites it.
func TestApplyPackKeepsOperatorBackendDespiteBackendsList(t *testing.T) {
	srv := newFullServer(t)
	enableScanner(t, srv)
	if _, err := srv.ApplyPack(3); err != nil {
		t.Fatalf("ApplyPack(3): %v", err)
	}
	srv.claimAgentFieldOwnership("scanner", "", "copilot")
	srv.deps.Config.Backends = config.BackendsConfig{Deny: []string{"copilot"}}
	if _, err := srv.ApplyPack(3); err != nil {
		t.Fatalf("ApplyPack(3) re-apply: %v", err)
	}
	if got := srv.deps.Config.Agents["scanner"].Backend; got != "copilot" {
		t.Errorf("operator backend rewritten: got %q, want copilot", got)
	}
}
