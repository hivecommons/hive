package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Regression test for #15723: /api/contribute/ is a public path, so the auth
// middleware never resolved the session cookie there and the in-handler
// requireContributorWrite gate failed closed for a logged-in owner on a
// self-hosted hive (no hub proxy headers).

func postStandby15723(s *Server, path string, sid string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"lane":"main","key":"o/r#1"}`))
	req.Header.Set("Content-Type", "application/json")
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestContributeStandby15723_OwnerSessionResolvesRole(t *testing.T) {
	s := newDirectRouteServer(t, "owner1:owner", "reader1:read")
	ownerSID := s.createUserSession("owner1", "owner")
	readSID := s.createUserSession("reader1", "read")

	for _, path := range []string{"/api/contribute/standby/dispatch", "/api/contribute/standby/clear"} {
		if w := postStandby15723(s, path, ownerSID); w.Code == http.StatusForbidden {
			t.Errorf("%s owner session = 403, want role gate passed; body=%q", path, w.Body.String())
		}
		if w := postStandby15723(s, path, readSID); w.Code != http.StatusForbidden {
			t.Errorf("%s read session = %d, want 403", path, w.Code)
		}
		if w := postStandby15723(s, path, ""); w.Code != http.StatusForbidden {
			t.Errorf("%s anonymous = %d, want 403", path, w.Code)
		}
	}
}
