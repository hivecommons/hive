package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/adminmcp"
)

// pr_models_read and contributor_model_stats_read answer with the JSON the
// dashboard gets from the same endpoints, for the window asked for, and a bad
// window is refused before any request reaches the router.
func TestDashboardAdminMCPModelEffectivenessReadsTheDashboardEndpoints(t *testing.T) {
	var seen []string
	answers := map[string]string{
		"/api/governor/pr-models?window=30d":      `{"window":"30d","min_merged_prs":3,"total":2,"buckets":[{"model":"gpt-6.1-sol","merged":1,"closed_unmerged":0,"open":1,"effectiveness":{"rank":1,"eligible":false},"agents":[{"agent":"scanner","merged":1}]}],"unknown":0,"fixer_queue":{"fixer":{"depth":0}}}`,
		"/api/contribute/run-stats/models?days=7": `{"window_days":7,"models":[{"model":"claude-sonnet-5.5","runs":4,"verified_pr_share":0.5,"failure_rate":0.25}]}`,
	}
	s := &Server{mux: http.NewServeMux()}
	serve := func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path + "?" + r.URL.RawQuery
		seen = append(seen, key)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answers[key]))
	}
	s.mux.HandleFunc("GET /api/governor/pr-models", serve)
	s.mux.HandleFunc("GET /api/contribute/run-stats/models", serve)
	p := dashboardAdminMCPProvider{server: s, role: "owner", ownerVerified: true}

	for _, tc := range []struct {
		tool string
		args map[string]any
		path string
	}{
		{adminmcp.ToolPRModelsRead, map[string]any{"window": "30d"}, "/api/governor/pr-models?window=30d"},
		{adminmcp.ToolContributorModelStatsRead, map[string]any{}, "/api/contribute/run-stats/models?days=7"},
	} {
		seen = nil
		got, err := p.Read(context.Background(), tc.tool, tc.args)
		if err != nil {
			t.Fatalf("%s: %v", tc.tool, err)
		}
		if len(seen) != 1 || seen[0] != tc.path {
			t.Fatalf("%s requested %v, want %s", tc.tool, seen, tc.path)
		}
		var want any
		if err := json.Unmarshal([]byte(answers[tc.path]), &want); err != nil {
			t.Fatal(err)
		}
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		if string(gotJSON) != string(wantJSON) {
			t.Fatalf("%s answer differs from the dashboard's:\n got %s\nwant %s", tc.tool, gotJSON, wantJSON)
		}
	}

	seen = nil
	for tool, args := range map[string]map[string]any{
		adminmcp.ToolPRModelsRead:              {"window": "90d"},
		adminmcp.ToolContributorModelStatsRead: {"days": float64(400)},
	} {
		_, err := p.Read(context.Background(), tool, args)
		if err == nil || !strings.Contains(err.Error(), "must be") {
			t.Errorf("%s %v = %v, want a refusal with its reason", tool, args, err)
		}
	}
	if len(seen) != 0 {
		t.Fatalf("refused reads reached the router: %v", seen)
	}
}
