package agentaudit

import "testing"

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
