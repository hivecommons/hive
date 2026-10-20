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
	if verdict != completionVerdictNeedsDecision || target != "" || !park || !strings.Contains(reason, "no valid target") {
		t.Fatalf("malformed target resolved = (%q, %q, %q, %v)", verdict, target, reason, park)
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
	comment := wrongBaseParkComment("alice", "v6", "target branch v6 does not exist", "stage/045-needs-decision")
	if strings.Contains(comment, "\n") {
		t.Fatalf("park comment must be one line: %q", comment)
	}
	for _, want := range []string{wrongBaseParkMarker, "@alice", "`v6`", "`stage/045-needs-decision`"} {
		if !strings.Contains(comment, want) {
			t.Errorf("park comment missing %q: %q", want, comment)
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
