package main

// Lives on the host side because it loads a full hive.yaml through
// config.LoadWithOverrides, which pkg/proxy (inside the Crux boundary) no
// longer imports; the proxy's own agent_repo_scope_test.go pins the predicate
// against a hand-built scope.

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/crux/proxy"
)

func TestAgentRepoScopeRefusal_UsesReposFromAgentSpec(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "schema.yaml")
	if err := os.WriteFile(specPath, []byte(`name: schema
backend: copilot
model: auto
repos: [console]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(configPath, []byte(fmt.Sprintf(`project:
  org: acme
  repos: [console, dashboard]
agents:
  schema:
    backend: copilot
    model: auto
    agent_spec: %q
github:
  token: test
`, specPath)), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadWithOverrides(configPath, "-")
	if err != nil {
		t.Fatalf("LoadWithOverrides: %v", err)
	}

	if _, refused := proxy.AgentRepoScopeRefusal(cfg.AgentServesRepo, "schema", http.MethodPost, "/repos/acme/console/issues"); refused {
		t.Fatal("refused an in-scope repo from the agent spec")
	}
	if _, refused := proxy.AgentRepoScopeRefusal(cfg.AgentServesRepo, "schema", http.MethodPost, "/repos/acme/dashboard/issues"); !refused {
		t.Fatal("allowed an out-of-scope repo because the spec repos were not wired into config")
	}
}
