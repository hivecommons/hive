package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestInceptionHandlers_NonOwnerReturns403 verifies that all owner-protected
// inception handlers reject requests without owner role headers with 403.
// This is a security boundary test — if requireOwnerRole is accidentally
// removed from any handler, this test catches it.
func TestInceptionHandlers_NonOwnerReturns403(t *testing.T) {
	srv := newMinimalServer(t)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"start", "POST", "/api/inception/start", `{"idea":"test"}`},
		{"scan", "POST", "/api/inception/scan", `{"repo_url":"https://example.com/repo"}`},
		{"questions", "POST", "/api/inception/questions", `{"questions":[]}`},
		{"answer", "POST", "/api/inception/answer", `{"answers":{}}`},
		{"facts", "POST", "/api/inception/facts", `{"facts":[]}`},
		{"approve", "POST", "/api/inception/approve", ``},
		{"reset", "POST", "/api/inception/reset", ``},
		{"rename-wiki", "PUT", "/api/inception/wiki-name", `{"name":"test"}`},
		{"import", "POST", "/api/inception/import", ``},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body *strings.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}

			var req *http.Request
			if body != nil {
				req = httptest.NewRequest(tc.method, tc.path, body)
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			// Explicitly set a non-owner role.
			req.Header.Set("X-Hive-Role", "contributor")

			w := httptest.NewRecorder()
			srv.mux.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Errorf("expected 403 for non-owner %s %s, got %d", tc.method, tc.path, w.Code)
			}
		})
	}
}

// TestInceptionHandlers_NoRoleReturns403 verifies that requests with
// no role header at all are rejected with 403.
func TestInceptionHandlers_NoRoleReturns403(t *testing.T) {
	srv := newMinimalServer(t)

	req := httptest.NewRequest("POST", "/api/inception/start", strings.NewReader(`{"idea":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for missing role header, got %d", w.Code)
	}
}

// TestInceptionHandlers_UnprotectedEndpointsNoAuthRequired verifies that
// read-only inception endpoints work without owner role headers.
func TestInceptionHandlers_UnprotectedEndpointsNoAuthRequired(t *testing.T) {
	s, _, _ := covFInceptionServer(t)

	// Start an inception so state endpoints return data.
	rec := doOwnerPost(s, "/api/inception/start", map[string]interface{}{"idea": "auth test idea"})
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d", rec.Code)
	}

	unprotected := []struct {
		name string
		path string
	}{
		{"state", "/api/inception/state"},
		{"scaffold", "/api/inception/scaffold"},
		{"has-files", "/api/inception/has-files"},
		{"ideation-facts", "/api/inception/ideation-facts"},
		{"download", "/api/inception/download"},
	}

	for _, tc := range unprotected {
		t.Run(tc.name, func(t *testing.T) {
			// GET without owner headers — should NOT return 403.
			rec := doGet(s, tc.path)
			if rec.Code == http.StatusForbidden {
				t.Errorf("%s should be accessible without owner role, got 403", tc.path)
			}
		})
	}
}
