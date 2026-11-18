package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestDerivePRRecoveryPermissionBlockIsNotSourceFixing(t *testing.T) {
	f := hiveFixingFacts("required-check-failing:gosec")
	f.PR.Pass = &PRPassFacts{
		AuthorIsApp: true, RecoveryUnauthorized: "rerun-shared-ci",
	}
	d := DerivePR(f)
	if d.Stage != stagelabels.NeedsDecision || d.Reason != "unauthorized:rerun-shared-ci" {
		t.Fatalf("permission block looks like source fixing: %+v", d)
	}
	f.PR.Pass.RecoveryUnauthorized = ""
	if d := DerivePR(f); d.Stage != stagelabels.Fixing {
		t.Fatalf("cleared recovery block must preserve actual CI failure: %+v", d)
	}
}

// A rerun refusal takes the stage only when CI is what holds the PR. Over a
// changes request, a decision the reviewer asked for, or another 085 it hid
// the real blocker (projectbluefin/utah#1313, utah-packages#1013).
func TestRecoveryUnauthorizedOwnsStage(t *testing.T) {
	changes := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}}
	decision := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true}}
	for _, tc := range []struct {
		name    string
		verdict stageVerdict
		reason  string
		detail  stageDetail
		want    bool
	}{
		{name: "red required check", verdict: stageVerdict{Stage: stagelabels.Fixing}, reason: "required-check-failing:gosec", want: true},
		{name: "deferred to a shared-CI incident", verdict: stageVerdict{Stage: stagelabels.ReviewPending}, reason: "shared-ci-incident:1336", want: true},
		{name: "red optional check", verdict: stageVerdict{Stage: stagelabels.Fixing}, reason: "optional-check-failing:build", want: true},
		{name: "changes requested", verdict: stageVerdict{Stage: stagelabels.Fixing}, reason: contributorLaneReasonChangesRequested},
		{name: "red check under a changes request", verdict: stageVerdict{Stage: stagelabels.Fixing}, reason: "required-check-failing:gosec", detail: changes},
		{name: "decision requested", verdict: stageVerdict{Stage: stagelabels.Reviewed}, reason: "required-check-failing:gosec", detail: decision},
		{name: "already at 085", verdict: stageVerdict{Stage: stagelabels.NeedsDecision}, reason: "fork-guard:Canary"},
		{name: "no stage", verdict: stageVerdict{Stage: stagelabels.None}, reason: "required-check-failing:gosec"},
		{name: "terminal", verdict: stageVerdict{Terminal: true}, reason: "required-check-failing:gosec"},
		{name: "not a CI reason", verdict: stageVerdict{Stage: stagelabels.ReviewPending}, reason: contributorLaneReasonNoVerdict},
	} {
		if got := RecoveryUnauthorizedOwnsStage(tc.verdict, tc.reason, tc.detail); got != tc.want {
			t.Errorf("%s: owns stage = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDerivePRRecoveryPermissionBlockKeepsAChangesRequest(t *testing.T) {
	f := hiveFixingFacts(contributorLaneReasonChangesRequested)
	f.PR.Pass = &PRPassFacts{AuthorIsApp: true, RecoveryUnauthorized: "rerun-shared-ci"}
	d := DerivePR(f)
	if d.Stage != stagelabels.Fixing || d.Reason == "unauthorized:rerun-shared-ci" {
		t.Fatalf("a rerun refusal hid the changes request: %+v", d)
	}
}
