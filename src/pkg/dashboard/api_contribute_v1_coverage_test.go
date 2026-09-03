package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// userMock serves GitHub's /user endpoint so validateGitHubToken resolves a
// login. login=="" returns 401 (invalid token).
func userMock(t *testing.T, login string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if login == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"login": login})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func v1Server(t *testing.T, login string) *Server {
	t.Helper()
	s := covApiServer(t)
	mock := userMock(t, login)
	s.deps.Config.GitHub.APIURL = mock.URL
	// validateGitHubToken resolves through OAuthAPIURL (pinned to api.github.com),
	// not APIURL, so the mock is only reached via the override.
	s.deps.Config.GitHub.OAuthAPIURLOverride = mock.URL
	// /api/v1 reads require allowlist authorization, so authorize the mock login.
	if login != "" {
		s.deps.Config.Dashboard.AuthorizedUsers = []string{login + ":" + config.RoleRead}
	}
	return s
}

func v1Get(s *Server, path, token string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestCovV1_Unauthorized(t *testing.T) {
	s := v1Server(t, "") // /user returns 401
	rec := v1Get(s, "/api/v1/status", "sometoken")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestCovV1_NoToken(t *testing.T) {
	s := v1Server(t, "octocat")
	rec := v1Get(s, "/api/v1/status", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: want 401, got %d", rec.Code)
	}
}

func TestCovV1_Status(t *testing.T) {
	s := v1Server(t, "octocat")
	rec := v1Get(s, "/api/v1/status", "goodtoken")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
}

func TestCovV1_Activity(t *testing.T) {
	s := v1Server(t, "octocat")
	rec := v1Get(s, "/api/v1/activity", "goodtoken2")
	if rec.Code != http.StatusOK {
		t.Fatalf("activity: want 200, got %d", rec.Code)
	}
}

func TestCovV1_Contributors(t *testing.T) {
	s := v1Server(t, "octocat")
	rec := v1Get(s, "/api/v1/contributors", "goodtoken3")
	if rec.Code != http.StatusOK {
		t.Fatalf("contributors: want 200, got %d", rec.Code)
	}
}

func TestCovV1_Knowledge(t *testing.T) {
	s := v1Server(t, "octocat")
	rec := v1Get(s, "/api/v1/knowledge", "goodtoken4")
	if rec.Code != http.StatusOK {
		t.Fatalf("knowledge: want 200, got %d", rec.Code)
	}
}

func TestCovV1_MeNotRegistered(t *testing.T) {
	s := v1Server(t, "nobody-registered")
	rec := v1Get(s, "/api/v1/me", "goodtoken5")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("me not-registered: want 404, got %d", rec.Code)
	}
}

func TestCovV1_UnknownEndpoint(t *testing.T) {
	s := v1Server(t, "octocat")
	rec := v1Get(s, "/api/v1/bogus", "goodtoken6")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown endpoint: want 404, got %d", rec.Code)
	}
}

func TestCovV1_RejectsQueryTokenWithoutReflectingIt(t *testing.T) {
	const secret = "ghp_query_secret_must_not_leak"
	s := v1Server(t, "octocat")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status?token="+secret, nil)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("query token: want 401, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("query token was reflected in the error response")
	}
}

func TestCovV1_RejectsUnknownAuthorizationSchemeWithoutReflectingIt(t *testing.T) {
	const secret = "ghp_scheme_secret_must_not_leak"
	s := v1Server(t, "octocat")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Header.Set("Authorization", "Basic "+secret)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown scheme: want 401, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("authorization credential was reflected in the error response")
	}
}

// The legacy "token <pat>" scheme (what `gh auth token` users send) must keep
// working alongside the bearer scheme.
func TestCovV1_LegacyTokenSchemeStillWorks(t *testing.T) {
	s := v1Server(t, "octocat")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Header.Set("Authorization", "token legacy-pat")
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("token scheme: want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// Reads are hive-private: an authenticated but unauthorized GitHub user gets 403.
func TestCovV1_UnauthorizedUserForbiddenOnReads(t *testing.T) {
	for _, path := range []string{"/api/v1/status", "/api/v1/contributors", "/api/v1/activity", "/api/v1/knowledge"} {
		s := v1Server(t, "outsider")
		s.deps.Config.Dashboard.AuthorizedUsers = []string{"someone-else:" + config.RoleOwner}
		rec := v1Get(s, path, "forbidden-user-token")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: want 403 for unauthorized user, got %d", path, rec.Code)
		}
	}
}

func TestCovV1_AuthorizedUserAllowedOnReads(t *testing.T) {
	s := v1Server(t, "octocat")
	rec := v1Get(s, "/api/v1/contributors", "authorized-read-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("authorized contributors read: want 200, got %d", rec.Code)
	}
}

// /api/v1/me is self-scoped, so it stays reachable without allowlist membership.
func TestCovV1_MeAllowedWithoutAllowlist(t *testing.T) {
	s := v1Server(t, "nobody-registered")
	s.deps.Config.Dashboard.AuthorizedUsers = nil
	rec := v1Get(s, "/api/v1/me", "me-no-allowlist-token")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("me without allowlist: want 404 (not 403), got %d", rec.Code)
	}
}
