package dashboard

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A nonexistent agent returns 404 from the full-log endpoint (CaptureFullLog
// reports "agent not found"), matching the /api/pane not-found contract.
func TestHandleAgentFullLog_AgentNotFound(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/agents/nonexistent/log")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// A known-but-not-running agent has no tmux session yet, so CaptureFullLog
// returns the "no active session" error and the handler reports 404 — never a
// 200 with an empty body that would look like a real (empty) log.
func TestHandleAgentFullLog_NoActiveSession(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/agents/scanner/log")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an agent with no active session", rec.Code)
	}
}

// With no agent manager wired, the endpoint reports 503 rather than panicking
// on a nil dereference.
func TestHandleAgentFullLog_NoManager(t *testing.T) {
	s, _ := apiServer(t)
	s.deps.AgentMgr = nil
	rec := doGet(s, "/api/agents/scanner/log")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when AgentMgr is nil", rec.Code)
	}
}

// The download variant is still routed to the same handler; with no active
// session it 404s exactly like the inline variant (the disposition header is
// only reached on success, exercised where a live tmux session exists).
func TestHandleAgentFullLog_DownloadRouted(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/agents/scanner/log?download=1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandleAgentFullLogSuccessDownloadAndExplainFilter(t *testing.T) {
	s, _ := apiServer(t)
	const capturedLog = "ordinary\nEXPLAIN: kept reasoning\n"
	s.captureFullLogFn = func(name string) (string, error) {
		if name != "scanner" {
			return "", fmt.Errorf("unexpected agent %q", name)
		}
		return capturedLog, nil
	}

	rec := doGet(s, "/api/agents/scanner/log?download=1&explain=only")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "EXPLAIN: kept reasoning\n" {
		t.Fatalf("body = %q, want explain-only log", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/plain", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	disposition := rec.Header().Get("Content-Disposition")
	if !strings.Contains(disposition, `attachment; filename="hive-scanner-`) || !strings.HasSuffix(disposition, `.log"`) {
		t.Fatalf("Content-Disposition = %q, want scanner log attachment", disposition)
	}
}

func TestSanitizeLogFilename(t *testing.T) {
	cases := map[string]string{
		"scanner":       "scanner",
		"my-agent_1":    "my-agent_1",
		"weird/../name": "weird----name",
		"quote\"inject": "quote-inject",
		"space name":    "space-name",
		"emoji-🐝":       "emoji--", // multi-byte rune collapses to a single '-'
		"":              "agent",
		"...":           "---",
		"UPPER123":      "UPPER123",
	}
	for in, want := range cases {
		if got := sanitizeLogFilename(in); got != want {
			t.Errorf("sanitizeLogFilename(%q) = %q, want %q", in, got, want)
		}
	}
	// The result must never contain a path separator or a double quote, which
	// would break out of the Content-Disposition filename.
	for _, in := range []string{"a/b", "a\\b", "a\"b", "../../etc/passwd"} {
		got := sanitizeLogFilename(in)
		if strings.ContainsAny(got, `/\"`) {
			t.Errorf("sanitizeLogFilename(%q) = %q contains an unsafe character", in, got)
		}
	}
}
