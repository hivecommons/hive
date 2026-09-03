package dashboard

// Tests for the kick-history endpoints (#4296, #4295): list archived kick
// logs, fetch one (with redaction and download support), and the HTML index
// page — plus the no-manager and unknown-agent failure contracts, mirroring
// terminal_log_test.go.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
)

// kickHistoryServer wires apiServer's manager at a temp archive dir and
// plants one archived kick log for the "scanner" agent.
func kickHistoryServer(t *testing.T, content string) (*Server, string) {
	t.Helper()
	s, deps := apiServer(t)
	dir := t.TempDir()
	deps.AgentMgr.SetKickLogDir(dir)
	id := "20260820-070000.000-kick.log"
	agentDir := filepath.Join(dir, "scanner")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, id), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return s, id
}

func TestHandleAgentKickLogList_UnknownAgent(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/agents/nonexistent/kicks")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// A known agent with no archive directory yet gets an empty JSON array, never
// an error — the "less history than normal" requirement from #4296.
func TestHandleAgentKickLogList_NoHistoryYet(t *testing.T) {
	s, deps := apiServer(t)
	deps.AgentMgr.SetKickLogDir(t.TempDir())
	rec := doGet(s, "/api/agents/scanner/kicks")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var infos []agent.KickLogInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &infos); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, rec.Body.String())
	}
	if len(infos) != 0 {
		t.Fatalf("archives = %d, want 0", len(infos))
	}
}

func TestHandleAgentKickLogList_ReturnsArchives(t *testing.T) {
	s, id := kickHistoryServer(t, "old run output")
	rec := doGet(s, "/api/agents/scanner/kicks")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var infos []agent.KickLogInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &infos); err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != id || infos[0].Reason != "kick" {
		t.Fatalf("infos = %+v, want the planted archive", infos)
	}
}

func TestHandleAgentKickLog_ServesContent(t *testing.T) {
	s, id := kickHistoryServer(t, "old run output")
	rec := doGet(s, "/api/agents/scanner/kicks/"+id)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "old run output") {
		t.Errorf("body missing archive content: %s", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

// Archived pane content is redacted exactly like the live full log — an
// archive must never become a token-exfiltration bypass.
func TestHandleAgentKickLog_RedactsTokens(t *testing.T) {
	secret := "ghp_" + strings.Repeat("a", 36)
	s, id := kickHistoryServer(t, "token: "+secret+"\n")
	rec := doGet(s, "/api/agents/scanner/kicks/"+id)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Error("archived token served unredacted")
	}
}

func TestHandleAgentKickLog_Download(t *testing.T) {
	s, id := kickHistoryServer(t, "content")
	rec := doGet(s, "/api/agents/scanner/kicks/"+id+"?download=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment;") || !strings.Contains(cd, "scanner") {
		t.Errorf("Content-Disposition = %q", cd)
	}
}

func TestHandleAgentKickLog_NotFoundAndTraversal(t *testing.T) {
	s, _ := kickHistoryServer(t, "content")
	for _, id := range []string{"20990101-000000.000-kick.log", "..%2F..%2Fetc%2Fpasswd", "evil.txt"} {
		rec := doGet(s, "/api/agents/scanner/kicks/"+id)
		if rec.Code != http.StatusNotFound {
			t.Errorf("id %q: status = %d, want 404", id, rec.Code)
		}
	}
}

func TestHandleAgentKickLog_NoManager(t *testing.T) {
	s, _ := apiServer(t)
	s.deps.AgentMgr = nil
	for _, path := range []string{
		"/api/agents/scanner/kicks",
		"/api/agents/scanner/kicks/x.log",
		"/agents/scanner/kicks",
	} {
		rec := doGet(s, path)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503", path, rec.Code)
		}
	}
}

func TestHandleAgentKickHistoryPage(t *testing.T) {
	s, id := kickHistoryServer(t, "content")
	rec := doGet(s, "/agents/scanner/kicks")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"kick log history",
		"/api/agents/scanner/log",         // live-log link first
		"/api/agents/scanner/kicks/" + id, // archived kick link
		"download",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q:\n%s", want, body)
		}
	}
}

func TestHandleAgentKickHistoryPage_EmptyHistory(t *testing.T) {
	s, deps := apiServer(t)
	deps.AgentMgr.SetKickLogDir(t.TempDir())
	rec := doGet(s, "/agents/scanner/kicks")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No archived kick logs yet") {
		t.Errorf("empty-history page missing placeholder:\n%s", rec.Body.String())
	}
	rec = doGet(s, "/agents/nonexistent/kicks")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown agent: status = %d, want 404", rec.Code)
	}
}
