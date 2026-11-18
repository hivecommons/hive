package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/utah-packages#1013 (11-18): parked at 085 on
// `fork-guard:Canary` with a head a trusted maintainer approved on GitHub.
// The row kept asking the reviewer's requires_human question that approval
// answered, and offered Approve, because the dashboard learned the
// approver only from the `:human-approved@` reason suffix, written at 090
// (the follow-up noted on hivecommons/hive#18849). With the approver on
// the row the question is the real blocker and Approve says why it is off.
func TestDecisionForPRApprovedHeadAt085(t *testing.T) {
	verdict := &review.Aggregate{DecisionQuestion: "security requires human: permission-actions: write on the merge-robot token"}
	it := StageTrackerItem{Repo: "acme/widget", Number: 1013, Kind: "pr", Stage: trackerNumberNeedsDecision, Head: "4b9cac9c10ec", Reason: "fork-guard:Canary"}

	plain := decisionForPR(it, verdict, "", false)
	if plain.AskedBy != decisionAskedByReviewer || !strings.Contains(plain.Question, "merge-robot token") || !approveEnabled(t, plain) {
		t.Fatalf("no approval on record: decision = %+v", plain)
	}

	it.HumanApprover = "octocat"
	d := decisionForPR(it, verdict, "", false)
	if d.AskedBy != decisionAskedByLane || !strings.HasPrefix(d.Question, "CI will not run for this fork PR (Canary)") {
		t.Fatalf("approved head re-asked the answered question: %+v", d)
	}
	for _, o := range d.Options {
		if o.Action == decisionActionApprove && (o.Enabled || !strings.Contains(o.ReasonIfDisabled, "@octocat already approved this head")) {
			t.Fatalf("approve option = %+v, want disabled naming the approver", o)
		}
	}

	// A sentinel alert keeps Approve: on the row it also clears the label.
	it.SentinelAlert = true
	if s := decisionForPR(it, verdict, "sentinel-alert", false); !approveEnabled(t, s) {
		t.Fatalf("sentinel row lost Approve: %+v", s.Options)
	}
}

func approveEnabled(t *testing.T, d *StageTrackerDecision) bool {
	t.Helper()
	for _, o := range d.Options {
		if o.Action == decisionActionApprove {
			return o.Enabled
		}
	}
	t.Fatal("no Approve option")
	return false
}

// End to end: the approval the stage pass recorded for the head reaches the
// 085 row; a later head does not inherit it.
func TestStageTrackerApprovedHeadAt085(t *testing.T) {
	now := time.Date(2026, 11, 18, 11, 40, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	key := stagelabels.Key("acme/console", 1013)
	store.ObserveHead(key, "head-approved", stagelabels.NeedsDecision, "fork-guard:Canary")
	store.NoteHeadApproval(key, "head-approved", "@octocat")
	s.statusMu.Lock()
	s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
		FrontendPR{PullRequest: github.PullRequest{Number: 1013, Title: "refresh outdated daily package updates", Author: "pat", HeadSHA: "head-approved", Labels: []string{"stage/085-needs-decision"}}})
	s.statusMu.Unlock()
	if it := trackerItem(t, s.buildStageTracker(s.status, store, now), "pr", 1013); it.Stage != 85 || it.HumanApprover != "octocat" {
		t.Fatalf("row = stage %d approver %q, want 85 octocat", it.Stage, it.HumanApprover)
	}

	s.statusMu.Lock()
	last := len(s.status.Repos[0].OpenPrs) - 1
	pr := s.status.Repos[0].OpenPrs[last].(FrontendPR)
	pr.HeadSHA = "head-pushed"
	s.status.Repos[0].OpenPrs[last] = pr
	s.statusMu.Unlock()
	if it := trackerItem(t, s.buildStageTracker(s.status, store, now), "pr", 1013); it.HumanApprover != "" {
		t.Fatalf("a new head inherited the approval: %q", it.HumanApprover)
	}
}
