package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#19421: a trusted author's PR parked at 085 on the
// reviewer's requires_human question, which the trusted-author lane merges
// anyway because its review gate does not enforce. The decision popover
// must not say the hive will not merge it; it names the label that stops
// the merge instead.
func TestDecisionForPRTrustedAuthorMerges(t *testing.T) {
	verdict := &review.Aggregate{DecisionQuestion: "@clubanderson should the hive resolve its own review threads?"}
	it := StageTrackerItem{Repo: "hivecommons/hive", Number: 19421, Kind: "pr", Stage: trackerNumberNeedsDecision, Head: "4902cf73db2c", Reason: stageengine.StageReasonVerdictRequiresHuman}

	plain := decisionForPR(it, verdict, "", false)
	if plain.Blocker != "The hive will not merge until a person decides." {
		t.Fatalf("blocker without the lane's note = %q", plain.Blocker)
	}

	it.TrustedAuthorMerges = "hold"
	d := decisionForPR(it, verdict, "", false)
	if !strings.Contains(d.Blocker, "trusted-author lane will still merge this") || !strings.Contains(d.Blocker, "Add the `hold` label") || strings.Contains(d.Blocker, "will not merge") {
		t.Fatalf("blocker = %q", d.Blocker)
	}
	if d.Question != verdict.DecisionQuestion {
		t.Fatalf("the reviewer's question is still owed: question = %q", d.Question)
	}
}

// End to end: the stage pass's note for the head reaches the 085 row; a
// later head does not inherit it.
func TestStageTrackerTrustedAuthorMergesAt085(t *testing.T) {
	now := time.Date(2026, 11, 21, 13, 54, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	key := stagelabels.Key("acme/console", 19421)
	store.ObserveHead(key, "head-1", stagelabels.NeedsDecision, stageengine.StageReasonVerdictRequiresHuman)
	store.NoteTrustedAuthorMerges(key, "head-1", "hold", now)
	s.statusMu.Lock()
	s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
		FrontendPR{PullRequest: github.PullRequest{Number: 19421, Title: "resolve the hive's own review threads", Author: "Danathar", HeadSHA: "head-1", Labels: []string{"stage/085-needs-decision"}}})
	s.statusMu.Unlock()
	if it := trackerItem(t, s.buildStageTracker(s.status, store, now), "pr", 19421); it.Stage != 85 || it.TrustedAuthorMerges != "hold" {
		t.Fatalf("row = stage %d trusted-author merges %q, want 85 hold", it.Stage, it.TrustedAuthorMerges)
	}

	s.statusMu.Lock()
	last := len(s.status.Repos[0].OpenPrs) - 1
	pr := s.status.Repos[0].OpenPrs[last].(FrontendPR)
	pr.HeadSHA = "head-2"
	s.status.Repos[0].OpenPrs[last] = pr
	s.statusMu.Unlock()
	if it := trackerItem(t, s.buildStageTracker(s.status, store, now), "pr", 19421); it.TrustedAuthorMerges != "" {
		t.Fatalf("a new head inherited the note: %q", it.TrustedAuthorMerges)
	}
}
