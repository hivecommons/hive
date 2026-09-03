package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// These tests pin the config-truth rule for the OPERATOR-SIDE credential
// states (githubAppCredsUndelivered), the sibling of the not-installed rule in
// github_auth_config_truth_test.go: a required App whose private key never
// arrived (key-missing / no-app-assigned) or cannot sign for it (key-invalid)
// must fail github_auth in BOTH health surfaces, even while a live token-based
// GHClient still exists. A token client is exactly what let a key-missing hive
// (kelly-headwaters, 2026-08-12 → 2026-08-20) show github_auth ✓ "token-based"
// for 8 days while no agent could act as the App.

// operatorStateServer builds a full test server carrying a live (non-nil)
// GHClient and the given operator-side App auth state.
func operatorStateServer(t *testing.T, state string) *Server {
	t.Helper()
	srv := newFullServer(t)
	srv.MarkReady()

	// A real, reachable token client — it must NOT mask the config truth.
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("{}"))
	}))
	t.Cleanup(gh.Close)
	srv.deps.GHClient = ghpkg.NewClientForTest(gh.URL, "testorg", []string{"testrepo"}, covBLogger())

	srv.SetGitHubAppRequired(true)
	srv.SetGitHubAppState(state)
	return srv
}

func deepGitHubAuth(t *testing.T, srv *Server) map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/health/deep", nil)
	w := httptest.NewRecorder()
	srv.handleHealthDeep(w, req)

	var result map[string]any
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	checks, ok := result["checks"].(map[string]any)
	if !ok {
		t.Fatalf("missing checks in %v", result)
	}
	ga, ok := checks["github_auth"].(map[string]any)
	if !ok {
		t.Fatalf("missing github_auth check in %v", checks)
	}
	return ga
}

func TestHealthDeep_GitHubAuthFailsOnOperatorSideStates_DespiteLiveClient(t *testing.T) {
	cases := []struct {
		state      string
		wantDetail string
	}{
		{"key-missing", "not delivered by the hub"},
		{"no-app-assigned", "not delivered by the hub"},
		{"key-invalid", "does not match the App"},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			srv := operatorStateServer(t, tc.state)
			ga := deepGitHubAuth(t, srv)
			if ga["status"] != "fail" {
				t.Errorf("github_auth status = %v, want fail (a token client must not mask undelivered App credentials)", ga["status"])
			}
			detail, _ := ga["detail"].(string)
			if !strings.Contains(detail, tc.wantDetail) {
				t.Errorf("github_auth detail = %q, want it to contain %q", detail, tc.wantDetail)
			}
		})
	}
}

func TestHealthSummary_GitHubAuthFailsOnKeyMissing_DespiteLiveClient(t *testing.T) {
	srv := operatorStateServer(t, "key-missing")

	sum := srv.HealthSummary()
	raw, err := json.Marshal(sum)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	var parsed struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal summary: %v", err)
	}
	found := false
	for _, c := range parsed.Checks {
		if c.Name != "github_auth" {
			continue
		}
		found = true
		if c.Status != "fail" {
			t.Errorf("github_auth status = %q, want fail (a token client must not mask undelivered App credentials)", c.Status)
		}
		if !strings.Contains(c.Detail, "not delivered by the hub") {
			t.Errorf("github_auth detail = %q, want it to name the undelivered credentials", c.Detail)
		}
	}
	if !found {
		t.Fatalf("no github_auth check in summary: %s", raw)
	}
}

// The contrast cases: the same live client must pass once the operator-side
// state clears (key delivered), and an operator-side token with the App NOT
// required must not fail — proving the failures above come from the
// config-truth branch and nothing else.
func TestGitHubAuth_PassesOnceOperatorStateClears(t *testing.T) {
	srv := operatorStateServer(t, "key-missing")
	srv.SetGitHubAppState("")

	ga := deepGitHubAuth(t, srv)
	if ga["status"] != "pass" {
		t.Errorf("github_auth status = %v, want pass once the key-missing state is cleared", ga["status"])
	}
}

func TestGitHubAuth_KeyMissingWithoutAppRequiredDoesNotFail(t *testing.T) {
	srv := operatorStateServer(t, "key-missing")
	srv.SetGitHubAppRequired(false)

	ga := deepGitHubAuth(t, srv)
	if ga["status"] != "pass" {
		t.Errorf("github_auth status = %v, want pass when the App is not required", ga["status"])
	}
}
