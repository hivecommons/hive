package adminmcp

import (
	"strings"
	"testing"
)

// sampleReviewQueueIssues and sampleReviewQueuePRs mirror the Overview export
// shape: rows carry the band display label, which ReviewQueueResult maps back
// to the key through bands[] (#10018).
func sampleReviewQueueIssues() map[string]any {
	return map[string]any{
		"bands": []any{
			map[string]any{"key": "ready", "label": "Unclaimed"},
			map[string]any{"key": "agent-filed", "label": "Needs triage"},
			map[string]any{"key": "waiting", "label": "Needs human"},
			map[string]any{"key": "done", "label": "Confirm & close"},
		},
		"rows": []any{
			map[string]any{"repo": "o/a", "number": float64(1), "title": "unclaimed", "band": "Unclaimed", "held": false, "updated_at": "2026-01-01T00:00:00Z"},
			map[string]any{"repo": "o/a", "number": float64(2), "title": "triage", "band": "Needs triage", "held": false, "updated_at": "2026-01-02T00:00:00Z"},
			map[string]any{"repo": "o/a", "number": float64(3), "title": "close me", "band": "Confirm & close", "held": false, "updated_at": "2026-01-03T00:00:00Z"},
			map[string]any{"repo": "o/a", "number": float64(4), "title": "decide", "band": "Needs human", "held": false, "updated_at": "2026-01-04T00:00:00Z"},
			map[string]any{"repo": "o/b", "number": float64(5), "title": "held issue", "band": "Unclaimed", "held": true, "hold_reason": "hold label", "updated_at": "2026-01-05T00:00:00Z"},
		},
	}
}

func sampleReviewQueuePRs() map[string]any {
	return map[string]any{
		"bands": []any{
			map[string]any{"key": "waiting", "label": "Needs human"},
			map[string]any{"key": "eligible", "label": "Merge-eligible"},
			map[string]any{"key": "in-review", "label": "In review"},
		},
		"rows": []any{
			map[string]any{"repo": "o/a", "number": float64(10), "title": "eligible", "band": "Merge-eligible", "held": false, "updated_at": "2026-01-01T00:00:00Z"},
			map[string]any{"repo": "o/a", "number": float64(11), "title": "outstanding", "band": "In review", "merge_verdict": "outstanding: awaiting review", "held": false, "updated_at": "2026-01-01T00:00:00Z"},
			map[string]any{"repo": "o/a", "number": float64(12), "title": "github review only", "band": "In review", "merge_verdict": "", "held": false, "updated_at": "2026-01-01T00:00:00Z"},
			map[string]any{"repo": "o/a", "number": float64(13), "title": "held pr", "band": "Needs human", "held": true, "hold_reason": "", "updated_at": "2026-01-06T00:00:00Z"},
			map[string]any{"repo": "o/a", "number": float64(14), "title": "gated pr", "band": "Needs human", "held": false, "updated_at": "2026-01-04T00:00:00Z"},
		},
	}
}

func reviewQueueRows(t *testing.T, out any) []reviewQueueRow {
	t.Helper()
	body, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("result = %#v", out)
	}
	rows, ok := body["rows"].([]reviewQueueRow)
	if !ok {
		t.Fatalf("rows = %#v", body["rows"])
	}
	return rows
}

func TestReviewQueueResultOrdersByReasonThenAge(t *testing.T) {
	out, err := ReviewQueueResult(sampleReviewQueueIssues(), sampleReviewQueuePRs(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	rows := reviewQueueRows(t, out)
	want := []struct {
		kind   string
		number float64
		code   string
	}{
		{"issue", 5, ReviewReasonHold},
		{"pr", 13, ReviewReasonHold},
		{"pr", 14, ReviewReasonNeedsHuman},
		{"issue", 4, ReviewReasonNeedsHuman},
		{"pr", 11, ReviewReasonSweepOutstanding},
		{"issue", 3, ReviewReasonConfirmClose},
		{"issue", 2, ReviewReasonAgentFiled},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %d rows", rows, len(want))
	}
	for i, w := range want {
		r := rows[i]
		if r.Kind != w.kind || r.Number != w.number || r.ReasonCode != w.code {
			t.Fatalf("row %d = %+v, want %s #%v %s", i, r, w.kind, w.number, w.code)
		}
		if strings.TrimSpace(r.Reason) == "" {
			t.Fatalf("row %d has no reason: %+v", i, r)
		}
	}
	if !strings.Contains(rows[0].Reason, "hold label") {
		t.Fatalf("hold reason = %q, want the hold_reason carried through", rows[0].Reason)
	}
	if rows[3].Band != "waiting" {
		t.Fatalf("band = %q, want the rekeyed band key", rows[3].Band)
	}
	body := out.(map[string]any)
	if body["total"] != len(want) || body["ordering"] != reviewQueueOrdering {
		t.Fatalf("body = %#v", body)
	}
	if _, ok := body["next_offset"]; ok {
		t.Fatalf("next_offset set on a complete answer: %#v", body)
	}
	counts := body["counts"].(map[string]int)
	if counts[ReviewReasonHold] != 2 || counts[ReviewReasonNeedsHuman] != 2 || counts[ReviewReasonAgentFiled] != 1 {
		t.Fatalf("counts = %#v", counts)
	}
}

func TestReviewQueueResultPagesAndDisclosesTruncation(t *testing.T) {
	out, err := ReviewQueueResult(sampleReviewQueueIssues(), sampleReviewQueuePRs(), map[string]any{"limit": float64(3)})
	if err != nil {
		t.Fatal(err)
	}
	body := out.(map[string]any)
	if rows := reviewQueueRows(t, out); len(rows) != 3 {
		t.Fatalf("rows = %+v, want 3", rows)
	}
	if body["rows_truncated"] != true || body["next_offset"] != 3 {
		t.Fatalf("body = %#v, want truncation and next_offset=3", body)
	}
	meta, ok := body["_admin_mcp"].(map[string]any)
	if !ok || meta["total"] != 7 || meta["limit"] != 3 || meta["truncated"] != true {
		t.Fatalf("_admin_mcp = %#v", body["_admin_mcp"])
	}

	last, err := ReviewQueueResult(sampleReviewQueueIssues(), sampleReviewQueuePRs(), map[string]any{"limit": float64(3), "offset": float64(6)})
	if err != nil {
		t.Fatal(err)
	}
	rows := reviewQueueRows(t, last)
	if len(rows) != 1 || rows[0].Number != float64(2) {
		t.Fatalf("last page = %+v, want only issue #2", rows)
	}
	if _, ok := last.(map[string]any)["rows_truncated"]; ok {
		t.Fatalf("last page must not report truncation: %#v", last)
	}
}

func TestReviewQueueResultRefusesNegativeOffset(t *testing.T) {
	out, err := ReviewQueueResult(sampleReviewQueueIssues(), sampleReviewQueuePRs(), map[string]any{"offset": float64(-1)})
	if err != nil {
		t.Fatal(err)
	}
	refusal, ok := out.(RefusalData)
	if !ok || refusal.Kind != RefusalKindInvalidArgument || refusal.Operation != ToolReviewQueue {
		t.Fatalf("result = %#v", out)
	}
}

func TestReviewQueueReadPathsForwardOnlyRepo(t *testing.T) {
	issues, prs := ReviewQueueReadPaths(map[string]any{"repo": "owner/name", "limit": float64(5), "offset": float64(2)})
	if issues != "/api/overview/issues.json?repo=owner%2Fname" || prs != "/api/overview/prs.json?repo=owner%2Fname" {
		t.Fatalf("paths = %q, %q", issues, prs)
	}
	issues, prs = ReviewQueueReadPaths(map[string]any{})
	if issues != "/api/overview/issues.json" || prs != "/api/overview/prs.json" {
		t.Fatalf("paths = %q, %q", issues, prs)
	}
}

func TestReviewQueueAndGovernorSetupToolsAreListedAsReadOnly(t *testing.T) {
	for _, tool := range []string{ToolReviewQueue, ToolGovernorSetup} {
		if !AllowedTool(tool) {
			t.Fatalf("%s is not allowed", tool)
		}
		var found bool
		for _, def := range Tools() {
			if def["name"] != tool {
				continue
			}
			found = true
			annotations, _ := def["annotations"].(map[string]any)
			if annotations["readOnlyHint"] != true {
				t.Fatalf("%s annotations = %#v", tool, annotations)
			}
		}
		if !found {
			t.Fatalf("%s tool not listed", tool)
		}
		if _, ok := DefaultWriteRegistry().Get(tool); ok {
			t.Fatalf("%s must not be registered as a write operation", tool)
		}
	}
	if path, ok := ReadPath(ToolGovernorSetup, 0); !ok || path != "/api/config/governor" {
		t.Fatalf("governor setup path = %q ok=%v", path, ok)
	}
	if _, ok := ReadPath(ToolReviewQueue, 0); ok {
		t.Fatal("review_queue reads two endpoints through ReviewQueueReadPaths, not ReadPath")
	}
}
