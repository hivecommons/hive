package review

import (
	"fmt"
	"strings"
)

// plan_match judges intent, not just diff (hivecommons/hive#8317).
//
// Every other perspective reads the change and the tree around it. This one
// also reads the plan the change claims to implement -- the approved wave the
// PR's Hive-Run / Hive-Plan trailers point at -- and asks two questions the
// diff alone cannot answer: did the PR do things the plan never asked for, and
// did it leave planned things undone. Both are the failure modes of a
// long-running run drifting from what a human approved, and both are invisible
// to a reviewer who never saw the plan.
//
// The plan text is the PLANNER's artifact, not the PR author's. The measured
// worst-performing reviewer arm was the one shown the author's own rationale
// (see groundingSection); an approved plan is the opposite kind of input, a
// reference the author was held to rather than an argument for the change.

// MaxPlanWaveLen bounds the plan text rendered into the kick. A plan wave is a
// list of tasks, not a document; a bound this generous only ever truncates a
// pasted design, which would crowd out the read and publish instructions.
const MaxPlanWaveLen = 6000

// PlanWaveTruncationNote is appended when the wave was cut at MaxPlanWaveLen,
// so the reviewer knows the list is partial rather than judging "missing"
// items that were simply not shown.
const PlanWaveTruncationNote = "\n[plan truncated: judge only the items shown]"

// PlanMatchApplicable reports whether plan_match has anything to judge on this
// PR: it needs at least one run trailer to name the plan. Without one the
// perspective returns a not-applicable report rather than guessing which plan
// an untagged PR might belong to.
func PlanMatchApplicable(pr PullRequest) bool {
	return strings.TrimSpace(pr.RunKey) != "" || strings.TrimSpace(pr.PlanRef) != ""
}

// planMatchSection is the plan_match half of the kick: the plan to judge
// against and the two findings this perspective exists to report.
func planMatchSection(pr PullRequest) string {
	var b strings.Builder
	b.WriteString("\nPLAN MATCH — judge the diff against the approved plan.\n")
	if !PlanMatchApplicable(pr) {
		b.WriteString("This PR carries no Hive-Run or Hive-Plan trailer, so there is no plan to compare it against. For the plan_match perspective return verdict approve, set \"not_applicable\": true, use [] for findings, and say \"not applicable: no run trailer\" in summary. Do not guess which plan it might belong to.\n")
		return b.String()
	}
	if pr.RunKey != "" {
		fmt.Fprintf(&b, "Hive-Run: %s\n", pr.RunKey)
	}
	if pr.PlanRef != "" {
		fmt.Fprintf(&b, "Hive-Plan: %s\n", pr.PlanRef)
	}
	wave := strings.TrimSpace(pr.PlanWave)
	if wave == "" {
		b.WriteString("The plan named by those trailers could not be loaded. For the plan_match perspective return verdict requires_human and say the plan was not found; do not infer one from the diff.\n")
		return b.String()
	}
	if len(wave) > MaxPlanWaveLen {
		wave = wave[:MaxPlanWaveLen] + PlanWaveTruncationNote
	}
	b.WriteString("The approved plan wave this PR claims to implement:\n")
	b.WriteString(wave)
	b.WriteString("\n\nCompare the diff summary (the files changed and what each change does) against that list, and report exactly two kinds of finding, each with the file:line evidence the grounding section requires:\n")
	b.WriteString("- \"scope exceeds plan\": a file, behaviour, or surface the diff adds or changes that no planned item covers. Use severity high when the unplanned change alters behaviour a user or operator can see, or touches a file outside every planned item's area; medium for supporting changes (tests, docs, helpers) the plan implies but does not name; low for incidental cleanup.\n")
	b.WriteString("- \"planned item missing\": an item in the wave this PR should have delivered and does not. Name the item as the plan does. A plan item with status done, or one this PR's title or trailer scopes out, is not missing.\n")
	b.WriteString("A diff that matches its plan gets verdict approve with no findings. Any high-severity scope finding is a human decision: the plan was approved and the PR left it, so use verdict requires_human.\n")
	return b.String()
}
