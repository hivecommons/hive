package scheduler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// TestPrimeKnowledge_AppliesAgentScope proves the kick primer honours
// knowledge.agent_scopes: a project-layer fact never reaches an agent scoped
// to the org layer, while an unscoped agent still receives it.
func TestPrimeKnowledge_AppliesAgentScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"total": 1,
			"results": []map[string]interface{}{{
				"slug": "guard-join", "title": "Guard .join() against undefined", "score": 0.95,
				"type": "gotcha", "confidence": 0.95, "tags": []string{"typescript"},
				"snippet": "Always use (arr || []).join()",
			}},
		})
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	primer := knowledge.NewPrimer([]knowledge.LayerConfig{{Type: knowledge.LayerProject, URL: srv.URL, Shared: true}},
		knowledge.PrimerConfig{MaxFacts: 25}, logger)

	s := newScheduler()
	s.cfg.Agents = map[string]config.AgentConfig{"scanner-2": {ReplicaOf: "scanner"}}
	s.cfg.Knowledge.AgentScopes = map[string]config.KnowledgeAgentScope{
		"scanner": {Layers: []string{"org"}},
		"quality": {Layers: []string{"project"}, Types: []string{"gotcha"}},
	}
	s.SetPrimer(primer)
	issues := []github.Issue{makeIssue("org/repo", 1, "Fix hook crash", "", 5, []string{"typescript", "hooks"}, false)}

	for _, agent := range []string{"scanner", "scanner-2"} {
		if got := s.primeKnowledge(agent, issues); got != "" {
			t.Errorf("%s received out-of-scope knowledge:\n%s", agent, got)
		}
	}
	for _, agent := range []string{"quality", "outreach", ""} {
		if got := s.primeKnowledge(agent, issues); !strings.Contains(got, "Guard .join()") {
			t.Errorf("%s missing in-scope knowledge, got:\n%s", agent, got)
		}
	}
}
