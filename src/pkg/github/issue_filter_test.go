package github

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// These tests pin project.issue_filter enforcement at THE choice point —
// EnumerateActionable/fetchIssues, where GitHub issues become the hive's
// actionable set. An issue the filter refuses must never enter
// Issues.Items: everything downstream (governor queue counts, scheduler
// kicks, plan-from-label, the contribute queue) consumes that set, so
// refusal here is what makes the gate un-bypassable by a re-listing agent.

func filterTestIssues() []wireIssue {
	return []wireIssue{
		{Number: 1, Title: "approved work", User: wireUser{"alice"},
			Labels: []wireLabel{{Name: "bug"}, {Name: "approved-for-agents"}}, CreatedAt: hoursAgo(3)},
		{Number: 2, Title: "unapproved work", User: wireUser{"bob"},
			Labels: []wireLabel{{Name: "bug"}}, CreatedAt: hoursAgo(2)},
		{Number: 3, Title: "approved but exempted", User: wireUser{"carol"},
			Labels: []wireLabel{{Name: "approved-for-agents"}, {Name: "no-ai"}}, CreatedAt: hoursAgo(1)},
	}
}

func enumerateWithFilter(t *testing.T, f config.IssueFilterConfig, exempt []string) *ActionableResult {
	t.Helper()
	org, repo := "testorg", "testrepo"
	mux := buildMux(t, org, repo, filterTestIssues(), nil)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	c.SetIssueFilter(f)
	if len(exempt) > 0 {
		c.SetExemptLabels(exempt)
	}
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	return result
}

func actionableNumbers(result *ActionableResult) map[int]bool {
	nums := make(map[int]bool)
	for _, is := range result.Issues.Items {
		nums[is.Number] = true
	}
	return nums
}

// TestEnumerateActionable_IssueFilterRequireLabel: with require_labels set,
// only labeled issues become actionable. Positive control (issue #1 IS
// admitted) plus a count floor, so the test cannot pass by admitting nothing.
func TestEnumerateActionable_IssueFilterRequireLabel(t *testing.T) {
	result := enumerateWithFilter(t, config.IssueFilterConfig{
		RequireLabels: []string{"approved-for-agents"},
	}, nil)
	nums := actionableNumbers(result)
	if !nums[1] {
		t.Error("positive control failed: issue #1 carries the required label but is not actionable")
	}
	if nums[2] {
		t.Error("issue #2 lacks the required label but entered the actionable set — the choice-point gate is not enforced")
	}
	if got := result.Issues.Count; got != 2 {
		t.Errorf("Issues.Count = %d, want 2 (issues #1 and #3)", got)
	}
}

// TestEnumerateActionable_ExemptWinsOverRequire pins the ONE-exclusion-story
// composition: the exclude polarity lives in governor.labels.exempt (the
// dashboard Labels tab), not in issue_filter, and it runs BEFORE the require
// gate — so an issue carrying both an exempt label and the approval label
// stays excluded. This is the "exclude wins over require" conflict rule,
// delivered by the pre-existing mechanism rather than a duplicate one.
func TestEnumerateActionable_ExemptWinsOverRequire(t *testing.T) {
	result := enumerateWithFilter(t, config.IssueFilterConfig{
		RequireLabels: []string{"approved-for-agents"},
	}, []string{"no-ai"})
	nums := actionableNumbers(result)
	if !nums[1] {
		t.Error("positive control failed: issue #1 refused")
	}
	if nums[3] {
		t.Error("issue #3 carries a governor exempt label but entered the actionable set — exemption must win over the require gate")
	}
	if nums[2] {
		t.Error("issue #2 lacks the required label but entered the actionable set")
	}
	if got := result.Issues.Count; got != 1 {
		t.Errorf("Issues.Count = %d, want 1 (only issue #1)", got)
	}
}

// TestEnumerateActionable_NoIssueFilterUnchanged is the regression pin: with
// no filter configured (the state of every existing hive), enumeration is
// byte-for-byte the old behavior — all three issues actionable.
func TestEnumerateActionable_NoIssueFilterUnchanged(t *testing.T) {
	result := enumerateWithFilter(t, config.IssueFilterConfig{}, nil)
	if got := result.Issues.Count; got != 3 {
		t.Errorf("Issues.Count = %d, want 3 — absent filter must change nothing", got)
	}
	nums := actionableNumbers(result)
	for _, n := range []int{1, 2, 3} {
		if !nums[n] {
			t.Errorf("issue #%d missing from actionable set with no filter configured", n)
		}
	}
}

// TestEnumerateActionable_IssueFilterDoesNotTouchPRs: the filter gates which
// ISSUES agents may initiate work on. Open PRs are in-flight work and must
// keep flowing regardless of their labels.
func TestEnumerateActionable_IssueFilterDoesNotTouchPRs(t *testing.T) {
	org, repo := "testorg", "testrepo"
	prs := []wirePR{
		{Number: 10, Title: "fix in flight", User: wireUser{"alice"},
			Labels: []wireLabel{{Name: "bug"}}, CreatedAt: hoursAgo(1)},
	}
	mux := buildMux(t, org, repo, nil, prs)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	c.SetIssueFilter(config.IssueFilterConfig{RequireLabels: []string{"approved-for-agents"}})
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	if result.PRs.Count != 1 {
		t.Errorf("PRs.Count = %d, want 1 — the issue filter must not gate PRs", result.PRs.Count)
	}
}

// TestEnumerateActionable_IssueFilterKeepsHold: hold-labeled issues keep
// appearing in the Hold list even when they would fail the require gate — the
// filter runs after the hold check, preserving the operator's hold view.
func TestEnumerateActionable_IssueFilterKeepsHold(t *testing.T) {
	org, repo := "testorg", "testrepo"
	issues := []wireIssue{
		{Number: 5, Title: "parked", User: wireUser{"dave"},
			Labels: []wireLabel{{Name: "hold"}}, CreatedAt: hoursAgo(1)},
	}
	mux := buildMux(t, org, repo, issues, nil)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	c.SetIssueFilter(config.IssueFilterConfig{RequireLabels: []string{"approved-for-agents"}})
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	if result.Hold.Issues != 1 {
		t.Errorf("Hold.Issues = %d, want 1 — the issue filter must not hide held items", result.Hold.Issues)
	}
	if result.Issues.Count != 0 {
		t.Errorf("Issues.Count = %d, want 0", result.Issues.Count)
	}
}

// TestSetIssueFilter_NilReceiver: the setter must be nil-receiver safe, like
// SetRepos/SetExemptLabels — a hive without GitHub credentials runs with a
// nil *Client and the reload paths call the setter unconditionally.
func TestSetIssueFilter_NilReceiver(t *testing.T) {
	var c *Client
	c.SetIssueFilter(config.IssueFilterConfig{RequireLabels: []string{"x"}}) // must not panic
}
