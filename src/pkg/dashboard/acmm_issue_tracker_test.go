package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// fakeLinearForACMM answers the team lookup + issueCreate that the ACMM
// Linear path sends and counts issueCreate calls so tests can prove which
// tracker a request landed on.
func fakeLinearForACMM(t *testing.T, created *atomic.Int32, lastInput *map[string]interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "lin_key" {
			t.Errorf("Authorization = %q, want bare api key", got)
		}
		var req struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(req.Query, "teams(filter"):
			key, _ := req.Variables["key"].(string)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{"teams": map[string]interface{}{
					"nodes": []map[string]string{{"id": "uuid-" + key, "key": key}},
				}},
			})
		case strings.Contains(req.Query, "issueCreate"):
			created.Add(1)
			input, _ := req.Variables["input"].(map[string]interface{})
			*lastInput = input
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{"issueCreate": map[string]interface{}{
					"success": true,
					"issue": map[string]interface{}{
						"id": "iss", "identifier": "ENG-7", "number": 7,
						"url": "https://linear.app/acme/issue/ENG-7", "title": input["title"],
					},
				}},
			})
		default:
			http.Error(w, "unexpected query", http.StatusBadRequest)
		}
	}))
}

// linearHive wires a server whose work source is Linear (two teams, repo1
// mapped to OPS) with both a fake GitHub and a fake Linear behind it.
func linearHive(t *testing.T, issueTracker string) (*Server, *atomic.Int32, *map[string]interface{}) {
	t.Helper()
	var created atomic.Int32
	lastInput := map[string]interface{}{}
	lin := fakeLinearForACMM(t, &created, &lastInput)
	t.Cleanup(lin.Close)
	gh := covBGitHubMux(t)

	s := NewServer(0, covBLogger())
	s.acmmLinearBaseURL = lin.URL
	deps := testDeps(t)
	deps.GHClient = ghpkg.NewClientForTest(gh.URL, "myorg", []string{"repo1"}, covBLogger())
	deps.Config.Governor.ACMM.IssueTracker = issueTracker
	deps.Config.Governor.WorkSource = config.WorkSourceConfig{
		Type: "linear",
		Linear: config.LinearSourceConfig{
			APIKey: "lin_key",
			Teams: []config.LinearTeamSourceConfig{
				{Key: "ENG", Repo: "myorg/other"},
				{Key: "OPS", Repo: "myorg/repo1"},
			},
		},
	}
	s.RegisterAPI(deps)
	return s, &created, &lastInput
}

func TestACMMIssue_WorkSourceLinear_CreatesLinearIssue(t *testing.T) {
	s, created, lastInput := linearHive(t, config.ACMMIssueTrackerWorkSource)

	rec := doPost(s, "/api/acmm/issue", map[string]interface{}{
		"repo": "repo1", "criterion_id": "acmm:claude-md",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	got := decodeJSON(t, rec)
	if got["tracker"] != "linear" || got["identifier"] != "ENG-7" || got["issue_url"] != "https://linear.app/acme/issue/ENG-7" {
		t.Fatalf("response = %v", got)
	}
	if created.Load() != 1 {
		t.Fatalf("issueCreate calls = %d, want 1", created.Load())
	}
	// repo1 is mapped to OPS, not the first team — the criterion's repo
	// picks the team.
	if (*lastInput)["teamId"] != "uuid-OPS" {
		t.Fatalf("teamId = %v, want uuid-OPS", (*lastInput)["teamId"])
	}
	title, _ := (*lastInput)["title"].(string)
	desc, _ := (*lastInput)["description"].(string)
	if !strings.HasPrefix(title, "[ACMM L") || !strings.Contains(desc, "## ACMM Gap:") {
		t.Fatalf("title/body not the ACMM template: %q / %q", title, desc)
	}
}

func TestACMMIssue_RequestOverridesConfig(t *testing.T) {
	// Config says GitHub; the click says work_source → Linear.
	s, created, _ := linearHive(t, "")
	rec := doPost(s, "/api/acmm/issue", map[string]interface{}{
		"repo": "repo1", "criterion_id": "acmm:claude-md", "tracker": "work_source",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeJSON(t, rec); got["tracker"] != "linear" {
		t.Fatalf("tracker = %v, want linear", got["tracker"])
	}
	if created.Load() != 1 {
		t.Fatalf("issueCreate calls = %d, want 1", created.Load())
	}

	// Config says work_source; the click says github → GitHub, Linear untouched.
	s2, created2, _ := linearHive(t, config.ACMMIssueTrackerWorkSource)
	rec = doPost(s2, "/api/acmm/issue", map[string]interface{}{
		"repo": "repo1", "criterion_id": "acmm:claude-md", "tracker": "github",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	got := decodeJSON(t, rec)
	if got["tracker"] != "github" || got["issue_number"] == nil {
		t.Fatalf("response = %v, want github issue", got)
	}
	if created2.Load() != 0 {
		t.Fatalf("Linear issueCreate calls = %d, want 0", created2.Load())
	}
}

func TestACMMIssue_UnknownTrackerIs400(t *testing.T) {
	s, created, _ := linearHive(t, config.ACMMIssueTrackerWorkSource)
	rec := doPost(s, "/api/acmm/issue", map[string]interface{}{
		"repo": "repo1", "criterion_id": "acmm:claude-md", "tracker": "linear",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "work_source") {
		t.Fatalf("400 body should name the accepted values: %s", rec.Body.String())
	}
	if created.Load() != 0 {
		t.Fatal("no issue may be created on a rejected tracker")
	}
}

// work_source on a GitHub-sourced hive is GitHub: the default path with no
// Linear involvement, so hives that never configured Linear are unchanged.
func TestACMMIssue_WorkSourceGitHubStaysGitHub(t *testing.T) {
	gh := covBGitHubMux(t)
	s := NewServer(0, covBLogger())
	deps := testDeps(t)
	deps.GHClient = ghpkg.NewClientForTest(gh.URL, "myorg", []string{"repo1"}, covBLogger())
	deps.Config.Governor.ACMM.IssueTracker = config.ACMMIssueTrackerWorkSource
	s.RegisterAPI(deps)

	rec := doPost(s, "/api/acmm/issue", map[string]interface{}{
		"repo": "repo1", "criterion_id": "acmm:claude-md",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeJSON(t, rec); got["tracker"] != "github" {
		t.Fatalf("tracker = %v, want github", got["tracker"])
	}
}

func TestACMMIssue_LinearMisconfiguredIs500(t *testing.T) {
	s, _, _ := linearHive(t, config.ACMMIssueTrackerWorkSource)
	s.deps.Config.Governor.WorkSource.Linear.APIKey = ""
	rec := doPost(s, "/api/acmm/issue", map[string]interface{}{
		"repo": "repo1", "criterion_id": "acmm:claude-md",
	})
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "api_key") {
		t.Fatalf("status = %d body=%s, want 500 naming api_key", rec.Code, rec.Body.String())
	}
}

// The evaluation response tells the UI the effective default so the
// selector can pre-select it, and reflects config edits immediately even
// when the evaluation itself is served from cache.
func TestACMMEvaluation_ReportsIssueTracker(t *testing.T) {
	s, _, _ := linearHive(t, "")
	rec := doGet(s, "/api/acmm/evaluation")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := decodeJSON(t, rec)
	if got["issue_tracker"] != "github" || got["work_source_type"] != "linear" {
		t.Fatalf("got issue_tracker=%v work_source_type=%v", got["issue_tracker"], got["work_source_type"])
	}

	s.deps.Config.Governor.ACMM.IssueTracker = config.ACMMIssueTrackerWorkSource
	rec = doGet(s, "/api/acmm/evaluation") // served from the cache
	if got := decodeJSON(t, rec); got["issue_tracker"] != "linear" {
		t.Fatalf("issue_tracker after config edit = %v, want linear", got["issue_tracker"])
	}
}
