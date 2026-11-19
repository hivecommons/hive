package proxy

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInferenceNoRouteReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(p *GitHubProxy)
		reason string
	}{
		{name: "never set", setup: func(*GitHubProxy) {}, reason: "reason=never_set"},
		{name: "cleared", setup: func(p *GitHubProxy) {
			p.inference.Set("scanner", &InferenceRoute{Backend: "litellm"})
			p.inference.Clear("scanner")
		}, reason: "reason=cleared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newInferenceTestProxy()
			var logs bytes.Buffer
			p.logger = slog.New(slog.NewTextHandler(&logs, nil))
			tc.setup(p)
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
			req.Header.Set("x-api-key", "sk-hive-scanner")
			w := httptest.NewRecorder()
			p.inferenceTranslatorHandler().ServeHTTP(w, req)
			if w.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", w.Code)
			}
			out := logs.String()
			if !strings.Contains(out, "inference no route") || !strings.Contains(out, tc.reason) {
				t.Fatalf("log missing %q: %s", tc.reason, out)
			}
		})
	}
}
