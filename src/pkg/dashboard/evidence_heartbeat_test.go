package dashboard

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func TestBuildHeartbeatEvidence(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-48 * time.Hour)
	tooOld := now.Add(-91 * 24 * time.Hour)

	// PR 1: approved at high confidence, merged, then reverted → false approve.
	reverted := evSummaryBundle("a/b", 1, evidence.AuthorAgent, recent, true, []evidence.Verdict{
		{Perspective: "security", Model: "claude-sonnet-5", Verdict: "approve", Confidence: 0.95},
	}, 0, true)
	reverted.Author.Model = "gpt-5.4"
	reverted.RevertedBy = []string{"x"}
	// An older bundle for PR 1 is superseded by the newer one above.
	superseded := evSummaryBundle("a/b", 1, evidence.AuthorAgent, recent.Add(-time.Hour), false, []evidence.Verdict{
		{Perspective: "security", Model: "grok-4.5", Verdict: "approve", Confidence: 0.1},
	}, 0, true)

	// PR 2: blocked, merged anyway → false block; approve verdict is fine.
	blocked := evSummaryBundle("a/b", 2, evidence.AuthorHuman, recent, true, []evidence.Verdict{
		{Perspective: "quality", Model: "gpt-5.5", Verdict: "changes_requested", Confidence: 0.4},
		{Perspective: "security", Model: "", Verdict: "approve", Confidence: 1.0},
	}, 0, true)

	// PR 3: followed up after merge, rejected; reject on a merged PR is a false block.
	followed := evSummaryBundle("c/d", 3, evidence.AuthorBot, recent, true, []evidence.Verdict{
		{Perspective: "security", Model: "claude-opus-5", Verdict: "reject", Confidence: 0.0},
	}, 0, true)
	followed.FollowedUpBy = []string{"y"}

	// PR 4: open, approved — no outcome yet, not bad.
	open := evSummaryBundle("c/d", 4, evidence.AuthorAgent, recent, false, []evidence.Verdict{
		{Perspective: "security", Model: "claude-sonnet-5", Verdict: "approve", Confidence: 0.6},
	}, 0, true)

	// Outside the 90-day window.
	old := evSummaryBundle("c/d", 5, evidence.AuthorAgent, tooOld, true, []evidence.Verdict{
		{Perspective: "security", Model: "mistral-large", Verdict: "approve", Confidence: 0.9},
	}, 0, true)
	old.RevertedBy = []string{"z"}

	yes := true
	got := buildHeartbeatEvidence([]*evidence.Bundle{superseded, reverted, blocked, followed, open, old, nil}, now, &yes)

	if got.Verdicts90d != 5 || got.Merged90d != 3 || got.Reverted14d90d != 2 {
		t.Errorf("counts = verdicts %d merged %d reverted %d, want 5/3/2", got.Verdicts90d, got.Merged90d, got.Reverted14d90d)
	}
	if got.FalseApprove90d != 1 || got.FalseBlock90d != 2 {
		t.Errorf("false approve/block = %d/%d, want 1/2", got.FalseApprove90d, got.FalseBlock90d)
	}
	if got.AutomergeSelfAuthored == nil || !*got.AutomergeSelfAuthored {
		t.Errorf("automerge_self_authored = %v, want true", got.AutomergeSelfAuthored)
	}
	if want := []string{"claude", "gpt"}; !reflect.DeepEqual(got.ReviewerFamilies, want) {
		t.Errorf("reviewer families = %v, want %v", got.ReviewerFamilies, want)
	}
	if want := []string{"gpt"}; !reflect.DeepEqual(got.AuthorFamilies, want) {
		t.Errorf("author families = %v, want %v", got.AuthorFamilies, want)
	}
	// Buckets: 0.95→5 bad, 0.4→2 bad, 1.0→5, 0.0→0 bad, 0.6→3.
	want := [6][2]int{{1, 1}, {0, 0}, {1, 1}, {1, 0}, {0, 0}, {2, 1}}
	for i, b := range got.Calibration {
		if b.N != want[i][0] || b.Bad != want[i][1] {
			t.Errorf("calibration[%d] = %+v, want n=%d bad=%d", i, b, want[i][0], want[i][1])
		}
	}
}

func TestBuildHeartbeatEvidenceEmpty(t *testing.T) {
	got := buildHeartbeatEvidence(nil, time.Now(), nil)
	if got == nil || got.Verdicts90d != 0 || got.AutomergeSelfAuthored != nil || got.ReviewerFamilies != nil {
		t.Fatalf("empty = %+v, want zero non-nil block", got)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"verdicts_90d", "false_approve_90d", "false_block_90d", "merged_90d", "reverted_14d_90d", "calibration"} {
		if _, ok := m[k]; !ok {
			t.Errorf("json missing %q: %s", k, raw)
		}
	}
	if cal, _ := m["calibration"].([]any); len(cal) != 6 {
		t.Errorf("calibration len = %d, want 6", len(cal))
	}
}

func TestEvidenceCalibrationBucket(t *testing.T) {
	for _, tc := range []struct {
		c    float64
		want int
	}{{-1, 0}, {0, 0}, {0.16, 0}, {0.17, 1}, {0.5, 3}, {0.99, 5}, {1, 5}, {2, 5}, {math.NaN(), 0}} {
		if got := evidenceCalibrationBucket(tc.c); got != tc.want {
			t.Errorf("bucket(%v) = %d, want %d", tc.c, got, tc.want)
		}
	}
}

func TestEvidenceHeartbeat(t *testing.T) {
	dir := t.TempDir()
	prev := ghpkg.ReviewEvidenceRoot
	ghpkg.ReviewEvidenceRoot = filepath.Join(dir, "evidence")
	t.Cleanup(func() { ghpkg.ReviewEvidenceRoot = prev })

	// No evidence dir yet: an empty, non-nil block; nil server has no posture.
	got := (*Server)(nil).EvidenceHeartbeat()
	if got == nil || got.Verdicts90d != 0 || got.AutomergeSelfAuthored != nil {
		t.Fatalf("nil server = %+v, want empty block without posture", got)
	}

	b := evSummaryBundle("a/b", 7, evidence.AuthorAgent, time.Now().UTC().Add(-time.Hour), true, []evidence.Verdict{
		{Perspective: "security", Model: "claude-sonnet-5", Verdict: "approve", Confidence: 0.9},
	}, 0, true)
	path := filepath.Join(ghpkg.ReviewEvidenceRoot, "a", "b", "7", "abc1234.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{}}
	got = s.EvidenceHeartbeat()
	if got == nil || got.Verdicts90d != 1 || got.Merged90d != 1 {
		t.Fatalf("got %+v, want 1 verdict / 1 merged", got)
	}
	want := s.deps.Config.AutoMerge.SelfAuthoredAutoMergeAllowed(s.deps.Config.ACMMLevel)
	if got.AutomergeSelfAuthored == nil || *got.AutomergeSelfAuthored != want {
		t.Errorf("automerge_self_authored = %v, want %v", got.AutomergeSelfAuthored, want)
	}
}
