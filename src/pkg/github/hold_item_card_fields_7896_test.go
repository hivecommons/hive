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
			if !hasLabel(h.Labels, "on-hold") || !hasLabel(h.Labels, "bug") {
				t.Errorf("held issue labels = %v, want on-hold and bug", h.Labels)
			}
		case 2:
			if h.URL != "https://github.com/org/repo/pull/2" {
				t.Errorf("held PR URL = %q", h.URL)
			}
			if !hasLabel(h.Labels, "hold") || !hasLabel(h.Labels, "needs-human") {
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
