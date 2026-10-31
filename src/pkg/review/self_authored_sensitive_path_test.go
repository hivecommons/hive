package review

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/outputschema"
)

// projectbluefin/bonedigger#106: the hive's own PR pinned the tools its
// checks install, but the pin itself lives in a workflow file. The review
// asked a maintainer to commit that hunk; under the resolve policy the ask
// was turned into changes_requested because its wording named no risk
// class, and the PR sat at 070 while the fixer could not land it.
func bonedigger106Reports(workflowFile string) []PerspectiveReport {
	intent := baseReport(PerspectiveIntentAlignment, VerdictRequiresHuman, outputschema.Finding{
		Title:       "pull-request check still installs the lint tool unpinned",
		Severity:    outputschema.SeverityMedium,
		ReviewScope: "in-scope",
		File:        workflowFile,
		Line:        28,
		Summary:     "update the pipx step to install from .github/requirements-ci.txt and remove the BASELINE entry",
	})
	intent.HiveAuthored = true
	intent.DecisionRequested = true
	intent.DecisionQuestion = "apply the `pr-validate.yml` + `BASELINE` hunks from the PR body on this branch before merging"
	correctness := baseReport(PerspectiveCorrectness, VerdictChangesRequested, outputschema.Finding{
		Title:       "pipx_errors treats a flag value as the package spec",
		Severity:    outputschema.SeverityLow,
		ReviewScope: "in-scope",
		File:        "tests/test_python_install_pins.py",
		Line:        143,
	})
	correctness.HiveAuthored = true
	return []PerspectiveReport{intent, correctness}
}

func TestSelfAuthoredWorkflowAskStaysAPersonsDecision(t *testing.T) {
	reports := bonedigger106Reports(".github/workflows/pr-validate.yml")
	got := AggregateReports(reports, AggregateOptions{})
	if got.Verdict != VerdictRequiresHuman || !got.RequiresHuman || got.FixCycle || got.MergeEligible {
		t.Fatalf("aggregate = %+v, want a requires_human hold, not the fixer", got)
	}
	if !got.DecisionRequested || got.DecisionQuestion != reports[0].DecisionQuestion {
		t.Fatalf("decision = %v %q, want the reviewer's ask kept verbatim", got.DecisionRequested, got.DecisionQuestion)
	}
	if !strings.Contains(got.DecisionFinding, "sensitive path .github/workflows/pr-validate.yml") {
		t.Fatalf("DecisionFinding = %q, want it to name the workflow file", got.DecisionFinding)
	}
	if n := got.Normalize(); n.Verdict != VerdictRequiresHuman || n.FixCycle || !n.DecisionRequested {
		t.Fatalf("Normalize = %+v, want the hold kept on re-read", n)
	}
}

// Without a decision marker the reviewer's requires_human alone is the ask.
// The hold carries the finding, and Normalize (run on every artifact read)
// must not hand it to the fixer as concrete remediation.
func TestSelfAuthoredBareRequiresHumanOnWorkflowStaysAPersonsDecision(t *testing.T) {
	r := bonedigger106Reports(".github/workflows/pr-validate.yml")[0]
	r.DecisionRequested = false
	r.DecisionQuestion = ""
	r.Summary = "Apply the pr-validate.yml hunk from the PR body; only a maintainer can commit that edit."
	got := AggregateReports([]PerspectiveReport{r}, AggregateOptions{})
	if got.Verdict != VerdictRequiresHuman || !got.RequiresHuman || got.FixCycle {
		t.Fatalf("aggregate = %+v, want a requires_human hold", got)
	}
	if !strings.Contains(got.DecisionFinding, "sensitive path .github/workflows/pr-validate.yml") {
		t.Fatalf("DecisionFinding = %q, want it to name the workflow file", got.DecisionFinding)
	}
	if n := got.Normalize(); n.Verdict != VerdictRequiresHuman || n.FixCycle {
		t.Fatalf("Normalize = %+v, want the hold kept on re-read", n)
	}
	// The same aggregate without the recorded finding is what Normalize
	// still routes to the fixer, so the guard is the finding, not the shape.
	legacy := got
	legacy.DecisionFinding = ""
	if n := legacy.Normalize(); n.Verdict != VerdictChangesRequested {
		t.Fatalf("legacy Normalize = %+v, want the existing concrete-remediation route", n)
	}
}

// Everything the hive can fix keeps its 070 route.
func TestSelfAuthoredAsksOffSensitivePathsStillGoToTheFixer(t *testing.T) {
	t.Run("same ask on an ordinary file", func(t *testing.T) {
		reports := bonedigger106Reports("tests/fixtures/pr-validate.yml")
		got := AggregateReports(reports, AggregateOptions{})
		if got.Verdict != VerdictChangesRequested || !got.FixCycle || got.RequiresHuman || got.DecisionRequested {
			t.Fatalf("aggregate = %+v, want changes_requested for the fixer", got)
		}
	})
	t.Run("changes requested on a workflow file is the fixer's to try", func(t *testing.T) {
		r := baseReport(PerspectiveCorrectness, VerdictChangesRequested, outputschema.Finding{
			Title: "job name typo", Severity: outputschema.SeverityLow, ReviewScope: "in-scope", File: ".github/workflows/ci.yml",
		})
		r.HiveAuthored = true
		got := AggregateReports([]PerspectiveReport{r}, AggregateOptions{})
		if got.Verdict != VerdictChangesRequested || !got.FixCycle || got.DecisionFinding != "" {
			t.Fatalf("aggregate = %+v, want changes_requested for the fixer", got)
		}
	})
	t.Run("decision on an ordinary file beside changes requested on a workflow file", func(t *testing.T) {
		// Only what a person is asked about can make it a person's; the
		// workflow finding is a changes_requested one, so the fixer tries.
		reports := bonedigger106Reports("tests/fixtures/pr-validate.yml")
		reports[1].Findings[0].File = ".github/workflows/ci.yml"
		got := AggregateReports(reports, AggregateOptions{})
		if got.Verdict != VerdictChangesRequested || !got.FixCycle || got.DecisionRequested {
			t.Fatalf("aggregate = %+v, want changes_requested for the fixer", got)
		}
	})
	t.Run("out-of-scope finding on a workflow file", func(t *testing.T) {
		reports := bonedigger106Reports(".github/workflows/pr-validate.yml")
		reports[0].Findings[0].ReviewScope = "out-of-scope"
		got := AggregateReports(reports, AggregateOptions{})
		if got.Verdict != VerdictChangesRequested || !got.FixCycle {
			t.Fatalf("aggregate = %+v, want changes_requested for the fixer", got)
		}
	})
	t.Run("permissions-tightening carve-out", func(t *testing.T) {
		reports := bonedigger106Reports(".github/workflows/pr-validate.yml")
		for i := range reports {
			reports[i].PermissionsTighteningOnly = true
		}
		got := AggregateReports(reports, AggregateOptions{})
		if got.Verdict != VerdictChangesRequested || !got.FixCycle {
			t.Fatalf("aggregate = %+v, want the tightening-only workflow diff to stay the hive's call", got)
		}
	})
}

func TestSelfAuthoredOperatorSensitivePathAskStaysAPersonsDecision(t *testing.T) {
	reports := bonedigger106Reports("internal/proxy/allow.go")
	if got := AggregateReports(reports, AggregateOptions{}); got.Verdict != VerdictChangesRequested {
		t.Fatalf("aggregate = %+v, want an unlisted path to go to the fixer", got)
	}
	got := AggregateReports(reports, AggregateOptions{SensitivePaths: []string{"internal/proxy/**"}})
	if got.Verdict != VerdictRequiresHuman || !strings.Contains(got.DecisionFinding, "sensitive path internal/proxy/allow.go") {
		t.Fatalf("aggregate = %+v, want security.sensitive_paths honoured", got)
	}
}

// The escalate policy already keeps every question; the new rule adds
// nothing there, including no finding text in the 085 comment.
func TestSelfAuthoredWorkflowAskUnderEscalatePolicyUnchanged(t *testing.T) {
	got := AggregateReports(bonedigger106Reports(".github/workflows/pr-validate.yml"), AggregateOptions{SelfAuthoredDecisionPolicy: "escalate"})
	if got.Verdict != VerdictRequiresHuman || !got.DecisionRequested {
		t.Fatalf("aggregate = %+v, want the escalate hold", got)
	}
	r := bonedigger106Reports(".github/workflows/pr-validate.yml")[0]
	r.DecisionRequested, r.DecisionQuestion = false, ""
	got = AggregateReports([]PerspectiveReport{r}, AggregateOptions{SelfAuthoredDecisionPolicy: "escalate"})
	if got.Verdict != VerdictRequiresHuman || got.DecisionFinding != "" {
		t.Fatalf("aggregate = %+v, want the escalate hold with no added finding", got)
	}
}
