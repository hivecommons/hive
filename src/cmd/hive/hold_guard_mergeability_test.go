package main

// Tests for #10437: a held PR must stay mergeable, not merely frozen. These
// exercise maintainHeldPRMergeability through enforceHoldGuard, mirroring the
// integration style of hold_guard_test.go.

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/holdguard"
)

func heldPRsActionable(prs ...github.PullRequest) *github.ActionableResult {
	a := &github.ActionableResult{}
	a.PRs.Held = prs
	return a
}

func TestMaintainHeldPRMergeabilityUpdatesBehindBranchAndAdvancesBaseline(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{
		7: {ghCommit("c1", "strategist-bot", "docs: plan"), ghCommit("c2", "hive-bot", "Merge branch 'main' into feature")},
	}}
	client := newHoldGuardClient(t, fake)
	cfg := escalationTestConfig()

	if !store.Snapshot("acme/widgets", 7, "c1", []holdguard.Commit{{SHA: "c1", Author: "strategist-bot"}}) {
		t.Fatal("test setup: snapshot must record")
	}

	held := heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "c1", MergeableState: "behind"})
	if got := enforceHoldGuard(context.Background(), cfg, client, client, held, discardLogger()); len(got) != 0 {
		t.Fatalf("keeping a held PR mergeable must not flag it for re-review, got %v", got)
	}

	fake.mu.Lock()
	calls := fake.updateBranchCalls[7]
	fake.mu.Unlock()
	if calls != 1 {
		t.Fatalf("update-branch calls for PR 7 = %d, want 1", calls)
	}
	rec, ok := store.Recorded("acme/widgets", 7)
	if !ok || rec.HeadSHA != "c2" {
		t.Fatalf("baseline = %+v ok=%v, want it advanced to the merged head c2", rec, ok)
	}
	if len(fake.comments) != 0 {
		t.Fatalf("a successful hygiene push must not comment, saw %v", fake.comments)
	}
}

func TestMaintainHeldPRMergeabilityNotesUnresolvableConflictOnce(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{
		failUpdateBranch: true,
		commits:          map[int][]map[string]any{7: {ghCommit("c1", "strategist-bot", "docs: plan")}},
	}
	client := newHoldGuardClient(t, fake)
	cfg := escalationTestConfig()
	store.Snapshot("acme/widgets", 7, "c1", nil)

	held := heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "c1", MergeableState: "dirty"})
	_ = enforceHoldGuard(context.Background(), cfg, client, client, held, discardLogger())

	if len(fake.comments) != 1 {
		t.Fatalf("want one conflict notice, got %v", fake.comments)
	}
	if !strings.Contains(fake.comments[0], "conflict") {
		t.Fatalf("conflict comment = %q, want it to name the conflict", fake.comments[0])
	}
	rec, ok := store.Recorded("acme/widgets", 7)
	if !ok || !rec.ConflictNoted {
		t.Fatalf("snapshot = %+v ok=%v, want ConflictNoted set", rec, ok)
	}

	// A retried tick with the same unresolved conflict must not re-comment.
	_ = enforceHoldGuard(context.Background(), cfg, client, client, held, discardLogger())
	if len(fake.comments) != 1 {
		t.Fatalf("conflict notice must be deduplicated, saw %v", fake.comments)
	}

	// The conflict resolves (a human merges by hand); the next behind push
	// succeeds, and the stale conflict notice must clear.
	fake.mu.Lock()
	fake.failUpdateBranch = false
	fake.commits[7] = append(fake.commits[7], ghCommit("c2", "hive-bot", "Merge branch 'main' into feature"))
	fake.mu.Unlock()
	heldResolved := heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "c1", MergeableState: "behind"})
	_ = enforceHoldGuard(context.Background(), cfg, client, client, heldResolved, discardLogger())
	rec, ok = store.Recorded("acme/widgets", 7)
	if !ok || rec.ConflictNoted {
		t.Fatalf("snapshot = %+v ok=%v, want ConflictNoted cleared after the branch merges clean", rec, ok)
	}
	if rec.HeadSHA != "c2" {
		t.Fatalf("baseline = %q, want it advanced to c2 once the conflict clears", rec.HeadSHA)
	}
}

func TestMaintainHeldPRMergeabilitySkipsUntrackedPR(t *testing.T) {
	newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{42: {ghCommit("z1", "strategist-bot", "docs: plan")}}}
	client := newHoldGuardClient(t, fake)
	cfg := escalationTestConfig()

	// No snapshot recorded for PR 42 — the guard must never touch its branch
	// before the snapshot step has pinned a baseline for it.
	held := heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 42, HeadSHA: "z1", MergeableState: "behind"})
	_ = enforceHoldGuard(context.Background(), cfg, client, client, held, discardLogger())

	fake.mu.Lock()
	calls := fake.updateBranchCalls[42]
	fake.mu.Unlock()
	if calls != 0 {
		t.Fatalf("update-branch calls for an untracked PR = %d, want 0", calls)
	}
}

func TestMaintainHeldPRMergeabilityIgnoresCleanPR(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{7: {ghCommit("c1", "strategist-bot", "docs: plan")}}}
	client := newHoldGuardClient(t, fake)
	cfg := escalationTestConfig()
	store.Snapshot("acme/widgets", 7, "c1", nil)

	held := heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "c1", MergeableState: "clean"})
	_ = enforceHoldGuard(context.Background(), cfg, client, client, held, discardLogger())

	fake.mu.Lock()
	calls := fake.updateBranchCalls[7]
	fake.mu.Unlock()
	if calls != 0 {
		t.Fatalf("update-branch calls for a clean held PR = %d, want 0", calls)
	}
}
