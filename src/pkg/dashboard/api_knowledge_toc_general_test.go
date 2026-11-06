package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// dashboardKnowledgeTOC calls GET /api/knowledge/toc and returns the total and
// the type each listed entry is shown with.
func dashboardKnowledgeTOC(t *testing.T, s *Server, query string) (int, map[string]string) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/knowledge/toc"+query, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/knowledge/toc%s: HTTP %d body=%q", query, rec.Code, rec.Body.String())
	}
	var toc struct {
		Total   int `json:"total"`
		Entries []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &toc); err != nil {
		t.Fatalf("GET /api/knowledge/toc%s: %v in %s", query, err, rec.Body.String())
	}
	types := map[string]string{}
	for _, e := range toc.Entries {
		types[e.ID] = e.Type
	}
	return toc.Total, types
}

// The dashboard table of contents lists a fact with no type as "general", but
// ?types=general compared that word with the empty type and returned nothing.
func TestKnowledgeTOCRouteGeneralFilterFindsUntypedFacts(t *testing.T) {
	// A wiki layer page with no type: file stores give every page one.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"results": []interface{}{
				map[string]interface{}{"slug": "house-style", "title": "House style", "snippet": "Write short sentences."},
				map[string]interface{}{"slug": "nil-map", "title": "Nil map", "type": "gotcha", "snippet": "Initialise maps."},
			},
			"total": 2,
		})
	})
	wiki := httptest.NewServer(mux)
	t.Cleanup(wiki.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServerWithAuth(0, "dashboard-secret", logger)
	s.RegisterAPI(testDeps(t))
	s.deps.Knowledge = knowledge.NewKnowledgeAPI(
		[]knowledge.LayerConfig{{Type: knowledge.LayerProject, URL: wiki.URL}},
		knowledge.KnowledgeConfig{Enabled: true, Engine: "file"}, logger)

	total, types := dashboardKnowledgeTOC(t, s, "")
	if total != 2 || types["house-style"] != "general" || types["nil-map"] != "gotcha" {
		t.Fatalf("fixture: unfiltered toc = %d entries %v, want house-style listed as general and nil-map as gotcha", total, types)
	}

	for _, q := range []string{"?types=general", "?types=General", "?types=pattern,general"} {
		total, types = dashboardKnowledgeTOC(t, s, q)
		if total != 1 || types["house-style"] != "general" {
			t.Errorf("toc%s = %d entries %v, want the one fact the toc lists as general", q, total, types)
		}
	}
	total, types = dashboardKnowledgeTOC(t, s, "?types=general,gotcha")
	if total != 2 {
		t.Errorf("toc?types=general,gotcha = %d entries %v, want both", total, types)
	}

	// The other types are still exact.
	total, types = dashboardKnowledgeTOC(t, s, "?types=gotcha")
	if total != 1 || types["nil-map"] != "gotcha" {
		t.Errorf("toc?types=gotcha = %d entries %v, want only nil-map", total, types)
	}
	if total, types = dashboardKnowledgeTOC(t, s, "?types=pattern"); total != 0 {
		t.Errorf("toc?types=pattern = %d entries %v, want none", total, types)
	}

	// The entry read and the effective view parse the same scope.
	for q, want := range map[string]bool{"general": true, "pattern,General": true, "gotcha": false} {
		scope, err := knowledgeTOCScope(httptest.NewRequest(http.MethodGet, "/api/knowledge/entry/x?types="+q, nil))
		if err != nil {
			t.Fatal(err)
		}
		if got := scope.InScope(knowledge.Fact{Slug: "x", Title: "x"}); got != want {
			t.Errorf("types=%s admits an untyped fact = %v, want %v", q, got, want)
		}
	}
}
