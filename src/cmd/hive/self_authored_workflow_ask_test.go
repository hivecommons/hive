package main

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/bonedigger#106 end to end: the review of the hive's own PR
// asks a maintainer to commit a hunk in a workflow file. The aggregate the
// review records, read back the way the lanes read it, must classify as
// 085 with the ask, not 070, and 085 has no stalled dwell, so the PR no
// longer flaps stage/stalled while it waits for the person.
func TestSelfAuthoredWorkflowAskParksAt085WithTheAsk(t *testing.T) {
	const head = "a3c2a5b431c40c23b34c4dbe968cb52446d10faf"
	const ask = "apply the `pr-validate.yml` + `BASELINE` hunks from the PR body on this branch before merging"
	report := func(file string) review.PerspectiveReport {
		return review.PerspectiveReport{
			AgentReport: outputschema.AgentReport{
				Lane: "review-swarm", Kind: outputschema.KindReview, Summary: "the title is not yet true",
				Findings: []outputschema.Finding{{
					Title: "pull-request check still installs the lint tool unpinned", Severity: outputschema.SeverityMedium,
					ReviewScope: "in-scope", File: file, Line: 28,
					Summary: "update the pipx step to install from .github/requirements-ci.txt",
				}},
				PRsOpened: []outputschema.PROpened{}, BeadsFiled: []outputschema.BeadFiled{},
			},
			Perspective: review.PerspectiveIntentAlignment, Verdict: review.VerdictRequiresHuman,
			Repo: "projectbluefin/bonedigger", Number: 106, HeadSHA: head,
			HiveAuthored: true, DecisionRequested: true, DecisionQuestion: ask,
		}
	}
	classify := func(t *testing.T, file, laneReason string) (prstage.Verdict, review.Aggregate) {
		t.Helper()
		agg := review.AggregateReports([]review.PerspectiveReport{report(file)}, review.AggregateOptions{})
		agg.RecordedAt = time.Date(2026, 10, 29, 18, 25, 47, 0, time.UTC)
		verdict := contributorLaneVerdictFromArtifact(review.Artifact{Items: []review.Aggregate{agg}}, "projectbluefin/bonedigger", 106, head, false)
		return stageengine.ClassifyStage(prstage.Observation{
			Lane: prstage.StageLaneSelf, Number: 106, HeadSHA: head, Reason: laneReason,
			Detail: prstage.Detail{VerdictKnown: true, Verdict: verdict},
		}), agg
	}

	got, agg := classify(t, ".github/workflows/pr-validate.yml", "review-verdict-requires-human")
	if got.Stage != stagelabels.NeedsDecision || !got.Human {
		t.Fatalf("stage = %+v (aggregate %s), want 085 needs-decision", got, agg.Verdict)
	}
	if got.Reason != stageengine.StageReasonDecisionRequested {
		t.Fatalf("reason = %q, want %q", got.Reason, stageengine.StageReasonDecisionRequested)
	}
	verdict := contributorLaneVerdictFromArtifact(review.Artifact{Items: []review.Aggregate{agg}}, "projectbluefin/bonedigger", 106, head, false)
	if verdict.DecisionQuestion != ask || verdict.DecisionFinding == "" {
		t.Fatalf("verdict = %+v, want the ask and the finding for the 085 comment", verdict)
	}

	entered := time.Date(2026, 10, 29, 19, 3, 12, 0, time.UTC)
	parked := stagelabels.Record{Stage: got.Stage, EnteredAt: entered, LastActivityAt: entered}
	if stagelabels.Stalled(parked, nil, entered.Add(40*time.Hour)) {
		t.Fatalf("085 marked stalled after 40h; it must wait for the person without the stalled label")
	}

	// The ask the hive can make itself still goes to the fixer at 070,
	// and 070 still stalls when nobody works it.
	fixing, _ := classify(t, "tests/test_python_install_pins.py", "changes-requested")
	if fixing.Stage != stagelabels.Fixing {
		t.Fatalf("stage = %+v, want 070 fixing for a fix off the sensitive paths", fixing)
	}
	working := stagelabels.Record{Stage: fixing.Stage, EnteredAt: entered, LastActivityAt: entered}
	if !stagelabels.Stalled(working, nil, entered.Add(3*time.Hour)) {
		t.Fatalf("070 not stalled after 3h quiet; the fixing dwell must be unchanged")
	}
}

// The stage-side fallback normalizer rewrites a hive-authored requires_human
// with concrete wording to changes_requested. A verdict whose aggregate
// recorded a DecisionFinding is a person's by policy and must stay so.
func TestNormalizeReviewVerdictKeepsARecordedDecisionFinding(t *testing.T) {
	v := prstage.ReviewVerdict{
		Configured: true, Verdict: "requires_human", HiveAuthoredKnown: true, HiveAuthored: true,
		HumanSummaries: []string{"update the pipx step in pr-validate.yml"},
	}
	if got := prstage.NormalizeReviewVerdict(v); got.Verdict != "changes_requested" {
		t.Fatalf("without a finding: verdict = %q, want the existing changes_requested rewrite", got.Verdict)
	}
	v.DecisionFinding = `sensitive path .github/workflows/pr-validate.yml: intent-alignment finding "unpinned"`
	if got := prstage.NormalizeReviewVerdict(v); got.Verdict != "requires_human" {
		t.Fatalf("with a finding: verdict = %q, want requires_human kept", got.Verdict)
	}
}
