package hub

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSaaSAuthCheckPublicStyleAssetsBypassLogin(t *testing.T) {
	srv := newHubServerForTest(t)
	paths := []string{
		"/tokens.css",
		"/components.css",
		"/api/theme.css?scope=contributor",
		"/api/themes?scope=contributor",
		"/api/style?src=owner/repo/dashboard/theme.css&scope=dashboard",
		"/static/integrations/claude.svg",
		"/static/integrations/goose.png",
		"/static/infra/oracle.svg",
	}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, path := range paths {
			t.Run(method+" "+path, func(t *testing.T) {
				req := httptest.NewRequest(method, "/api/saas/auth-check?hive=test-hive", nil)
				req.Header.Set("X-Original-URI", path)
				w := httptest.NewRecorder()
				srv.mux.ServeHTTP(w, req)

				if w.Code != http.StatusOK {
					t.Fatalf("auth-check for public asset %s = %d, want %d", path, w.Code, http.StatusOK)
				}
				if loc := w.Header().Get("Location"); loc != "" {
					t.Fatalf("auth-check for public asset %s redirected to %q", path, loc)
				}
			})
		}
	}
}

func TestSaaSAuthCheckPublicAssetsAreExactAndNarrow(t *testing.T) {
	srv := newHubServerForTest(t)
	paths := []string{
		"/api/other",
		"/api/theme.cssx",
		"/api/themes-extra",
		"/tokens.css.map",
		"/components.css.map",
		"/static/integrations/claude.svg.map",
		"/static/other/logo.svg",
		"/api/contributors",
		"/api/leaderboardish",
		"/contributex",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/saas/auth-check?hive=test-hive", nil)
			req.Header.Set("X-Original-URI", path)
			w := httptest.NewRecorder()
			srv.mux.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("auth-check for non-public path %s = %d, want %d", path, w.Code, http.StatusUnauthorized)
			}
		})
	}
}
