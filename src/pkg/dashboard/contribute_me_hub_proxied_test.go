package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// ownerTokenServer builds a server that has the hub owner's GitHub token
// persisted on disk (the single-owner fallback resolveViewerUsername reads) and
// a GitHub /user mock that resolves that token to `owner`.
func ownerTokenServer(t *testing.T, owner string, hubProxied bool) *Server {
	t.Helper()
	setupContributeEnv(t)
	s := covApiServer(t)
	mock := userMock(t, owner)
	s.deps.Config.GitHub.OAuthAPIURLOverride = mock.URL
	s.deps.Config.Dashboard.HubProxied = hubProxied
	// testDeps → isolateDashboardState already pointed userTokenPath into a
	// temp dir; write the owner's token there.
	if err := os.WriteFile(userTokenPath, []byte("gho_owner\n"), 0o600); err != nil {
		t.Fatalf("write owner token: %v", err)
	}
	return s
}

// TestContributeMeHubProxiedAnonymousIsNotTheOwner reproduces the Operations
// "Your contribution" panel showing the hub OWNER's totals to a signed-out
// visitor. On a hub-proxied spoke the hub's nginx injects X-Hive-User for every
// signed-in visitor, so a request without it is anonymous — it must get 401,
// not the owner's persisted-token identity.
func TestContributeMeHubProxiedAnonymousIsNotTheOwner(t *testing.T) {
	s := ownerTokenServer(t, "castrojo", true)
	seedStatsProfile(t, "castrojo", 300, 14, 3066)

	rec := getAs(s, "/api/contribute/me", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /api/contribute/me on hub-proxied spoke = %d, body %s; want 401", rec.Code, rec.Body.String())
	}

	// A hub-identified visitor still gets THEIR record, not the owner's.
	seedStatsProfile(t, "Danathar", 228, 40, 23)
	rec = getAs(s, "/api/contribute/me", "Danathar")
	if rec.Code != http.StatusOK {
		t.Fatalf("identified GET = %d, want 200", rec.Code)
	}
	var got struct {
		Username string `json:"github_username"`
		Failed   int    `json:"total_tasks_failed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Username != "Danathar" || got.Failed != 23 {
		t.Fatalf("identified viewer got %+v, want Danathar/23", got)
	}
}

// TestContributeMeSingleOwnerSpokeStillResolvesOwner locks in that the fix is
// scoped to hub-proxied spokes: on a single-owner spoke (no hub nginx, no
// allowlist) the persisted owner token is still the operator's identity.
func TestContributeMeSingleOwnerSpokeStillResolvesOwner(t *testing.T) {
	s := ownerTokenServer(t, "solo", false)
	seedStatsProfile(t, "solo", 5, 2, 1)

	rec := getAs(s, "/api/contribute/me", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("single-owner GET /api/contribute/me = %d, body %s; want 200", rec.Code, rec.Body.String())
	}
	var got struct {
		Username string `json:"github_username"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Username != "solo" {
		t.Fatalf("single-owner viewer resolved as %q, want solo", got.Username)
	}
}

// TestGHUserAuthStatusHubProxiedAnonymousLoggedOut covers the sibling
// fallback in handleGHUserAuthStatus: the same owner token must not make an
// anonymous hub-proxied request report logged_in=true as the owner.
func TestGHUserAuthStatusHubProxiedAnonymousLoggedOut(t *testing.T) {
	s := ownerTokenServer(t, "castrojo", true)

	req := httptest.NewRequest(http.MethodGet, "/api/gh-user-auth/status", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth status = %d, want 200", rec.Code)
	}
	var got struct {
		LoggedIn bool   `json:"logged_in"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.LoggedIn || got.Username != "" {
		t.Fatalf("anonymous hub-proxied auth status = %+v, want logged_in=false", got)
	}
}

// TestContributeMeTokenProtectedSpokeAnonymousIsNotTheOwner covers the third
// deployment shape (#7394, residual of #7362): a standalone spoke protected by
// a dashboard auth token. /api/contribute* is public, so anonymous internet
// requests reach the handler — the persisted owner token must not stand in
// for them there either.
func TestContributeMeTokenProtectedSpokeAnonymousIsNotTheOwner(t *testing.T) {
	s := ownerTokenServer(t, "solo", false)
	s.authToken = "spoke-dashboard-token"
	seedStatsProfile(t, "solo", 5, 2, 1)

	rec := getAs(s, "/api/contribute/me", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /api/contribute/me on token-protected spoke = %d, body %s; want 401", rec.Code, rec.Body.String())
	}
}

// TestGHUserAuthStatusTokenProtectedSpokeAnonymousLoggedOut is the sibling
// for handleGHUserAuthStatus: an anonymous caller on a token-protected spoke
// must read as logged-out, not learn the owner's GitHub identity.
func TestGHUserAuthStatusTokenProtectedSpokeAnonymousLoggedOut(t *testing.T) {
	s := ownerTokenServer(t, "solo", false)
	s.authToken = "spoke-dashboard-token"

	req := httptest.NewRequest(http.MethodGet, "/api/gh-user-auth/status", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth status = %d, want 200", rec.Code)
	}
	var got struct {
		LoggedIn bool   `json:"logged_in"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.LoggedIn || got.Username != "" {
		t.Fatalf("anonymous token-protected auth status = %+v, want logged_in=false", got)
	}
}
