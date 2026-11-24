package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func boolPtr19618(b bool) *bool { return &b }

// signInPageFor serves an anonymous browser page request through authenticate
// on a direct-route spoke and returns the response.
func signInPageFor(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.authenticate(recordingHandler(nil, nil)).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	return w
}

// With dashboard.github_sign_in unset the login page is the GitHub device-flow
// page, exactly as before #19618.
func TestGitHubSignInDefaultServesDeviceFlowPage(t *testing.T) {
	s := newDirectRouteServer(t, "custom:abc:owner")
	w := signInPageFor(t, s)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if w.Body.String() != loginPage {
		t.Fatal("default config must serve the unchanged GitHub device-flow login page")
	}
}

// With dashboard.github_sign_in: false the login page drops the GitHub button
// and links to the hub's /open route for this hive.
func TestGitHubSignInOffServesHubLink(t *testing.T) {
	s := newDirectRouteServer(t, "custom:abc:owner")
	s.deps.Config.Dashboard.GitHubSignIn = boolPtr19618(false)
	s.deps.Config.Hub.URL = "http://hub.hive.svc:8080"
	s.deps.Config.Dashboard.HubSignInURL = "https://hub.example.com/"
	s.deps.Config.HiveID = "my-hive"

	w := signInPageFor(t, s)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "Sign in with GitHub") || strings.Contains(body, "/api/gh-user-auth/") {
		t.Fatalf("GitHub sign-in must be gone from the page, got:\n%s", body)
	}
	if strings.Contains(body, "<script") {
		t.Fatal("the hub sign-in page must carry no script")
	}
	want := `href="https://hub.example.com/api/saas/hives/my-hive/open"`
	if !strings.Contains(body, want) || !strings.Contains(body, "Sign in through the hub") {
		t.Fatalf("page must link to the browser-facing hub /open route (%s), got:\n%s", want, body)
	}
}

// Without a usable hub URL the page explains the gap instead of a dead link.
func TestGitHubSignInOffWithoutHubExplains(t *testing.T) {
	s := newDirectRouteServer(t, "custom:abc:owner")
	s.deps.Config.Dashboard.GitHubSignIn = boolPtr19618(false)
	s.deps.Config.Hub.URL = ""
	s.deps.Config.HiveID = "my-hive"

	body := signInPageFor(t, s).Body.String()
	if strings.Contains(body, "href=") || strings.Contains(body, "Sign in with GitHub") {
		t.Fatalf("no hub URL: page must show neither a link nor the GitHub button, got:\n%s", body)
	}
	if !strings.Contains(body, "not configured") {
		t.Fatalf("page must say hub sign-in is not configured, got:\n%s", body)
	}
}

// The setting is a real off switch: start and poll refuse, and start never
// reaches GitHub.
func TestGitHubSignInOffRefusesDeviceFlowEndpoints(t *testing.T) {
	s, deps, _ := dfServer(t, "complete", "octocat")
	deps.Config.Dashboard.GitHubSignIn = boolPtr19618(false)

	for _, path := range []string{"/api/gh-user-auth/start", "/api/gh-user-auth/poll"} {
		rec := doPost(s, path, map[string]string{"flow_id": "x"})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403 body=%s", path, rec.Code, rec.Body.String())
		}
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if msg, _ := body["error"].(string); !strings.Contains(msg, "dashboard.github_sign_in") {
			t.Fatalf("%s: error must name the setting, got %q", path, msg)
		}
	}
	if s.deviceFlowState != nil || s.deviceFlowID != "" {
		t.Fatal("a refused start must not leave a device flow in progress")
	}
}

// A flow started while sign-in was on cannot complete after it is turned off.
func TestGitHubSignInOffRefusesPollOfEarlierFlow(t *testing.T) {
	s, deps, _ := dfServer(t, "complete", "octocat")
	rec := doPost(s, "/api/gh-user-auth/start", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: want 200, got %d", rec.Code)
	}
	var start map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &start)

	deps.Config.Dashboard.GitHubSignIn = boolPtr19618(false)
	rec = doPost(s, "/api/gh-user-auth/poll", map[string]any{"flow_id": start["flow_id"]})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("poll after switch-off: status = %d, want 403", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			t.Fatal("poll after switch-off must not mint a session")
		}
	}
}
