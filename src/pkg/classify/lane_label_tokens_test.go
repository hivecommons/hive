package classify

import (
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// #5856: lane routing matched keywords against the space-joined label list
// with strings.Contains. Scanner's L4 keyword `fix` is a substring of
// `ai-fix-requested` — the label the dashboard's ACMM evaluator stamps on every
// maturity-gap issue it files — so every ACMM gap issue routed to scanner,
// which is ISSUES_ONLY at L4 and cannot open a pull request. A live 4-repo L4
// hive had 11 of 11 ACMM issues parked there, untouched, with nothing logging
// an error.

// level4Lanes is the lane table a real ACMM L4 hive runs, from
// src/pkg/config/packs/level-4.yaml.
//
// Only three of the seven L4 agents declare lane_keywords, which is itself
// worth knowing: `quality` is ISSUES_AND_PRS at L4 and declares none, so it is
// absent from the lane table entirely and can never be routed anything.
//
// Returned in NAME ORDER, which is what initAgentConfigDrivenSystems now
// guarantees (cmd/hive/main.go sorts before SetLanes). SetLanes itself does not
// sort — it honours the order it is given — so a test that wants the production
// order has to supply it. That the order is load-bearing is visible below: the
// "Add AI fix workflow" title matches ci-maintainer's `workflow` AND scanner's
// `fix`, and first-match-wins decides between them.
func level4Lanes() []LaneConfig {
	return []LaneConfig{
		{Name: "ci-maintainer", Keywords: []string{"ci", "build", "pipeline", "workflow"}},
		{Name: "scanner", Keywords: []string{"bug", "triage", "fix", "issue"}},
		{Name: "sec-check", Keywords: []string{"security", "cve", "vulnerability", "audit"}},
	}
}

// acmmLabels are the labels api_acmm_eval.go hardcodes on every gap issue.
var acmmLabels = []string{"acmm", "ai-fix-requested"}

// TestACMMFixLabelNoLongerCapturesTheIssue is the regression test for the
// reported defect, in the reporter's own reproduction: hold the title and the
// lane table fixed and toggle ONLY the label.
//
// Before this fix the label changed the answer, and changed it to the one agent
// that cannot act. Without the label the issue routed to sec-check
// (ISSUES_AND_PRS at L4, and would fix it); with the label — the label whose
// entire purpose is to mark the issue as AI-fixable — it routed to scanner.
func TestACMMFixLabelNoLongerCapturesTheIssue(t *testing.T) {
	defer SetLanes(nil)
	SetLanes(level4Lanes())

	const title = "[ACMM L4] Add AI security policy"

	bare := Classify(github.Issue{Title: title}).Lane
	labeled := Classify(github.Issue{Title: title, Labels: acmmLabels}).Lane

	if labeled != bare {
		t.Errorf("the acmm/ai-fix-requested label changed the lane: bare=%q labeled=%q — routing must not turn on %q appearing inside %q",
			bare, labeled, "fix", "ai-fix-requested")
	}
	// Naming the expected lane, not merely "unchanged": an implementation that
	// broke label routing altogether would also report unchanged.
	if labeled != "sec-check" {
		t.Errorf("lane = %q, want sec-check — the title says 'security', and sec-check is the L4 agent that can open the PR", labeled)
	}
}

// TestLabelRoutingMatchesWholeLabelsAndNamespaceSegments pins the rule itself,
// clause by clause, so a regression names WHICH part of it broke.
//
// The two separator characters are treated differently on purpose. "/" is a
// GitHub label NAMESPACE separator (kind/bug, area/api), so its segments are
// the label's real subject and must still route — this package already relies
// on that convention elsewhere. "-" is part of a single label's own name:
// `ai-fix-requested` is one word meaning one thing, and splitting on "-" would
// leave the reported bug exactly where it was.
func TestLabelRoutingMatchesWholeLabelsAndNamespaceSegments(t *testing.T) {
	cases := []struct {
		label string
		token string
		want  bool
		why   string
	}{
		{"ai-fix-requested", "fix", false, "the reported defect: a hyphen-joined part of a label name is not the label's subject"},
		{"do-not-merge", "merge", false, "same shape, opposite meaning — this label is a request NOT to merge"},
		{"ai-fix-requested", "ai-fix-requested", true, "the whole label always matches"},
		{"kind/regression", "regression", true, "a namespace segment is the label's subject and must keep routing"},
		{"kind/regression", "kind", true, "the namespace itself is a segment too"},
		{"area/ci", "ci", true, "the convention this package already uses for kind/security"},
		{"quality-of-life", "quality", false, "a lane NAME must not be captured by a longer unrelated label"},
		{"scanner", "scanner", true, "the operator's lane-name override"},
		{"", "fix", false, "an empty label matches nothing"},
		{"fix", "", false, "an empty token matches nothing"},
	}

	for _, tc := range cases {
		if got := labelMatchesRoutingToken(tc.label, tc.token); got != tc.want {
			t.Errorf("labelMatchesRoutingToken(%q, %q) = %v, want %v — %s", tc.label, tc.token, got, tc.want, tc.why)
		}
	}
}

// TestLaneKeywordCannotStraddleTwoLabels covers the second bug the joined
// string allowed: a multi-word keyword matching across two unrelated labels
// because the join put a space between them.
func TestLaneKeywordCannotStraddleTwoLabels(t *testing.T) {
	defer SetLanes(nil)
	SetLanes([]LaneConfig{
		{Name: "architect", Keywords: []string{"breaking change"}},
	})

	straddle := Classify(github.Issue{Title: "Bump a dependency", Labels: []string{"breaking", "change"}}).Lane
	if straddle == "architect" {
		t.Error("keyword 'breaking change' matched across two separate labels ('breaking' + 'change') — joining the labels into one string is what made that possible")
	}

	single := Classify(github.Issue{Title: "Bump a dependency", Labels: []string{"breaking change"}}).Lane
	if single != "architect" {
		t.Errorf("lane = %q, want architect — one label literally named 'breaking change' must still route", single)
	}
}

// TestLaneNameLabelOverrideStillWorks pins the operator workaround the issue
// documents. It is the escape hatch for a misrouted issue, so narrowing label
// matching must not take it away.
func TestLaneNameLabelOverrideStillWorks(t *testing.T) {
	defer SetLanes(nil)
	SetLanes(append(level4Lanes(), LaneConfig{Name: "quality", Keywords: []string{"test-gap"}}))

	got := Classify(github.Issue{
		Title:  "[ACMM L0] Add Coverage gate",
		Labels: []string{"acmm", "ai-fix-requested", "quality"},
	}).Lane
	if got != "quality" {
		t.Errorf("lane = %q, want quality — an exact lane-name label is the operator's override and must outrank keywords", got)
	}
}

// TestNamespacedLabelStillRoutes guards the behaviour narrowing could plausibly
// have broken: `kind/regression` routing on the `regression` keyword. Losing it
// would trade one silent misroute for another.
func TestNamespacedLabelStillRoutes(t *testing.T) {
	defer SetLanes(nil)
	SetLanes([]LaneConfig{
		{Name: "ci-maintainer", Keywords: []string{"regression", "coverage"}},
	})

	got := Classify(github.Issue{Title: "Nightly job is red", Labels: []string{"kind/regression"}}).Lane
	if got != "ci-maintainer" {
		t.Errorf("lane = %q, want ci-maintainer — a kind/ namespaced label must still route on its subject", got)
	}
}

// TestTitleKeepsSubstringMatching asserts the narrowing applies to LABELS only.
// Titles are prose, and "fix the login crash" should reach the fix lane.
func TestTitleKeepsSubstringMatching(t *testing.T) {
	defer SetLanes(nil)
	SetLanes(level4Lanes())

	if got := Classify(github.Issue{Title: "Please fix the login crash"}).Lane; got != "scanner" {
		t.Errorf("lane = %q, want scanner — a title containing 'fix' must still match on substring", got)
	}
}

// TestACMMLabelIsRoutingNeutral states the fix as an invariant over the exact
// issue set the report measured: the ACMM labels must not change where any of
// them goes.
//
// It also records, by assertion, what this fix does NOT achieve. Eleven of
// these thirteen titles still land on scanner — one on the title keyword
// `issue`, ten on DefaultLane, which is "scanner" rather than "". filterByLane
// (pkg/scheduler) admits an issue to an agent only when issue.Lane ==
// agentName || issue.Lane == "", so a fallback of "scanner" keeps an unmatched
// issue invisible to every other agent. The report calls that out as a separate
// matter and does not propose changing it, and changing it would make every
// unclassified issue in the fleet visible to every agent — a policy call, not a
// bug fix. Asserting the count here means that decision has measured data
// attached rather than an estimate, and that moving it is a deliberate act.
func TestACMMLabelIsRoutingNeutral(t *testing.T) {
	defer SetLanes(nil)
	SetLanes(level4Lanes())

	// The eleven observed live on hive-wild-mole, plus the two the report used
	// to demonstrate the collision.
	titles := []string{
		"[ACMM L2] Add Copilot instructions",
		"[ACMM L2] Add Cursor rules",
		"[ACMM L2] Add Prompts catalog",
		"[ACMM L2] Add EditorConfig",
		"[ACMM L2] Add Simple skills",
		"[ACMM L2] Add Correction capture",
		"[ACMM L0] Add E2E tests",
		"[ACMM L0] Add PR template",
		"[ACMM L0] Add Issue templates",
		"[ACMM L0] Add Code style config",
		"[ACMM L0] Add Coverage gate",
		"[ACMM L4] Add AI security policy",
		"[ACMM L4] Add AI fix workflow",
	}

	scanner := 0
	for _, title := range titles {
		bare := Classify(github.Issue{Title: title}).Lane
		labeled := Classify(github.Issue{Title: title, Labels: acmmLabels}).Lane
		if labeled != bare {
			t.Errorf("%q: the acmm labels changed the lane (bare=%q labeled=%q)", title, bare, labeled)
		}
		if labeled == "scanner" {
			scanner++
		}
	}

	// 11 of 13 still reach scanner — by the fallback now, no longer by the label.
	if scanner != 11 {
		t.Errorf("%d of %d titles reach scanner, want 11 — if this moved, either a lane keyword changed or the DefaultLane fallback did, and the follow-up decision described above needs re-measuring", scanner, len(titles))
	}
	// The two the report used must reach agents that can open a PR at L4.
	if got := Classify(github.Issue{Title: "[ACMM L4] Add AI security policy", Labels: acmmLabels}).Lane; got != "sec-check" {
		t.Errorf("security-policy gap routed to %q, want sec-check", got)
	}
	if got := Classify(github.Issue{Title: "[ACMM L4] Add AI fix workflow", Labels: acmmLabels}).Lane; got != "ci-maintainer" {
		t.Errorf("ai-fix-workflow gap routed to %q, want ci-maintainer — its title matches ci-maintainer's 'workflow' AND scanner's 'fix', so this answer exists only because the lane table has a deterministic order", got)
	}
}
