package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// #6681: an ISSUES_AND_PRS agent's token is minted at the `contributor` tier,
// which does not carry the Workflows permission, so a push touching
// .github/workflows/** is rejected server-side. Nothing said so, and a
// hold-gated sec-check agent silently degraded to issue-only on a workflow
// finding. These tests pin that the ceiling is stated, that it reaches a
// CUSTOMIZED template (the seam the held-PR preflight uses for the same
// reason), and that it is stated to exactly the mode that has it.

func TestWorkflowPushCeilingReachesCustomizedKick(t *testing.T) {
	s := heldPRCoordinationScheduler(t, 4, "ISSUES_AND_PRS")
	msg := s.BuildAgentMessage("quality", nil, &github.ActionableResult{})
	for _, want := range []string{
		"[agent:quality]",
		"Workflow files are out of reach at this mode — preflight",
		".github/workflows/**",
		"contributor",
		"Do not open a PR for it",
		"needs a human or an\n   ISSUES_PRS_MERGE agent to land",
		"CUSTOM POLICY BODY",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("kick missing %q:\n%s", want, msg)
		}
	}
	if strings.Index(msg, "Workflow files are out of reach") > strings.Index(msg, "CUSTOM POLICY BODY") {
		t.Fatalf("the ceiling must precede policy work selection:\n%s", msg)
	}
}

func TestWorkflowPushCeilingOnlyForIssuesAndPRs(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		want bool
	}{
		{name: "issues and prs", mode: "ISSUES_AND_PRS", want: true},
		// Merge-capable agents mint at the trusted tier, which DOES request
		// workflows:write — telling them they cannot push a workflow would be
		// false, and would stop the one agent that can do the work.
		{name: "merge capable", mode: "ISSUES_PRS_MERGE", want: false},
		{name: "issues only", mode: "ISSUES_ONLY", want: false},
		{name: "advisory", mode: "ADVISORY", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := heldPRCoordinationScheduler(t, 4, tc.mode)
			msg := s.BuildAgentMessage("quality", nil, &github.ActionableResult{})
			got := strings.Contains(msg, "Workflow files are out of reach")
			if got != tc.want {
				t.Fatalf("mode %s: ceiling stated = %v, want %v:\n%s", tc.mode, got, tc.want, msg)
			}
		})
	}
}

// The ceiling is a property of the MODE, not of the ACMM level: an
// ISSUES_AND_PRS agent mints at the contributor tier at every level, so unlike
// the held-PR preflight this section must not be level-gated.
func TestWorkflowPushCeilingIsNotLevelGated(t *testing.T) {
	for _, level := range []int{2, 3, 4, 5, 6} {
		s := heldPRCoordinationScheduler(t, level, "ISSUES_AND_PRS")
		msg := s.BuildAgentMessage("quality", nil, &github.ActionableResult{})
		if !strings.Contains(msg, "Workflow files are out of reach") {
			t.Errorf("ACMM level %d: ceiling missing from an ISSUES_AND_PRS kick:\n%s", level, msg)
		}
	}
}

// An unknown agent has no configured mode; say nothing rather than guess.
func TestWorkflowPushCeilingSilentForUnknownAgent(t *testing.T) {
	s := heldPRCoordinationScheduler(t, 4, "ISSUES_AND_PRS")
	msg := s.BuildAgentMessage("not-configured", nil, &github.ActionableResult{})
	if strings.Contains(msg, "Workflow files are out of reach") {
		t.Fatalf("ceiling stated for an agent with no configured mode:\n%s", msg)
	}
}
