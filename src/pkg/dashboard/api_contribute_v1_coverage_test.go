package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestCovV1_RejectsNonBearerAuthorizationWithoutReflectingIt(t *testing.T) {
	const secret = "ghp_scheme_secret_must_not_leak"
	s := v1Server(t, "octocat")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Header.Set("Authorization", "token "+secret)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("non-bearer authorization: want 401, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("authorization credential was reflected in the error response")
	}
}
