package adminmcp

import (
	"errors"
	"strings"
	"testing"
)

// The sample exports mirror the Overview export shape: rows carry the band
// display label (mapped back through bands[]) and the server's stored
// ownership decision. The queue only reads that decision; rows whose
// your_turn is false never appear however their band or flags look.
func sampleReviewQueueIssues() map[string]any {
	return map[string]any{
		"bands": []any{
			map[string]any{"key": "waiting", "label": "Parked"},
			map[string]any{"key": "done", "label": "Done"},
			map[string]any{"key": "ready", "label": "Unclaimed"},
		},
		"rows": []any{
			map[string]any{"band": "Parked", "repo": "o/a", "number": float64(2), "title": "parked decision", "url": "u2", "updated_at": "2026-10-01T00:00:00Z", "your_turn": true, "verb": "DECIDE", "reason_code": "parked_needs_decision", "reason": "parked for a decision", "inbox_rank": float64(2)},
			map[string]any{"band": "Parked", "repo": "o/a", "number": float64(3), "title": "blocked only", "updated_at": "2026-10-01T00:00:00Z", "your_turn": false},
			map[string]any{"band": "Done", "repo": "o/a", "number": float64(4), "title": "verified", "updated_at": "2026-09-01T00:00:00Z", "your_turn": true, "verb": "CLOSE", "reason_code": "verified_done", "reason": "verified done", "inbox_rank": float64(5)},
			map[string]any{"band": "outside", "repo": "o/b", "number": float64(5), "title": "untrusted", "updated_at": "2026-10-02T00:00:00Z", "your_turn": true, "verb": "ADMIT", "reason_code": "untrusted_reporter", "reason": "untrusted reporter", "inbox_rank": float64(4)},
			map[string]any{"band": "Unclaimed", "repo": "o/b", "number": float64(6), "title": "backlog", "updated_at": "2026-08-01T00:00:00Z", "your_turn": false, "waiting_elsewhere": true},
		},
	}
}

func sampleReviewQueuePRs() map[string]any {
	return map[string]any{
		"bands": []any{map[string]any{"key": "waiting", "label": "Gated"}},
		"rows": []any{
			map[string]any{"band": "Gated", "repo": "o/a", "number": float64(11), "title": "sentinel", "updated_at": "2026-10-03T00:00:00Z", "held": true, "your_turn": true, "verb": "INSPECT", "reason_code": "sentinel_flagged", "reason": "sentinel flagged", "inbox_rank": float64(1)},
			map[string]any{"band": "Gated", "repo": "o/a", "number": float64(12), "title": "person hold", "updated_at": "2026-10-03T00:00:00Z", "held": true, "your_turn": false, "waiting_elsewhere": true},
			map[string]any{"band": "Gated", "repo": "o/b", "number": float64(13), "title": "level hold", "updated_at": "2026-10-03T00:00:00Z", "held": true, "your_turn": true, "verb": "REVIEW", "reason_code": "level_gate_hold", "reason": "held by the level gate", "inbox_rank": float64(3)},
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

func TestReviewQueueResultIsExactlyTheYourTurnItemsInInboxOrder(t *testing.T) {
	out, err := ReviewQueueResult(sampleReviewQueueIssues(), sampleReviewQueuePRs(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	rows := reviewQueueRows(t, out)
	want := []struct {
		kind   string
		number float64
		verb   string
		code   string
		rank   int
	}{
		{"pr", 11, "INSPECT", "sentinel_flagged", 1},
		{"issue", 2, "DECIDE", "parked_needs_decision", 2},
		{"pr", 13, "REVIEW", "level_gate_hold", 3},
		{"issue", 5, "ADMIT", "untrusted_reporter", 4},
		{"issue", 4, "CLOSE", "verified_done", 5},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %d rows", rows, len(want))
	}
	for i, w := range want {
		r := rows[i]
		if r.Kind != w.kind || r.Number != w.number || r.Verb != w.verb || r.ReasonCode != w.code || r.InboxRank != w.rank || r.Reason == "" {
			t.Fatalf("row %d = %+v, want %+v", i, r, w)
		}
	}
	if rows[1].Band != "waiting" {
		t.Fatalf("band = %q, want the rekeyed band key (parked issues are included)", rows[1].Band)
	}
	body := out.(map[string]any)
	if body["total"] != len(want) || body["ordering"] != reviewQueueOrdering {
		t.Fatalf("body = %#v", body)
	}
	if _, ok := body["next_offset"]; ok {
		t.Fatalf("next_offset set on a complete answer: %#v", body)
	}
	counts := body["counts"].(map[string]int)
	if counts["your_turn"] != 4 || counts["to_close"] != 1 {
		t.Fatalf("counts = %#v, want the inbox header's N=4 and M=1", counts)
	}
	verbs := body["verbs"].(map[string]int)
	if verbs["INSPECT"] != 1 || verbs["CLOSE"] != 1 || verbs["REVIEW"] != 1 {
		t.Fatalf("verbs = %#v", verbs)
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
	if !ok || meta["total"] != 5 || meta["limit"] != 3 || meta["truncated"] != true {
		t.Fatalf("_admin_mcp = %#v", body["_admin_mcp"])
	}

	last, err := ReviewQueueResult(sampleReviewQueueIssues(), sampleReviewQueuePRs(), map[string]any{"limit": float64(3), "offset": float64(3)})
	if err != nil {
		t.Fatal(err)
	}
	rows := reviewQueueRows(t, last)
	if len(rows) != 2 || rows[0].InboxRank != 4 || rows[1].InboxRank != 5 {
		t.Fatalf("last page = %+v, want inbox ranks 4 and 5", rows)
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

func withSnapshot(export map[string]any, seq float64) map[string]any {
	export["snapshot_instance"] = "i1"
	export["snapshot_seq"] = seq
	export["snapshot_at"] = "2026-10-13T00:00:00Z"
	return export
}

func TestReviewQueueResultReportsExportsFromDifferentSnapshots(t *testing.T) {
	issues := withSnapshot(sampleReviewQueueIssues(), 4)
	prs := withSnapshot(sampleReviewQueuePRs(), 5)
	out, err := ReviewQueueResult(issues, prs, map[string]any{})
	if !errors.Is(err, ErrReviewQueueSnapshotChanged) || out != nil {
		t.Fatalf("out = %#v err = %v, want ErrReviewQueueSnapshotChanged", out, err)
	}
	if !strings.Contains(err.Error(), "retry") {
		t.Fatalf("err = %q, want it to tell the caller to retry", err)
	}
}

func TestReviewQueueResultMergesExportsFromOneSnapshot(t *testing.T) {
	out, err := ReviewQueueResult(withSnapshot(sampleReviewQueueIssues(), 4), withSnapshot(sampleReviewQueuePRs(), 4), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if rows := reviewQueueRows(t, out); len(rows) != 5 {
		t.Fatalf("rows = %d, want the 5 your-turn rows", len(rows))
	}
}

// snapshotFetcher serves the two exports from scripted snapshot sequences, one
// entry consumed per GET of that export.
func snapshotFetcher(issueSeqs, prSeqs []float64, calls *int) func(string) (any, error) {
	return func(path string) (any, error) {
		*calls++
		if strings.Contains(path, "issues.json") {
			seq := issueSeqs[0]
			issueSeqs = issueSeqs[1:]
			return withSnapshot(sampleReviewQueueIssues(), seq), nil
		}
		seq := prSeqs[0]
		prSeqs = prSeqs[1:]
		return withSnapshot(sampleReviewQueuePRs(), seq), nil
	}
}

func TestReadReviewQueueRereadsOnceAfterASnapshotMismatch(t *testing.T) {
	calls := 0
	out, err := ReadReviewQueue(snapshotFetcher([]float64{4, 5}, []float64{5, 5}, &calls), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatalf("GETs = %d, want both exports read twice", calls)
	}
	if rows := reviewQueueRows(t, out); len(rows) != 5 {
		t.Fatalf("rows = %d, want the queue from the matching second read", len(rows))
	}
}

func TestReadReviewQueueRefusesAfterTwoMismatches(t *testing.T) {
	calls := 0
	_, err := ReadReviewQueue(snapshotFetcher([]float64{4, 6}, []float64{5, 7}, &calls), map[string]any{})
	if !errors.Is(err, ErrReviewQueueSnapshotChanged) {
		t.Fatalf("err = %v, want ErrReviewQueueSnapshotChanged", err)
	}
	if calls != 4 {
		t.Fatalf("GETs = %d, want exactly one re-read", calls)
	}
}
