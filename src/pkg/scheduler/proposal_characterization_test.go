package scheduler

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/knowledge"
)

func TestNormalSchedulerCharacterizationCarriesPolicyProjectAndPrimer(t *testing.T) {
	policyRoot := t.TempDir()
	agentsDir := filepath.Join(policyRoot, "examples", "kubestellar", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	policy := "QUALITY_POLICY_MARKER\norg=${PROJECT_ORG}\nrepos=${AUTHORIZED_REPOS}\nknowledge=${KNOWLEDGE}\n"
	if err := os.WriteFile(filepath.Join(agentsDir, "quality.md"), []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total": 1,
			"results": []map[string]any{{
				"slug": "visual-regression", "title": "Project primer marker",
				"score": 0.99, "type": "pattern", "status": "verified",
				"confidence": 0.99, "snippet": "Preserve the repository fixture contract.",
			}},
		})
	}))
	defer server.Close()

	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "acme", Name: "console", PrimaryRepo: "console", Repos: []string{"console"}},
		Agents: map[string]config.AgentConfig{
			"quality": {Enabled: true, Role: "quality", KickTemplate: "quality.md"},
		},
		Policies: config.PoliciesConfig{LocalDir: policyRoot},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scheduler := New(cfg, logger)
	scheduler.SetPrimer(knowledge.NewPrimer([]knowledge.LayerConfig{
		{Type: knowledge.LayerProject, URL: server.URL, Shared: true},
	}, knowledge.PrimerConfig{MaxFacts: 5}, logger))

	issue := makeIssue("acme/console", 7, "Visual regression in account settings", "quality", 3, []string{"visual-regression"}, false)
	issues := []github.Issue{issue}
	message := scheduler.BuildAgentMessage("quality", issues, actionableWithIssues(issues))
	for _, marker := range []string{
		"[agent:quality]", "QUALITY_POLICY_MARKER", "org=acme",
		"acme/console", "Relevant Knowledge", "Project primer marker",
	} {
		if !strings.Contains(message, marker) {
			t.Errorf("normal scheduler message missing %q:\n%s", marker, message)
		}
	}
}
