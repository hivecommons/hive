package github

import (
	"context"
	"net/http/httptest"
	"testing"
)

// A parked issue must never reach a kick work list or contributor offer, or
// the kick path re-claims it every cycle (#10553).
func TestHardSuppressLabelsExcludedFromActionableWork(t *testing.T) {
	for _, label := range []string{"needs-direction", "needs-decision", "needs-spec", "needs-human", " Needs-Direction ", "hold"} {
		t.Run(label, func(t *testing.T) {
			issues := []wireIssue{
				{Number: 1, Title: "parked", User: wireUser{"alice"}, CreatedAt: hoursAgo(1), Labels: []wireLabel{{Name: "help wanted"}, {Name: label}}},
				{Number: 2, Title: "open work", User: wireUser{"alice"}, CreatedAt: hoursAgo(1), Labels: []wireLabel{{Name: "help wanted"}}},
			}
			srv := httptest.NewServer(buildMux(t, "o", "r", issues, nil))
			defer srv.Close()
			result, err := newTestClient(t, srv, "o", []string{"r"}).EnumerateActionable(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Issues.Items) != 1 || result.Issues.Items[0].Number != 2 {
				t.Fatalf("actionable issues = %+v, want only #2", result.Issues.Items)
			}
		})
	}
}

func TestHasHardSuppressIssueLabel(t *testing.T) {
	for _, tc := range []struct {
		labels []string
		want   bool
	}{
		{[]string{"needs-direction"}, true},
		{[]string{"bug", "NEEDS-DECISION"}, true},
		{[]string{" needs-spec "}, true},
		{[]string{"needs-human"}, true},
		{[]string{"help wanted"}, false},
		{[]string{"needs-signal"}, false},
		{[]string{"needs-human-followup"}, false},
		{nil, false},
	} {
		if got := hasHardSuppressIssueLabel(tc.labels); got != tc.want {
			t.Errorf("hasHardSuppressIssueLabel(%q) = %v, want %v", tc.labels, got, tc.want)
		}
	}
}
