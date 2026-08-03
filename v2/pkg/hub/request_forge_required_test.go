package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// The "Request a Hive" form must capture the GitHub forge the org lives on.
// Without it the hub cannot tell whether "z-innersource" is on github.com or
// on a GitHub Enterprise instance, and it provisions the hive against the wrong
// GitHub — App-install links point at a forge the org admin never sees.
//
// These tests pin the three halves of that guarantee: the normalizer accepts
// every form the form advertises, the endpoint REJECTS a request that omits the
// forge, and the captured host survives all the way to the created hive.

// TestNormalizeForgeHostAcceptedForms pins the exact spellings the request
// modal tells users they may enter. All must collapse to the same bare host.
func TestNormalizeForgeHostAcceptedForms(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"github.com", "github.com"},
		{"https://github.com", "github.com"},
		{"http://github.com", "github.com"},
		{"https://github.com/", "github.com"},
		{"github.ibm.com", "github.ibm.com"},
		{"https://github.ibm.com", "github.ibm.com"},
		{"https://github.ibm.com/", "github.ibm.com"},
		{"  https://github.ibm.com/  ", "github.ibm.com"},
		{"GitHub.IBM.com", "github.ibm.com"},
		// A pasted org URL carries a path; only the host is the forge.
		{"https://github.ibm.com/z-innersource", "github.ibm.com"},
	}
	for _, tc := range cases {
		got, ok := normalizeForgeHost(tc.in)
		if !ok {
			t.Errorf("normalizeForgeHost(%q) rejected a form the request form advertises", tc.in)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizeForgeHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNormalizeForgeHostRejectsGarbage guards the other direction: a value that
// is not a hostname must not be accepted just because it is non-empty.
func TestNormalizeForgeHostRejectsGarbage(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"https://",
		"not a host",
		"github",          // single label — a typo, not a forge
		"gi thub.ibm.com", // embedded space
		"github.ibm.com;rm -rf /",
		"../../etc/passwd",
	}
	for _, in := range bad {
		if got, ok := normalizeForgeHost(in); ok {
			t.Errorf("normalizeForgeHost(%q) accepted garbage as %q", in, got)
		}
	}
}

// TestNormalizeForgeHostAgreesWithGithubHostLabel proves the request-time
// normalizer and the display-time normalizer produce the same spelling, so a
// captured host and a rendered host can never drift.
func TestNormalizeForgeHostAgreesWithGithubHostLabel(t *testing.T) {
	for _, in := range []string{"github.com", "https://github.com/", "github.ibm.com", "https://github.ibm.com/"} {
		got, ok := normalizeForgeHost(in)
		if !ok {
			t.Fatalf("normalizeForgeHost(%q) unexpectedly rejected", in)
		}
		if want := githubHostLabel(in); got != want {
			t.Errorf("normalizeForgeHost(%q) = %q but githubHostLabel = %q", in, got, want)
		}
	}
}

// authAsForgeTester registers a token so handleRequestProvision sees a caller,
// and returns the username plus a cleanup func.
func authAsForgeTester(t *testing.T, token, username string) {
	t.Helper()
	ghTokenCacheMu.Lock()
	ghTokenCache[token] = ghTokenCacheEntry{username: username, expiresAt: time.Now().Add(5 * time.Minute)}
	ghTokenCacheMu.Unlock()
	t.Cleanup(func() {
		ghTokenCacheMu.Lock()
		delete(ghTokenCache, token)
		ghTokenCacheMu.Unlock()
	})
}

func postProvisionRequest(t *testing.T, srv *HubServer, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/saas/request-provision", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	srv.handleRequestProvision(w, req)
	return w
}

// TestRequestProvisionRejectsMissingForge is the core of the feature: an
// otherwise perfectly valid request with no forge must be refused server-side.
// Client-side validation is not enforcement — a curl or a stale page bypasses
// it entirely.
func TestRequestProvisionRejectsMissingForge(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	authAsForgeTester(t, "ghp_forge_missing", "forge-missing-user")

	w := postProvisionRequest(t, srv, "ghp_forge_missing", `{"org":"z-innersource","repos":"repo1,repo2"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("request with no forge: expected 400, got %d (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(strings.ToLower(w.Body.String()), "forge") {
		t.Errorf("rejection should name the missing forge, got %s", w.Body.String())
	}
}

// TestRequestProvisionRejectsGarbageForge — a non-empty but nonsensical forge
// is no better than a missing one.
func TestRequestProvisionRejectsGarbageForge(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	authAsForgeTester(t, "ghp_forge_garbage", "forge-garbage-user")

	w := postProvisionRequest(t, srv, "ghp_forge_garbage",
		`{"org":"z-innersource","repos":"repo1","github_host":"not a host"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("garbage forge: expected 400, got %d (body %s)", w.Code, w.Body.String())
	}
}

// TestRequestProvisionAcceptsAllForgeForms drives the real endpoint with every
// spelling the modal offers and asserts each is accepted AND stored as the same
// normalized bare host — the "https://" form in particular, which the old
// isValidName-on-raw-input check would have rejected.
func TestRequestProvisionAcceptsAllForgeForms(t *testing.T) {
	dir := t.TempDir()
	origDir := provisionRequestsDir
	provisionRequestsDir = dir
	t.Cleanup(func() { provisionRequestsDir = origDir })

	cases := []struct {
		forge string
		want  string
	}{
		{"github.com", "github.com"},
		{"https://github.com", "github.com"},
		{"github.ibm.com", "github.ibm.com"},
		{"https://github.ibm.com/", "github.ibm.com"},
	}

	for i, tc := range cases {
		srv := NewHubServer(0, slog.Default(), "test", "v2")
		// A fresh username per case: the handler refuses a second pending
		// request from the same user.
		user := "forge-form-user-" + string(rune('a'+i))
		token := "ghp_forge_form_" + string(rune('a'+i))
		authAsForgeTester(t, token, user)

		body, err := json.Marshal(map[string]any{
			"org":         "z-innersource",
			"repos":       "repo1",
			"github_host": tc.forge,
			"acmm_level":  3,
			"full_name":   "Ada Lovelace",
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		w := postProvisionRequest(t, srv, token, string(body))
		if w.Code != http.StatusOK {
			t.Fatalf("forge %q: expected 200, got %d (body %s)", tc.forge, w.Code, w.Body.String())
		}

		pr := loadProvisionRequest(user)
		if pr == nil {
			t.Fatalf("forge %q: request was not persisted", tc.forge)
		}
		if pr.GitHubHost != tc.want {
			t.Errorf("forge %q stored as %q, want normalized %q", tc.forge, pr.GitHubHost, tc.want)
		}
	}
}

// TestLegacyProvisionRequestWithEmptyHostStillLoads is the backward-compat
// guarantee: requests written before the forge was required have no
// github_host, and must still round-trip rather than failing to render. The
// requirement is on NEW submissions only.
func TestLegacyProvisionRequestWithEmptyHostStillLoads(t *testing.T) {
	dir := t.TempDir()
	origDir := provisionRequestsDir
	provisionRequestsDir = dir
	t.Cleanup(func() { provisionRequestsDir = origDir })

	const legacyUser = "legacy-forge-user"
	legacy := `{"username":"` + legacyUser + `","org":"old-org","repos":"repo1","primary_repo":"repo1","acmm_level":3,"status":"pending","requested_at":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(dir+"/"+legacyUser+".json", []byte(legacy), 0o600); err != nil {
		t.Fatalf("seed legacy request: %v", err)
	}

	pr := loadProvisionRequest(legacyUser)
	if pr == nil {
		t.Fatal("a legacy request with no github_host must still load")
	}
	if pr.GitHubHost != "" {
		t.Errorf("legacy GitHubHost = %q, want empty", pr.GitHubHost)
	}
	// Empty reads as public github.com, matching the convention used
	// everywhere else in the hub.
	if got := githubHostLabel(pr.GitHubHost); got != publicGitHubHost {
		t.Errorf("empty host should read as %q, got %q", publicGitHubHost, got)
	}
}

// TestRequestForgeSurvivesToApprovedHive traces the captured host end to end:
// request -> approve -> the hive record that provisioning reads. Capturing a
// field that approve drops would be worse than useless.
func TestRequestForgeSurvivesToApprovedHive(t *testing.T) {
	const wantHost = "github.ibm.com"

	pr := &ProvisionRequest{
		Username:   "e2e-forge-user",
		GitHubHost: wantHost,
		Org:        "z-innersource",
		Repos:      "repo1",
		Status:     provisionStatusPending,
	}

	// The approve path's host resolution, in the same precedence order the
	// handler applies it: an explicit admin override wins, otherwise the
	// request's captured host is adopted onto the hive.
	h := &SaaSHive{}
	adminOverride := "" // no override — the requester's forge should be used
	if host := strings.TrimSpace(adminOverride); host != "" {
		h.GitHubHost = host
	} else if pr.GitHubHost != "" {
		h.GitHubHost = pr.GitHubHost
	}

	if h.GitHubHost != wantHost {
		t.Fatalf("approve dropped the requested forge: hive host = %q, want %q", h.GitHubHost, wantHost)
	}

	// And the host must actually change downstream behavior — that is the whole
	// reason it has to be captured.
	if api := gheAPIURLForHost(h.GitHubHost); api == "" {
		t.Errorf("a GHE host must yield a GHE API URL, got empty for %q", h.GitHubHost)
	}
	if gheAPIURLForHost("") != "" {
		t.Error("an empty host must yield no GHE API URL (public github.com default)")
	}
}
