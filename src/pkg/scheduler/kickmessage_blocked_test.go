package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// TestFormatIssueList_BlockedIssuesHeldBack pins #9839: an issue with an
// unresolved GitHub "blocked by" dependency is not offered in the kick list
// but is named once in a footer with its blockers, while an issue whose only
// blocker is closed is offered normally.
func TestFormatIssueList_BlockedIssuesHeldBack(t *testing.T) {
	s := newSchedulerWithIoscan(false)
	issues := []github.Issue{
		{Repo: "test-org/console", Number: 41, Title: "first step", AgeMinutes: 5},
		{Repo: "test-org/console", Number: 42, Title: "second step SECRET-TITLE", AgeMinutes: 5,
			DependsOn: []github.IssueDependency{{Key: "test-org/console#41"}}},
		{Repo: "test-org/console", Number: 43, Title: "after a closed blocker", AgeMinutes: 5,
			DependsOn: []github.IssueDependency{{Key: "test-org/console#40", Resolved: true}}},
	}
	out, _ := s.formatIssueListWithPolicy(issues)

	for _, want := range []string{"#41", "first step", "#43", "after a closed blocker"} {
		if !strings.Contains(out, want) {
			t.Errorf("ready issue text %q missing from list:\n%s", want, out)
		}
	}
	if strings.Contains(out, "SECRET-TITLE") {
		t.Errorf("blocked issue title must not be offered:\n%s", out)
	}
	if !strings.Contains(out, "Blocked by open dependencies (1") {
		t.Errorf("blocked footer missing:\n%s", out)
	}
	if !strings.Contains(out, "console#42 blocked by test-org/console#41") {
		t.Errorf("footer must name the blocked issue and its blocker:\n%s", out)
	}
	// The footer must come after the offered lines so it never reads as an offer.
	if strings.Index(out, "Blocked by open dependencies") < strings.Index(out, "#43") {
		t.Errorf("footer must follow the ready list:\n%s", out)
	}
}

func TestFormatIssueList_AllBlockedSaysNoneReady(t *testing.T) {
	s := newSchedulerWithIoscan(false)
	issues := []github.Issue{
		{Repo: "test-org/console", Number: 42, Title: "second step", AgeMinutes: 5,
			DependsOn: []github.IssueDependency{{Key: "test-org/console#41"}}},
	}
	out, _ := s.formatIssueListWithPolicy(issues)
	if !strings.Contains(out, "(none ready)") {
		t.Errorf("expected (none ready) when every issue is blocked:\n%s", out)
	}
	if !strings.Contains(out, "console#42 blocked by test-org/console#41") {
		t.Errorf("footer missing:\n%s", out)
	}
}

func TestFormatBlockedIssuesNote_Caps(t *testing.T) {
	var blocked []github.Issue
	for i := 0; i < maxBlockedIssuesNamed+3; i++ {
		blocked = append(blocked, github.Issue{Repo: "o/r", Number: 100 + i,
			DependsOn: []github.IssueDependency{{Key: "o/r#1"}}})
	}
	out := formatBlockedIssuesNote(blocked)
	if !strings.Contains(out, "… and 3 more") {
		t.Errorf("expected overflow marker:\n%s", out)
	}
	if strings.Contains(out, "#112") {
		t.Errorf("issue past the cap must not be named:\n%s", out)
	}
	if formatBlockedIssuesNote(nil) != "" {
		t.Errorf("empty input must render nothing")
	}
}

// TestBuildKickMessages_BlockedIssuesNotInIssueRefs pins the claim half of
// #9839: a dependency-blocked issue is named in the footer as "do NOT start"
// and so must not appear in KickMessage.IssueRefs — that list is what records
// the agent's claim and posts the 🔒 comment on GitHub every kick.
func TestBuildKickMessages_BlockedIssuesNotInIssueRefs(t *testing.T) {
	s := newScheduler()
	issues := []github.Issue{
		{Repo: "test-org/console", Number: 41, Title: "ready one", AgeMinutes: 5},
		{Repo: "test-org/console", Number: 42, Title: "blocked one", AgeMinutes: 5,
			DependsOn: []github.IssueDependency{{Key: "test-org/console#41"}}},
	}
	kicks := s.BuildKickMessages(&github.ActionableResult{Issues: github.IssueResult{Items: issues}}, []string{"scanner"})
	if !strings.Contains(kicks[0].Message, "console#42 blocked by test-org/console#41") {
		t.Errorf("blocked footer missing:\n%s", kicks[0].Message)
	}
	if len(kicks) != 1 {
		t.Fatalf("kicks = %d", len(kicks))
	}
	var sawReady bool
	for _, ref := range kicks[0].IssueRefs {
		if strings.HasSuffix(ref, "#42") {
			t.Errorf("blocked issue must not be in IssueRefs: %v", kicks[0].IssueRefs)
		}
		if strings.HasSuffix(ref, "#41") {
			sawReady = true
		}
	}
	if !sawReady {
		t.Errorf("ready issue missing from IssueRefs: %v", kicks[0].IssueRefs)
	}
}
