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

// multiRepoAdvisoryForge is a stateful fake GitHub holding the issues of
// several repos, so EnsureAdvisoryIssue's search-before-create runs against
// what earlier calls (or an earlier process) created.
type multiRepoAdvisoryForge struct {
	mu      sync.Mutex
	issues  map[string][]map[string]any // "owner/repo" -> issues
	creates map[string]int
	next    int
}

func (f *multiRepoAdvisoryForge) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var items []map[string]any
		for _, field := range strings.Fields(r.URL.Query().Get("q")) {
			if full, ok := strings.CutPrefix(field, "repo:"); ok {
				items = f.issues[full]
			}
		}
		if items == nil {
			items = []map[string]any{}
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
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	mux.HandleFunc("/repos/{owner}/{repo}/labels", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"name": advisoryLabelName})
	})
	return mux
}

// TestEnsureAdvisoryIssue_OnePerRepoAndReusedAfterRestart is #18448 at the
// client: two repos each get their own governor issue, and a fresh client
// (a restart) finds both instead of creating duplicates.
func TestEnsureAdvisoryIssue_OnePerRepoAndReusedAfterRestart(t *testing.T) {
	forge := &multiRepoAdvisoryForge{issues: map[string][]map[string]any{}, creates: map[string]int{}}
	server := httptest.NewServer(forge.handler(t))
	defer server.Close()

	const org = "nate"
	repos := []string{"pr-pal", "talks"}
	first := map[string]int{}
	c := newTestClient(t, server, org, repos)
	for _, repo := range repos {
		n, err := c.EnsureAdvisoryIssue(context.Background(), repo)
		if err != nil {
			t.Fatalf("EnsureAdvisoryIssue(%s): %v", repo, err)
		}
		first[repo] = n
	}
	if first["pr-pal"] <= 0 || first["talks"] <= 0 || first["pr-pal"] == first["talks"] {
		t.Fatalf("each repo needs its own governor issue, got %v", first)
	}

	restarted := newTestClient(t, server, org, repos)
	for _, repo := range repos {
		n, err := restarted.EnsureAdvisoryIssue(context.Background(), repo)
		if err != nil {
			t.Fatalf("EnsureAdvisoryIssue(%s) after restart: %v", repo, err)
		}
		if n != first[repo] {
			t.Fatalf("%s after restart resolved #%d, want existing #%d", repo, n, first[repo])
		}
	}
	for _, repo := range repos {
		if got := forge.creates[org+"/"+repo]; got != 1 {
			t.Fatalf("%s: %d governor issues created, want exactly 1", repo, got)
		}
	}
}
