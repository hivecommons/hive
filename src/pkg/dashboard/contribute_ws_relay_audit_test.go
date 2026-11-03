package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agentaudit"
)

// Hub-side relay audit rows (hivecommons/hive#13441, child a): the hub records
// the relay lifecycle in the existing audit log from frames it already
// receives. These tests pin who the row is attributed to, which actions are
// sensitive, and that no token ever reaches a row.

func relayAuditEntries(t *testing.T, hub *ContributeWSHub, action string) []AuditEntry {
	t.Helper()
	var out []AuditEntry
	for _, e := range srvAuditEntries(t, hub) {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func TestRelayAudit_SensitiveVocabulary(t *testing.T) {
	tests := []struct {
		action    string
		sensitive bool
	}{
		{agentaudit.AuditRelayConnected, false},
		{agentaudit.AuditRelayRejected, false},
		{agentaudit.AuditRelayDisconnected, false},
		{agentaudit.AuditRelayTaskAssigned, false},
		{agentaudit.AuditRelayTaskAccepted, false},
		{agentaudit.AuditRelayTaskDeclined, false},
		{agentaudit.AuditRelayTokenMinted, true},
		{agentaudit.AuditRelayMCPLeaseIssued, false},
		{agentaudit.AuditRelayMCPLeaseRevoked, false},
		{agentaudit.AuditRelayTaskCompleted, false},
		{agentaudit.AuditRelayTaskFailed, false},
		{agentaudit.AuditRelayTaskRevoked, true},
		{agentaudit.AuditRelayTaskAbandoned, false},
		{agentaudit.AuditRelayTaskAttested, false},
	}
	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			if got := auditEntryIsSensitive(AuditEntry{Action: tt.action}); got != tt.sensitive {
				t.Errorf("auditEntryIsSensitive(%q) = %v, want %v", tt.action, got, tt.sensitive)
			}
		})
	}
}

func TestRelayAudit_TokenMintedCarriesNoTokenAndIsAttributed(t *testing.T) {
	hub, _ := covK2Hub(t)
	server, client := wsPipe(t)
	const secret = "ghs_never_in_the_audit_log"
	conn := &ContributorConnection{
		ws:          server,
		session:     "s1",
		profile:     &ContributorProfile{TrustTier: "contributor", GitHubUsername: "ben", ContributorID: "c-ben"},
		currentTask: &WSTaskAssign{TaskID: "t-mint", Repo: "myorg/repo1", Number: 42},
	}

	if err := hub.sendTokenRefresh(conn, secret); err != nil {
		t.Fatalf("sendTokenRefresh: %v", err)
	}
	readWire(t, client)

	entries := relayAuditEntries(t, hub, agentaudit.AuditRelayTokenMinted)
	if len(entries) != 1 {
		t.Fatalf("relay_token_minted rows = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.User != "ben" || e.Agent != "c-ben#s1" || e.Repo != "myorg/repo1" || e.Target != 42 {
		t.Errorf("attribution = user %q agent %q repo %q target %d", e.User, e.Agent, e.Repo, e.Target)
	}
	for _, want := range []string{"task=t-mint", "expires_at=", "scopes=issues:write|contents:write|pulls:write"} {
		if !strings.Contains(e.Detail, want) {
			t.Errorf("detail %q missing %q", e.Detail, want)
		}
	}
	if strings.Contains(e.Detail, secret) {
		t.Errorf("detail leaked the token: %q", e.Detail)
	}
}

func TestRelayAudit_TokenMintedCarriesInstallation(t *testing.T) {
	hub, s := covK2Hub(t)
	s.deps.GHAppAuth = newSucceedingAppAuth(t, "ghs_unused")
	server, client := wsPipe(t)
	conn := &ContributorConnection{
		ws:          server,
		profile:     &ContributorProfile{TrustTier: "newcomer", GitHubUsername: "ben", ContributorID: "c-ben"},
		currentTask: &WSTaskAssign{TaskID: "t-inst", Repo: "myorg/repo1", Number: 7},
	}

	if err := hub.sendTokenRefresh(conn, "ghs_unused"); err != nil {
		t.Fatalf("sendTokenRefresh: %v", err)
	}
	readWire(t, client)

	entries := relayAuditEntries(t, hub, agentaudit.AuditRelayTokenMinted)
	if len(entries) != 1 || !strings.Contains(entries[0].Detail, "installation=2") {
		t.Fatalf("relay_token_minted rows = %+v, want one carrying installation=2", entries)
	}
}

func TestRelayAudit_NoTaskNoTokenRow(t *testing.T) {
	hub, _ := covK2Hub(t)
	server, client := wsPipe(t)
	conn := &ContributorConnection{
		ws:      server,
		profile: &ContributorProfile{TrustTier: "contributor", GitHubUsername: "ben"},
	}

	if err := hub.sendTokenRefresh(conn, "ghs_idle"); err != nil {
		t.Fatalf("sendTokenRefresh: %v", err)
	}
	readWire(t, client)

	if rows := relayAuditEntries(t, hub, agentaudit.AuditRelayTokenMinted); len(rows) != 0 {
		t.Fatalf("a token with no task must not produce a row: %+v", rows)
	}
}

func TestRelayAudit_TaskAcceptedOncePerTask(t *testing.T) {
	hub, _ := covK2Hub(t)
	conn := &ContributorConnection{
		profile:     &ContributorProfile{TrustTier: "contributor", GitHubUsername: "ben", ContributorID: "c-ben"},
		currentTask: &WSTaskAssign{TaskID: "t-acc", Repo: "myorg/repo1", Number: 9},
	}
	s := &wsSession{h: hub, contributor: conn}

	tests := []struct {
		name     string
		taskID   string
		wantRows int
	}{
		{"stale task id is ignored", "t-other", 0},
		{"first acceptance is audited", "t-acc", 1},
		{"re-asserted acceptance is not repeated", "t-acc", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s.auditTaskAccepted(tt.taskID)
			rows := relayAuditEntries(t, hub, agentaudit.AuditRelayTaskAccepted)
			if len(rows) != tt.wantRows {
				t.Fatalf("relay_task_accepted rows = %d, want %d", len(rows), tt.wantRows)
			}
			if tt.wantRows > 0 && (rows[0].Target != 9 || rows[0].Repo != "myorg/repo1" || !strings.Contains(rows[0].Detail, "task=t-acc")) {
				t.Errorf("row = %+v", rows[0])
			}
		})
	}
}

func TestRelayAudit_OperatorRevokeIsSensitiveAndBoundsReason(t *testing.T) {
	hub, _ := covK2Hub(t)
	conn := &ContributorConnection{
		profile: &ContributorProfile{TrustTier: "contributor", GitHubUsername: "ben", ContributorID: "c-ben"},
	}
	task := WSTaskAssign{TaskID: "t-rev", Repo: "myorg/repo1", Number: 11}

	hub.bookAndRevokeReleased([]releaseTarget{{conn: conn, task: task}}, "duplicate\nPR", "yanked by operator")

	rows := relayAuditEntries(t, hub, agentaudit.AuditRelayTaskRevoked)
	if len(rows) != 1 {
		t.Fatalf("relay_task_revoked rows = %d, want 1", len(rows))
	}
	if rows[0].User != "ben" || rows[0].Target != 11 || strings.Contains(rows[0].Detail, "\n") {
		t.Errorf("row = %+v", rows[0])
	}
	if !strings.Contains(rows[0].Detail, "task=t-rev") || !strings.Contains(rows[0].Detail, "reason=") {
		t.Errorf("detail = %q", rows[0].Detail)
	}
}

func TestRelayAudit_LeaseExpiryIsAbandonedWithLastProgress(t *testing.T) {
	hub, _ := covK2Hub(t)
	now := time.Now()
	renewed := now.Add(-(wsTaskTimeout + time.Minute))
	conn := &ContributorConnection{
		profile:        &ContributorProfile{TrustTier: "contributor", GitHubUsername: "wedged", ContributorID: "c-wedged"},
		currentTask:    &WSTaskAssign{TaskID: "t-wedged", Repo: "myorg/repo1", Number: 42},
		currentTaskGen: 3,
		lastLeaseRenew: renewed,
		lastPong:       now,
	}
	hub.mu.Lock()
	hub.connections["conn-wedged"] = conn
	hub.mu.Unlock()

	if released := hub.reclaimExpiredLeases(now); released != 1 {
		t.Fatalf("reclaimExpiredLeases = %d, want 1", released)
	}

	rows := relayAuditEntries(t, hub, agentaudit.AuditRelayTaskAbandoned)
	if len(rows) != 1 {
		t.Fatalf("relay_task_abandoned rows = %d, want 1", len(rows))
	}
	if want := "last_progress_at=" + renewed.UTC().Format(time.RFC3339); !strings.Contains(rows[0].Detail, want) {
		t.Errorf("detail %q missing %q", rows[0].Detail, want)
	}
}

func TestRelayAudit_TaskFailedRecordsClassAndDuration(t *testing.T) {
	hub, _ := covK2Hub(t)
	conn := &ContributorConnection{
		profile:        &ContributorProfile{TrustTier: "contributor", GitHubUsername: "ben", ContributorID: "c-ben"},
		currentTask:    &WSTaskAssign{TaskID: "t-fail", Kind: "issue", Repo: "myorg/repo1", Number: 21, Title: "t"},
		currentTaskGen: 4,
		taskAssignedAt: time.Now().Add(-90 * time.Second),
	}
	s := &wsSession{h: hub, contributor: conn}

	s.handleTaskFailed(WSMessage{Type: "task_failed", TaskID: "t-fail", TaskGen: 4, Reason: "tests failed", FailureKind: "environment"})

	rows := relayAuditEntries(t, hub, agentaudit.AuditRelayTaskFailed)
	if len(rows) != 1 {
		t.Fatalf("relay_task_failed rows = %d, want 1", len(rows))
	}
	for _, want := range []string{"task=t-fail", "class=environment", "duration_s=9"} {
		if !strings.Contains(rows[0].Detail, want) {
			t.Errorf("detail %q missing %q", rows[0].Detail, want)
		}
	}
	if strings.Contains(rows[0].Detail, "tests failed") {
		t.Errorf("free-text failure reason must not be copied into the row: %q", rows[0].Detail)
	}
}

func TestRelayAudit_CloseReasonFirstWins(t *testing.T) {
	conn := &ContributorConnection{}
	conn.setAuditCloseReason("heartbeat_timeout")
	conn.setAuditCloseReason("server_shutdown")
	if conn.auditCloseReason != "heartbeat_timeout" {
		t.Fatalf("auditCloseReason = %q, want the first reason", conn.auditCloseReason)
	}
}

func TestRelayAudit_NilServerIsANoop(t *testing.T) {
	hub := &ContributeWSHub{logger: covBLogger()}
	conn := &ContributorConnection{profile: &ContributorProfile{GitHubUsername: "ben"}}
	hub.auditRelay(conn, agentaudit.AuditRelayConnected, nil, "backend", "claude")
	hub.auditRelayAs("ben", "", agentaudit.AuditRelayRejected, nil, "reason", "revoked")
}
