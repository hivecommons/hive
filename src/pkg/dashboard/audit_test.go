package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestAuditLogLogDefaultsEmptyUserToSystem(t *testing.T) {
	audit := &AuditLog{}

	audit.Log("", "startup", "", "")

	entries := audit.Recent(1)
	if len(entries) != 1 {
		t.Fatalf("Recent(1) returned %d entries, want 1", len(entries))
	}
	if entries[0].User != "system" {
		t.Fatalf("empty user logged as %q, want system", entries[0].User)
	}
	if actions := audit.LastUserActions(); len(actions) != 0 {
		t.Fatalf("system audit entry updated LastUserActions: %#v", actions)
	}
}

func TestAuditLogLogRecordsAllFields(t *testing.T) {
	audit := &AuditLog{}

	audit.Log("alice", "config.save", "file=hive.yaml", "scanner")

	entries := audit.Recent(1)
	if len(entries) != 1 {
		t.Fatalf("Recent(1) returned %d entries, want 1", len(entries))
	}
	entry := entries[0]
	if _, err := time.Parse(time.RFC3339, entry.Timestamp); err != nil {
		t.Fatalf("Timestamp %q is not RFC3339: %v", entry.Timestamp, err)
	}
	if entry.User != "alice" || entry.Action != "config.save" || entry.Detail != "file=hive.yaml" || entry.Agent != "scanner" {
		t.Fatalf("entry = %#v, want all fields preserved", entry)
	}
}

func TestAuditLogRingCapEnforced(t *testing.T) {
	audit := &AuditLog{}

	for i := 0; i < auditRingCap+5; i++ {
		audit.Log("alice", fmt.Sprintf("action-%03d", i), "", "")
	}

	entries := audit.Recent(0)
	if len(entries) != auditRingCap {
		t.Fatalf("Recent(0) returned %d entries, want ring cap %d", len(entries), auditRingCap)
	}
	if newest := entries[0].Action; newest != fmt.Sprintf("action-%03d", auditRingCap+4) {
		t.Fatalf("newest action = %q, want latest action", newest)
	}
	if oldest := entries[len(entries)-1].Action; oldest != "action-005" {
		t.Fatalf("oldest retained action = %q, want action-005 after trimming first five", oldest)
	}
}

func TestAuditLogRecentReturnsNewestFirst(t *testing.T) {
	audit := &AuditLog{}
	audit.ring = []AuditEntry{
		{Action: "oldest"},
		{Action: "middle"},
		{Action: "newest"},
	}

	entries := audit.Recent(0)

	want := []string{"newest", "middle", "oldest"}
	if len(entries) != len(want) {
		t.Fatalf("Recent(0) returned %d entries, want %d", len(entries), len(want))
	}
	for i, wantAction := range want {
		if entries[i].Action != wantAction {
			t.Fatalf("entries[%d].Action = %q, want %q (entries=%#v)", i, entries[i].Action, wantAction, entries)
		}
	}
}

func TestAuditLogRecentLimitsOutput(t *testing.T) {
	audit := &AuditLog{}
	audit.ring = []AuditEntry{
		{Action: "oldest"},
		{Action: "middle"},
		{Action: "newest"},
	}

	entries := audit.Recent(2)

	want := []string{"newest", "middle"}
	if len(entries) != len(want) {
		t.Fatalf("Recent(2) returned %d entries, want %d", len(entries), len(want))
	}
	for i, wantAction := range want {
		if entries[i].Action != wantAction {
			t.Fatalf("entries[%d].Action = %q, want %q", i, entries[i].Action, wantAction)
		}
	}
}

func TestAuditLogNoteUserActionSkipsPseudoUsers(t *testing.T) {
	audit := &AuditLog{}
	ts := "2026-08-11T20:31:38Z"

	for _, user := range []string{"", "system", "local", "unknown"} {
		audit.noteUserAction(user, ts)
	}

	if actions := audit.LastUserActions(); len(actions) != 0 {
		t.Fatalf("pseudo-users updated LastUserActions: %#v", actions)
	}
}

func TestAuditLogNoteUserActionTracksRealUsersAsRFC3339(t *testing.T) {
	audit := &AuditLog{}
	ts := "2026-08-11T20:31:38Z"

	audit.noteUserAction("alice", ts)

	actions := audit.LastUserActions()
	if got := actions["alice"]; got != ts {
		t.Fatalf("LastUserActions()[alice] = %q, want %q", got, ts)
	}
	if _, err := time.Parse(time.RFC3339, actions["alice"]); err != nil {
		t.Fatalf("LastUserActions()[alice] is not RFC3339: %v", err)
	}
}

func TestAuditLogNoteUserActionNeverMovesBackward(t *testing.T) {
	audit := &AuditLog{}
	later := "2026-08-11T20:31:38Z"
	earlier := "2026-08-11T19:31:38Z"

	audit.noteUserAction("alice", later)
	audit.noteUserAction("alice", earlier)

	if got := audit.LastUserActions()["alice"]; got != later {
		t.Fatalf("LastUserActions()[alice] moved backward to %q, want %q", got, later)
	}
}

func TestAuditLogNoteUserActionBoundsMapSize(t *testing.T) {
	audit := &AuditLog{}
	initial := "2026-08-11T20:31:38Z"
	later := "2026-08-11T21:31:38Z"

	for i := 0; i < auditMaxTrackedActionUsers; i++ {
		audit.noteUserAction(fmt.Sprintf("user-%03d", i), initial)
	}
	audit.noteUserAction("overflow", initial)
	audit.noteUserAction("user-000", later)

	actions := audit.LastUserActions()
	if len(actions) != auditMaxTrackedActionUsers {
		t.Fatalf("LastUserActions has %d users, want cap %d", len(actions), auditMaxTrackedActionUsers)
	}
	if _, ok := actions["overflow"]; ok {
		t.Fatal("overflow user was tracked after map reached cap")
	}
	if got := actions["user-000"]; got != later {
		t.Fatalf("existing user was not updated after map reached cap: got %q, want %q", got, later)
	}
}

func TestHandleAuditLogForbidsInsufficientRole(t *testing.T) {
	server := &Server{audit: &AuditLog{}}
	req := httptest.NewRequest(http.MethodGet, "/api/audit", nil)
	req.Header.Set("X-Hive-Role", config.RoleRead)
	rec := httptest.NewRecorder()

	server.handleAuditLog(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestHandleAuditLogRejectsMissingRole(t *testing.T) {
	server := &Server{audit: &AuditLog{}}
	req := httptest.NewRequest(http.MethodGet, "/api/audit", nil)
	rec := httptest.NewRecorder()

	server.handleAuditLog(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing X-Hive-Role status = %d, want 403 fail-closed", rec.Code)
	}
}

func TestHandleAuditLogAllowsReadWriteRole(t *testing.T) {
	server := &Server{audit: &AuditLog{}}
	server.audit.Log("alice", "config.save", "file=hive.yaml", "scanner")
	req := httptest.NewRequest(http.MethodGet, "/api/audit", nil)
	req.Header.Set("X-Hive-Role", config.RoleReadWrite)
	rec := httptest.NewRecorder()

	server.handleAuditLog(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	var body struct {
		Entries []AuditEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v; body=%q", err, rec.Body.String())
	}
	if len(body.Entries) != 1 {
		t.Fatalf("response had %d entries, want 1", len(body.Entries))
	}
	if body.Entries[0].User != "alice" || body.Entries[0].Action != "config.save" {
		t.Fatalf("response entry = %#v, want logged audit entry", body.Entries[0])
	}
}

func TestHandleAuditLogAllowsOwnerRole(t *testing.T) {
	server := &Server{audit: &AuditLog{}}
	server.audit.Log("alice", "config.save", "file=hive.yaml", "scanner")
	req := httptest.NewRequest(http.MethodGet, "/api/audit", nil)
	req.Header.Set("X-Hive-Role", config.RoleOwner)
	rec := httptest.NewRecorder()

	server.handleAuditLog(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	var body struct {
		Entries []AuditEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v; body=%q", err, rec.Body.String())
	}
	if len(body.Entries) != 1 {
		t.Fatalf("response had %d entries, want 1", len(body.Entries))
	}
}

func TestAuditDetailFormatsKeyValuePairs(t *testing.T) {
	if got := auditDetail(); got != "" {
		t.Fatalf("auditDetail() = %q, want empty string", got)
	}
	if got := auditDetail("file", "hive.yaml", "agent", "scanner"); got != "file=hive.yaml, agent=scanner" {
		t.Fatalf("auditDetail pairs = %q, want formatted key/value pairs", got)
	}
	if got := auditDetail("file", "hive.yaml", "dangling"); got != "file=hive.yaml" {
		t.Fatalf("auditDetail with dangling key = %q, want dangling key ignored", got)
	}
}
