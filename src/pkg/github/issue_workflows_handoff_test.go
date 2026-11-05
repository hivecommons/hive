package github

import (
	"context"
	"strings"
	"sync"
	"testing"

	itemblocker "github.com/hivecommons/hive/pkg/blocker"
)

// The texts below are the live parks: an ISSUES_AND_PRS lane (contributor
// tier, no Workflows permission) wrote the exact workflow change and parked it
// for the owner, while scanner (ISSUES_PRS_MERGE) was landing workflow changes
// in the same hive.
const (
	// Danathar/exult#374, ci-maintainer after the owner's `/hive decision`.
	handoff374 = "**Why there is no PR:** the change is inside `.github/workflows/`; the hive's App token has no `workflows` permission, so GitHub rejects the push. Nothing outside that file needs to change for the check to pass.\n\nIn the \"Audit merged agent pull requests\" step of `.github/workflows/agent-audit.yml`, replace ..."
	// Danathar/arch-bootc#634, ci-maintainer filing the issue.
	handoff634 = "## Why no PR\n\nThe change is inside `.github/workflows/`. This agent's GitHub App token does not carry the Workflows permission, so GitHub rejects the push. A human or an ISSUES_PRS_MERGE agent needs to apply it."
	// Danathar/zfs-aurora-complex#266, quality filing the issue.
	handoff266 = "Risk: credentials — the change is in `.github/workflows/`, which the Hive app's contributor-tier token cannot push (GitHub refuses workflow edits without the `workflows` permission). It needs a human or an ISSUES_PRS_MERGE agent to land."
)

func TestParkIsWorkflowsOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{name: "exult#374 (blocker.Infer)", text: handoff374, want: true},
		{name: "arch-bootc#634 (\"does not carry the Workflows permission\")", text: handoff634, want: true},
		{name: "zfs-aurora-complex#266", text: handoff266, want: true},
		{name: "structured marker", text: "Apply the replacement.\n" + itemblocker.Marker(itemblocker.CapabilityWorkflowsPermission), want: true},
		{name: "another capability's marker", text: "needs-human: " + itemblocker.Marker(itemblocker.CapabilityBranchProtection) + " the workflows permission", want: false},
		{name: "a workflow discussed, nothing it cannot push", text: "The `.github/workflows/build.yml` retry loop gives up on the first 502. Retry it.", want: false},
		{name: "a permission named but no workflow file", text: "The token lacks the `contents: write` permission for this fork.", want: false},
		{name: "lettered options are the owner's choice", text: handoff634 + "\n\nOption A: retry three times\nOption B: drop the cleanup step", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parkIsWorkflowsOnly(tc.text); got != tc.want {
				t.Fatalf("parkIsWorkflowsOnly = %v, want %v for:\n%s", got, tc.want, tc.text)
			}
		})
	}
}

func handToScanner(c *Client) {
	c.SetWorkflowsLaneFunc(func(agent string) string {
		if agent == "scanner" {
			return ""
		}
		return "scanner"
	})
}

// exult#374: the comment park goes to scanner, not the owner. No park label,
// the routing label instead, a note naming the lane, no owner-step marker
// (so the notice never offers `/hive done` for it).
func TestCommentWorkflowsParkHandsToCapableLane(t *testing.T) {
	writes, bodies, resp := runNeedsDecisionCommentCapture(t, needsDecisionFilter{label: "owner-call"}, IssueRequest{
		Kind: "comment", Repo: "o/r", Number: 374, Agent: "ci-maintainer", NeedsDecision: true, Body: handoff374,
	}, false, handToScanner)
	if got := strings.Join(writes, ","); got != "label:"+WorkflowsLaneLabel+",comment" {
		t.Fatalf("writes = %q, want the routing label then the comment, and no park label", got)
	}
	if !resp.OK || resp.Number != 374 {
		t.Fatalf("result = %+v, want ok on #374", resp)
	}
	if len(bodies) != 1 {
		t.Fatalf("bodies = %v, want one comment", bodies)
	}
	if strings.Contains(bodies[0], unparkOwnerStepMarker) {
		t.Fatalf("a handed-off park must not carry the owner-step marker:\n%s", bodies[0])
	}
	for _, want := range []string{"Handed to `scanner`", "`ci-maintainer`'s token cannot push", "`" + WorkflowsLaneLabel + "`", "`/hive hold`"} {
		if !strings.Contains(bodies[0], want) {
			t.Errorf("comment lacks %q:\n%s", want, bodies[0])
		}
	}
	if strings.Contains(bodies[0], "Decision taken by hive:") {
		t.Fatalf("a hand-off is not a hive-taken decision:\n%s", bodies[0])
	}
}

// Everything that should still reach the owner still parks, marker intact.
func TestCommentWorkflowsParkStaysWithOwner(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent string
		body  string
		setup func(*Client)
	}{
		{name: "no capable lane in the fleet", agent: "ci-maintainer", body: handoff374, setup: func(c *Client) { c.SetWorkflowsLaneFunc(func(string) string { return "" }) }},
		{name: "lookup never installed", agent: "ci-maintainer", body: handoff374},
		{name: "the capable lane itself could not push", agent: "scanner", body: handoff374, setup: handToScanner},
		{name: "not a workflow change", agent: "ci-maintainer", body: "Agents do not edit repository settings; the owner runs: gh api --method PUT ...", setup: handToScanner},
		{name: "a choice between options", agent: "ci-maintainer", body: handoff374 + "\n\nOption A: keep 10 tags\nOption B: keep 40 tags", setup: handToScanner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes, bodies, _ := runNeedsDecisionCommentCapture(t, needsDecisionFilter{label: "owner-call"}, IssueRequest{
				Kind: "comment", Repo: "o/r", Number: 374, Agent: tc.agent, NeedsDecision: true, Body: tc.body,
			}, false, tc.setup)
			if got := strings.Join(writes, ","); got != "label:owner-call,comment" {
				t.Fatalf("writes = %q, want the park label then the comment", got)
			}
			if len(bodies) != 1 || strings.Contains(bodies[0], "Handed to") {
				t.Fatalf("bodies = %v, want one comment with no hand-off note", bodies)
			}
		})
	}
}

// arch-bootc#634: a filed workflow-only park is created unparked, actionable,
// with the routing label and the note.
func TestCreateWorkflowsParkHandsToCapableLane(t *testing.T) {
	var mu sync.Mutex
	var got []issueCreatePayload
	srv := newIssueCreateLabelServer(t, &mu, &got)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.SetAgentIssueDecisionPolicy("escalate")
	handToScanner(c)
	dir := withIssueDir(t)
	if _, err := WriteIssueRequest(dir, IssueRequest{
		Repo: "o/r", Title: "[ci-maintainer] Daily build goes red when GitHub returns one temporary error during cache cleanup",
		Body: handoff634, Labels: []string{"agent/ci-maintainer", "ci"}, Agent: "ci-maintainer", NeedsDecision: true,
	}); err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("creates = %d, want 1", len(got))
	}
	p := got[0]
	if hasLabelFold(p.Labels, NeedsDecisionStageLabel) {
		t.Fatalf("labels = %v: a handed-off park must not be parked", p.Labels)
	}
	for _, want := range []string{WorkflowsLaneLabel, "stage/010-actionable", "agent/ci-maintainer"} {
		if !hasLabelFold(p.Labels, want) {
			t.Errorf("labels = %v, want %s", p.Labels, want)
		}
	}
	if strings.Contains(p.Body, unparkOwnerStepMarker) || !strings.Contains(p.Body, "Handed to `scanner`") {
		t.Fatalf("body must name the hand-off and carry no owner-step marker:\n%s", p.Body)
	}
}

// The routing label is a hive signal, never read as a legacy hive-ID label.
func TestWorkflowsLaneLabelIsHiveSignal(t *testing.T) {
	if !isHiveSignalLabel(WorkflowsLaneLabel) {
		t.Fatalf("%s must be a hive signal label", WorkflowsLaneLabel)
	}
}
