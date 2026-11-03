package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNoIndexServesRobotsTxtBeforeAuth(t *testing.T) {
	s := newFullServer(t)
	s.authToken = "shared-secret-token"
	h := s.Handler()

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req := httptest.NewRequest(method, "/robots.txt", nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s /robots.txt = %d, want 200 (must not hit the login gate)", method, rec.Code)
		}
		if method == http.MethodGet && rec.Body.String() != dashboardRobotsTxt {
			t.Fatalf("robots.txt body = %q, want %q", rec.Body.String(), dashboardRobotsTxt)
		}
		if got := rec.Header().Get(xRobotsTagHeader); got != xRobotsTagNoIndex {
			t.Fatalf("X-Robots-Tag = %q, want %q", got, xRobotsTagNoIndex)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Fatalf("Content-Type = %q, want text/plain", ct)
		}
	}

	// A POST to the same path is not robots.txt and goes through the normal
	// chain — which, unauthenticated, is the 401 login gate.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/robots.txt", nil))
	if rec.Code == http.StatusOK {
		t.Fatalf("POST /robots.txt = 200; the robots short-circuit must be GET/HEAD only")
	}
}

func TestNoIndexTagsUnauthenticatedLoginPageAndAPI(t *testing.T) {
	s := newFullServer(t)
	s.authToken = "shared-secret-token"
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET / = %d, want 401 login page", rec.Code)
	}
	if got := rec.Header().Get(xRobotsTagHeader); got != xRobotsTagNoIndex {
		t.Fatalf("401 login page X-Robots-Tag = %q, want %q", got, xRobotsTagNoIndex)
	}
	if !strings.Contains(rec.Body.String(), dashboardRobotsMetaTag) {
		t.Fatalf("login page lacks %s", dashboardRobotsMetaTag)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /api/status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get(xRobotsTagHeader); got != xRobotsTagNoIndex {
		t.Fatalf("/api/status X-Robots-Tag = %q, want %q", got, xRobotsTagNoIndex)
	}

	// Public, unauthenticated paths are still not search-engine content.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if got := rec.Header().Get(xRobotsTagHeader); got != xRobotsTagNoIndex {
		t.Fatalf("/api/health X-Robots-Tag = %q, want %q", got, xRobotsTagNoIndex)
	}
}

func TestNoIndexTagsAuthenticatedResponses(t *testing.T) {
	s := newFullServer(t)
	s.authToken = "shared-secret-token"
	h := s.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Header.Set("Authorization", "Bearer shared-secret-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// A fresh server has no status snapshot yet, so health may answer 503; the
	// point here is only that the request cleared the auth gate.
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("authenticated GET /api/health = %d, want it to clear the auth gate", rec.Code)
	}
	if got := rec.Header().Get(xRobotsTagHeader); got != xRobotsTagNoIndex {
		t.Fatalf("authenticated response X-Robots-Tag = %q, want %q", got, xRobotsTagNoIndex)
	}
}
