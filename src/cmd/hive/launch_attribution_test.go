package main

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/tokens"
)

func TestLaunchRecordForKickGovernor(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	msg := scheduler.KickMessage{Agent: "scanner", Repo: "o/r",
		IssueRefs: []string{"o/r#1", "o/r#2", "o/r#1", "", "lin!EXT-7"}}
	rec := launchRecordForKick(msg, review.DispatchKick{}, false, now)
	if rec.Agent != "scanner" || rec.Trigger != launchTriggerGovernor || rec.Repo != "o/r" || rec.LaunchedAtMs != now.UnixMilli() {
		t.Fatalf("record = %+v", rec)
	}
	if len(rec.Refs) != 3 {
		t.Fatalf("refs = %+v, want 3 deduped non-empty refs", rec.Refs)
	}
	if rec.Refs[0].Kind != tokens.LaunchRefKindIssue || rec.Refs[0].Number != 1 || rec.Refs[2].Kind != tokens.LaunchRefKindExternal {
		t.Fatalf("refs = %+v", rec.Refs)
	}
	for _, r := range rec.Refs {
		if r.HeadSHA != "" {
			t.Fatalf("issue kicks carry no head SHA: %+v", r)
		}
	}
}

func TestLaunchRecordForKickReview(t *testing.T) {
	now := time.Now()
	msg := scheduler.KickMessage{Agent: "reviewer", IssueRefs: []string{"o/r#9"}}
	k := review.DispatchKick{Agent: "reviewer", PRRef: "o/r#9", Kind: "review", Repo: "o/r", Number: 9, HeadSHA: "abc123"}
	rec := launchRecordForKick(msg, k, true, now)
	if rec.Trigger != "review-review" || rec.Repo != "o/r" || len(rec.Refs) != 1 {
		t.Fatalf("record = %+v", rec)
	}
	r := rec.Refs[0]
	if r.Kind != tokens.LaunchRefKindPR || r.Ref != "o/r#9" || r.Number != 9 || r.HeadSHA != "abc123" {
		t.Fatalf("ref = %+v", r)
	}

	// A review kick missing structured repo/number falls back to the PRRef
	// string but still tags it as a PR with its head.
	k2 := review.DispatchKick{Kind: "fix", HeadSHA: "def"}
	rec = launchRecordForKick(msg, k2, true, now)
	if rec.Trigger != "review-fix" || len(rec.Refs) != 1 || rec.Refs[0].Kind != tokens.LaunchRefKindPR || rec.Refs[0].HeadSHA != "def" {
		t.Fatalf("fallback record = %+v", rec)
	}
}
