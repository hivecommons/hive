package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/search"
)

// getRepoHealth serves path through the route pattern RegisterAPI uses, so
// PathValue resolves as in production without building full Dependencies.
func getRepoHealth(s *Server, path string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/repos/{owner}/{repo}/health", s.handleRepoHealth)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestRepoHealthEndpoint(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	changed := now.Add(-30 * 24 * time.Hour)
	health := search.ComputeHealth("org/a", nil, search.HealthSignals{
		SignOff:              &search.HealthCount{Count: 8, Total: 10},
		SecurityDocChangedAt: &changed,
	}, search.HealthOptions{}, now)
	var asked []string
	lookup := func(repo string) (search.Health, bool) {
		asked = append(asked, repo)
		return health, repo == "org/a"
	}

	cases := []struct {
		name   string
		lookup func(string) (search.Health, bool)
		path   string
		code   int
	}{
		{"search disabled", nil, "/api/repos/org/a/health", http.StatusServiceUnavailable},
		{"blank owner", lookup, "/api/repos/%20/a/health", http.StatusBadRequest},
		{"slash in owner", lookup, "/api/repos/x%2Fy/a/health", http.StatusBadRequest},
		{"unknown repo", lookup, "/api/repos/org/none/health", http.StatusNotFound},
		{"ok", lookup, "/api/repos/org/a/health", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer()
			s.SetRepoHealthSource(tc.lookup)
			rec := getRepoHealth(s, tc.path)
			if rec.Code != tc.code {
				t.Fatalf("code %d, want %d: %s", rec.Code, tc.code, rec.Body.String())
			}
			if tc.code != http.StatusOK {
				return
			}
			var got struct {
				Repo       string   `json:"repo"`
				Score      *float64 `json:"score"`
				ComputedAt string   `json:"computed_at"`
				Components []struct {
					Name   string         `json:"name"`
					Status string         `json:"status"`
					N      *int           `json:"n"`
					Inputs map[string]any `json:"inputs"`
				} `json:"components"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Repo != "org/a" || got.Score == nil || *got.Score != 86.7 || got.ComputedAt != "2026-10-01T12:00:00Z" {
				t.Fatalf("body %s", rec.Body.String())
			}
			if len(got.Components) != len(search.HealthComponents) {
				t.Fatalf("components %d, want %d", len(got.Components), len(search.HealthComponents))
			}
			for i, c := range got.Components {
				if c.Name != search.HealthComponents[i] || c.N == nil || c.Inputs == nil || c.Inputs["source"] == nil {
					t.Fatalf("component %d missing name/n/inputs: %+v", i, c)
				}
			}
		})
	}
	if len(asked) != 2 || asked[0] != "org/none" || asked[1] != "org/a" {
		t.Fatalf("lookups %v", asked)
	}
}

func TestRepoHealthSourceCanBeCleared(t *testing.T) {
	s := newTestServer()
	s.SetRepoHealthSource(func(string) (search.Health, bool) { return search.Health{}, true })
	s.SetRepoHealthSource(nil)
	if rec := getRepoHealth(s, "/api/repos/org/a/health"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code %d, want 503", rec.Code)
	}
}
