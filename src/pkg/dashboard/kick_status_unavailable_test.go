package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestKickStatusWithoutAgentManagerIs503 pins the availability contract of the
// kick-status poll (#5325): when the server has no agent manager wired — nil
// deps or nil deps.AgentMgr — the poll must answer 503 with ok=false and an
// error message, NOT a fabricated "unknown"/"failed" outcome. A poll that
// invented a status for an unwired manager could steer the UI's retry logic,
// and 503 is the one code that tells a client the answer is temporarily
// unobtainable rather than that the kick failed.
func TestKickStatusWithoutAgentManagerIs503(t *testing.T) {
	cases := map[string]*Server{
		"nil deps":     {},
		"nil AgentMgr": {deps: &Dependencies{}},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/kick/scanner/status", nil)
			req.SetPathValue("agent", "scanner")
			rec := httptest.NewRecorder()

			s.handleKickStatus(rec, req)

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("kick status without an agent manager = %d, want 503", rec.Code)
			}
			body := decodeKickJSON(t, rec.Body.String())
			if ok, _ := body["ok"].(bool); ok {
				t.Errorf("unavailable manager reported ok=true: %v", body)
			}
			if msg, _ := body["error"].(string); msg == "" {
				t.Errorf("unavailable manager carried no error message: %v", body)
			}
			if _, has := body["status"]; has {
				t.Errorf("unavailable manager must not fabricate a kick status: %v", body)
			}
		})
	}
}
