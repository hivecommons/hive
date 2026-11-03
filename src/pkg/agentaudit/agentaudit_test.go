package agentaudit

import (
	"strings"
	"testing"
)

func TestAuditFieldsDropsEmptyStringsAndOddKey(t *testing.T) {
	f := Fields("backend", "claude", "model", "", "count", 3, "dangling")
	if _, ok := f["model"]; ok {
		t.Error("empty-string value should be dropped so the UI has no noisy 'model='")
	}
	if f["backend"] != "claude" {
		t.Errorf("backend = %v, want claude", f["backend"])
	}
	if f["count"] != 3 {
		t.Errorf("count = %v, want 3", f["count"])
	}
	if _, ok := f["dangling"]; ok {
		t.Error("odd trailing key should be dropped")
	}
}

// TestFormatAuditDetailIsStableAndOrdered pins the detail string shape the
// Audit Log UI renders and filters over. Go map iteration is randomized, so
// without the sort the same event would render differently each time.
func TestFormatAuditDetailIsStableAndOrdered(t *testing.T) {
	fields := Fields(
		"error", "unknown backend: watsonx",
		"model", "granite-3",
		"outcome", "failure",
		"backend", "watsonx",
	)
	got := FormatAuditDetail(fields)
	want := "outcome=failure, backend=watsonx, model=granite-3, error=unknown backend: watsonx"
	if got != want {
		t.Errorf("FormatAuditDetail =\n  %q\nwant\n  %q", got, want)
	}
	for i := 0; i < 10; i++ {
		if FormatAuditDetail(fields) != want {
			t.Fatal("FormatAuditDetail is not stable across calls")
		}
	}
	if FormatAuditDetail(nil) != "" {
		t.Error("empty fields should render as the empty string")
	}
}

func TestFormatAuditDetail_ToolApproval(t *testing.T) {
	fields := Fields(
		"rationale", "read-only inspection tool auto-approved",
		"acmm_level", 4,
		"tool", "Read",
		"decision", "auto-approve",
	)
	got := FormatAuditDetail(fields)
	want := "acmm_level=4, decision=auto-approve, rationale=read-only inspection tool auto-approved, tool=Read"
	if got != want {
		t.Errorf("FormatAuditDetail(tool approval) =\n  %q\nwant\n  %q", got, want)
	}
}

// TestRelayAuditActionVocabulary pins the wire names of the relay_* actions the
// hub records for the contributor relay (hivecommons/hive#13441). Operators
// filter the Audit Log on these strings, so a rename is a breaking change.
func TestRelayAuditActionVocabulary(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"connected", AuditRelayConnected, "relay_connected"},
		{"rejected", AuditRelayRejected, "relay_rejected"},
		{"disconnected", AuditRelayDisconnected, "relay_disconnected"},
		{"task assigned", AuditRelayTaskAssigned, "relay_task_assigned"},
		{"task accepted", AuditRelayTaskAccepted, "relay_task_accepted"},
		{"task declined", AuditRelayTaskDeclined, "relay_task_declined"},
		{"token minted", AuditRelayTokenMinted, "relay_token_minted"},
		{"mcp lease issued", AuditRelayMCPLeaseIssued, "relay_mcp_lease_issued"},
		{"mcp lease revoked", AuditRelayMCPLeaseRevoked, "relay_mcp_lease_revoked"},
		{"task completed", AuditRelayTaskCompleted, "relay_task_completed"},
		{"task failed", AuditRelayTaskFailed, "relay_task_failed"},
		{"task revoked", AuditRelayTaskRevoked, "relay_task_revoked"},
		{"task abandoned", AuditRelayTaskAbandoned, "relay_task_abandoned"},
		{"task attested", AuditRelayTaskAttested, "relay_task_attested"},
	}
	seen := make(map[string]bool, len(tests))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("action = %q, want %q", tt.got, tt.want)
			}
			if !strings.HasPrefix(tt.got, "relay_") || tt.got != strings.ToLower(tt.got) {
				t.Errorf("action %q must be lower snake_case with the relay_ prefix", tt.got)
			}
			if seen[tt.got] {
				t.Errorf("action %q is declared twice", tt.got)
			}
			seen[tt.got] = true
		})
	}
}

func TestFormatAuditDetail_RelayTokenMintedCarriesInstallation(t *testing.T) {
	got := FormatAuditDetail(Fields(
		"task", "t-1", "installation", int64(12345), "expires_at", "2026-10-21T22:00:00Z", "scopes", "issues:write",
	))
	want := "expires_at=2026-10-21T22:00:00Z, installation=12345, scopes=issues:write, task=t-1"
	if got != want {
		t.Errorf("FormatAuditDetail =\n  %q\nwant\n  %q", got, want)
	}
}
