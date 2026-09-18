package github

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
)

func validVerdictJSON(t *testing.T, repo string, number int, perspective, verdict string) string {
	t.Helper()
	report := map[string]any{
		"lane":        "review-swarm",
		"kind":        "review",
		"findings":    []any{},
		"prs_opened":  []any{},
		"beads_filed": []any{},
		"summary":     "judged",
		"perspective": perspective,
		"verdict":     verdict,
		"repo":        repo,
		"number":      number,
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := review.ValidateReport(raw); err != nil {
		t.Fatalf("fixture is not a valid review report: %v", err)
	}
	return string(raw)
}

// The whole point of the relay verdict path: the file review.Collect reads must
// actually appear, and Collect must be able to aggregate it. A test that only
// asserts "some file was written" would pass even if the name or the contents
// were shaped wrong, which is exactly the failure that left review-verdicts.json
// empty while 117 reviews were posted.
func TestRecordReviewVerdictIsCollectable(t *testing.T) {
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	raw := validVerdictJSON(t, "projectbluefin/common", 1121, "correctness", "requires_human")

	c.recordReviewVerdict(ReviewRequest{Repo: "projectbluefin/common", Number: 1121, Report: raw}, dir)

	matches, _ := filepath.Glob(filepath.Join(dir, review.ReviewReportFilePrefix+"*"+review.ReviewReportFileSuffix))
	if len(matches) != 1 {
		t.Fatalf("want exactly one collectable report, got %v", matches)
	}
	artifact, err := review.Collect(dir, review.AggregateOptions{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(artifact.Items) != 1 {
		t.Fatalf("want 1 aggregated item, got %d", len(artifact.Items))
	}
	got := artifact.Items[0]
	if got.Repo != "projectbluefin/common" || got.Number != 1121 {
		t.Fatalf("wrong target: %s#%d", got.Repo, got.Number)
	}
	if !got.RequiresHuman {
		t.Fatalf("requires_human verdict did not survive into the aggregate: %+v", got)
	}
}

// A verdict naming a different PR than the one reviewed must be discarded: an
// agent authorized to comment on one PR must not be able to record a binding
// verdict against another.
func TestRecordReviewVerdictRejectsMismatchedTarget(t *testing.T) {
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	raw := validVerdictJSON(t, "projectbluefin/other", 9999, "correctness", "reject")

	c.recordReviewVerdict(ReviewRequest{Repo: "projectbluefin/common", Number: 1121, Report: raw}, dir)

	matches, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(matches) != 0 {
		t.Fatalf("verdict for a different PR was recorded: %v", matches)
	}
}

// review.Collect fails the entire collection on the first unparseable file, so
// a malformed verdict must never be allowed to land in the dir it scans.
func TestRecordReviewVerdictRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	c := &Client{logger: testLogger()}

	for name, raw := range map[string]string{
		"not json":       "{{{",
		"empty":          "   ",
		"bad verdict":    strings.Replace(validVerdictJSON(t, "o/r", 1, "correctness", "approve"), `"approve"`, `"merge_it"`, 1),
		"bad perspectve": strings.Replace(validVerdictJSON(t, "o/r", 1, "correctness", "approve"), `"correctness"`, `"vibes"`, 1),
	} {
		c.recordReviewVerdict(ReviewRequest{Repo: "o/r", Number: 1, Report: raw}, dir)
		if matches, _ := filepath.Glob(filepath.Join(dir, "*")); len(matches) != 0 {
			t.Fatalf("%s: malformed verdict was written: %v", name, matches)
		}
	}
	if _, err := review.Collect(dir, review.AggregateOptions{}); err != nil {
		t.Fatalf("Collect must still succeed on the untouched dir: %v", err)
	}
}

// report.Repo is agent-supplied and becomes part of a filename. filepath.Join
// cleans lexically, so an unsanitized "../.." escapes into a sibling directory
// of the metrics dir — create that target first, or the write merely fails for
// want of a directory and the test passes without proving anything.
func TestRecordReviewVerdictDoesNotEscapeDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "metrics")
	if err := os.MkdirAll(filepath.Join(dir, "etc", "cron.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "etc", "cron.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Client{logger: testLogger()}
	evil := "../../etc/cron.d/x"
	raw := validVerdictJSON(t, evil, 1, "correctness", "approve")

	c.recordReviewVerdict(ReviewRequest{Repo: evil, Number: 1, Report: raw}, dir)

	// The only acceptable outcome is a plain file directly inside dir.
	var stray []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if filepath.Dir(p) != dir {
			stray = append(stray, p)
		}
		return nil
	})
	if len(stray) != 0 {
		t.Fatalf("verdict escaped the metrics dir: %v", stray)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !e.IsDir() && strings.Contains(e.Name(), "..") {
			t.Fatalf("unsafe filename written: %q", e.Name())
		}
	}
}

// No verdict supplied is the pre-existing behaviour and must stay a silent
// no-op, not an error or an empty file.
func TestRecordReviewVerdictNoReportIsNoop(t *testing.T) {
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	c.recordReviewVerdict(ReviewRequest{Repo: "o/r", Number: 1}, dir)
	if matches, _ := filepath.Glob(filepath.Join(dir, "*")); len(matches) != 0 {
		t.Fatalf("no-op wrote files: %v", matches)
	}
}

// Re-reviewing the same PR from the same perspective must overwrite, not
// accumulate: the reports dir is scanned in full on every tick.
func TestRecordReviewVerdictIsIdempotentPerPerspective(t *testing.T) {
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	req := ReviewRequest{Repo: "o/r", Number: 7}

	req.Report = validVerdictJSON(t, "o/r", 7, "correctness", "changes_requested")
	c.recordReviewVerdict(req, dir)
	req.Report = validVerdictJSON(t, "o/r", 7, "correctness", "approve")
	c.recordReviewVerdict(req, dir)

	matches, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(matches) != 1 {
		t.Fatalf("want 1 report after re-review, got %v", matches)
	}
	artifact, err := review.Collect(dir, review.AggregateOptions{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	// The aggregate stays requires_human because a single perspective is never
	// unanimous; what must change is the recorded verdict for THIS perspective.
	if len(artifact.Items) != 1 || artifact.Items[0].Perspectives["correctness"] != review.VerdictApprove {
		t.Fatalf("latest verdict did not win: %+v", artifact.Items)
	}
}

// record_verdict must be accepted by the request shape validator. If it is
// rejected there the file is quarantined as ".bad" and the verdict is lost —
// which is the exact failure this event exists to prevent.
func TestRecordVerdictEventPassesShapeValidation(t *testing.T) {
	dir := t.TempDir()
	// Keep the accepted verdict off the host's real metrics dir.
	prevReportDir := outputschema.AgentReportDir
	outputschema.AgentReportDir = t.TempDir()
	defer func() { outputschema.AgentReportDir = prevReportDir }()
	raw := validVerdictJSON(t, "o/r", 5, "correctness", "approve")

	for name, tc := range map[string]struct {
		req     ReviewRequest
		wantBad bool
	}{
		"verdict only":   {ReviewRequest{Repo: "o/r", Number: 5, Event: ReviewEventRecordVerdict, Report: raw, Agent: "reviewer"}, false},
		"missing report": {ReviewRequest{Repo: "o/r", Number: 5, Event: ReviewEventRecordVerdict, Agent: "reviewer"}, true},
		"unknown event":  {ReviewRequest{Repo: "o/r", Number: 5, Event: "merge_it", Report: raw, Agent: "reviewer"}, true},
	} {
		path, err := WriteReviewRequest(dir, tc.req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		c := &Client{logger: testLogger(), reviewAuthz: func(string, int) error { return nil }}
		reviewRequestDirForTest = dir
		c.ProcessReviewRequestsOnce(t.Context())
		_, badErr := os.Stat(path + ".bad")
		gotBad := badErr == nil
		if gotBad != tc.wantBad {
			t.Fatalf("%s: quarantined=%v want %v", name, gotBad, tc.wantBad)
		}
		os.Remove(path)
		os.Remove(path + ".bad")
	}
	reviewRequestDirForTest = ""
}
