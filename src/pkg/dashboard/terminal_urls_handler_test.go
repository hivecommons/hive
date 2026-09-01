package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"
)

// These tests pin the HTTP contract of handleAgentTerminalURLs (#5188),
// previously at 0% coverage: the pipeline behind it (prepareTerminalURLs,
// filterAuthURLs) is tested in terminal_urls_test.go, but the handler's own
// branches — nil deps, the "no pane is not an error" contract, and the
// no-store header — were exercised nowhere.

func decodeTerminalURLs(t *testing.T, body []byte) (urls, authURLs []string) {
	t.Helper()
	var resp struct {
		URLs     []string `json:"urls"`
		AuthURLs []string `json:"authUrls"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("response is not the documented JSON shape: %v (body %q)", err, body)
	}
	if resp.URLs == nil || resp.AuthURLs == nil {
		t.Fatalf("urls/authUrls must be empty lists, never null: body %q", body)
	}
	return resp.URLs, resp.AuthURLs
}

// With no agent manager wired, the endpoint reports 503 rather than panicking
// on a nil dereference — same contract as handleAgentFullLog.
func TestHandleAgentTerminalURLs_NoManager(t *testing.T) {
	s, _ := apiServer(t)
	s.deps.AgentMgr = nil
	rec := doGet(s, "/api/agents/scanner/terminal-urls")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when AgentMgr is nil", rec.Code)
	}
}

// A known-but-not-running agent has no tmux pane to read. Unlike the full-log
// endpoint (which 404s), this endpoint documents "nothing to copy right now"
// as a normal state: 200 with empty lists, so the dashboard hides the control
// instead of surfacing an error.
func TestHandleAgentTerminalURLs_NoActiveSessionIsEmptyNotError(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/agents/scanner/terminal-urls")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an agent with no active session", rec.Code)
	}
	urls, authURLs := decodeTerminalURLs(t, rec.Body.Bytes())
	if len(urls) != 0 || len(authURLs) != 0 {
		t.Fatalf("want empty lists for an agent with no pane, got urls=%v authUrls=%v", urls, authURLs)
	}
}

// A nonexistent agent likewise yields empty lists, never an error — the
// handler treats every CaptureFullLog failure as "no pane".
func TestHandleAgentTerminalURLs_UnknownAgentIsEmptyNotError(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/agents/nonexistent/terminal-urls")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an unknown agent", rec.Code)
	}
	urls, authURLs := decodeTerminalURLs(t, rec.Body.Bytes())
	if len(urls) != 0 || len(authURLs) != 0 {
		t.Fatalf("want empty lists for an unknown agent, got urls=%v authUrls=%v", urls, authURLs)
	}
}
