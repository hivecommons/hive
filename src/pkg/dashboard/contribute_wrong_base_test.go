package dashboard

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWrongBaseVerdict_ValidBranchRequeuesWithRoutingOverride(t *testing.T) {
	hub, _ := covK2Hub(t)
	hub.wrongBaseBranchChecker = func(_ context.Context, repo, branch string) (bool, error) {
		if repo != "hivecommons/hive" || branch != "v6" {
			t.Fatalf("unexpected validation: repo=%q branch=%q", repo, branch)
		}
		return true, nil
	}
	task := &WSTaskAssign{Kind: "issue", Repo: "hivecommons/hive", Number: 13146}
	verdict, target, reason, park := hub.resolveWrongBaseVerdict(task, completionVerdictWrongBase, "V6")
	if verdict != completionVerdictWrongBase || target != "v6" || reason != "v6" || park {
		t.Fatalf("resolved = (%q, %q, %q, %v)", verdict, target, reason, park)
	}

	key := task.identityKey()
	hub.markTaskCompletedVerdictKeySignal(key, "", verdict, "dev", reason, completionSignalVerdict)
	if hub.isSuppressedByNoWorkVerdictKey(key, time.Now().Add(-time.Hour)) {
		t.Fatal("wrong_base must not suppress the issue as no_work_needed")
	}
	if got := hub.wrongBaseTargetBranchKey(key, time.Now().Add(-time.Hour)); got != "v6" {
		t.Fatalf("routing override = %q, want v6", got)
	}
	if got := recordedCooldown(t, hub, key); got != completedNoPRCooldownHours*time.Hour {
		t.Fatalf("wrong_base cooldown = %v, want short cooldown", got)
	}
}

func TestWrongBaseVerdict_InvalidOrUnverifiableBranchParks(t *testing.T) {
	hub, _ := covK2Hub(t)
	task := &WSTaskAssign{Kind: "issue", Repo: "hivecommons/hive", Number: 13146}

	verdict, target, reason, park := hub.resolveWrongBaseVerdict(task, completionVerdictWrongBase, "feature/x")
	if verdict != completionVerdictNeedsDecision || target != "" || !park || reason != wrongBaseNoTargetReason {
		t.Fatalf("malformed target resolved = (%q, %q, %q, %v)", verdict, target, reason, park)
	}
	// A repository on main/testing/stable has no release branch to supply
	// (projectbluefin/utah#1272); the reason must not say one was missing.
	if strings.Contains(reason, "release branch was supplied") {
		t.Fatalf("no-target reason blames a missing release branch: %q", reason)
	}

	hub.wrongBaseBranchChecker = func(context.Context, string, string) (bool, error) {
		return false, errors.New("rate limited")
	}
	verdict, target, reason, park = hub.resolveWrongBaseVerdict(task, completionVerdictWrongBase, "v6")
	if verdict != completionVerdictNeedsDecision || target != "v6" || !park || !strings.Contains(reason, "could not be validated") {
		t.Fatalf("API failure resolved = (%q, %q, %q, %v)", verdict, target, reason, park)
	}
}

func TestWrongBaseVerdict_ParkCommentIsOneLineAndNamesBranch(t *testing.T) {
	comment := wrongBaseParkComment("ct-1", "alice", "v6", "target branch v6 does not exist", "", "stage/045-needs-decision")
	if strings.Contains(comment, "\n") {
		t.Fatalf("park comment must be one line: %q", comment)
	}
	for _, want := range []string{wrongBaseParkMarker("ct-1"), "@alice", "`v6`", "`stage/045-needs-decision`", "The agent gave no reason.", "/hive decision"} {
		if !strings.Contains(comment, want) {
			t.Errorf("park comment missing %q: %q", want, comment)
		}
	}
}

// projectbluefin/utah#1272 and projectbluefin/ghostscript-printer-app#127: a
// wrong_base verdict whose target is not a vN branch parks with the agent's
// own reason quoted, never as a ref, and without the "release branch was
// supplied" phrase a main/testing/stable repository cannot act on.
func TestWrongBaseVerdict_ParkCommentQuotesAgentReason(t *testing.T) {
	agent := sanitizeReportedReason("testing — utah's AGENTS.md says all PRs target `testing`; @someone <!-- x --> knows")
	for _, tc := range []struct {
		name, target, detail string
	}{
		{"non-vN branch named", "", wrongBaseNoTargetReason},
		{"vN that does not exist", "v9", "wrong_base: target branch v9 does not exist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			comment := wrongBaseParkComment("ct-utah-1272", "Karibhou", tc.target, tc.detail, agent, "stage/045-needs-decision")
			if strings.Contains(comment, "\n") {
				t.Fatalf("park comment must be one line: %q", comment)
			}
			for _, want := range []string{
				wrongBaseParkMarker("ct-utah-1272"),
				"@Karibhou's agent",
				"The agent said: \u201ctesting \u2014 utah's AGENTS.md says all PRs target testing;",
				"/hive decision",
			} {
				if !strings.Contains(comment, want) {
					t.Errorf("park comment missing %q: %q", want, comment)
				}
			}
			for _, bad := range []string{"release branch was supplied", "@someone", "<!-- x", "`testing`"} {
				if strings.Contains(comment, bad) {
					t.Errorf("park comment contains %q: %q", bad, comment)
				}
			}
		})
	}
	if got := wrongBaseParkReason(wrongBaseNoTargetReason, agent); !strings.HasPrefix(got, wrongBaseNoTargetReason+"; the agent said: testing") {
		t.Fatalf("recorded reason = %q", got)
	}
	if got := wrongBaseParkReason(wrongBaseNoTargetReason, ""); got != wrongBaseNoTargetReason {
		t.Fatalf("recorded reason without agent text = %q", got)
	}
	if wrongBaseParkMarker("ct-a") == wrongBaseParkMarker("ct-b") {
		t.Fatal("a later task's park must get its own note")
	}
}

// The wrong_base outcome is offered only to a task with a declared release
// line; elsewhere the agent asks in the decision form instead.
func TestBuildTaskPrompt_WrongBaseOnlyWithDeclaredBase(t *testing.T) {
	const verdictLine = "HIVE_VERDICT: wrong_base"
	declared := buildTaskPromptBody("acme/widgets", "acme/widgets#7", "[v6] fix it", "", "v6")
	if !strings.Contains(declared, verdictLine) {
		t.Fatalf("a declared-base prompt must offer wrong_base:\n%s", declared)
	}
	base := taskBaseBranchForIssue("plain title", "no target here", "", "projectbluefin/utah", "v6")
	if base != "" {
		t.Fatalf("utah-style task resolved base %q, want none", base)
	}
	none := buildTaskPromptBody("projectbluefin/utah", "projectbluefin/utah#1272", "plain title", "", base)
	if strings.Contains(none, verdictLine) {
		t.Fatalf("a task with no declared base must not offer wrong_base:\n%s", none)
	}
	for _, want := range []string{"If you cannot tell which branch this work belongs on", "do not guess a branch", "HIVE_VERDICT: no_work_needed — decision:"} {
		if !strings.Contains(none, want) {
			t.Errorf("no-base prompt missing %q", want)
		}
	}
}

func TestNormalizeCompletionVerdict_WrongBaseNeverBecomesAlreadyDone(t *testing.T) {
	if got := normalizeCompletionVerdict("wrong_base", false, false, ""); got != completionVerdictWrongBase {
		t.Fatalf("normalize wrong_base = %q", got)
	}
	if got := normalizeCompletionVerdict("wrong_base", false, false, "https://github.com/o/r/pull/1"); got != completionVerdictShipped {
		t.Fatalf("verified PR must still win, got %q", got)
	}
	if _, alreadyDone := alreadyDoneVerdictRefs("o/r", 1, "v6", "", nil); alreadyDone {
		t.Fatal("a wrong-base branch name must not classify as already done")
	}
}
