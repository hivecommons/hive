package planning

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#8010: the label trigger ran every eval cycle and re-kicked
// the architect for the same pending epic each time, because nothing ever
// cleared decompose_pending. These tests pin the throttle (no second kick
// inside DecomposeRekickAfter), the attempt cap (DecomposeMaxAttempts kicks
// then decompose_failed), the human reset, and the prompt that closes the
// loop by telling the architect to run `bd decompose` itself.

func withDecomposeClock(t *testing.T, start time.Time) *time.Time {
	t.Helper()
	now := start
	prev := decomposeNow
	decomposeNow = func() time.Time { return now }
	t.Cleanup(func() { decomposeNow = prev })
	return &now
}

func TestPlanIssuesFromLabels_NoRekickInsideWindow(t *testing.T) {
	store := newStore(t)
	now := withDecomposeClock(t, time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC))
	issues := []github.Issue{{Repo: "a/b", Number: 1, Title: "plan me", Labels: []string{"plan"}}}
	kicker := &fakeDecomposeKicker{}

	first := PlanIssuesFromLabels(store, kicker, issues, &recordingSink{}, nil, PlanningMinACMMLevel)
	if first.Kicked != 1 || kicker.kicks != 1 {
		t.Fatalf("first pass: res=%+v kicks=%d, want one kick", first, kicker.kicks)
	}
	epic := store.FindByExternalRef("gh-a/b#1")
	if got := DecomposeAttempts(epic); got != 1 {
		t.Fatalf("attempts after first kick = %d, want 1", got)
	}
	if DecomposeKickedAt(epic).IsZero() {
		t.Fatal("kicked_at not recorded")
	}

	// Next eval cycle, one minute later: still pending, but inside the window.
	*now = now.Add(time.Minute)
	second := PlanIssuesFromLabels(store, kicker, issues, &recordingSink{}, nil, PlanningMinACMMLevel)
	if second.Kicked != 0 || second.Waiting != 1 || kicker.kicks != 1 {
		t.Fatalf("second pass inside window: res=%+v kicks=%d, want Waiting=1 and no new kick", second, kicker.kicks)
	}

	// Past the window: kick again.
	*now = now.Add(DecomposeRekickAfter)
	third := PlanIssuesFromLabels(store, kicker, issues, &recordingSink{}, nil, PlanningMinACMMLevel)
	if third.Kicked != 1 || kicker.kicks != 2 {
		t.Fatalf("third pass past window: res=%+v kicks=%d, want a second kick", third, kicker.kicks)
	}
}

func TestPlanIssuesFromLabels_AttemptCapMarksFailed(t *testing.T) {
	store := newStore(t)
	now := withDecomposeClock(t, time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC))
	issues := []github.Issue{{Repo: "a/b", Number: 1, Title: "plan me", Labels: []string{"plan"}}}
	kicker := &fakeDecomposeKicker{}
	sink := &recordingSink{}

	for i := 0; i < DecomposeMaxAttempts; i++ {
		res := PlanIssuesFromLabels(store, kicker, issues, sink, nil, PlanningMinACMMLevel)
		if res.Kicked != 1 {
			t.Fatalf("attempt %d: res=%+v, want Kicked=1", i+1, res)
		}
		*now = now.Add(DecomposeRekickAfter + time.Second)
	}
	if kicker.kicks != DecomposeMaxAttempts {
		t.Fatalf("kicks = %d, want %d", kicker.kicks, DecomposeMaxAttempts)
	}

	// Budget spent: this pass trips the failed marker instead of kicking.
	res := PlanIssuesFromLabels(store, kicker, issues, sink, nil, PlanningMinACMMLevel)
	if res.Kicked != 0 || res.Failed != 1 || kicker.kicks != DecomposeMaxAttempts {
		t.Fatalf("over budget: res=%+v kicks=%d, want Failed=1 and no kick", res, kicker.kicks)
	}
	if len(sink.failed) != 1 {
		t.Fatalf("sink.failed = %v, want exactly one FailedPlan callback", sink.failed)
	}
	epic := store.FindByExternalRef("gh-a/b#1")
	if !DecomposeFailed(epic) || !DecomposePending(epic) {
		t.Fatalf("epic should be failed AND still pending: failed=%v pending=%v", DecomposeFailed(epic), DecomposePending(epic))
	}

	// Subsequent passes: counted as failed, no kick, no second callback.
	*now = now.Add(24 * time.Hour)
	again := PlanIssuesFromLabels(store, kicker, issues, sink, nil, PlanningMinACMMLevel)
	if again.Failed != 1 || again.Kicked != 0 || kicker.kicks != DecomposeMaxAttempts || len(sink.failed) != 1 {
		t.Fatalf("failed epic must stay quiet: res=%+v kicks=%d failedCallbacks=%d", again, kicker.kicks, len(sink.failed))
	}

	// The plan list surfaces it as stuck, ranked first.
	plans := ListPlans(map[string]*beads.Store{"architect": store})
	if len(plans) != 1 || !plans[0].DecomposeFailed || plans[0].DecomposeAttempts != DecomposeMaxAttempts {
		t.Fatalf("ListPlans = %+v, want one stuck plan with %d attempts", plans, DecomposeMaxAttempts)
	}
	tree, err := GetPlanTree(store, epic.ID)
	if err != nil || !tree.DecomposeFailed || !tree.PendingDecompose {
		t.Fatalf("GetPlanTree = %+v err=%v, want failed+pending", tree, err)
	}
}

func TestResetDecomposeAttempts_HumanRetryKicksAgain(t *testing.T) {
	store := newStore(t)
	now := withDecomposeClock(t, time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC))
	issues := []github.Issue{{Repo: "a/b", Number: 1, Title: "plan me", Labels: []string{"plan"}}}
	kicker := &fakeDecomposeKicker{}
	for i := 0; i <= DecomposeMaxAttempts; i++ {
		PlanIssuesFromLabels(store, kicker, issues, &recordingSink{}, nil, PlanningMinACMMLevel)
		*now = now.Add(DecomposeRekickAfter + time.Second)
	}
	epic := store.FindByExternalRef("gh-a/b#1")
	if !DecomposeFailed(epic) {
		t.Fatal("precondition: epic should be failed")
	}

	// The dashboard button resets the budget; the next cycle kicks again.
	if err := ResetDecomposeAttempts(store, epic.ID); err != nil {
		t.Fatal(err)
	}
	epic, _ = store.Get(epic.ID)
	if DecomposeFailed(epic) || DecomposeAttempts(epic) != 0 || !DecomposeKickedAt(epic).IsZero() || !DecomposePending(epic) {
		t.Fatalf("reset left state: failed=%v attempts=%d kickedAt=%v pending=%v", DecomposeFailed(epic), DecomposeAttempts(epic), DecomposeKickedAt(epic), DecomposePending(epic))
	}
	before := kicker.kicks
	res := PlanIssuesFromLabels(store, kicker, issues, &recordingSink{}, nil, PlanningMinACMMLevel)
	if res.Kicked != 1 || kicker.kicks != before+1 {
		t.Fatalf("after reset: res=%+v kicks=%d, want one more kick", res, kicker.kicks)
	}
}

func TestClearDecomposePending_DropsThrottleMarkers(t *testing.T) {
	store := newStore(t)
	epic, err := EpicFromIssue(store, github.Issue{Repo: "a/b", Number: 1, Title: "t"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordDecomposeKick(store, epic.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(epic.ID, MetaDecomposeFailed, "true"); err != nil {
		t.Fatal(err)
	}
	if err := ClearDecomposePending(store, epic.ID); err != nil {
		t.Fatal(err)
	}
	epic, _ = store.Get(epic.ID)
	for _, k := range []string{MetaDecomposePending, MetaDecomposeKickedAt, MetaDecomposeAttempts, MetaDecomposeFailed} {
		if v := epic.Meta(k); v != "" {
			t.Errorf("%s still set to %q after ClearDecomposePending", k, v)
		}
	}
}

func TestBuildPrompt_TellsArchitectToRunBdDecompose(t *testing.T) {
	store := newStore(t)
	epic, err := EpicFromIssue(store, github.Issue{Repo: "a/b", Number: 7, Title: "Big thing", URL: "https://github.com/a/b/issues/7", Labels: []string{"plan"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	p := BuildPrompt(epic)
	for _, want := range []string{
		"EPIC ID: " + epic.ID,
		"ISSUE: https://github.com/a/b/issues/7",
		"gh issue view",
		"bd decompose " + epic.ID + " --plan <file>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, p)
		}
	}
	if strings.Contains(p, "output only the plan") {
		t.Error("prompt still tells the architect to only output the plan — the loop is not closed")
	}
	// A bd-created epic with no issue gets no ISSUE line or read-first step.
	bare := BuildPrompt(&beads.Bead{ID: "e9", Title: "Local epic", Type: beads.TypeEpic})
	if strings.Contains(bare, "ISSUE:") || strings.Contains(bare, "gh issue view") {
		t.Error("bare epic prompt should not reference an issue")
	}
	if !strings.Contains(bare, "bd decompose e9 --plan <file>") {
		t.Error("bare epic prompt must still close the loop via bd decompose")
	}
}
