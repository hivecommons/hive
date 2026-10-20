package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// The hive/in-progress start signal is stated at the post-resolution seam so
// no template path can omit it from a kick that lists issues.
func TestBuildKickMessages_IncludesInProgressClaimRule(t *testing.T) {
	s := newScheduler()
	msgs := s.BuildKickMessages(extraActionable(), []string{"scanner"})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	for _, want := range []string{
		"hive-open-issue claim --repo <owner/repo> <issue-number>",
		"labels the issue `hive/in-progress`",
		"Claim one issue at a time",
		"never claim an issue you are only triaging, verifying, gate-checking or reporting on",
		"never claim an epic or tracking issue",
		"<!-- hive:start -->",
	} {
		if !strings.Contains(msgs[0].Message, want) {
			t.Errorf("kick missing in-progress rule %q:\n%s", want, msgs[0].Message)
		}
	}
	if strings.Count(msgs[0].Message, inProgressClaimRule) != 1 {
		t.Fatalf("rule must appear exactly once")
	}
}

func TestAddInProgressClaimRule(t *testing.T) {
	issues := []github.Issue{{Repo: "r", Number: 1}}
	if got := addInProgressClaimRule("", issues); got != "" {
		t.Fatalf("empty message must stay empty, got %q", got)
	}
	if got := addInProgressClaimRule("kick body", nil); got != "kick body" {
		t.Fatalf("no issues → unchanged, got %q", got)
	}
	once := addInProgressClaimRule("kick body\n", issues)
	if !strings.HasPrefix(once, "kick body\n\n"+inProgressClaimRule) {
		t.Fatalf("rule not appended:\n%s", once)
	}
	if twice := addInProgressClaimRule(once, issues); twice != once {
		t.Fatalf("rule must be idempotent")
	}
	custom := "template already says: run hive-open-issue claim --repo o/r 7 first"
	if got := addInProgressClaimRule(custom, issues); got != custom {
		t.Fatalf("template carrying its own instruction must not get a second one")
	}
}
