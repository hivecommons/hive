package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestSecurityHeadersSnapshotFrameAncestorsMatrix(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		ancestors   []string
		wantFrame   string
		wantNoXFO   bool
		wantXFODeny bool
		wantXFO     string
	}{
		{
			name:        "default denies snapshot framing",
			path:        "/snapshot",
			wantFrame:   "frame-ancestors 'none'",
			wantXFODeny: true,
		},
		{
			name:      "allowlist applies to snapshot document only",
			path:      "/snapshot",
			ancestors: []string{"https://docs.projectbluefin.io", "https://example.com:8443"},
			wantFrame: "frame-ancestors https://docs.projectbluefin.io https://example.com:8443",
			wantNoXFO: true,
		},
		{
			name:        "other routes stay denied with allowlist configured",
			path:        "/",
			ancestors:   []string{"https://docs.projectbluefin.io"},
			wantFrame:   "frame-ancestors 'none'",
			wantXFODeny: true,
		},
		{
			name:        "snapshot dependencies do not get framing exemption",
			path:        "/api/snapshot",
			ancestors:   []string{"https://docs.projectbluefin.io"},
			wantFrame:   "frame-ancestors 'none'",
			wantXFODeny: true,
		},
		{
			// The Sessions section embeds this document in a same-origin iframe.
			name:      "user-session terminal may be framed by this origin only",
			path:      "/terminal/?arg=hive-sess-6772199e9bc0&code=abc",
			wantFrame: "frame-ancestors 'self'",
			wantXFO:   "SAMEORIGIN",
		},
		{
			name:      "user-session terminal websocket keeps the same framing policy",
			path:      "/terminal/ws?arg=hive-sess-6772199e9bc0",
			wantFrame: "frame-ancestors 'self'",
			wantXFO:   "SAMEORIGIN",
		},
		{
			name:        "agent terminal stays unframeable",
			path:        "/terminal/?arg=hive-scanner&code=abc",
			wantFrame:   "frame-ancestors 'none'",
			wantXFODeny: true,
		},
		{
			name:        "session prefix outside /terminal grants nothing",
			path:        "/api/sessions?arg=hive-sess-6772199e9bc0",
			wantFrame:   "frame-ancestors 'none'",
			wantXFODeny: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{deps: &Dependencies{Config: &config.Config{
				Dashboard: config.DashboardConfig{SnapshotFrameAncestors: tt.ancestors},
			}}}
			handler := s.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))

			csp := rec.Header().Get("Content-Security-Policy")
			if !strings.Contains(csp, tt.wantFrame) {
				t.Fatalf("CSP = %q, want to contain %q", csp, tt.wantFrame)
			}
			xfo := rec.Header().Get("X-Frame-Options")
			if tt.wantNoXFO && xfo != "" {
				t.Fatalf("X-Frame-Options = %q, want omitted", xfo)
			}
			if tt.wantXFODeny && xfo != "DENY" {
				t.Fatalf("X-Frame-Options = %q, want DENY", xfo)
			}
			if tt.wantXFO != "" && xfo != tt.wantXFO {
				t.Fatalf("X-Frame-Options = %q, want %q", xfo, tt.wantXFO)
			}
		})
	}
}
