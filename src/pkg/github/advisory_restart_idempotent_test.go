package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// advisoryRestartForge is a stateful fake GitHub for one hive's repos. It
// serves pre-existing issues through search and list, counts every create
// (POST) and every edit (PATCH) per repo, and appends created issues so a
// later client sees them.
type advisoryRestartForge struct {
	mu      sync.Mutex
	issues  map[string][]map[string]any // "owner/repo" -> issues
	creates map[string]int
	edits   map[string]int
	next    int
}

func newAdvisoryRestartForge() *advisoryRestartForge {
	return &advisoryRestartForge{
		issues:  map[string][]map[string]any{},
		creates: map[string]int{},
		edits:   map[string]int{},
		next:    1000,
	}
}

func (f *advisoryRestartForge) seed(full string, number int, title string) {
	f.issues[full] = append(f.issues[full], map[string]any{
		"number": number,
		"title":  title,
		"state":  "open",
		"labels": []map[string]any{{"name": advisoryLabelName}},
	})
}

func (f *advisoryRestartForge) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		items := []map[string]any{}
		for _, field := range strings.Fields(r.URL.Query().Get("q")) {
			if full, ok := strings.CutPrefix(field, "repo:"); ok && f.issues[full] != nil {
				items = f.issues[full]
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(items), "items": items})
	})
	mux.HandleFunc("/repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		full := r.PathValue("owner") + "/" + r.PathValue("repo")
		if r.Method == http.MethodPost {
			f.next++
			issue := map[string]any{"number": f.next, "title": advisoryTitle, "state": "open"}
			f.issues[full] = append(f.issues[full], issue)
			f.creates[full]++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(issue)
			return
		}
		items := f.issues[full]
		if items == nil {
			items = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(items)
	})
	mux.HandleFunc("/repos/{owner}/{repo}/issues/{number}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		full := r.PathValue("owner") + "/" + r.PathValue("repo")
		if r.Method == http.MethodPatch {
			f.edits[full]++
		}
		for _, issue := range f.issues[full] {
			if jsonNumberString(issue["number"]) == r.PathValue("number") {
				_ = json.NewEncoder(w).Encode(issue)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "Not Found"})
	})
	mux.HandleFunc("/repos/{owner}/{repo}/labels", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"name": advisoryLabelName})
	})
	return mux
}

func jsonNumberString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// TestEnsureAdvisoryIssue_RestartReusesExistingOpenReport pins #18568 at the
// client: a hive restarted against a primary repo that already holds its open
// advisory report (found by title and the hive/advisory label) resolves that
// report and never files a new one, whichever title spelling the report
// carries. Every fresh client is a restart with no in-process memory.
func TestEnsureAdvisoryIssue_RestartReusesExistingOpenReport(t *testing.T) {
	const org = "hivecommons"
	const primary = "hive"
	const existing = 2364
	for _, title := range []string{advisoryTitle, "[governor] " + advisoryTitle} {
		t.Run(title, func(t *testing.T) {
			forge := newAdvisoryRestartForge()
			forge.seed(org+"/"+primary, existing, title)
			server := httptest.NewServer(forge.handler())
			defer server.Close()

			const restarts = 3
			for i := 0; i < restarts; i++ {
				c := newTestClient(t, server, org, []string{primary, "crux", "dibs"})
				n, err := c.EnsureAdvisoryIssue(context.Background(), primary)
				if err != nil {
					t.Fatalf("restart %d: EnsureAdvisoryIssue: %v", i, err)
				}
				if n != existing {
					t.Fatalf("restart %d: resolved #%d, want existing #%d", i, n, existing)
				}
			}
			if got := forge.creates[org+"/"+primary]; got != 0 {
				t.Fatalf("%d advisory reports created across restarts, want 0", got)
			}
			if got := forge.edits[org+"/"+primary]; got != 0 {
				t.Fatalf("%d edits of the open report, want 0 (it was never closed)", got)
			}
			for full, n := range forge.creates {
				if full != org+"/"+primary && n != 0 {
					t.Fatalf("%s: %d advisory reports created in a non-primary repo, want 0", full, n)
				}
			}
		})
	}
}

// TestEnsureAdvisoryIssue_CreatesOnceThenReusesAfterRestart pins the cold
// start: with no report yet, the first ensure files exactly one, and a
// restarted client finds it rather than filing a second.
func TestEnsureAdvisoryIssue_CreatesOnceThenReusesAfterRestart(t *testing.T) {
	const org = "hivecommons"
	const primary = "hive"
	forge := newAdvisoryRestartForge()
	server := httptest.NewServer(forge.handler())
	defer server.Close()

	first, err := newTestClient(t, server, org, []string{primary}).EnsureAdvisoryIssue(context.Background(), primary)
	if err != nil || first <= 0 {
		t.Fatalf("first ensure = (%d, %v), want a created issue", first, err)
	}
	again, err := newTestClient(t, server, org, []string{primary}).EnsureAdvisoryIssue(context.Background(), primary)
	if err != nil {
		t.Fatalf("ensure after restart: %v", err)
	}
	if again != first {
		t.Fatalf("after restart resolved #%d, want #%d", again, first)
	}
	if got := forge.creates[org+"/"+primary]; got != 1 {
		t.Fatalf("%d advisory reports created, want exactly 1", got)
	}
}
