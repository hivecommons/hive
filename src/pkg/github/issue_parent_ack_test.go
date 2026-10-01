package github

import (
	"context"
	"net/http/httptest"
	"strconv"
	"testing"
)

func parentURL(org, repo string, n int) string {
	return "https://api.github.com/repos/" + org + "/" + repo + "/issues/" + strconv.Itoa(n)
}

// TestEnumerateActionable_ChildOfHumanIssueInheritsAcknowledgement is the
// #9840 case: a maintainer files #100, the scanner splits it into #101–#102
// as sub-issues. The children must rank in the acknowledged tier (ahead of
// the unrelated hive-filed backlog) and name the parent they inherit from.
func TestEnumerateActionable_ChildOfHumanIssueInheritsAcknowledgement(t *testing.T) {
	org, repo := "testorg", "testrepo"
	bot := wireUser{"testorg-hive[bot]"}
	issues := []wireIssue{
		{Number: 100, Title: "big human issue", User: wireUser{"alice"}, CreatedAt: hoursAgo(10)},
		{Number: 50, Title: "unrelated hive backlog, older", User: bot, CreatedAt: hoursAgo(20)},
		{Number: 101, Title: "child one", User: bot, CreatedAt: hoursAgo(2), ParentIssueURL: parentURL(org, repo, 100)},
		{Number: 102, Title: "child two", User: bot, CreatedAt: hoursAgo(1), ParentIssueURL: parentURL(org, repo, 100)},
	}
	server := httptest.NewServer(buildMux(t, org, repo, issues, nil))
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	byNumber := map[int]Issue{}
	for _, it := range result.Issues.Items {
		byNumber[it.Number] = it
	}
	for _, n := range []int{101, 102} {
		got := byNumber[n]
		if !got.HumanAcknowledged || got.AckParent != 100 {
			t.Errorf("#%d: HumanAcknowledged=%v AckParent=%d, want true/100", n, got.HumanAcknowledged, got.AckParent)
		}
	}
	if got := byNumber[100]; got.AckParent != 0 || got.HumanAcknowledged {
		t.Errorf("parent #100 must not be marked as inheriting: %+v", got)
	}
	if got := byNumber[50]; got.HumanAcknowledged || got.AckParent != 0 {
		t.Errorf("unrelated hive issue #50 must stay unacknowledged: %+v", got)
	}
	// Ranking: human parent (tier 1), then the two acknowledged children
	// (tier 2, oldest first), then the unrelated backlog (tier 3) even though
	// it is the oldest issue in the repo.
	wantOrder := []int{100, 101, 102, 50}
	for i, want := range wantOrder {
		if result.Issues.Items[i].Number != want {
			var got []int
			for _, it := range result.Issues.Items {
				got = append(got, it.Number)
			}
			t.Fatalf("kick order = %v, want %v", got, wantOrder)
		}
	}
}

// TestEnumerateActionable_ParentAcknowledgementLimits pins the guards that
// keep inheritance from becoming a way around #5117.
func TestEnumerateActionable_ParentAcknowledgementLimits(t *testing.T) {
	org, repo := "testorg", "testrepo"
	bot := wireUser{"testorg-hive[bot]"}
	cases := []struct {
		name   string
		issues []wireIssue
		child  int
		want   bool
	}{{
		name: "hive-filed, unacknowledged parent confers nothing",
		issues: []wireIssue{
			{Number: 1, Title: "hive parent", User: bot, CreatedAt: hoursAgo(5)},
			{Number: 2, Title: "child", User: bot, CreatedAt: hoursAgo(1), ParentIssueURL: parentURL(org, repo, 1)},
		},
		child: 2,
	}, {
		name: "hive-filed parent with the approval label confers (one level)",
		issues: []wireIssue{
			{Number: 1, Title: "hive parent", User: bot, Labels: []wireLabel{{Name: HumanAckLabel}}, CreatedAt: hoursAgo(5)},
			{Number: 2, Title: "child", User: bot, CreatedAt: hoursAgo(1), ParentIssueURL: parentURL(org, repo, 1)},
		},
		child: 2, want: true,
	}, {
		name: "grandchild does not inherit through an inheriting child",
		issues: []wireIssue{
			{Number: 1, Title: "human root", User: wireUser{"alice"}, CreatedAt: hoursAgo(9)},
			{Number: 2, Title: "child", User: bot, CreatedAt: hoursAgo(5), ParentIssueURL: parentURL(org, repo, 1)},
			{Number: 3, Title: "grandchild", User: bot, CreatedAt: hoursAgo(1), ParentIssueURL: parentURL(org, repo, 2)},
		},
		child: 3,
	}, {
		name: "held parent confers nothing",
		issues: []wireIssue{
			{Number: 1, Title: "human parent on hold", User: wireUser{"alice"}, Labels: []wireLabel{{Name: "hold"}}, CreatedAt: hoursAgo(5)},
			{Number: 2, Title: "child", User: bot, CreatedAt: hoursAgo(1), ParentIssueURL: parentURL(org, repo, 1)},
		},
		child: 2,
	}, {
		name: "parent not in the open set (closed) confers nothing",
		issues: []wireIssue{
			{Number: 2, Title: "child", User: bot, CreatedAt: hoursAgo(1), ParentIssueURL: parentURL(org, repo, 1)},
		},
		child: 2,
	}, {
		name: "parent in another repository confers nothing",
		issues: []wireIssue{
			{Number: 1, Title: "human parent", User: wireUser{"alice"}, CreatedAt: hoursAgo(5)},
			{Number: 2, Title: "child", User: bot, CreatedAt: hoursAgo(1), ParentIssueURL: parentURL(org, "otherrepo", 1)},
		},
		child: 2,
	}, {
		name: "a PR masquerading as the parent confers nothing",
		issues: []wireIssue{
			{Number: 1, Title: "human PR", User: wireUser{"alice"}, CreatedAt: hoursAgo(5), PullRequest: &struct{}{}},
			{Number: 2, Title: "child", User: bot, CreatedAt: hoursAgo(1), ParentIssueURL: parentURL(org, repo, 1)},
		},
		child: 2,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(buildMux(t, org, repo, tc.issues, nil))
			defer server.Close()
			c := newTestClient(t, server, org, []string{repo})
			result, err := c.EnumerateActionable(context.Background())
			if err != nil {
				t.Fatalf("EnumerateActionable: %v", err)
			}
			for _, it := range result.Issues.Items {
				if it.Number != tc.child {
					continue
				}
				if it.HumanAcknowledged != tc.want {
					t.Fatalf("child #%d HumanAcknowledged=%v AckParent=%d, want %v", tc.child, it.HumanAcknowledged, it.AckParent, tc.want)
				}
				if tc.want && it.AckParent == 0 {
					t.Fatalf("child #%d inherits but AckParent is unset", tc.child)
				}
				return
			}
			t.Fatalf("child #%d missing from actionable set: %+v", tc.child, result.Issues.Items)
		})
	}
}

// TestEnumerateActionable_HeldChildStaysHeld: `hold` on the child wins over
// inheritance — the child never reaches the actionable set.
func TestEnumerateActionable_HeldChildStaysHeld(t *testing.T) {
	org, repo := "testorg", "testrepo"
	bot := wireUser{"testorg-hive[bot]"}
	issues := []wireIssue{
		{Number: 1, Title: "human parent", User: wireUser{"alice"}, CreatedAt: hoursAgo(5)},
		{Number: 2, Title: "child on hold", User: bot, Labels: []wireLabel{{Name: "hold"}}, CreatedAt: hoursAgo(1), ParentIssueURL: parentURL(org, repo, 1)},
	}
	server := httptest.NewServer(buildMux(t, org, repo, issues, nil))
	defer server.Close()
	c := newTestClient(t, server, org, []string{repo})
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	if result.Issues.Count != 1 || result.Issues.Items[0].Number != 1 {
		t.Fatalf("actionable = %+v, want only the parent", result.Issues.Items)
	}
	if result.Hold.Issues != 1 || result.Hold.Items[0].Number != 2 {
		t.Fatalf("held = %+v, want the child", result.Hold.Items)
	}
}

func TestParentIssueNumberFromURL(t *testing.T) {
	cases := []struct {
		url  string
		want int
	}{
		{"", 0},
		{"https://api.github.com/repos/o/r/issues/12", 12},
		{"https://ghe.example.com/api/v3/repos/O/R/issues/7", 7},
		{"https://api.github.com/repos/o/other/issues/12", 0},
		{"https://api.github.com/repos/o/r/pulls/12", 0},
		{"https://api.github.com/repos/o/r/issues/abc", 0},
		{"https://api.github.com/repos/o/r/issues/0", 0},
		{"not a url", 0},
	}
	for _, tc := range cases {
		if got := parentIssueNumberFromURL(tc.url, "o", "r"); got != tc.want {
			t.Errorf("parentIssueNumberFromURL(%q) = %d, want %d", tc.url, got, tc.want)
		}
	}
}

func TestActionableIssueRankTier_InheritedAcknowledgementIsTierTwo(t *testing.T) {
	child := Issue{Number: 2, Author: "hive[bot]", HumanAcknowledged: true, AckParent: 1}
	if got := actionableIssueRankTier(child, map[string]struct{}{}); got != 2 {
		t.Fatalf("tier = %d, want 2", got)
	}
	plain := Issue{Number: 3, Author: "hive[bot]"}
	if got := actionableIssueRankTier(plain, map[string]struct{}{}); got != 3 {
		t.Fatalf("tier = %d, want 3", got)
	}
}
