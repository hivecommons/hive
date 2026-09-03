package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// These tests pin the config-truth rule for github_auth: when the spoke knows
// from CONFIG that its GitHub App has no installation (SetGitHubAppRequired
// true + state "not-installed"), BOTH health surfaces must fail github_auth
// with "GitHub App not installed" — even while a live GHClient still exists,
// because any such client is riding a cached token that cannot be renewed.

// notInstalledServer builds a full test server carrying a live (non-nil)
// GHClient and the not-installed config-truth state.
func notInstalledServer(t *testing.T) *Server {
	t.Helper()
	srv := newFullServer(t)
	srv.MarkReady()

	// A real, reachable client object — it must NOT mask the config truth.
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("{}"))
	}))
	t.Cleanup(gh.Close)
	srv.deps.GHClient = ghpkg.NewClientForTest(gh.URL, "testorg", []string{"testrepo"}, covBLogger())

	srv.SetGitHubAppRequired(true)
	srv.SetGitHubAppState("not-installed")
	return srv
}

func TestHealthDeep_GitHubAuthFailsFromConfigTruth_DespiteLiveClient(t *testing.T) {
	srv := notInstalledServer(t)

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
	if ga["status"] != "fail" {
		t.Errorf("github_auth status = %v, want fail (live GHClient must not mask a missing installation)", ga["status"])
	}
	detail, _ := ga["detail"].(string)
	if !strings.Contains(detail, "GitHub App not installed") {
		t.Errorf("github_auth detail = %q, want it to name the missing installation", detail)
	}
}

func TestHealthSummary_GitHubAuthFailsFromConfigTruth_DespiteLiveClient(t *testing.T) {
	srv := notInstalledServer(t)

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
			t.Errorf("github_auth status = %q, want fail (live GHClient must not mask a missing installation)", c.Status)
		}
		if !strings.Contains(c.Detail, "GitHub App not installed") {
			t.Errorf("github_auth detail = %q, want it to name the missing installation", c.Detail)
		}
	}
	if !found {
		t.Fatalf("no github_auth check in summary: %s", raw)
	}
}

// TestGitHubAuth_PassesWhenInstallationStateCleared is the contrast case: the
// same live client with the not-installed state cleared must pass, proving the
// failure above comes from the config-truth branch and nothing else.
func TestGitHubAuth_PassesWhenInstallationStateCleared(t *testing.T) {
	srv := notInstalledServer(t)
	srv.SetGitHubAppState("")

	req := httptest.NewRequest("GET", "/api/health/deep", nil)
	w := httptest.NewRecorder()
	srv.handleHealthDeep(w, req)

	var result map[string]any
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ga := result["checks"].(map[string]any)["github_auth"].(map[string]any)
	if ga["status"] != "pass" {
		t.Errorf("github_auth status = %v, want pass once state is cleared", ga["status"])
	}
}
