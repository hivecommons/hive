package github

import (
	"context"
	"net/http/httptest"
	"testing"
)

// A held item now carries enough for the repo card to draw it as a pill of
// its own (hivecommons/hive#7896): its GitHub URL and the labels that hold
// it, so the pill links somewhere and its tooltip can name the label an
// operator has to remove. Before this a held issue existed on the wire only
// as number + title, and the card had nothing to link or explain.
func TestEnumerateActionable_HoldItemsCarryURLAndLabels(t *testing.T) {
	org, repo := "org", "repo"
	issues := []wireIssue{
		{Number: 1, Title: "held iss", User: wireUser{"u"}, Labels: []wireLabel{{Name: "on-hold"}, {Name: "bug"}}, CreatedAt: hoursAgo(1), HTMLURL: "https://github.com/org/repo/issues/1"},
	}
	prs := []wirePR{
		{Number: 2, Title: "held pr", User: wireUser{"renovate[bot]"}, Labels: []wireLabel{{Name: "hold"}, {Name: "needs-human"}}, CreatedAt: hoursAgo(1), HTMLURL: "https://github.com/org/repo/pull/2"},
	}
	mux := buildMux(t, org, repo, issues, prs)
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	if len(result.Hold.Items) != 2 {
		t.Fatalf("hold items = %d, want 2: %+v", len(result.Hold.Items), result.Hold.Items)
	}
	for _, h := range result.Hold.Items {
		switch h.Number {
		case 1:
			if h.URL != "https://github.com/org/repo/issues/1" {
				t.Errorf("held issue URL = %q", h.URL)
			}
			if !heldTestHasLabel(h.Labels, "on-hold") || !heldTestHasLabel(h.Labels, "bug") {
				t.Errorf("held issue labels = %v, want on-hold and bug", h.Labels)
			}
		case 2:
			if h.URL != "https://github.com/org/repo/pull/2" {
				t.Errorf("held PR URL = %q", h.URL)
			}
			if !heldTestHasLabel(h.Labels, "hold") || !heldTestHasLabel(h.Labels, "needs-human") {
				t.Errorf("held PR labels = %v, want hold and needs-human", h.Labels)
			}
		default:
			t.Errorf("unexpected hold item %+v", h)
		}
	}
	// The hold gate itself is untouched: neither item is actionable.
	if len(result.Issues.Items) != 0 || len(result.PRs.Items) != 0 {
		t.Errorf("held items leaked into the actionable sets: issues=%d prs=%d", len(result.Issues.Items), len(result.PRs.Items))
	}
}

// A held issue also carries its assignees and the cheap #5117 acknowledgment
// bit (#9019), so the repo card can keep a human-acknowledged agent proposal
// out of the Needs triage band even while it is parked. Held issues used to
// ride as labels only, which made a held agent issue assigned to a human
// look unacknowledged.
func TestEnumerateActionable_HeldIssuesCarryAssigneesAndAcknowledgment(t *testing.T) {
	org, repo := "org", "repo"
	issues := []wireIssue{
		{Number: 1, Title: "held, human assignee", User: wireUser{"hive[bot]"}, Labels: []wireLabel{{Name: "hold"}, {Name: "agent/strategist"}}, Assignees: []wireUser{{"dan"}}, CreatedAt: hoursAgo(1)},
		{Number: 2, Title: "held, bot assignee", User: wireUser{"hive[bot]"}, Labels: []wireLabel{{Name: "hold"}, {Name: "agent/strategist"}}, Assignees: []wireUser{{"worker[bot]"}}, CreatedAt: hoursAgo(1)},
		{Number: 3, Title: "held, approved-direction", User: wireUser{"hive[bot]"}, Labels: []wireLabel{{Name: "hold"}, {Name: "agent/quality"}, {Name: "approved-direction"}}, CreatedAt: hoursAgo(1)},
		{Number: 4, Title: "held, untouched", User: wireUser{"hive[bot]"}, Labels: []wireLabel{{Name: "hold"}, {Name: "agent/quality"}}, CreatedAt: hoursAgo(1)},
	}
	mux := buildMux(t, org, repo, issues, nil)
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	if len(result.Hold.Items) != 4 {
		t.Fatalf("hold items = %d, want 4: %+v", len(result.Hold.Items), result.Hold.Items)
	}
	want := map[int]struct {
		assignees []string
		ack       bool
	}{
		1: {[]string{"dan"}, true},
		2: {[]string{"worker[bot]"}, false},
		3: {nil, true},
		4: {nil, false},
	}
	for _, h := range result.Hold.Items {
		w, ok := want[h.Number]
		if !ok {
			t.Errorf("unexpected hold item %+v", h)
			continue
		}
		if h.HumanAcknowledged != w.ack {
			t.Errorf("held issue #%d human_acknowledged = %v, want %v", h.Number, h.HumanAcknowledged, w.ack)
		}
		if len(h.Assignees) != len(w.assignees) {
			t.Errorf("held issue #%d assignees = %v, want %v", h.Number, h.Assignees, w.assignees)
			continue
		}
		for i := range w.assignees {
			if h.Assignees[i] != w.assignees[i] {
				t.Errorf("held issue #%d assignees = %v, want %v", h.Number, h.Assignees, w.assignees)
			}
		}
	}
}

// heldTestHasLabel: v5 moved hasLabel into pkg/github/automerge, so the test
// carries its own one-liner.
func heldTestHasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}
