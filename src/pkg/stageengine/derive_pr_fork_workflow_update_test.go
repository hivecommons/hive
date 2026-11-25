package stageengine

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// GitHub refused the App's update-branch on a fork head because the base
// changed .github/workflows/ files (fork-workflow-update). The App cannot
// push to the fork, but an agent author's relay can push to its own branch:
// projectbluefin/utah-packages#1294's relay merged the base in and the PR
// recovered with no person, while #1296 and #1298 sat at 085 all night with
// nothing sent to their authors, approved and green. That one case goes to
// 072; every other head keeps today's 085 hand-off.

func fwuApproved() stageDetail {
	return stageDetail{Verdict: ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}, VerdictKnown: true}
}

func fwuObs(kind authorkind.Kind, d stageDetail, labels ...string) stageObservation {
	return stageObservation{Lane: stageLaneContributor, Reason: autoMergeReasonForkWorkflowUpdate, HeadSHA: goldenHead, AuthorKind: kind, Detail: d, Labels: labels}
}

// ClassifyStage itself is unchanged: with or without the kind, a
// fork-workflow-update is the 085 hand-off.
func TestClassifyStageForkWorkflowUpdateUnchanged(t *testing.T) {
	for _, kind := range []authorkind.Kind{authorkind.Unknown, authorkind.Human, authorkind.Clanker} {
		got := ClassifyStage(fwuObs(kind, fwuApproved()))
		if got.Stage != stagelabels.NeedsDecision || !got.Human || got.Rank != stageRankOwned || got.Reason != "" {
			t.Fatalf("kind %q: %+v, want the owned 085 hand-off", kind, got)
		}
	}
}

// Only an agent's approved, green head with nothing else owed goes to 072
// under the lane reason. Everything else keeps what it had before: the 085
// hand-off the human route places by the head's evidence (or, for a
// trusted change request, its own 07x).
func TestSplitByAuthorKindForkWorkflowUpdate(t *testing.T) {
	green := &PRPassFacts{HeadChecksGreen: true}
	requiresHuman := func(approver, head string) stageDetail {
		return stageDetail{Verdict: ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}, VerdictKnown: true, HumanApprover: approver, HumanApprovalHead: head}
	}
	verdictOf := func(v ContributorLaneVerdict) stageDetail { return stageDetail{Verdict: v, VerdictKnown: true} }
	redGuard := fwuApproved()
	redGuard.RedChecks, redGuard.RedChecksHead = []string{"Canary"}, goldenHead
	redOther := fwuApproved()
	redOther.RedChecks, redOther.RedChecksHead = []string{"lint"}, goldenHead
	changeRequest := fwuApproved()
	changeRequest.HumanChangeRequester, changeRequest.HumanChangeRequestHead = "maintainer", goldenHead
	guarded := fwuObs(authorkind.Clanker, fwuApproved())
	guarded.ForkGuardChecks = []string{"Canary"}
	selfLane := fwuObs(authorkind.Clanker, fwuApproved())
	selfLane.Lane = stageLaneSelf
	merged := fwuObs(authorkind.Clanker, fwuApproved())
	merged.Merged = true
	cases := []struct {
		name    string
		obs     stageObservation
		pass    *PRPassFacts
		toAgent bool
	}{
		{name: "agent, approved, green", obs: fwuObs(authorkind.Clanker, fwuApproved()), pass: green, toAgent: true},
		{name: "agent, requires_human a trusted person approved on the head", obs: fwuObs(authorkind.Clanker, requiresHuman("maintainer", goldenHead)), pass: green, toAgent: true},
		{name: "person", obs: fwuObs(authorkind.Human, fwuApproved()), pass: green},
		{name: "kind unknown", obs: fwuObs(authorkind.Unknown, fwuApproved()), pass: green},
		{name: "self-authored lane", obs: selfLane, pass: green},
		{name: "merged", obs: merged, pass: green},
		{name: "no pass facts", obs: fwuObs(authorkind.Clanker, fwuApproved())},
		{name: "checks not read green", obs: fwuObs(authorkind.Clanker, fwuApproved()), pass: &PRPassFacts{}},
		{name: "kind only from the registry", obs: fwuObs(authorkind.Clanker, fwuApproved()), pass: &PRPassFacts{HeadChecksGreen: true, AuthorKindByRegistry: true}},
		{name: "sentinel alert", obs: fwuObs(authorkind.Clanker, fwuApproved()), pass: &PRPassFacts{HeadChecksGreen: true, SentinelAlert: true}},
		{name: "verdict not noted (split drain)", obs: fwuObs(authorkind.Clanker, stageDetail{}), pass: green},
		{name: "verdict pending", obs: fwuObs(authorkind.Clanker, verdictOf(ContributorLaneVerdict{Configured: true})), pass: green},
		{name: "changes_requested", obs: fwuObs(authorkind.Clanker, verdictOf(ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"})), pass: green},
		{name: "reject", obs: fwuObs(authorkind.Clanker, verdictOf(ContributorLaneVerdict{Configured: true, Verdict: "reject"})), pass: green},
		{name: "incomplete", obs: fwuObs(authorkind.Clanker, verdictOf(ContributorLaneVerdict{Configured: true, Verdict: "incomplete"})), pass: green},
		{name: "no reviewer configured", obs: fwuObs(authorkind.Clanker, verdictOf(ContributorLaneVerdict{})), pass: green},
		{name: "requires_human", obs: fwuObs(authorkind.Clanker, requiresHuman("", "")), pass: green},
		{name: "requires_human approved on an older head", obs: fwuObs(authorkind.Clanker, requiresHuman("maintainer", "0ld0000000")), pass: green},
		{name: "decision requested", obs: fwuObs(authorkind.Clanker, verdictOf(ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve", DecisionRequested: true, DecisionQuestion: "ship it?"})), pass: green},
		{name: "trusted change request on the head", obs: fwuObs(authorkind.Clanker, changeRequest), pass: green},
		{name: "red check on the head", obs: fwuObs(authorkind.Clanker, redOther), pass: green},
		{name: "red fork guard", obs: fwuObs(authorkind.Clanker, redGuard), pass: green},
		{name: "fork guard read for the head", obs: guarded, pass: green},
		{name: "hold label", obs: fwuObs(authorkind.Clanker, fwuApproved(), "hold"), pass: green},
		{name: "needs-human label", obs: fwuObs(authorkind.Clanker, fwuApproved(), "needs-human"), pass: green},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.obs
			raw.AuthorKind = authorkind.Unknown
			verdict, _, _ := ResolveStage([]stageObservation{raw})
			got := SplitByAuthorKind(verdict, tc.obs, tc.pass)
			if tc.toAgent {
				if got.Stage != stagelabels.ReturnedToContributor || got.Rank != stageRankOwned || got.Human || got.Reason != autoMergeReasonForkWorkflowUpdate || !AuthorUpdateOwed(got) {
					t.Fatalf("%+v, want owned 072 on %q", got, autoMergeReasonForkWorkflowUpdate)
				}
				return
			}
			if AuthorUpdateOwed(got) || got.Reason == autoMergeReasonForkWorkflowUpdate {
				t.Fatalf("%+v: sent to the agent for the branch update", got)
			}
			// The plain 07x split, the only thing it does to these.
			want := verdict
			if want.Stage == stagelabels.NeedsAuthor || want.Stage == stagelabels.ReturnedToContributor {
				want.Stage = FixingStage(tc.obs)
			}
			if got != want {
				t.Fatalf("%+v, want %+v as before", got, want)
			}
		})
	}
}

// A pass that lost the verdict (a split drain) keeps an agent's 072 when
// the record already has this very head there for the update, and every
// other input still says route. Anything else takes the hand-off.
func TestSplitByAuthorKindLostVerdictKeepsSameHead072(t *testing.T) {
	held := &PriorStage{Stage: stagelabels.ReturnedToContributor, Reason: autoMergeReasonForkWorkflowUpdate, ActivityHead: goldenHead, AuthorUpdateOwedHead: goldenHead}
	redOther := stageDetail{RedChecks: []string{"lint"}, RedChecksHead: goldenHead}
	newHead := fwuObs(authorkind.Clanker, stageDetail{})
	newHead.HeadSHA = "n3wh3ad000"
	guarded := fwuObs(authorkind.Clanker, stageDetail{})
	guarded.ForkGuardChecks = []string{"Canary"}
	for _, tc := range []struct {
		name string
		obs  stageObservation
		pass PRPassFacts
		keep bool
	}{
		{name: "same head held at 072", obs: fwuObs(authorkind.Clanker, stageDetail{}), pass: PRPassFacts{HeadChecksGreen: true, Prior: held}, keep: true},
		{name: "new head", obs: newHead, pass: PRPassFacts{HeadChecksGreen: true, Prior: held}},
		{name: "red check", obs: fwuObs(authorkind.Clanker, redOther), pass: PRPassFacts{HeadChecksGreen: true, Prior: held}},
		{name: "fork guard", obs: guarded, pass: PRPassFacts{HeadChecksGreen: true, Prior: held}},
		{name: "checks not green", obs: fwuObs(authorkind.Clanker, stageDetail{}), pass: PRPassFacts{Prior: held}},
		{name: "sentinel alert", obs: fwuObs(authorkind.Clanker, stageDetail{}), pass: PRPassFacts{HeadChecksGreen: true, SentinelAlert: true, Prior: held}},
		{name: "kind by registry", obs: fwuObs(authorkind.Clanker, stageDetail{}), pass: PRPassFacts{HeadChecksGreen: true, AuthorKindByRegistry: true, Prior: held}},
		{name: "hold label", obs: fwuObs(authorkind.Clanker, stageDetail{}, "hold"), pass: PRPassFacts{HeadChecksGreen: true, Prior: held}},
		{name: "person", obs: fwuObs(authorkind.Human, stageDetail{}), pass: PRPassFacts{HeadChecksGreen: true, Prior: held}},
		{name: "no prior record", obs: fwuObs(authorkind.Clanker, stageDetail{}), pass: PRPassFacts{HeadChecksGreen: true}},
		{name: "prior at 085", obs: fwuObs(authorkind.Clanker, stageDetail{}), pass: PRPassFacts{HeadChecksGreen: true, Prior: &PriorStage{Stage: stagelabels.NeedsDecision, Reason: autoMergeReasonForkWorkflowUpdate, AuthorUpdateOwedHead: goldenHead}}},
		{name: "prior 072 for another reason", obs: fwuObs(authorkind.Clanker, stageDetail{}), pass: PRPassFacts{HeadChecksGreen: true, Prior: &PriorStage{Stage: stagelabels.ReturnedToContributor, Reason: "conflicting", AuthorUpdateOwedHead: goldenHead}}},
		{name: "prior 072 with no owed clock", obs: fwuObs(authorkind.Clanker, stageDetail{}), pass: PRPassFacts{HeadChecksGreen: true, Prior: &PriorStage{Stage: stagelabels.ReturnedToContributor, Reason: autoMergeReasonForkWorkflowUpdate}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.obs
			raw.AuthorKind = authorkind.Unknown
			verdict, _, _ := ResolveStage([]stageObservation{raw})
			pass := tc.pass
			got := SplitByAuthorKind(verdict, tc.obs, &pass)
			if tc.keep {
				if got.Stage != stagelabels.ReturnedToContributor || got.Reason != autoMergeReasonForkWorkflowUpdate || !AuthorUpdateOwed(got) {
					t.Fatalf("%+v, want 072 kept on %q", got, autoMergeReasonForkWorkflowUpdate)
				}
			} else if got != verdict {
				t.Fatalf("%+v, want the hand-off %+v", got, verdict)
			}
			// The engine's pass derivation agrees.
			f := FactsFromPRObservations([]stageObservation{raw})
			pass.AuthorKind = tc.obs.AuthorKind
			f.PR.Pass = &pass
			d := derivePR(f)
			if keep := d.Stage == stagelabels.ReturnedToContributor; keep != tc.keep || d.Reason != autoMergeReasonForkWorkflowUpdate {
				t.Fatalf("derived %+v, want kept=%v on %q", d, tc.keep, autoMergeReasonForkWorkflowUpdate)
			}
		})
	}
}

// A fork guard read for the head keeps the hand-off, and the human route
// names the guard (hivecommons/hive#19420), as it does today.
func TestForkWorkflowUpdateForkGuardKeepsHandOff(t *testing.T) {
	route := stagelabels.RouteHuman(stagelabels.Evidence{
		Head: goldenHead, Verdict: stagelabels.VerdictApproved, Cause: stagelabels.CauseDecision,
		Checks: stagelabels.CheckSummary{Total: 3, Green: 2, Red: []string{"Canary"}, Known: true}, ForkGuard: []string{"Canary"},
		AuthorKind: authorkind.Clanker,
	})
	if route.Stage != stagelabels.NeedsDecision || route.LaneReason != ForkGuardReason([]string{"Canary"}) {
		t.Fatalf("human route = %+v, want 085 %q", route, ForkGuardReason([]string{"Canary"}))
	}
}

// The 07x split SplitByAuthorKind took over is unchanged, and so is any
// 072 another path produces: a needs-rebase label on an agent's
// fork-workflow-update reads 072 with no reason of its own, so it owes no
// branch update and a lapse flag does nothing to it.
func TestSplitByAuthorKindKeeps07xSplit(t *testing.T) {
	fixing := ClassifyStage(stageObservation{Lane: stageLaneContributor, Reason: "conflicting", HeadSHA: goldenHead})
	if got := SplitByAuthorKind(fixing, stageObservation{Lane: stageLaneContributor, Reason: "conflicting", AuthorKind: authorkind.Clanker}, nil); got.Stage != stagelabels.ReturnedToContributor {
		t.Fatalf("conflicting clanker: %+v, want 072", got)
	}
	if got := SplitByAuthorKind(fixing, stageObservation{Lane: stageLaneContributor, Reason: "conflicting", AuthorKind: authorkind.Human}, nil); got.Stage != stagelabels.NeedsAuthor {
		t.Fatalf("conflicting human: %+v, want 071", got)
	}
	rebase := fwuObs(authorkind.Unknown, fwuApproved(), "Needs-Rebase")
	verdict, _, _ := ResolveStage([]stageObservation{rebase})
	rebase.AuthorKind = authorkind.Clanker
	got := SplitByAuthorKind(verdict, rebase, &PRPassFacts{HeadChecksGreen: true, AuthorUpdateLapsed: true})
	if got.Stage != stagelabels.ReturnedToContributor || got.Reason != "" || AuthorUpdateOwed(got) {
		t.Fatalf("needs-rebase: %+v, want 072 with no reason and no owed update", got)
	}
	f := FactsFromPRObservations([]stageObservation{fwuObs(authorkind.Unknown, fwuApproved(), "Needs-Rebase")})
	f.PR.Pass = &PRPassFacts{AuthorKind: authorkind.Clanker, HeadChecksGreen: true, AuthorUpdateLapsed: true}
	if d := derivePR(f); d.Stage != stagelabels.ReturnedToContributor || d.Reason != autoMergeReasonForkWorkflowUpdate {
		t.Fatalf("needs-rebase with a lapse flag: derived %+v, want 072 unchanged", d)
	}
}

// Through the engine's pass derivation: the Facts the pass submits land on
// 072 for an agent's approved, green head, and on the 085 hand-off for a
// person, a red head, a sentinel alert, a registry-only kind or a lapsed
// update.
func TestDerivePRForkWorkflowUpdateRoutesByAuthorKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind authorkind.Kind
		red  bool
		pass PRPassFacts
		want stagelabels.Stage
	}{
		{name: "agent, approved, green", kind: authorkind.Clanker, pass: PRPassFacts{HeadChecksGreen: true}, want: stagelabels.ReturnedToContributor},
		{name: "agent, checks not green", kind: authorkind.Clanker, want: stagelabels.NeedsDecision},
		{name: "agent, red check noted", kind: authorkind.Clanker, red: true, pass: PRPassFacts{HeadChecksGreen: true}, want: stagelabels.NeedsDecision},
		{name: "agent, sentinel alert", kind: authorkind.Clanker, pass: PRPassFacts{HeadChecksGreen: true, SentinelAlert: true}, want: stagelabels.NeedsDecision},
		{name: "agent, by registry only", kind: authorkind.Clanker, pass: PRPassFacts{HeadChecksGreen: true, AuthorKindByRegistry: true}, want: stagelabels.NeedsDecision},
		{name: "agent, update lapsed", kind: authorkind.Clanker, pass: PRPassFacts{HeadChecksGreen: true, AuthorUpdateLapsed: true}, want: stagelabels.NeedsDecision},
		{name: "person", kind: authorkind.Human, pass: PRPassFacts{HeadChecksGreen: true}, want: stagelabels.NeedsDecision},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lane := LaneFact{Lane: stageLaneTrusted, Reason: autoMergeReasonForkWorkflowUpdate}
			if tc.red {
				lane.RedChecks, lane.RedChecksHead = []string{"lint"}, goldenHead
			}
			pass := tc.pass
			pass.AuthorKind = tc.kind
			pr := &PRFacts{HeadSHA: goldenHead, Lanes: []LaneFact{lane}, Review: ReviewFactsFrom(fwuApproved(), goldenHead), Pass: &pass}
			d := derivePR(Facts{Ref: Ref{Owner: "o", Repo: "r", Number: 1294, Kind: stagelabels.PRItem}, Open: true, Author: AuthorFacts{Login: "relay-author"}, PR: pr})
			if d.Abstain || d.Stage != tc.want || d.Reason != autoMergeReasonForkWorkflowUpdate {
				t.Fatalf("derived %+v, want %s (%s)", d, tc.want, autoMergeReasonForkWorkflowUpdate)
			}
		})
	}
}

// Three of the new pass flags are not flap evidence: stageWriteEvidence is
// byte for byte the same for a PR with or without the sentinel label,
// whatever they say (writer.go suppressStageFlap compares it). The lapse
// flag is evidence, but only when set, so a PR that never lapses
// serialises exactly as before; a set flag changes it, so the move off 072
// is not read as the reversal of the write that started the clock.
func TestStageWriteEvidenceForkWorkflowUpdateFlags(t *testing.T) {
	facts := func(labels []string, pass PRPassFacts) Facts {
		pass.AuthorKind = authorkind.Clanker
		return Facts{
			Ref: Ref{Owner: "o", Repo: "r", Number: 1294, Kind: stagelabels.PRItem}, Open: true, Labels: labels,
			PR: &PRFacts{HeadSHA: goldenHead, Lanes: []LaneFact{{Lane: stageLaneTrusted, Reason: autoMergeReasonForkWorkflowUpdate}}, Review: ReviewFactsFrom(fwuApproved(), goldenHead), Pass: &pass},
		}
	}
	want := stageWriteEvidence(facts(nil, PRPassFacts{}))
	for _, name := range []string{"SentinelAlert", "AuthorKindByRegistry", "HeadChecksGreen", "AuthorUpdateLapsed"} {
		if strings.Contains(want, name) {
			t.Fatalf("evidence names %s with the flag unset:\n%s", name, want)
		}
	}
	for name, f := range map[string]Facts{
		"sentinel label":        facts([]string{"sentinel-alert"}, PRPassFacts{SentinelAlert: true}),
		"registry kind":         facts(nil, PRPassFacts{AuthorKindByRegistry: true}),
		"green head":            facts(nil, PRPassFacts{HeadChecksGreen: true}),
		"all three with labels": facts([]string{"sentinel-alert"}, PRPassFacts{SentinelAlert: true, AuthorKindByRegistry: true, HeadChecksGreen: true}),
	} {
		if got := stageWriteEvidence(f); got != want {
			t.Fatalf("%s: evidence changed:\n got  %s\n want %s", name, got, want)
		}
	}
	if got := stageWriteEvidence(facts(nil, PRPassFacts{AuthorUpdateLapsed: true})); got == want {
		t.Fatal("a lapsed update must change the flap evidence")
	}
}
