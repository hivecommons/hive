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
		})
	}
}
