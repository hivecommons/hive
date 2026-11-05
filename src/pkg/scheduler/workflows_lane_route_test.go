package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// workflowsFleetScheduler is the live fleet shape: ci-maintainer and quality
// at ISSUES_AND_PRS (contributor-tier token, no Workflows permission), scanner
// at ISSUES_PRS_MERGE (trusted tier).
func workflowsFleetScheduler(t *testing.T, scannerMode string) *Scheduler {
	t.Helper()
	s := heldPRCoordinationScheduler(t, 6, "ISSUES_AND_PRS")
	s.cfg.Agents["quality"] = func() config.AgentConfig {
		a := s.cfg.Agents["quality"]
		a.Enabled = true
		return a
	}()
	s.cfg.Agents["ci-maintainer"] = config.AgentConfig{Role: "ci-maintainer", Mode: "ISSUES_AND_PRS", Enabled: true, KickTemplate: "custom-quality.md"}
	s.cfg.Agents["scanner"] = config.AgentConfig{Role: "scanner", Mode: scannerMode, Enabled: true}
	return s
}

// Danathar/exult#374: the issue's `[ci-maintainer]` title and
// agent/ci-maintainer label route it back to ci-maintainer, which cannot push
// the workflow change. The relay's routing label sends it to scanner instead.
func TestRouteWorkflowsLaneSendsLabelledIssueToCapableLane(t *testing.T) {
	s := workflowsFleetScheduler(t, "ISSUES_PRS_MERGE")
	issues := []github.Issue{
		{Repo: "acme/widget", Number: 374, Title: "[ci-maintainer] Weekly check fails", Lane: "ci-maintainer",
			Labels: []string{"approved-direction", "agent/ci-maintainer", "Hive/Workflows-Lane"}},
		{Repo: "acme/widget", Number: 375, Title: "[ci-maintainer] Flaky test", Lane: "ci-maintainer",
			Labels: []string{"agent/ci-maintainer"}},
	}
	got := s.routeWorkflowsLane(issues)
	if got[0].Lane != "scanner" {
		t.Fatalf("#374 lane = %q, want scanner", got[0].Lane)
	}
	if got[1].Lane != "ci-maintainer" {
		t.Fatalf("#375 lane = %q, an unlabelled issue keeps its lane", got[1].Lane)
	}
	if refs := strings.Join(issueRefsForAgent("ci-maintainer", got, 0), " "); strings.Contains(refs, "374") || !strings.Contains(refs, "375") {
		t.Fatalf("ci-maintainer refs = %q, want #375 only", refs)
	}
}

// With no capable lane the label routes nowhere: the issue keeps its lane.
func TestRouteWorkflowsLaneNoCapableLane(t *testing.T) {
	s := workflowsFleetScheduler(t, "ISSUES_AND_PRS")
	got := s.routeWorkflowsLane([]github.Issue{{Number: 374, Lane: "ci-maintainer", Labels: []string{github.WorkflowsLaneLabel}}})
	if got[0].Lane != "ci-maintainer" {
		t.Fatalf("lane = %q, want ci-maintainer kept", got[0].Lane)
	}
}

// A capable lane scoped to other repositories is refused on this one, so the
// label routes each issue to a lane that serves its repo, or nowhere
// (adjudicator on hivecommons/hive#16692).
func TestRouteWorkflowsLaneHonoursRepoScope(t *testing.T) {
	s := workflowsFleetScheduler(t, "ISSUES_PRS_MERGE")
	s.cfg.Agents["scanner"] = config.AgentConfig{Role: "scanner", Mode: "ISSUES_PRS_MERGE", Enabled: true, Repos: []string{"acme/widget"}}
	got := s.routeWorkflowsLane([]github.Issue{
		{Repo: "acme/widget", Number: 374, Lane: "ci-maintainer", Labels: []string{github.WorkflowsLaneLabel}},
		{Repo: "acme/other", Number: 9, Lane: "ci-maintainer", Labels: []string{github.WorkflowsLaneLabel}},
	})
	if got[0].Lane != "scanner" || got[1].Lane != "ci-maintainer" {
		t.Fatalf("lanes = %q, %q; want scanner in its repo and ci-maintainer kept elsewhere", got[0].Lane, got[1].Lane)
	}
}

// End to end through the kick build: the labelled issue leaves
// ci-maintainer's kick, the unlabelled one stays.
func TestKickRoutesWorkflowsLaneIssueAwayFromIncapableLane(t *testing.T) {
	s := workflowsFleetScheduler(t, "ISSUES_PRS_MERGE")
	actionable := &github.ActionableResult{}
	actionable.Issues.Items = []github.Issue{
		{Repo: "acme/widget", Number: 374, Title: "[ci-maintainer] Weekly audit check fails every run",
			Labels: []string{"approved-direction", "agent/ci-maintainer", github.WorkflowsLaneLabel}},
		{Repo: "acme/widget", Number: 375, Title: "[ci-maintainer] Flaky cache step",
			Labels: []string{"agent/ci-maintainer"}},
	}
	messages, _ := s.BuildKickMessagesWithSkips(actionable, []string{"ci-maintainer"})
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want one ci-maintainer kick", len(messages))
	}
	msg := messages[0].Message
	if strings.Contains(msg, "#374") || !strings.Contains(msg, "#375") {
		t.Fatalf("ci-maintainer kick must carry #375 and not #374:\n%s", msg)
	}
}

// The ceiling tells an ISSUES_AND_PRS lane where its workflow-only park goes,
// and only when such a lane exists.
func TestWorkflowPushCeilingNamesCapableLane(t *testing.T) {
	msg := workflowsFleetScheduler(t, "ISSUES_PRS_MERGE").BuildAgentMessage("quality", nil, &github.ActionableResult{})
	for _, want := range []string{"Workflow files are out of reach", "for example `scanner`", "`" + github.WorkflowsLaneLabel + "`", "not as a\nstep for the owner", "Where no such lane\nserves the repository, the park stands"} {
		if !strings.Contains(msg, want) {
			t.Errorf("kick lacks %q:\n%s", want, msg)
		}
	}
	msg = workflowsFleetScheduler(t, "ISSUES_AND_PRS").BuildAgentMessage("quality", nil, &github.ActionableResult{})
	if !strings.Contains(msg, "Workflow files are out of reach") || strings.Contains(msg, "This hive runs") {
		t.Fatalf("with no capable lane the ceiling must stand alone:\n%s", msg)
	}
}
