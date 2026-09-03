package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// covBLogger returns a quiet logger for coverage tests.
func covBLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// covBPostRaw POSTs a raw (possibly malformed) body to exercise decode-error
// branches that doPost (which JSON-encodes) cannot reach.
func covBPostRaw(s *Server, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	return rec
}

// covBGitHubMux serves the subset of the go-github REST API that the ACMM
// evaluation and issue-creation handlers exercise: contents listing, label
// creation, and issue creation. Directory listings are returned for the
// prefetched parent dirs so patternExists resolves from the cache; unknown
// content paths 404 so the "does not exist" branch is also hit.
func covBGitHubMux(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	// Directory + single-content listing. go-github hits
	// GET /repos/{owner}/{repo}/contents/{path...}
	mux.HandleFunc("/repos/myorg/repo1/contents/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/repos/myorg/repo1/contents/")
		w.Header().Set("Content-Type", "application/json")
		switch path {
		case "", ".github", ".github/workflows", ".github/ISSUE_TEMPLATE",
			".github/prompts", ".github/agents", ".claude", "docs", "docs/security":
			// A directory listing containing a couple of well-known entries so
			// several criteria pass from the cache.
			entries := []map[string]interface{}{
				{"name": "README.md", "type": "file"},
				{"name": "go.mod", "type": "file"},
				{"name": "CLAUDE.md", "type": "file"},
				{"name": "CONTRIBUTING.md", "type": "file"},
				{"name": "workflows", "type": "dir"},
				{"name": "ISSUE_TEMPLATE", "type": "dir"},
			}
			_ = json.NewEncoder(w).Encode(entries)
		default:
			// Unknown direct-content lookups (patternExists cache-miss path).
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	})

	mux.HandleFunc("/repos/myorg/repo1/labels", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"name": "acmm"})
	})

	mux.HandleFunc("/repos/myorg/repo1/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"number":   42,
			"html_url": "https://github.com/myorg/repo1/issues/42",
		})
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestCovB_ACMMEvaluation_NoGHClient(t *testing.T) {
	s := NewServer(0, covBLogger())
	deps := testDeps(t)
	deps.GHClient = nil
	s.RegisterAPI(deps)

	rec := doGet(s, "/api/acmm/evaluation")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := decodeJSON(t, rec)
	if got["error"] != "GitHub client not available" {
		t.Fatalf("error = %v, want GitHub client not available", got["error"])
	}
}

func TestCovB_ACMMEvaluation_Success_AndCacheHit(t *testing.T) {
	ts := covBGitHubMux(t)
	s := NewServer(0, covBLogger())
	deps := testDeps(t)
	deps.GHClient = ghpkg.NewClientForTest(ts.URL, "myorg", []string{"repo1"}, covBLogger())
	s.RegisterAPI(deps)

	rec := doGet(s, "/api/acmm/evaluation")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := decodeJSON(t, rec)
	if _, ok := got["codebase_level"]; !ok {
		t.Fatalf("missing codebase_level in %v", got)
	}
	if _, ok := got["repo_results"]; !ok {
		t.Fatalf("missing repo_results in %v", got)
	}

	// Second call must hit the cache-age branch (cached != nil && age < TTL).
	rec2 := doGet(s, "/api/acmm/evaluation")
	if rec2.Code != http.StatusOK {
		t.Fatalf("cache-hit status = %d, want 200", rec2.Code)
	}
}

func TestCovB_ACMMEvaluation_OrgEmpty(t *testing.T) {
	s := NewServer(0, covBLogger())
	deps := testDeps(t)
	deps.Config.Project.Org = ""
	s.RegisterAPI(deps)

	rec := doGet(s, "/api/acmm/evaluation")
	got := decodeJSON(t, rec)
	if got["error"] != "org not configured" {
		t.Fatalf("error = %v, want org not configured", got["error"])
	}
}

func TestCovB_ACMMEvaluation_NoRepos(t *testing.T) {
	ts := covBGitHubMux(t)
	s := NewServer(0, covBLogger())
	deps := testDeps(t)
	deps.Config.Project.Repos = nil
	deps.Config.Project.PrimaryRepo = ""
	deps.GHClient = ghpkg.NewClientForTest(ts.URL, "myorg", nil, covBLogger())
	s.RegisterAPI(deps)

	rec := doGet(s, "/api/acmm/evaluation")
	got := decodeJSON(t, rec)
	if got["error"] != "no repos configured" {
		t.Fatalf("error = %v, want no repos configured", got["error"])
	}
}

func TestCovB_ACMMEvaluation_PrimaryRepoFallback(t *testing.T) {
	ts := covBGitHubMux(t)
	s := NewServer(0, covBLogger())
	deps := testDeps(t)
	deps.Config.Project.Repos = nil
	deps.Config.Project.PrimaryRepo = "repo1"
	deps.GHClient = ghpkg.NewClientForTest(ts.URL, "myorg", []string{"repo1"}, covBLogger())
	s.RegisterAPI(deps)

	rec := doGet(s, "/api/acmm/evaluation")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestCovB_ACMMCreateIssue_Errors(t *testing.T) {
	s := NewServer(0, covBLogger())
	deps := testDeps(t)
	s.RegisterAPI(deps)

	// Bad JSON body.
	rec := covBPostRaw(s, "/api/acmm/issue", "{not json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body status = %d, want 400", rec.Code)
	}

	// Missing repo / criterion_id.
	rec = doPost(s, "/api/acmm/issue", map[string]interface{}{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing fields status = %d, want 400", rec.Code)
	}

	// Unknown criterion.
	rec = doPost(s, "/api/acmm/issue", map[string]interface{}{
		"repo": "repo1", "criterion_id": "acmm:does-not-exist",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown criterion status = %d, want 400", rec.Code)
	}
}

func TestCovB_ACMMCreateIssue_NoGHClient(t *testing.T) {
	s := NewServer(0, covBLogger())
	deps := testDeps(t)
	deps.GHClient = nil
	s.RegisterAPI(deps)

	rec := doPost(s, "/api/acmm/issue", map[string]interface{}{
		"repo": "repo1", "criterion_id": "acmm:claude-md",
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("nil GHClient status = %d, want 500", rec.Code)
	}
}

func TestCovB_ACMMCreateIssue_OrgEmpty(t *testing.T) {
	s := NewServer(0, covBLogger())
	deps := testDeps(t)
	deps.Config.Project.Org = ""
	s.RegisterAPI(deps)

	rec := doPost(s, "/api/acmm/issue", map[string]interface{}{
		"repo": "repo1", "criterion_id": "acmm:claude-md",
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("org empty status = %d, want 500", rec.Code)
	}
}

func TestCovB_ACMMCreateIssue_Success(t *testing.T) {
	ts := covBGitHubMux(t)
	s := NewServer(0, covBLogger())
	deps := testDeps(t)
	deps.GHClient = ghpkg.NewClientForTest(ts.URL, "myorg", []string{"repo1"}, covBLogger())
	s.RegisterAPI(deps)

	rec := doPost(s, "/api/acmm/issue", map[string]interface{}{
		"repo": "repo1", "criterion_id": "acmm:claude-md",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := decodeJSON(t, rec)
	if got["issue_number"] == nil {
		t.Fatalf("missing issue_number in %v", got)
	}
}

func TestCovB_ScoreResults_LevelsAndPromotion(t *testing.T) {
	s := NewServer(0, covBLogger())

	// L0 fully passing → codebaseLevel promoted from 0 to 1.
	results := []CriterionResult{
		{ID: "a", Level: 0, Passed: true},
		{ID: "b", Level: 0, Passed: true},
		{ID: "c", Level: 2, Passed: false},
		{ID: "d", Level: 2, Passed: false},
	}
	eval := s.scoreResults(results)
	if eval.CodebaseLevel != 1 {
		t.Fatalf("CodebaseLevel = %d, want 1 (L0 promotion)", eval.CodebaseLevel)
	}
	if eval.CriteriaTotal != 4 || eval.CriteriaPassed != 2 {
		t.Fatalf("total/passed = %d/%d, want 4/2", eval.CriteriaTotal, eval.CriteriaPassed)
	}
	if len(eval.Levels) != 2 {
		t.Fatalf("levels = %d, want 2", len(eval.Levels))
	}

	// Higher levels passing → codebaseLevel climbs.
	results2 := []CriterionResult{
		{ID: "a", Level: 0, Passed: true},
		{ID: "b", Level: 2, Passed: true},
		{ID: "c", Level: 3, Passed: true},
	}
	eval2 := s.scoreResults(results2)
	if eval2.CodebaseLevel < 3 {
		t.Fatalf("CodebaseLevel = %d, want >= 3", eval2.CodebaseLevel)
	}

	// Empty input → codebase level 0 with no scored levels.
	empty := s.scoreResults(nil)
	if empty.CodebaseLevel != 0 {
		t.Fatalf("empty CodebaseLevel = %d, want 0", empty.CodebaseLevel)
	}
	if len(empty.Levels) != 0 {
		t.Fatalf("empty Levels = %d, want 0", len(empty.Levels))
	}
}

func TestCovB_MinInt(t *testing.T) {
	if minInt(1, 2) != 1 {
		t.Fatal("minInt(1,2) != 1")
	}
	if minInt(5, 3) != 3 {
		t.Fatal("minInt(5,3) != 3")
	}
	if minInt(4, 4) != 4 {
		t.Fatal("minInt(4,4) != 4")
	}
}

func TestCovB_ACMMCriterionWhyItMatters(t *testing.T) {
	categories := []string{
		"prerequisite", "feedback-loop", "learning", "governance",
		"observability", "self-tuning", "autonomy", "traceability",
		"something-unknown",
	}
	for _, cat := range categories {
		if got := acmmCriterionWhyItMatters(3, cat); got == "" {
			t.Fatalf("acmmCriterionWhyItMatters(%q) returned empty", cat)
		}
	}
}
