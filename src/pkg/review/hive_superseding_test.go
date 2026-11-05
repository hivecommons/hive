package review

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/outputschema"
)

// hiveSupersederSecurityReport is a security-sensitive requires_human on a
// PR the hive created to supersede another PR, with a finding the fixer
// can act on.
func hiveSupersederSecurityReport() PerspectiveReport {
	r := baseReport(PerspectiveSecurity, VerdictRequiresHuman, outputschema.Finding{
		Title: "token logged at debug level", Severity: outputschema.SeverityMedium, File: "auth.go", Line: 12,
	})
	r.HiveAuthored = true
	r.HiveSuperseding = true
	r.DecisionRequested = true
	r.DecisionQuestion = "Should we accept this public-facing auth behavior change?"
	return r
}

// hiveSupersederSignOffReport is the hivecommons/hive#16607 shape: every
// perspective requires_human, each only asking an approver to confirm a
// trust-boundary change, with at most an informational finding.
func hiveSupersederSignOffReport(p Perspective) PerspectiveReport {
	r := baseReport(p, VerdictRequiresHuman)
	r.HiveAuthored = true
	r.HiveSuperseding = true
	r.DecisionRequested = true
	r.DecisionRiskHigh = true
	r.DecisionQuestion = "@hanthor should confirm that the relay may now App-merge trusted-author pin-only PRs."
	if p == PerspectiveSecurity {
		r.Findings = []outputschema.Finding{{
			Title: "relay now lifts the human-merge-path hold", Severity: outputschema.SeverityInfo,
			Summary: "Needs approver sign-off, not a code fix.", File: "src/pkg/github/human_merge_paths.go", Line: 269,
		}}
	}
	return r
}

// TestAggregateHiveSupersedingSignOffOnlyParksForDecision: when every
// perspective's requires_human is a pure approver question with nothing
// above an informational finding, the superseder rule has no fix to route.
// The aggregate keeps the decision request so the stage engine parks the PR
// at 085 needs-decision instead of kicking a fixer that has nothing to fix
// until the cycle is exhausted and the PR is re-authored (#16398 → #16456
// → #16607).
func TestAggregateHiveSupersedingSignOffOnlyParksForDecision(t *testing.T) {
	reports := []PerspectiveReport{
		hiveSupersederSignOffReport(PerspectiveCorrectness),
		hiveSupersederSignOffReport(PerspectiveSecurity),
		hiveSupersederSignOffReport(PerspectiveStyle),
	}
	got := AggregateReports(reports, AggregateOptions{})

	if got.Verdict != VerdictRequiresHuman || !got.RequiresHuman || got.FixCycle || got.MergeEligible {
		t.Fatalf("aggregate = %+v, want requires_human park", got)
	}
	if !got.DecisionRequested || got.DecisionQuestion == "" {
		t.Fatalf("aggregate = %+v, want the decision question preserved", got)
	}
	if hasReason(got.Reasons, HiveSupersedingFixRouteReason) {
		t.Fatalf("reasons = %v, must not carry the fix-route reason", got.Reasons)
	}
	for p, v := range got.Perspectives {
		if v != VerdictRequiresHuman {
			t.Fatalf("perspective %s = %s, want requires_human kept", p, v)
		}
	}
	if got.Normalize().Verdict != VerdictRequiresHuman {
		t.Fatalf("Normalize() = %+v, must not re-route a decision request to the fixer", got.Normalize())
	}
}

// TestAggregateHiveSupersedingSignOffWithActionableFindingRoutesToFixer:
// a sibling perspective that found code to change keeps the owner's fixer
// route; the sign-off park is only for a swarm with nothing to fix.
func TestAggregateHiveSupersedingSignOffWithActionableFindingRoutesToFixer(t *testing.T) {
	for name, extra := range map[string]PerspectiveReport{
		"changes_requested sibling": func() PerspectiveReport {
			r := baseReport(PerspectiveCorrectness, VerdictChangesRequested)
			r.HiveAuthored, r.HiveSuperseding = true, true
			return r
		}(),
		"requires_human with medium finding": hiveSupersederSecurityReport(),
	} {
		t.Run(name, func(t *testing.T) {
			got := AggregateReports([]PerspectiveReport{hiveSupersederSignOffReport(PerspectiveStyle), extra}, AggregateOptions{})
			if got.Verdict != VerdictChangesRequested || !got.FixCycle || got.DecisionRequested {
				t.Fatalf("aggregate = %+v, want changes_requested fixer route", got)
			}
		})
	}
}

// TestAggregateHiveSupersedingRiskRequiresHumanRoutesToFixer: the owner's
// rule — a security/risk requires_human on the hive's superseding PR is a
// fix request, not an 085 park.
func TestAggregateHiveSupersedingRiskRequiresHumanRoutesToFixer(t *testing.T) {
	got := AggregateReports([]PerspectiveReport{hiveSupersederSecurityReport()}, AggregateOptions{})

	if got.Verdict != VerdictChangesRequested || !got.FixCycle || got.RequiresHuman || got.DecisionRequested || got.DecisionQuestion != "" {
		t.Fatalf("aggregate = %+v, want changes_requested fixer route", got)
	}
	if !got.HiveSuperseding || got.Perspectives[PerspectiveSecurity] != VerdictChangesRequested {
		t.Fatalf("aggregate = %+v, want hive_superseding + normalized perspective", got)
	}
	if !hasReason(got.Reasons, HiveSupersedingFixRouteReason) {
		t.Fatalf("reasons = %v, want %q", got.Reasons, HiveSupersedingFixRouteReason)
	}
}

// TestAggregateHiveSupersedingPlainRequiresHumanRoutesToFixer: an
// unresolved (non-question) requires_human on the superseding PR goes the
// same way.
func TestAggregateHiveSupersedingPlainRequiresHumanRoutesToFixer(t *testing.T) {
	r := baseReport(PerspectiveSecurity, VerdictRequiresHuman)
	r.HiveAuthored = true
	r.HiveSuperseding = true
	r.Summary = "Could not read the diff for an authorization change."

	got := AggregateReports([]PerspectiveReport{r}, AggregateOptions{})

	if got.Verdict != VerdictChangesRequested || !got.FixCycle || got.RequiresHuman {
		t.Fatalf("aggregate = %+v, want changes_requested fixer route", got)
	}
}

// TestAggregateContributorRiskRequiresHumanStillParks: the same finding on
// a PR that is not the hive's superseding copy keeps the human hold.
func TestAggregateContributorRiskRequiresHumanStillParks(t *testing.T) {
	for name, mutate := range map[string]func(*PerspectiveReport){
		"contributor":              func(r *PerspectiveReport) { r.HiveAuthored = false; r.HiveSuperseding = false },
		"hive but not superseding": func(r *PerspectiveReport) { r.HiveSuperseding = false },
	} {
		t.Run(name, func(t *testing.T) {
			r := hiveSupersederSecurityReport()
			mutate(&r)
			got := AggregateReports([]PerspectiveReport{r}, AggregateOptions{})
			if got.Verdict != VerdictRequiresHuman || !got.RequiresHuman || got.FixCycle {
				t.Fatalf("aggregate = %+v, want requires_human", got)
			}
		})
	}
}

// TestAggregateHiveSupersedingKnobOffKeepsOldBehaviour:
// auto_merge.hive_superseding.auto_merge: false restores the park.
func TestAggregateHiveSupersedingKnobOffKeepsOldBehaviour(t *testing.T) {
	got := AggregateReports([]PerspectiveReport{hiveSupersederSecurityReport()}, AggregateOptions{HiveSupersedingAutoMergeOff: true})
	if got.Verdict != VerdictRequiresHuman || !got.RequiresHuman || !got.DecisionRequested || got.FixCycle {
		t.Fatalf("aggregate = %+v, want requires_human with knob off", got)
	}
	if hasReason(got.Reasons, HiveSupersedingFixRouteReason) {
		t.Fatalf("reasons = %v, must not carry the fix-route reason", got.Reasons)
	}
}

// TestAggregateHiveSupersedingFixCapExhaustedParks: once the fixer's
// attempts are spent the superseding PR does park — the emergent-problem
// exception — and says so.
func TestAggregateHiveSupersedingFixCapExhaustedParks(t *testing.T) {
	got := AggregateReports([]PerspectiveReport{hiveSupersederSecurityReport()}, AggregateOptions{FixAttempts: DefaultFixCycleCap})
	if got.Verdict != VerdictRequiresHuman || !got.RequiresHuman || got.FixCycle {
		t.Fatalf("aggregate = %+v, want requires_human at fix cap", got)
	}
	if len(got.Reasons) != 1 || !strings.HasPrefix(got.Reasons[0], "fix cycle cap reached") {
		t.Fatalf("reasons = %v, want fix cycle cap reason", got.Reasons)
	}
}

// TestAggregateHiveSupersedingRejectStaysReject: a reject is an emergent
// problem with the PR, never converted to a fix request.
func TestAggregateHiveSupersedingRejectStaysReject(t *testing.T) {
	reject := baseReport(PerspectiveCorrectness, VerdictReject)
	reject.HiveAuthored = true
	reject.HiveSuperseding = true
	got := AggregateReports([]PerspectiveReport{hiveSupersederSecurityReport(), reject}, AggregateOptions{})
	if got.Verdict == VerdictChangesRequested || got.FixCycle || hasReason(got.Reasons, HiveSupersedingFixRouteReason) {
		t.Fatalf("aggregate = %+v, must not become a fix request", got)
	}
}

func hasReason(reasons []string, want string) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}
