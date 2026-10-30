package dashboard

import (
	"regexp"
	"slices"
	"strconv"
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
		if len(d.Options) != 3 || d.Options[0].Action != decisionActionApprove || !d.Options[0].Enabled || d.Options[0].Consequence == "" || d.Options[1].Action != decisionActionDelegate || d.Options[2].Action != decisionActionHold {
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

// The lead merge-gate answer is "Approve and let hive merge" whenever it
// can be clicked; "Merge now as maintainer" steps forward only when
// queueing is greyed out, so the lead is never a button nobody can press.
func TestStageTrackerDecisionHumanMergeLeadOptions(t *testing.T) {
	since := time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC)
	base := StageTrackerItem{Repo: "hivecommons/hotshot", Number: 317, Kind: "pr", Author: "Karibhou", AuthorKind: authorKindHuman, BaseRef: "main",
		Stage: stagelabels.ReadyForHumanMerge.Number(), StageSince: &since, Reason: stageengineprstage.ContributorLaneReasonAuthorAssociation}
	for _, tc := range []struct {
		name     string
		mutate   func(*StageTrackerItem)
		held     bool
		wantLead string
		leadOn   bool
	}{
		{name: "plain row leads with queue-merge", mutate: func(*StageTrackerItem) {}, wantLead: decisionActionQueueMerge, leadOn: true},
		{name: "needs-rebase row still leads with queue-merge", mutate: func(it *StageTrackerItem) { it.Labels = []string{"needs-rebase"} }, wantLead: decisionActionQueueMerge, leadOn: true},
		{name: "admitted head leads with merge-now", mutate: func(it *StageTrackerItem) { it.AdmittedBy = stagelabels.OperatorApprovalAdmission("octo") }, wantLead: decisionActionMergeNow, leadOn: true},
		{name: "breaking change leads with merge-now", mutate: func(it *StageTrackerItem) { it.Reason = stageengineprstage.AutoMergeReasonHumanMergeRequired }, wantLead: decisionActionMergeNow, leadOn: true},
		{name: "held row keeps queue-merge first, both greyed", mutate: func(*StageTrackerItem) {}, held: true, wantLead: decisionActionQueueMerge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			it := base
			tc.mutate(&it)
			d := decisionForHumanMerge(it, tc.held)
			if len(d.Options) < 2 {
				t.Fatalf("options = %+v", d.Options)
			}
			lead, second := d.Options[0], d.Options[1]
			if lead.Action != tc.wantLead || lead.Enabled != tc.leadOn {
				t.Fatalf("lead = %+v, want %s enabled=%v", lead, tc.wantLead, tc.leadOn)
			}
			merge := map[string]bool{decisionActionQueueMerge: true, decisionActionMergeNow: true}
			if !merge[second.Action] || second.Action == lead.Action {
				t.Fatalf("second = %+v, want the other merge action", second)
			}
			if lead.Action == decisionActionQueueMerge && lead.Label != "Approve and let hive merge" {
				t.Fatalf("lead label = %q", lead.Label)
			}
		})
	}
}

// TestStageTrackerDecisionForHumanMerge: a PR at 090 Ready for human merge
// asks the person to merge, so the decision leads with the two ways to do
// that — "Approve and let hive merge" first (the maintainer's default last
// word: the hive's sweep merges on its next tick once CI is green), then
// "Merge now as maintainer" — each with its consequence, then direction /
// close / hold (hivecommons/hive#15078: hotshot#317 had no such action).
func TestStageTrackerDecisionForHumanMerge(t *testing.T) {
	since := time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC)
	it := StageTrackerItem{Repo: "hivecommons/hotshot", Number: 317, Kind: "pr", Author: "Karibhou", AuthorKind: authorKindHuman, BaseRef: "main",
		Stage: stagelabels.ReadyForHumanMerge.Number(), StageSince: &since, Reason: stageengineprstage.ContributorLaneReasonAuthorAssociation}
	d := stageTrackerDecisionNeeded(it, nil, "")
	if d == nil || d.AskedBy != decisionAskedByLane {
		t.Fatalf("decision = %+v", d)
	}
	if got := []string{d.Options[0].Action, d.Options[1].Action}; got[0] != decisionActionQueueMerge || got[1] != decisionActionMergeNow {
		t.Fatalf("lead options = %v, want queue-merge then merge-now", got)
	}
	queue := d.Options[0]
	if !queue.Enabled || queue.Label != "Approve and let hive merge" || queue.Consequence != "Hive merges #317 on its next tick once CI is green" {
		t.Fatalf("queue-merge = %+v", queue)
	}
	merge := d.Options[1]
	if !merge.Enabled || merge.Label != "Merge now as maintainer" || merge.Consequence != "Merges #317 into main now, credited to you" {
		t.Fatalf("merge-now = %+v", merge)
	}
	// hivecommons/hive#15394: the first read is one plain sentence of
	// state and one of why; the how-to-trust line sits behind Details.
	if d.Question != "Ready to merge. Hive won't merge it alone — @Karibhou isn't a trusted author yet." {
		t.Errorf("question = %q", d.Question)
	}
	if !strings.Contains(d.Detail, "To trust @Karibhou: give them the trusted tier under 👥 Contributors") || strings.Contains(d.Detail, "auto_merge.") {
		t.Errorf("detail = %q", d.Detail)
	}
	if d.Blocker != "" {
		t.Errorf("blocker should be empty on the first read: %q", d.Blocker)
	}
	for _, o := range d.Options {
		if n := len(strings.Fields(o.Consequence)); n > 12 {
			t.Errorf("%s consequence is %d words: %q", o.Action, n, o.Consequence)
		}
	}
	dir := decisionOptionByAction(t, d, decisionActionDirection)
	if dir.Enabled || !strings.Contains(dir.ReasonIfDisabled, "did not open this PR") {
		t.Fatalf("direction on a human PR = %+v", dir)
	}
	for _, want := range []string{decisionActionCloseSuperseded, decisionActionCloseWontfix, decisionActionHold} {
		if o := decisionOptionByAction(t, d, want); !o.Enabled {
			t.Errorf("%s should be enabled: %+v", want, o)
		}
	}

	t.Run("hive-authored PR may take direction", func(t *testing.T) {
		agent := it
		agent.Author, agent.AuthorKind, agent.Reason = "hive[bot]", "hive", "contributor-lane:workflow-files"
		d := stageTrackerDecisionNeeded(agent, nil, "")
		if d == nil || !decisionOptionByAction(t, d, decisionActionDirection).Enabled {
			t.Fatalf("decision = %+v", d)
		}
		if d.Options[0].Action != decisionActionQueueMerge || d.Options[1].Action != decisionActionMergeNow {
			t.Fatalf("queue-merge must lead, merge-now second: %+v", d.Options)
		}
	})

	t.Run("held row disables the merges with the reason", func(t *testing.T) {
		held := it
		held.Modifiers = []string{modHold}
		d := stageTrackerDecisionNeeded(held, nil, "")
		for _, a := range []string{decisionActionMergeNow, decisionActionQueueMerge} {
			if o := decisionOptionByAction(t, d, a); o.Enabled || o.ReasonIfDisabled != "Release the hold first" {
				t.Errorf("%s on a held row = %+v", a, o)
			}
		}
		decisionOptionByAction(t, d, decisionActionRelease)
	})

	// hivecommons/hive#15136: the popover warns about DCO BEFORE anyone
	// presses Approve and offers re-authoring as its own choice; once a
	// person approved, the row says who admitted the head and the second
	// Approve is no longer offered.
	t.Run("unsigned commits warn before acting and offer re-author", func(t *testing.T) {
		agent := it
		agent.Author, agent.AuthorKind, agent.DCOUnsigned, agent.DCOTotal = "hive[bot]", "hive", 2, 3
		d := stageTrackerDecisionNeeded(agent, nil, "")
		if !strings.Contains(d.Detail, "2 of 3 commits have no Signed-off-by") || !strings.Contains(d.Detail, "Re-author as hive PR is the separate choice") {
			t.Fatalf("details lack the DCO warning: %q", d.Detail)
		}
		if got := []string{d.Options[0].Action, d.Options[1].Action, d.Options[2].Action}; got[2] != decisionActionReauthor || got[0] != decisionActionQueueMerge || got[1] != decisionActionMergeNow {
			t.Fatalf("options = %v, want queue-merge, merge-now, re-author", got)
		}
		for _, a := range []string{decisionActionMergeNow, decisionActionQueueMerge} {
			if o := decisionOptionByAction(t, d, a); !o.Enabled || !strings.Contains(o.Consequence, "no Signed-off-by") {
				t.Errorf("%s must stay available with the DCO caveat: %+v", a, o)
			}
		}
		re := d.Options[2]
		if !re.Enabled || re.Label != "Re-author as hive PR" || !strings.Contains(re.Consequence, "nothing is merged as-is") {
			t.Fatalf("re-author = %+v", re)
		}
		human := it
		human.DCOUnsigned, human.DCOTotal = 1, 1
		d = stageTrackerDecisionNeeded(human, nil, "")
		if o := decisionOptionByAction(t, d, decisionActionReauthor); o.Enabled || !strings.Contains(o.ReasonIfDisabled, "@Karibhou to sign off") {
			t.Fatalf("re-author on a person's PR = %+v", o)
		}
		if d := stageTrackerDecisionNeeded(it, nil, ""); decisionHasAction(d, decisionActionReauthor) {
			t.Fatalf("re-author offered without unsigned commits: %+v", d.Options)
		}
	})

	t.Run("admitted head names the approver and drops the second approve", func(t *testing.T) {
		admitted := it
		admitted.AdmittedBy = stagelabels.OperatorApprovalAdmission("octo")
		d := stageTrackerDecisionNeeded(admitted, nil, "")
		if !strings.Contains(d.Detail, "Admitted by @octo (operator-approval:octo)") || !strings.HasPrefix(d.Question, "@octo approved it.") {
			t.Fatalf("question = %q detail = %q", d.Question, d.Detail)
		}
		if o := decisionOptionByAction(t, d, decisionActionQueueMerge); o.Enabled || !strings.Contains(o.ReasonIfDisabled, "@octo already approved") {
			t.Fatalf("queue-merge after admission = %+v", o)
		}
		if o := decisionOptionByAction(t, d, decisionActionMergeNow); !o.Enabled {
			t.Fatalf("merge-now must stay available: %+v", o)
		}
	})
}

func decisionHasAction(d *StageTrackerDecision, action string) bool {
	if d == nil {
		return false
	}
	for _, o := range d.Options {
		if o.Action == action {
			return true
		}
	}
	return false
}

// TestStageTrackerNeedsHumanStatesCarryAnAction: every state
// stageTrackerNeedsHuman flags — the stages, the author stages on a human
// PR and the modifiers — yields a decision_needed whose options are
// non-empty and hold at least one enabled action, or a disabled one whose
// reason_if_disabled is plain words. A pill that asks for a person with no
// matching action is the bug this guards (hivecommons/hive#15078).
func TestStageTrackerNeedsHumanStatesCarryAnAction(t *testing.T) {
	since := time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC)
	type tc struct {
		name string
		it   StageTrackerItem
	}
	var cases []tc
	for _, stage := range stageTrackerNeedsHumanStages {
		for _, kind := range []string{"issue", "pr"} {
			it := StageTrackerItem{Repo: "o/r", Number: 1, Kind: kind, Stage: stage, StageSince: &since}
			if kind == "pr" {
				it.Author, it.AuthorKind, it.BaseRef = "someone", authorKindHuman, "main"
				if stage == stagelabels.ReadyForHumanMerge.Number() {
					it.Reason = stageengineprstage.ContributorLaneReasonAuthorAssociation
				}
			}
			cases = append(cases, tc{"stage " + strconv.Itoa(stage) + " " + kind, it})
		}
	}
	for _, stage := range stageTrackerNeedsHumanAuthorStages {
		cases = append(cases, tc{"author stage " + strconv.Itoa(stage) + " human pr", StageTrackerItem{Repo: "o/r", Number: 2, Kind: "pr", Author: "someone", AuthorKind: authorKindHuman, Stage: stage, StageSince: &since, Reason: "changes-requested"}})
	}
	for _, mod := range stageTrackerNeedsHumanMods {
		for _, kind := range []string{"issue", "pr"} {
			it := StageTrackerItem{Repo: "o/r", Number: 3, Kind: kind, Stage: 60, StageSince: &since, Modifiers: []string{mod}}
			if mod == modUnauthorized {
				it.Reason = "unauthorized:approve-workflow-runs"
			}
			cases = append(cases, tc{"modifier " + mod + " " + kind, it})
		}
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !stageTrackerNeedsHuman(c.it) {
				t.Fatalf("fixture is not flagged needs-human: %+v", c.it)
			}
			d := stageTrackerDecisionNeeded(c.it, nil, "hive:sentinel-alert")
			if d == nil {
				t.Fatalf("no decision_needed for a needs-human row: %+v", c.it)
			}
			if strings.TrimSpace(d.Question) == "" {
				t.Fatal("decision has no question")
			}
			if len(d.Options) == 0 {
				t.Fatalf("decision_needed.options is empty: %+v", d)
			}
			enabled := false
			for _, o := range d.Options {
				if o.Action == "" || o.Label == "" {
					t.Errorf("option without action/label: %+v", o)
				}
				if o.Enabled {
					enabled = true
					continue
				}
				if r := strings.TrimSpace(o.ReasonIfDisabled); r == "" || strings.Contains(r, ":") && !strings.Contains(r, " ") {
					t.Errorf("disabled option %q lacks a plain-words reason: %+v", o.Action, o)
				}
			}
			if !enabled {
				t.Errorf("no enabled action among %+v", d.Options)
			}
			// "Let hive decide" is on every needs-human state: where the
			// hive cannot act it answers in plain words why, with options.
			if !slices.ContainsFunc(d.Options, func(o StageTrackerDecisionOption) bool { return o.Action == decisionActionDelegate }) {
				t.Errorf("no Let hive decide among %+v", d.Options)
			}
		})
	}
}

// TestStageTrackerHumanMergeSaysItOnce (hivecommons/hive#15394): the 090
// hover is built from the question, the folded details, the gate word's
// tip and the next-move line. No sentence may appear twice across them,
// the next-move line must not echo the reason text, and none of the
// operator-visible strings may carry a config key or internal jargon.
func TestStageTrackerHumanMergeSaysItOnce(t *testing.T) {
	since := time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC)
	for _, reason := range []string{
		stageengineprstage.ContributorLaneReasonAuthorAssociation, "untrusted-author-role", "author-permission-missing", "fork-non-member",
		stageengineprstage.ContributorLaneReasonClaimTier, stageengineprstage.ContributorLaneReasonClaimUnverified, stageengineprstage.ContributorLaneReasonNoClaim,
		"contributor-lane:workflow-files", "contributor-lane:tests-removed", "repo-auto-merge-disabled", "something-brand-new",
	} {
		it := StageTrackerItem{Repo: "hivecommons/hive", Number: 15315, Kind: "pr", Author: "odttlnt", AuthorKind: authorKindHuman, BaseRef: "v5",
			Stage: stagelabels.ReadyForHumanMerge.Number(), StageSince: &since, Reason: reason}
		base, _ := stagelabels.HumanApprovedReason(reason)
		if why := stageTrackerUntrustedAuthorWhy(base, it.Author); why != "" {
			it.HumanMergeReasonText = "Hive won't merge it alone — " + why
		}
		it.HumanMergeReason, _, _ = stageTrackerHumanMergeReason(reason)
		d := stageTrackerDecisionNeeded(it, nil, "")
		actor := stageTrackerStageActor(it, nil, since)
		if n := len(strings.Fields(d.Question)); n > 25 {
			t.Errorf("%s: question is %d words: %q", reason, n, d.Question)
		}
		seen := map[string]string{}
		for _, part := range []struct{ name, text string }{{"question", d.Question}, {"blocker", d.Blocker}, {"detail", d.Detail}, {"next-move", actor.Why}} {
			for _, sent := range stageTrackerSentences(part.text) {
				if prev, dup := seen[sent]; dup {
					t.Errorf("%s: %q appears in both %s and %s", reason, sent, prev, part.name)
				}
				seen[sent] = part.name
			}
		}
		if it.HumanMergeReasonText != "" && strings.Contains(actor.Why, it.HumanMergeReasonText) {
			t.Errorf("%s: next-move echoes the reason text: %q", reason, actor.Why)
		}
		all := strings.Join([]string{d.Question, d.Blocker, d.Detail, actor.Why, it.HumanMergeReasonText}, " ")
		if m := configKeyRE.FindString(all); m != "" {
			t.Errorf("%s: config key %q in hover text: %q", reason, m, all)
		}
		for _, word := range []string{"sweep", "soak", "sentinel", "audit marker", "trust override", " lane", "requires_human", " 085", " 090"} {
			if strings.Contains(strings.ToLower(all), word) {
				t.Errorf("%s: jargon %q in hover text: %q", reason, word, all)
			}
		}
	}
	if got := stageTrackerSayOnce("Ready to merge. Hive won't merge it alone — @x isn't trusted.", "hive won't merge it alone — @x isn't trusted. To trust @x: do this.", "To trust @x: do this. Rails apply."); got != "To trust @x: do this. Rails apply." {
		t.Errorf("stageTrackerSayOnce = %q", got)
	}
}

var configKeyRE = regexp.MustCompile(`\b[a-z_]+\.[a-z_]+\.[a-z_]+\b`)
