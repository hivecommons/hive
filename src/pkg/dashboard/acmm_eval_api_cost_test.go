package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// The hourly ACMM evaluation spent most of its GitHub calls asking about
// files under directories it had already been told were missing: a 404 on a
// prefetched listing was dropped, so every literal pattern under that dir
// paid its own 404. These tests pin the fix and the API-budget gate.

// acmmCostRepo is a fake repo1 whose contents endpoint answers from status
// (path -> HTTP status) and listings (path -> entry names, a trailing "/"
// marking a dir), 404s everything else, and records every path requested.
type acmmCostRepo struct {
	status   map[string]int
	listings map[string][]string

	mu   sync.Mutex
	hits []string
}

func (f *acmmCostRepo) serve(t *testing.T) *httptest.Server {
	t.Helper()
	t.Cleanup(ghpkg.ResetRateLimitPacingForTest)
	const prefix = "/repos/myorg/repo1/contents/"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, prefix) && r.URL.Path != strings.TrimSuffix(prefix, "/") {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		path := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, strings.TrimSuffix(prefix, "/")), "/")
		f.mu.Lock()
		f.hits = append(f.hits, path)
		f.mu.Unlock()
		if code, ok := f.status[path]; ok {
			http.Error(w, fmt.Sprintf(`{"message":"status %d"}`, code), code)
			return
		}
		names, ok := f.listings[path]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		entries := make([]map[string]string, 0, len(names))
		for _, n := range names {
			typ := "file"
			if strings.HasSuffix(n, "/") {
				typ, n = "dir", strings.TrimSuffix(n, "/")
			}
			entries = append(entries, map[string]string{"name": n, "type": typ})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entries)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (f *acmmCostRepo) requested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hits...)
}

func (f *acmmCostRepo) count(match func(string) bool) int {
	n := 0
	for _, p := range f.requested() {
		if match(p) {
			n++
		}
	}
	return n
}

func acmmCostServer(t *testing.T, f *acmmCostRepo) *Server {
	t.Helper()
	ts := f.serve(t)
	s := NewServer(0, acmmEvalTestLogger())
	deps := testDeps(t)
	deps.GHClient = ghpkg.NewClientForTest(ts.URL, "myorg", []string{"repo1"}, acmmEvalTestLogger())
	s.RegisterAPI(deps)
	return s
}

// pinACMMBudgetMode makes the handler see mode until the test ends.
func pinACMMBudgetMode(t *testing.T, mode *ghpkg.APIBudgetMode) {
	t.Helper()
	prev := acmmAPIBudgetMode
	acmmAPIBudgetMode = func(*Server) (ghpkg.APIBudgetMode, ghpkg.APIBudgetSnapshot) {
		return *mode, ghpkg.APIBudgetSnapshot{Mode: mode.String(), Remaining: 42}
	}
	t.Cleanup(func() { acmmAPIBudgetMode = prev })
}

func TestACMMPathNotFoundOnlyMatches404(t *testing.T) {
	resp := func(code int) *http.Response { return &http.Response{StatusCode: code} }
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"404", &gh.ErrorResponse{Response: resp(http.StatusNotFound)}, true},
		{"wrapped 404", fmt.Errorf("list: %w", &gh.ErrorResponse{Response: resp(http.StatusNotFound)}), true},
		{"403", &gh.ErrorResponse{Response: resp(http.StatusForbidden)}, false},
		{"500", &gh.ErrorResponse{Response: resp(http.StatusInternalServerError)}, false},
		{"primary rate limit", &gh.RateLimitError{Response: resp(http.StatusForbidden)}, false},
		{"secondary rate limit", &gh.AbuseRateLimitError{Response: resp(http.StatusForbidden)}, false},
		{"no response", &gh.ErrorResponse{}, false},
		{"transport", errors.New("context deadline exceeded"), false},
		{"nil", nil, false},
	} {
		if got := acmmPathNotFound(tc.err); got != tc.want {
			t.Errorf("%s: acmmPathNotFound = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestACMMKnownMissingFollowsEmptyAncestors(t *testing.T) {
	cache := map[string]map[string]bool{
		"":        {"README.md": true, "docs": true, "docs/": true},
		"docs":    {},
		".claude": {},
	}
	for dir, want := range map[string]bool{
		"docs/security":       true,
		"docs/security/deep":  true,
		".claude/skills":      true,
		".github/agents":      false, // root lists no .github, but a non-empty listing proves nothing
		"web/src/components":  false,
		"":                    false,
		"docs":                false, // docs itself is answered by its own entry, not here
		"README.md/not-a-dir": false,
	} {
		if got := acmmKnownMissing(cache, dir); got != want {
			t.Errorf("acmmKnownMissing(%q) = %v, want %v", dir, got, want)
		}
	}
	if !acmmKnownMissing(map[string]map[string]bool{"": {}}, ".github") {
		t.Error("an empty root (repo 404) should rule out every path")
	}
	if acmmKnownMissing(nil, "docs/security") {
		t.Error("a nil cache rules out nothing")
	}
}

func TestPrefetchDirectories_Records404sButNotOtherErrors(t *testing.T) {
	f := &acmmCostRepo{
		listings: map[string][]string{
			"":                  {"README.md", ".github/"},
			".github":           {"workflows/", "agents/", "prompts/"},
			".github/workflows": {"ci.yml"},
		},
		status: map[string]int{
			".github/agents":  http.StatusForbidden,
			".github/prompts": http.StatusInternalServerError,
		},
	}
	s := acmmCostServer(t, f)

	cache := s.prefetchDirectories(t.Context(), "myorg", "repo1")

	for _, dir := range []string{".claude", "docs", ".github/ISSUE_TEMPLATE"} {
		entries, ok := cache[dir]
		if !ok || len(entries) != 0 {
			t.Errorf("cache[%q] = %v (present %v), want recorded empty after a 404", dir, entries, ok)
		}
	}
	if entries, ok := cache["docs/security"]; !ok || len(entries) != 0 {
		t.Errorf("cache[docs/security] = %v (present %v), want recorded empty because docs is missing", entries, ok)
	}
	if n := f.count(func(p string) bool { return p == "docs/security" }); n != 0 {
		t.Errorf("docs/security was listed %d time(s) after docs was a 404", n)
	}
	for _, dir := range []string{".github/agents", ".github/prompts"} {
		if _, ok := cache[dir]; ok {
			t.Errorf("cache[%q] recorded after a non-404 error; a rate limit or 5xx is not proof the dir is absent", dir)
		}
	}
	if !cache[".github/workflows"]["ci.yml"] {
		t.Error("a successful listing should still be cached")
	}

	before := len(f.requested())
	for _, p := range []string{".claude/settings.json", ".claude/CLAUDE.md", ".claude/skills/", "docs/contributing.md", "docs/security/SECURITY-AI.md", "docs/metrics/"} {
		if s.patternExists(t.Context(), "myorg", "repo1", p, cache) {
			t.Errorf("patternExists(%q) = true under a missing dir", p)
		}
	}
	if after := len(f.requested()); after != before {
		t.Errorf("literal patterns under 404'd dirs made %d GetContents call(s), want 0: %v", after-before, f.requested()[before:])
	}

	// Under a dir whose listing failed for another reason, the pattern is
	// still looked up on its own: the answer is unknown, not "missing".
	s.patternExists(t.Context(), "myorg", "repo1", ".github/agents/reviewer.md", cache)
	if n := f.count(func(p string) bool { return p == ".github/agents/reviewer.md" }); n != 1 {
		t.Errorf(".github/agents/reviewer.md requested %d time(s), want 1 (its dir's listing was a 403)", n)
	}
}

func TestPatternExists_GlobCachesOnly404s(t *testing.T) {
	for _, tc := range []struct {
		code       int
		wantCached bool
	}{
		{http.StatusNotFound, true},
		{http.StatusForbidden, false},
		{http.StatusInternalServerError, false},
	} {
		t.Run(fmt.Sprint(tc.code), func(t *testing.T) {
			f := &acmmCostRepo{status: map[string]int{".github/workflows": tc.code}}
			s := acmmCostServer(t, f)
			cache := map[string]map[string]bool{}

			if s.patternExists(t.Context(), "myorg", "repo1", ".github/workflows/*e2e*.yml", cache) {
				t.Fatal("glob matched under a failed listing")
			}
			entries, cached := cache[".github/workflows"]
			if cached != tc.wantCached {
				t.Fatalf("listing that answered %d cached = %v, want %v", tc.code, cached, tc.wantCached)
			}
			if cached && len(entries) != 0 {
				t.Fatalf("a 404 should cache an empty listing, got %v", entries)
			}

			s.patternExists(t.Context(), "myorg", "repo1", ".github/workflows/test*-boot*.yml", cache)
			want := 2
			if tc.wantCached {
				want = 1
			}
			if n := f.count(func(p string) bool { return p == ".github/workflows" }); n != want {
				t.Fatalf("listing requested %d time(s) for two sibling globs, want %d", n, want)
			}
		})
	}
}

// TestEvaluateAllRepos_MissingDirsCostNoPatternCalls is the count the bug was
// found by: a repo with no .claude, docs, .github/agents or .github/prompts
// used to pay one 404 per literal pattern under each of them, every hour.
func TestEvaluateAllRepos_MissingDirsCostNoPatternCalls(t *testing.T) {
	f := &acmmCostRepo{
		listings: map[string][]string{
			"":                  {"README.md", "go.mod", ".github/"},
			".github":           {"workflows/"},
			".github/workflows": {"ci.yml"},
		},
	}
	s := acmmCostServer(t, f)

	eval := s.evaluateAllRepos()
	if len(eval.RepoResults) != 1 {
		t.Fatalf("RepoResults = %d, want 1 (error %q)", len(eval.RepoResults), eval.Error)
	}

	under := func(dir string) func(string) bool {
		return func(p string) bool { return strings.HasPrefix(p, dir+"/") }
	}
	for _, dir := range []string{".claude", "docs", ".github/agents", ".github/prompts", ".github/ISSUE_TEMPLATE"} {
		if n := f.count(under(dir)); n != 0 {
			t.Errorf("%d GetContents call(s) under missing %s/, want 0", n, dir)
		}
	}
	for _, dir := range []string{".claude", "docs", ".github/agents", ".github/prompts", ".github/ISSUE_TEMPLATE"} {
		if n := f.count(func(p string) bool { return p == dir }); n != 1 {
			t.Errorf("%s listed %d time(s), want exactly 1 (the prefetch)", dir, n)
		}
	}
	if n := f.count(func(p string) bool { return p == "docs/security" }); n != 0 {
		t.Errorf("docs/security listed %d time(s) after docs was a 404", n)
	}
	if res := acmmResultFor(t, eval, "acmm:prereq-cicd"); !res.Passed {
		t.Error("the CI criterion should still pass from the .github/workflows listing")
	}
}

func TestHandleACMMEvaluation_BudgetGateServesStale(t *testing.T) {
	for _, tc := range []struct {
		mode ghpkg.APIBudgetMode
		path string
	}{
		{ghpkg.APIBudgetCritical, "/api/acmm/evaluation"},
		{ghpkg.APIBudgetConserve, "/api/acmm/evaluation"},
		{ghpkg.APIBudgetCritical, "/api/acmm/evaluation?refresh=1"},
	} {
		t.Run(tc.mode.String()+" "+tc.path, func(t *testing.T) {
			f := &acmmCostRepo{listings: map[string][]string{"": {"README.md"}}}
			s := acmmCostServer(t, f)
			mode := tc.mode
			pinACMMBudgetMode(t, &mode)

			cachedAt := time.Now().Add(-2 * acmmEvalTTL)
			s.acmmEvalMu.Lock()
			s.acmmEvalCache = &ACMMEvaluation{
				CodebaseLevel:   4,
				CriteriaTotal:   43,
				CriteriaPassed:  30,
				LastEvaluatedAt: cachedAt.UTC().Format(time.RFC3339),
				RepoResults:     []RepoEvaluation{{Repo: "cached-repo"}},
			}
			s.acmmEvalCachedAt = cachedAt
			s.acmmEvalMu.Unlock()

			rec := doGet(s, tc.path)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			var eval ACMMEvaluation
			if err := json.Unmarshal(rec.Body.Bytes(), &eval); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if n := len(f.requested()); n != 0 {
				t.Fatalf("budget %s made %d GitHub call(s), want 0: %v", tc.mode, n, f.requested())
			}
			if !eval.Stale || !strings.Contains(eval.StaleReason, tc.mode.String()) {
				t.Fatalf("stale = %v, stale_reason = %q; want stale naming the %s budget", eval.Stale, eval.StaleReason, tc.mode)
			}
			if eval.CodebaseLevel != 4 || len(eval.RepoResults) != 1 || eval.RepoResults[0].Repo != "cached-repo" {
				t.Fatalf("expected the cached evaluation, got level %d repos %+v", eval.CodebaseLevel, eval.RepoResults)
			}
			if eval.Error != "" {
				t.Fatalf("error = %q, want empty when a cached result exists", eval.Error)
			}
			if eval.OperationalName == "" || eval.IssueTracker == "" {
				t.Fatal("operational level and issue tracker should still be stamped on a stale response")
			}
			s.acmmEvalMu.RLock()
			storedStale, storedAt := s.acmmEvalCache.Stale, s.acmmEvalCachedAt
			s.acmmEvalMu.RUnlock()
			if storedStale || !storedAt.Equal(cachedAt) {
				t.Fatal("a deferred request must not change the cache")
			}

			// Once the budget recovers, the next request runs the pass.
			mode = ghpkg.APIBudgetNormal
			rec = doGet(s, tc.path)
			eval = ACMMEvaluation{}
			if err := json.Unmarshal(rec.Body.Bytes(), &eval); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if eval.Stale || eval.StaleReason != "" {
				t.Fatalf("fresh evaluation marked stale: %q", eval.StaleReason)
			}
			if len(eval.RepoResults) != 1 || eval.RepoResults[0].Repo != "repo1" {
				t.Fatalf("expected a fresh pass over repo1, got %+v", eval.RepoResults)
			}
			if len(f.requested()) == 0 {
				t.Fatal("normal budget made no GitHub calls")
			}
		})
	}
}

func TestHandleACMMEvaluation_BudgetGateWithNoCache(t *testing.T) {
	f := &acmmCostRepo{listings: map[string][]string{"": {"README.md"}}}
	s := acmmCostServer(t, f)
	mode := ghpkg.APIBudgetCritical
	pinACMMBudgetMode(t, &mode)

	rec := doGet(s, "/api/acmm/evaluation")
	var eval ACMMEvaluation
	if err := json.Unmarshal(rec.Body.Bytes(), &eval); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n := len(f.requested()); n != 0 {
		t.Fatalf("critical budget with no cache made %d GitHub call(s), want 0", n)
	}
	if !eval.Stale || eval.StaleReason == "" || eval.Error != eval.StaleReason {
		t.Fatalf("stale = %v, stale_reason = %q, error = %q; want stale with the reason as the error", eval.Stale, eval.StaleReason, eval.Error)
	}
	if eval.CriteriaTotal != 0 || len(eval.RepoResults) != 0 || eval.LastEvaluatedAt != "" {
		t.Fatalf("expected an empty not-evaluated body, got total %d repos %d last %q", eval.CriteriaTotal, len(eval.RepoResults), eval.LastEvaluatedAt)
	}
	s.acmmEvalMu.RLock()
	defer s.acmmEvalMu.RUnlock()
	if s.acmmEvalCache != nil {
		t.Fatal("a deferred request must not populate the cache")
	}
}

// The panel shows stale_reason in its warning line, so an operator can tell
// an hour-old reading held back by the budget from a fresh one.
func TestACMMCardShowsStaleReason(t *testing.T) {
	html := indexHTML(t)
	body := jsFunc(t, html, "acmmRenderCard")
	if !strings.Contains(body, "d.error || (d.stale && d.stale_reason)") {
		t.Fatal("acmmRenderCard does not show stale_reason when the evaluation was deferred")
	}
}
