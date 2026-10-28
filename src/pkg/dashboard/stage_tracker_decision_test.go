package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine"
	stageengineprstage "github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func decisionOptionByAction(t *testing.T, d *StageTrackerDecision, action string) StageTrackerDecisionOption {
	t.Helper()
	for _, o := range d.Options {
		if o.Action == action {
			return o
		}
	}
	t.Fatalf("option %q missing from %+v", action, d.Options)
	return StageTrackerDecisionOption{}
}

func TestStageTrackerDecisionNeeded(t *testing.T) {
	since := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	recorded := time.Date(2026, 10, 27, 10, 30, 0, 0, time.UTC)
	pr := func(reason string, mods ...string) StageTrackerItem {
		return StageTrackerItem{Repo: "hivecommons/hive", Number: 14800, Kind: "pr", Head: "b0f5f80abcdef0123",
			Stage: stagelabels.NeedsDecision.Number(), StageSince: &since, Reason: reason, Modifiers: mods}
	}

	t.Run("reviewer requires_human question is quoted", func(t *testing.T) {
		agg := &review.Aggregate{DecisionQuestion: "@clubanderson should decide whether the retry budget may be raised to 5", RecordedAt: recorded}
		d := stageTrackerDecisionNeeded(pr(stageengine.StageReasonVerdictRequiresHuman), agg, "hive:sentinel-alert")
		if d == nil {
			t.Fatal("nil decision")
		}
		if d.Question != agg.DecisionQuestion {
			t.Fatalf("question = %q", d.Question)
		}
		if d.AskedBy != decisionAskedByReviewer || d.Since == nil || !d.Since.Equal(recorded) {
			t.Fatalf("asked_by/since = %q %v", d.AskedBy, d.Since)
		}
		if d.BlockerReason != stageengine.StageReasonVerdictRequiresHuman || d.Blocker == "" {
			t.Fatalf("blocker = %q (%q)", d.Blocker, d.BlockerReason)
		}
		if d.Options[0].Action != decisionActionDelegate || !d.Options[0].Enabled {
			t.Fatalf("Let hive decide must lead and be enabled: %+v", d.Options[0])
		}
		if !decisionOptionByAction(t, d, decisionActionApprove).Enabled {
			t.Fatal("approve should be enabled for a plain requires_human")
		}
		for _, want := range []string{decisionActionDirection, decisionActionCloseSuperseded, decisionActionCloseWontfix, decisionActionHold} {
			decisionOptionByAction(t, d, want)
		}
	})

	t.Run("requires_human without a question falls back to the reason", func(t *testing.T) {
		d := stageTrackerDecisionNeeded(pr(stageengine.StageReasonVerdictRequiresHuman), nil, "")
		if d == nil || !strings.Contains(d.Question, "A reviewer handed this PR to a person") || d.AskedBy != decisionAskedByReviewer {
			t.Fatalf("decision = %+v", d)
		}
		// Plain words only: no reason slugs, stage codes or verdict enums.
		for _, bad := range []string{"requires_human", "contributor-lane", "085", "sentinel"} {
			if strings.Contains(d.Question+" "+d.Blocker, bad) {
				t.Fatalf("jargon %q in %q / %q", bad, d.Question, d.Blocker)
			}
		}
		if d.Since == nil || !d.Since.Equal(since) {
			t.Fatalf("since should fall back to stage_since: %v", d.Since)
		}
	})

	t.Run("sentinel", func(t *testing.T) {
		it := pr(stageengineprstage.ContributorLaneReasonSentinelBlocked)
		it.SentinelAlert, it.SentinelReason = true, "a workflow file changed"
		d := stageTrackerDecisionNeeded(it, nil, "hive:sentinel-alert")
		if d == nil || d.AskedBy != decisionAskedBySentinel {
			t.Fatalf("decision = %+v", d)
		}
		if !strings.Contains(d.Question, "The security check flagged a workflow file changed") || !strings.Contains(d.Blocker, "hive:sentinel-alert") {
			t.Fatalf("question/blocker = %q / %q", d.Question, d.Blocker)
		}
		if d.BlockerReason != stageengineprstage.ContributorLaneReasonSentinelBlocked {
			t.Fatalf("blocker_reason = %q", d.BlockerReason)
		}
		// A sentinel alert with no lane reason coins its own token.
		it.Reason = ""
		if d := stageTrackerDecisionNeeded(it, nil, "x"); d.BlockerReason != decisionBlockerSentinel {
			t.Fatalf("blocker_reason = %q", d.BlockerReason)
		}
	})

	t.Run("contributor-lane changes requested disables approve", func(t *testing.T) {
		d := stageTrackerDecisionNeeded(pr(stageengineprstage.ContributorLaneReasonChangesRequested), nil, "")
		if d == nil {
			t.Fatal("nil")
		}
		want := "A reviewer requested changes on b0f5f80 — needs a new commit. Approving cannot override that."
		if d.Blocker != want {
			t.Fatalf("blocker = %q", d.Blocker)
		}
		if d.BlockerReason != stageengineprstage.ContributorLaneReasonChangesRequested {
			t.Fatalf("blocker_reason = %q", d.BlockerReason)
		}
		approve := decisionOptionByAction(t, d, decisionActionApprove)
		if approve.Enabled || approve.ReasonIfDisabled != want {
			t.Fatalf("approve = %+v", approve)
		}
		if !decisionOptionByAction(t, d, decisionActionDelegate).Enabled || !decisionOptionByAction(t, d, decisionActionDirection).Enabled {
			t.Fatal("delegate and direction stay enabled")
		}
		// The adoption-stalled form of the same reason reads the same way.
		d = stageTrackerDecisionNeeded(pr(stagelabels.LaneReasonAdoptionStalledPrefix+stagelabels.LaneReasonChangesRequested), nil, "")
		if decisionOptionByAction(t, d, decisionActionApprove).Enabled {
			t.Fatal("approve should be disabled for adoption-stalled changes_requested")
		}
	})

	t.Run("adoption stalled on a lane rule", func(t *testing.T) {
		d := stageTrackerDecisionNeeded(pr(stagelabels.LaneReasonAdoptionStalledPrefix+stageengineprstage.ContributorLaneReasonWorkflowPath), nil, "")
		if d == nil || !strings.Contains(d.Question, "took this PR over") || !strings.Contains(d.Question, "edits CI workflow files") || d.AskedBy != decisionAskedByLane {
			t.Fatalf("decision = %+v", d)
		}
		if d.BlockerReason != "adoption-stalled:contributor-lane:workflow-path" {
			t.Fatalf("blocker_reason = %q", d.BlockerReason)
		}
	})

	t.Run("human approval suffix is stripped from the token", func(t *testing.T) {
		d := stageTrackerDecisionNeeded(pr(stageengineprstage.ContributorLaneReasonWorkflowPath+stagelabels.HumanApprovedReasonSuffix+"octocat"), nil, "")
		if d == nil || d.BlockerReason != stageengineprstage.ContributorLaneReasonWorkflowPath {
			t.Fatalf("decision = %+v", d)
		}
		if !strings.Contains(d.Blocker, "a maintainer merges") {
			t.Fatalf("blocker = %q", d.Blocker)
		}
	})

	t.Run("unauthorized step disables delegate and approve", func(t *testing.T) {
		it := pr(stageengineprstage.StageReasonUnauthorizedPrefix + "merge")
		it.UnauthorizedStep, it.Unauthorized = "merge", "The hive's GitHub App may not merge pull requests on this repo."
		d := stageTrackerDecisionNeeded(it, nil, "")
		if d == nil || d.AskedBy != decisionAskedByApp || d.Blocker != it.Unauthorized {
			t.Fatalf("decision = %+v", d)
		}
		if decisionOptionByAction(t, d, decisionActionDelegate).Enabled || decisionOptionByAction(t, d, decisionActionApprove).Enabled {
			t.Fatal("delegate/approve must be disabled")
		}
	})

	t.Run("held PR offers release and disables delegate", func(t *testing.T) {
		d := stageTrackerDecisionNeeded(pr(stageengine.StageReasonVerdictRequiresHuman, modHold), nil, "")
		del := decisionOptionByAction(t, d, decisionActionDelegate)
		if del.Enabled || del.ReasonIfDisabled != decisionWhyHeld {
			t.Fatalf("delegate = %+v", del)
		}
		decisionOptionByAction(t, d, decisionActionRelease)
		if !strings.Contains(d.Blocker, "On hold") {
			t.Fatalf("blocker = %q", d.Blocker)
		}
	})

	t.Run("issue at 045", func(t *testing.T) {
		it := StageTrackerItem{Repo: "hivecommons/hive", Number: 42, Kind: "issue", Stage: stagelabels.IssueNeedsDecision.Number(),
			StageSince: &since, Reason: "parked on a person: needs-decision"}
		d := stageTrackerDecisionNeeded(it, nil, "")
		if d == nil || d.AskedBy != decisionAskedByHiveAgent {
			t.Fatalf("decision = %+v", d)
		}
		if !strings.Contains(d.Question, "parked this issue for a person") || !strings.Contains(d.Question, "Approve the direction") {
			t.Fatalf("question = %q", d.Question)
		}
		if d.Options[0].Action != decisionActionDelegate {
			t.Fatalf("first option = %+v", d.Options[0])
		}
		if decisionOptionByAction(t, d, decisionActionApprove).Label != "Approve direction" {
			t.Fatal("issue approve label")
		}
		if decisionOptionByAction(t, d, decisionActionDirection).Label != "Send back" {
			t.Fatal("issue direction label")
		}
		// A mirrored issue defers to its covering PR.
		it.Mirrored = true
		if d := stageTrackerDecisionNeeded(it, nil, ""); d != nil {
			t.Fatalf("mirrored issue should carry no decision: %+v", d)
		}
	})

	t.Run("issue at 015 triage", func(t *testing.T) {
		it := StageTrackerItem{Repo: "hivecommons/hive", Number: 43, Kind: "issue", Stage: stagelabels.TriageNeeded.Number(), StageSince: &since, Modifiers: []string{modNeedsTriage}}
		d := stageTrackerDecisionNeeded(it, nil, "")
		if d == nil || d.AskedBy != decisionAskedByTriage || !strings.Contains(d.Question, "Accept triage") {
			t.Fatalf("decision = %+v", d)
		}
		if d.Options[0].Action != decisionActionAcceptTriage {
			t.Fatalf("first option = %+v", d.Options[0])
		}
		decisionOptionByAction(t, d, decisionActionHold)
		decisionOptionByAction(t, d, decisionActionNeedsHuman)
	})

	t.Run("needs-human chip on any stage", func(t *testing.T) {
		it := StageTrackerItem{Repo: "hivecommons/hive", Number: 44, Kind: "pr", Stage: 60, StageSince: &since, Modifiers: []string{modNeedsHuman}}
		d := stageTrackerDecisionNeeded(it, nil, "")
		if d == nil || d.AskedBy != decisionAskedByEscalation || d.BlockerReason != "label:needs-human" {
			t.Fatalf("decision = %+v", d)
		}
		if !strings.Contains(d.Question, "This PR is marked needs-human") {
			t.Fatalf("question = %q", d.Question)
		}
		if len(d.Options) != 1 || d.Options[0].Action != decisionActionHold {
			t.Fatalf("options = %+v", d.Options)
		}
	})

	t.Run("plain rows carry none", func(t *testing.T) {
		it := StageTrackerItem{Repo: "hivecommons/hive", Number: 45, Kind: "pr", Stage: 60, Reason: "ci-running"}
		if d := stageTrackerDecisionNeeded(it, nil, ""); d != nil {
			t.Fatalf("unexpected decision %+v", d)
		}
	})
}

func TestStageTrackerVerdictLookup(t *testing.T) {
	older := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	art := &review.Artifact{Items: []review.Aggregate{
		{Repo: "hivecommons/hive", Number: 7, HeadSHA: "abcdef0123456789", RecordedAt: older, DecisionQuestion: "old"},
		{Repo: "hivecommons/hive", Number: 7, HeadSHA: "abcdef0123456789", RecordedAt: newer, DecisionQuestion: "new"},
		{Repo: "hivecommons/hive", Number: 7, HeadSHA: "0000000", RecordedAt: newer.Add(time.Hour), DecisionQuestion: "other head"},
		{Repo: "other/repo", Number: 7, HeadSHA: "abcdef0123456789", RecordedAt: newer.Add(time.Hour), DecisionQuestion: "other repo"},
	}}
	look := stageTrackerVerdictLookup(art)
	agg, ok := look("HiveCommons/Hive", 7, "abcdef0")
	if !ok || agg.DecisionQuestion != "new" {
		t.Fatalf("lookup = %+v %v", agg, ok)
	}
	if _, ok := look("hivecommons/hive", 8, "abcdef0"); ok {
		t.Fatal("number mismatch matched")
	}
	if _, ok := look("hivecommons/hive", 7, ""); ok {
		t.Fatal("empty head matched")
	}
	if _, ok := stageTrackerVerdictLookup(nil)("hivecommons/hive", 7, "abcdef0"); ok {
		t.Fatal("nil artifact matched")
	}
}
