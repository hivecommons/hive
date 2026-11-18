package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

type auditFilteredBody struct {
	Entries   []AuditEntry `json:"entries"`
	Matched   int          `json:"matched"`
	Truncated bool         `json:"truncated"`
	Scanned   string       `json:"scanned"`
}

func auditGet(t *testing.T, s *Server, target, role string) (*httptest.ResponseRecorder, auditFilteredBody) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("X-Hive-Role", role)
	rec := httptest.NewRecorder()
	if strings.HasPrefix(target, "/api/audit/item") {
		s.handleAuditItemHistory(rec, req)
	} else {
		s.handleAuditLog(rec, req)
	}
	var body auditFilteredBody
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("response is not JSON: %v; body=%q", err, rec.Body.String())
		}
	}
	return rec, body
}

// noAuditFiles points the audit file path at an empty directory so the
// filtered scan reads the ring, whatever the test machine has at /data.
func noAuditFiles(t *testing.T) {
	t.Helper()
	prev := auditLogPath
	auditLogPath = filepath.Join(t.TempDir(), "audit.jsonl")
	t.Cleanup(func() { auditLogPath = prev })
}

// A repo/number filter finds an item's rows that the default 200-row page
// has scrolled past, including a legacy row that names them only in detail
// (repo=<short>, number=N), and nothing from another item or repository.
func TestHandleAuditLogRepoNumberFilterScansPastTheDefaultPage(t *testing.T) {
	noAuditFiles(t)
	now := time.Now().UTC()
	s := &Server{audit: &AuditLog{}}
	ts := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	s.audit.ring = append(s.audit.ring,
		AuditEntry{Timestamp: ts(5 * time.Hour), User: "system", Action: github.AuditActionPRMerged, Detail: "repo=hive, number=42, path=sweep"},
		AuditEntry{Timestamp: ts(4 * time.Hour), User: "reviewer", Action: github.AuditActionPRReviewed, Repo: "hivecommons/hive", Target: 42, Detail: "state=approved"},
		AuditEntry{Timestamp: ts(4 * time.Hour), User: "reviewer", Action: github.AuditActionPRReviewed, Repo: "hivecommons/hive", Target: 43},
		AuditEntry{Timestamp: ts(4 * time.Hour), User: "reviewer", Action: github.AuditActionPRReviewed, Repo: "hivecommons/docs", Target: 42},
		AuditEntry{Timestamp: ts(3 * time.Hour), User: "scanner", Action: github.AuditActionAgentPRCreated, Repo: "hivecommons/hive", Target: 42},
	)
	for i := 0; i < auditMaxEntries+10; i++ {
		s.audit.ring = append(s.audit.ring, AuditEntry{Timestamp: ts(time.Duration(auditMaxEntries+10-i) * time.Second), User: "system", Action: "repos_rescan"})
	}

	rec, body := auditGet(t, s, "/api/audit?repo=hivecommons/hive&number=42", config.RoleReadWrite)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if body.Matched != 3 || len(body.Entries) != 3 || body.Truncated || body.Scanned != "ring" {
		t.Fatalf("matched=%d entries=%d truncated=%v scanned=%q, want the item's 3 rows from the ring", body.Matched, len(body.Entries), body.Truncated, body.Scanned)
	}
	if body.Entries[0].Action != github.AuditActionAgentPRCreated || body.Entries[2].Action != github.AuditActionPRMerged {
		t.Fatalf("entries = %#v, want newest first, ending with the legacy pr_merged row", body.Entries)
	}
	// The default page is unchanged: no matched field, newest 200 only.
	rec, body = auditGet(t, s, "/api/audit", config.RoleReadWrite)
	if rec.Code != http.StatusOK || len(body.Entries) != auditMaxEntries || body.Matched != 0 || body.Scanned != "" {
		t.Fatalf("default page: status=%d entries=%d matched=%d scanned=%q", rec.Code, len(body.Entries), body.Matched, body.Scanned)
	}
}

// The filters run before the 200-row cap: 250 matching rows behind 100
// newer non-matching ones return 200 matching rows, matched=250, truncated.
func TestHandleAuditLogFiltersApplyBeforeTheCap(t *testing.T) {
	noAuditFiles(t)
	now := time.Now().UTC()
	s := &Server{audit: &AuditLog{}}
	for i := 0; i < 250; i++ {
		s.audit.ring = append(s.audit.ring, AuditEntry{Timestamp: now.Add(-time.Duration(400-i) * time.Second).Format(time.RFC3339), User: "scanner", Action: github.AuditActionAgentCommentCreated, Repo: "o/r", Target: 7})
	}
	for i := 0; i < 100; i++ {
		s.audit.ring = append(s.audit.ring, AuditEntry{Timestamp: now.Add(-time.Duration(100-i) * time.Second).Format(time.RFC3339), User: "scanner", Action: github.AuditActionAgentCommentCreated, Repo: "o/other", Target: 7})
	}
	_, body := auditGet(t, s, "/api/audit?repo=o/r", config.RoleReadWrite)
	if body.Matched != 250 || len(body.Entries) != auditMaxEntries || !body.Truncated {
		t.Fatalf("matched=%d entries=%d truncated=%v, want 250 matched, %d returned, truncated", body.Matched, len(body.Entries), body.Truncated, auditMaxEntries)
	}
	for _, e := range body.Entries {
		if e.Repo != "o/r" {
			t.Fatalf("a non-matching row survived the filter: %#v", e)
		}
	}
}

// action matches exact names and trailing-* prefixes; since bounds the
// window; contributor and sensitive combine with them on the same pass.
func TestHandleAuditLogActionSinceContributorSensitive(t *testing.T) {
	noAuditFiles(t)
	now := time.Now().UTC()
	s := &Server{audit: &AuditLog{}}
	s.audit.ring = append(s.audit.ring,
		AuditEntry{Timestamp: now.Add(-30 * time.Hour).Format(time.RFC3339), User: "system", Action: github.AuditActionPRMerged, Repo: "o/r", Target: 1},
		AuditEntry{Timestamp: now.Add(-2 * time.Hour).Format(time.RFC3339), User: "system", Action: github.AuditActionPRMerged, Repo: "o/r", Target: 2},
		AuditEntry{Timestamp: now.Add(-2 * time.Hour).Format(time.RFC3339), User: "system", Action: github.AuditActionPRClosed, Repo: "o/r", Target: 3},
		AuditEntry{Timestamp: now.Add(-1 * time.Hour).Format(time.RFC3339), User: "octocat", Action: "relay_token_minted", Repo: "o/r", Target: 4},
		AuditEntry{Timestamp: now.Add(-1 * time.Hour).Format(time.RFC3339), User: "octocat", Action: "relay_task_assigned", Repo: "o/r", Target: 4},
	)
	_, body := auditGet(t, s, "/api/audit?action=pr_*&since=24h", config.RoleReadWrite)
	if body.Matched != 2 {
		t.Fatalf("pr_* in 24h matched %d, want pr_merged #2 and pr_closed #3: %#v", body.Matched, body.Entries)
	}
	_, body = auditGet(t, s, "/api/audit?action=pr_merged&since="+now.Add(-48*time.Hour).Format(time.RFC3339), config.RoleReadWrite)
	if body.Matched != 2 {
		t.Fatalf("pr_merged since 48h ago matched %d, want 2", body.Matched)
	}
	_, body = auditGet(t, s, "/api/audit?action=relay_*&contributor=Octocat&sensitive=1", config.RoleReadWrite)
	if body.Matched != 1 || body.Entries[0].Action != "relay_token_minted" || !body.Entries[0].Sensitive {
		t.Fatalf("relay_* + contributor + sensitive = %#v, want octocat's sensitive relay_token_minted", body.Entries)
	}
}

func TestHandleAuditLogRejectsBadFilters(t *testing.T) {
	noAuditFiles(t)
	s := &Server{audit: &AuditLog{}}
	for _, target := range []string{"/api/audit?number=abc", "/api/audit?number=0", "/api/audit?since=yesterday", "/api/audit?since=120d"} {
		if rec, _ := auditGet(t, s, target, config.RoleReadWrite); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, rec.Code)
		}
	}
	if rec, _ := auditGet(t, s, "/api/audit?repo=o/r", config.RoleRead); rec.Code != http.StatusForbidden {
		t.Fatalf("read role: status = %d, want 403", rec.Code)
	}
}

// With audit files on disk the filtered scan reads them (the same sources
// the activity collector reads), not the ring a restart emptied.
func TestHandleAuditLogFiltersScanTheAuditFiles(t *testing.T) {
	dir := t.TempDir()
	prev := auditLogPath
	auditLogPath = filepath.Join(dir, "audit.jsonl")
	t.Cleanup(func() { auditLogPath = prev })
	old := time.Now().UTC().Add(-20 * 24 * time.Hour).Format(time.RFC3339)
	line, _ := json.Marshal(AuditEntry{Timestamp: old, User: "system", Action: github.AuditActionPRMerged, Repo: "o/r", Target: 9})
	if err := os.WriteFile(auditLogPath, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{audit: &AuditLog{}}
	_, body := auditGet(t, s, "/api/audit?repo=o/r&number=9", config.RoleReadWrite)
	if body.Scanned != "files" || body.Matched != 1 || body.Entries[0].Target != 9 {
		t.Fatalf("scanned=%q matched=%d entries=%#v, want the file's row", body.Scanned, body.Matched, body.Entries)
	}
}

type itemHistoryBody struct {
	State       string              `json:"state"`
	Entries     []AuditEntry        `json:"entries"`
	LinkedIssue int                 `json:"linked_issue"`
	Merged      *MergedLedgerEntry  `json:"merged"`
	Tracker     *StageTrackerItem   `json:"tracker"`
	Closed      *StageTrackerClosed `json:"closed"`
	Scanned     string              `json:"scanned"`
}

func itemHistory(t *testing.T, s *Server, target string) itemHistoryBody {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("X-Hive-Role", config.RoleReadWrite)
	rec := httptest.NewRecorder()
	s.handleAuditItemHistory(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status = %d, body=%q", target, rec.Code, rec.Body.String())
	}
	var body itemHistoryBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	return body
}

// historyServer is a Server with an in-memory merged ledger and the given
// cached Activity monitor body.
func historyServer(t *testing.T, trackerBody string) *Server {
	t.Helper()
	noAuditFiles(t)
	s := &Server{audit: &AuditLog{}}
	s.merged = newMergedLedger("")
	s.mergedOnce.Do(func() {})
	s.stageTrackerCache.body = []byte(trackerBody)
	return s
}

// The acceptance case: a merged PR that left the board answers from one
// call with its claim (on the linked issue), creation, review, merge and
// who merged it.
func TestAuditItemHistoryMergedPR(t *testing.T) {
	s := historyServer(t, `{"items":[]}`)
	now := time.Now().UTC()
	ts := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	s.audit.ring = append(s.audit.ring,
		AuditEntry{Timestamp: ts(5 * time.Hour), User: "scanner", Action: github.AuditActionIssueClaimed, Repo: "hivecommons/hive", Target: 41},
		AuditEntry{Timestamp: ts(4 * time.Hour), User: "scanner", Action: github.AuditActionAgentPRCreated, Repo: "hivecommons/hive", Target: 42},
		AuditEntry{Timestamp: ts(3 * time.Hour), User: "reviewer", Action: github.AuditActionPRReviewed, Repo: "hivecommons/hive", Target: 42, Detail: "state=approved"},
		AuditEntry{Timestamp: ts(2 * time.Hour), User: "system", Action: github.AuditActionPRMerged, Repo: "hivecommons/hive", Target: 42, Detail: "path=sweep"},
		AuditEntry{Timestamp: ts(2 * time.Hour), User: "system", Action: github.AuditActionPRMerged, Repo: "hivecommons/hive", Target: 77},
	)
	s.merged.Record(MergedLedgerEntry{Repo: "hivecommons/hive", Number: 42, MergedAt: now.Add(-2 * time.Hour), MergedBy: MergedByHive, Lane: "issue", Issue: 41}, now)

	h := itemHistory(t, s, "/api/audit/item?repo=hivecommons/hive&number=42")
	if h.State != "merged" || h.Merged == nil || h.Merged.MergedBy != MergedByHive || h.LinkedIssue != 41 || h.Tracker != nil {
		t.Fatalf("history = %#v, want merged by the hive with linked issue 41", h)
	}
	var actions []string
	for _, e := range h.Entries {
		actions = append(actions, e.Action)
	}
	want := []string{github.AuditActionPRMerged, github.AuditActionPRReviewed, github.AuditActionAgentPRCreated, github.AuditActionIssueClaimed}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Fatalf("entries = %v, want %v (newest first, #77 excluded)", actions, want)
	}
}

func TestAuditItemHistoryOpenAndClosed(t *testing.T) {
	s := historyServer(t, `{"items":[{"repo":"o/r","number":7,"kind":"issue","title":"t","stage":20}],"recently_closed":[{"repo":"o/r","number":8,"closed_at":"2026-11-18T20:00:00Z"}]}`)
	s.audit.ring = append(s.audit.ring,
		AuditEntry{Timestamp: time.Now().UTC().Format(time.RFC3339), User: "system", Action: github.AuditActionPRClosed, Repo: "o/r", Target: 9},
	)
	if h := itemHistory(t, s, "/api/audit/item?repo=o/r&number=7"); h.State != "open" || h.Tracker == nil || h.Tracker.Number != 7 {
		t.Fatalf("open item = %#v", h)
	}
	if h := itemHistory(t, s, "/api/audit/item?repo=o/r&number=8"); h.State != "closed" || h.Closed == nil {
		t.Fatalf("recently closed item = %#v", h)
	}
	if h := itemHistory(t, s, "/api/audit/item?repo=o/r&number=9"); h.State != "closed" || len(h.Entries) != 1 {
		t.Fatalf("item the hive closed = %#v, want closed from its pr_closed row", h)
	}
	if h := itemHistory(t, s, "/api/audit/item?repo=o/r&number=10"); h.State != "unknown" || h.Entries == nil || len(h.Entries) != 0 {
		t.Fatalf("unknown item = %#v, want state unknown and an empty (not null) entries list", h)
	}
}

func TestAuditItemHistoryRefusesBadRequests(t *testing.T) {
	s := historyServer(t, "")
	for _, target := range []string{"/api/audit/item?number=1", "/api/audit/item?repo=o/r", "/api/audit/item?repo=o/r&number=-1"} {
		if rec, _ := auditGet(t, s, target, config.RoleReadWrite); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, rec.Code)
		}
	}
	if rec, _ := auditGet(t, s, "/api/audit/item?repo=o/r&number=1", config.RoleRead); rec.Code != http.StatusForbidden {
		t.Fatalf("read role: status = %d, want 403", rec.Code)
	}
}

// The admin MCP's audit_log sends its filters to GET /api/audit, and
// item_history_read reads GET /api/audit/item, both as the caller.
func TestAdminMCPAuditLogFiltersAndItemHistory(t *testing.T) {
	s := NewServerWithAuth(0, "secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	noAuditFiles(t)
	s.merged = newMergedLedger("")
	s.mergedOnce.Do(func() {})
	now := time.Now().UTC()
	s.audit.ring = append(s.audit.ring,
		AuditEntry{Timestamp: now.Add(-time.Hour).Format(time.RFC3339), User: "system", Action: github.AuditActionPRMerged, Repo: "o/r", Target: 5},
		AuditEntry{Timestamp: now.Format(time.RFC3339), User: "system", Action: github.AuditActionPRMerged, Repo: "o/r", Target: 6},
	)
	s.merged.Record(MergedLedgerEntry{Repo: "o/r", Number: 5, MergedAt: now.Add(-time.Hour), MergedBy: "alice"}, now)
	p := dashboardAdminMCPProvider{server: s, user: "alice", role: config.RoleReadWrite}

	got, err := p.Read(context.Background(), adminmcp.ToolAuditLog, map[string]any{"repo": "o/r", "number": float64(5)})
	if err != nil {
		t.Fatal(err)
	}
	out := got.(map[string]any)
	entries, _ := out["entries"].([]any)
	if out["matched"] != float64(1) || len(entries) != 1 || entries[0].(map[string]any)["target"] != float64(5) {
		t.Fatalf("audit_log = %#v", out)
	}
	hist, err := p.Read(context.Background(), adminmcp.ToolItemHistoryRead, map[string]any{"repo": "o/r", "number": float64(5)})
	if err != nil {
		t.Fatal(err)
	}
	h := hist.(map[string]any)
	if h["state"] != "merged" || h["merged"].(map[string]any)["merged_by"] != "alice" {
		t.Fatalf("item_history_read = %#v", h)
	}
	if _, err := p.Read(context.Background(), adminmcp.ToolItemHistoryRead, map[string]any{"repo": "o/r"}); err == nil {
		t.Fatal("item_history_read without number must be refused")
	}
}
