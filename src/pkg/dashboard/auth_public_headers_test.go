package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func publicIdentityTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	setupContributeEnv(t)
	oldUserTokenPath := userTokenPath
	userTokenPath = t.TempDir() + "/missing-gh-user-token"
	t.Cleanup(func() { userTokenPath = oldUserTokenPath })
	s, _ := apiServer(t)
	s.authToken = "shared-secret-token"
	return s, s.authenticate(s.roleEnforcement(s.mux))
}

func seedPublicIdentityProfile(t *testing.T, username string) {
	t.Helper()
	if err := saveContributorProfile(&ContributorProfile{
		GitHubUsername: username,
		ContributorID:  "c-" + username,
		TrustTier:      "trusted",
		RegisteredAt:   time.Now().UTC().Format(time.RFC3339),
		LabelInterests: []string{"old"},
		Archetype:      "old archetype",
	}); err != nil {
		t.Fatalf("save profile: %v", err)
	}
}

func servePublicIdentityRequest(handler http.Handler, method, path, body string, proof bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-User", "trusted")
	req.Header.Set("X-Hive-Role", "owner")
	if proof {
		req.Header.Set(proxyAuthHeader, "shared-secret-token")
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func TestPublicContributeQueueControlsIgnoreForgedIdentityHeadersWithoutProxyProof(t *testing.T) {
	_, handler := publicIdentityTestServer(t)
	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"queue_order", http.MethodPut, "/api/contribute/queue/order", `{"order":[]}`},
		{"queue_hold", http.MethodPost, "/api/contribute/queue/hold", `{"key":"myorg/repo1#1","held":true}`},
		{"queue_hold_clear", http.MethodPost, "/api/contribute/queue/hold/clear", ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := servePublicIdentityRequest(handler, tc.method, tc.path, tc.body, false)
			if w.Code != http.StatusForbidden {
				t.Fatalf("forged identity without proxy proof got %d, want 403: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestPublicContributeQueueControlsAcceptProxiedIdentityWithValidProof(t *testing.T) {
	_, handler := publicIdentityTestServer(t)
	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"queue_order", http.MethodPut, "/api/contribute/queue/order", `{"order":[]}`},
		{"queue_hold", http.MethodPost, "/api/contribute/queue/hold", `{"key":"myorg/repo1#1","held":true}`},
		{"queue_hold_clear", http.MethodPost, "/api/contribute/queue/hold/clear", ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := servePublicIdentityRequest(handler, tc.method, tc.path, tc.body, true)
			if w.Code != http.StatusOK {
				t.Fatalf("proxied identity with valid proof got %d, want 200: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestPublicContributorSelfServiceIgnoresForgedIdentityHeadersWithoutProxyProof(t *testing.T) {
	_, handler := publicIdentityTestServer(t)
	seedPublicIdentityProfile(t, "trusted")

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   int
	}{
		{"invite", http.MethodPost, "/api/contribute/invite", `{}`, http.StatusUnauthorized},
		{"interests", http.MethodPut, "/api/contribute/interests", `{"interests":["pwned"]}`, http.StatusUnauthorized},
		{"dossier", http.MethodPost, "/api/contribute/dossier", `{"archetype":"pwned"}`, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := servePublicIdentityRequest(handler, tc.method, tc.path, tc.body, false)
			if w.Code != tc.want {
				t.Fatalf("forged identity without proxy proof got %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}

	prof := findContributor("trusted")
	if prof == nil {
		t.Fatal("seeded profile missing")
	}
	if len(prof.LabelInterests) != 1 || prof.LabelInterests[0] != "old" {
		t.Fatalf("forged interests request mutated profile: %+v", prof.LabelInterests)
	}
	if prof.Archetype != "old archetype" {
		t.Fatalf("forged dossier request mutated archetype to %q", prof.Archetype)
	}

	limits := servePublicIdentityRequest(handler, http.MethodGet, "/api/contribute/limits", ``, false)
	if limits.Code != http.StatusOK {
		t.Fatalf("anonymous limits got %d, want 200: %s", limits.Code, limits.Body.String())
	}
	var limitsResp map[string]any
	if err := json.Unmarshal(limits.Body.Bytes(), &limitsResp); err != nil {
		t.Fatalf("decode limits: %v", err)
	}
	if _, ok := limitsResp["you"]; ok {
		t.Fatalf("forged limits request exposed viewer-specific usage: %s", limits.Body.String())
	}

	status := servePublicIdentityRequest(handler, http.MethodGet, "/api/gh-user-auth/status", ``, false)
	if status.Code != http.StatusOK {
		t.Fatalf("auth status got %d, want 200: %s", status.Code, status.Body.String())
	}
	var statusResp map[string]any
	if err := json.Unmarshal(status.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if loggedIn, _ := statusResp["logged_in"].(bool); loggedIn {
		t.Fatalf("forged auth status reported logged in: %s", status.Body.String())
	}
}

func TestPublicContributorSelfServiceAcceptsProxiedIdentityWithValidProof(t *testing.T) {
	_, handler := publicIdentityTestServer(t)
	seedPublicIdentityProfile(t, "trusted")

	invite := servePublicIdentityRequest(handler, http.MethodPost, "/api/contribute/invite", `{}`, true)
	if invite.Code != http.StatusOK {
		t.Fatalf("proxied invite got %d, want 200: %s", invite.Code, invite.Body.String())
	}
	var inviteResp map[string]any
	if err := json.Unmarshal(invite.Body.Bytes(), &inviteResp); err != nil {
		t.Fatalf("decode invite: %v", err)
	}
	if inviteResp["inviter"] != "trusted" {
		t.Fatalf("proxied invite inviter = %v, want trusted", inviteResp["inviter"])
	}

	interests := servePublicIdentityRequest(handler, http.MethodPut, "/api/contribute/interests", `{"interests":["gpu"]}`, true)
	if interests.Code != http.StatusOK {
		t.Fatalf("proxied interests got %d, want 200: %s", interests.Code, interests.Body.String())
	}
	dossier := servePublicIdentityRequest(handler, http.MethodPost, "/api/contribute/dossier", `{"archetype":"verified"}`, true)
	if dossier.Code != http.StatusOK {
		t.Fatalf("proxied dossier got %d, want 200: %s", dossier.Code, dossier.Body.String())
	}

	limits := servePublicIdentityRequest(handler, http.MethodGet, "/api/contribute/limits", ``, true)
	if limits.Code != http.StatusOK {
		t.Fatalf("proxied limits got %d, want 200: %s", limits.Code, limits.Body.String())
	}
	var limitsResp map[string]any
	if err := json.Unmarshal(limits.Body.Bytes(), &limitsResp); err != nil {
		t.Fatalf("decode limits: %v", err)
	}
	you, ok := limitsResp["you"].(map[string]any)
	if !ok || you["username"] != "trusted" {
		t.Fatalf("proxied limits did not include trusted viewer: %s", limits.Body.String())
	}

	status := servePublicIdentityRequest(handler, http.MethodGet, "/api/gh-user-auth/status", ``, true)
	if status.Code != http.StatusOK {
		t.Fatalf("proxied auth status got %d, want 200: %s", status.Code, status.Body.String())
	}
	var statusResp map[string]any
	if err := json.Unmarshal(status.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if loggedIn, _ := statusResp["logged_in"].(bool); !loggedIn || statusResp["username"] != "trusted" || statusResp["role"] != "owner" {
		t.Fatalf("proxied auth status did not report trusted owner: %s", status.Body.String())
	}
}
