package review

import (
	"strings"
	"testing"
)

// hiveSupersederSecurityReport is a security-sensitive requires_human on a
// PR the hive created to supersede another PR.
func hiveSupersederSecurityReport() PerspectiveReport {
	r := baseReport(PerspectiveSecurity, VerdictRequiresHuman)
	r.HiveAuthored = true
	r.HiveSuperseding = true
	r.DecisionRequested = true
	r.DecisionQuestion = "Should we accept this public-facing auth behavior change?"
	return r
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
