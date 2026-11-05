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
		7: {ghCommit("c1", "strategist-bot", "docs: plan"), ghMergeCommit("c2", "hive-bot", "Merge branch 'main' into feature", "c1", "b1")},
	}}
	client := newHoldGuardClient(t, fake)
	cfg := escalationTestConfig()

	if !store.Snapshot("acme/widgets", 7, "c1", []holdguard.Commit{{SHA: "c1", Author: "strategist-bot"}}) {
		t.Fatal("test setup: snapshot must record")
	}

	held := heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "c1", BaseSHA: "b1", MergeableState: "behind"})
	if got := enforceHoldGuard(context.Background(), cfg, client, client, held, discardLogger()); len(got) != 0 {
		t.Fatalf("keeping a held PR mergeable must not flag it for re-review, got %v", got)
	}

	fake.mu.Lock()
	calls := fake.updateBranchCalls[7]
	fake.mu.Unlock()
	if calls != 1 {
		t.Fatalf("update-branch calls for PR 7 = %d, want 1", calls)
	}
	fake.mu.Lock()
	expected := fake.updateBranchExpected[7]
	fake.mu.Unlock()
	if len(expected) != 1 || expected[0] != "c1" {
		t.Fatalf("update-branch expected_head_sha = %v, want [c1] (the enumerated head)", expected)
	}
	rec, ok := store.Recorded("acme/widgets", 7)
	if !ok || rec.HeadSHA != "c2" {
		t.Fatalf("baseline = %+v ok=%v, want it advanced to the merged head c2", rec, ok)
	}
	if len(fake.comments) != 0 {
		t.Fatalf("a successful hygiene push must not comment, saw %v", fake.comments)
	}
}

// ghMergeCommit is ghCommit with explicit parent SHAs, first parent first, so
// a test can serve the merge-from-base GitHub's update-branch produces.
func ghMergeCommit(sha, login, msg string, parents ...string) map[string]any {
	c := ghCommit(sha, login, msg)
	ps := make([]map[string]string, 0, len(parents))
	for _, p := range parents {
		ps = append(ps, map[string]string{"sha": p})
	}
	c["parents"] = ps
	return c
}

// #14534: a head that moved after the update request but is not the merge of
// the base into the requested head (a contributor push racing the refetch, or
// a merge of some other branch) must not become the baseline.
func TestMaintainHeldPRMergeabilityKeepsBaselineWhenNewHeadIsNotTheHygieneMerge(t *testing.T) {
	tests := []struct {
		name string
		head map[string]any
	}{
		{"contributor push on top of the held head", ghMergeCommit("c2", "contributor", "feat: sneak in", "c1")},
		{"merge of a non-base commit into the held head", ghMergeCommit("c2", "contributor", "Merge branch 'evil'", "c1", "x9")},
		{"merge into a head other than the requested one", ghMergeCommit("c2", "contributor", "Merge branch 'main'", "x8", "b1")},
		{"head with no parent information", ghCommit("c2", "contributor", "Merge branch 'main'")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestHoldGuardStore(t)
			fake := &holdGuardServer{commits: map[int][]map[string]any{
				7: {ghCommit("c1", "strategist-bot", "docs: plan"), tt.head},
			}}
			client := newHoldGuardClient(t, fake)
			cfg := escalationTestConfig()
			if !store.Snapshot("acme/widgets", 7, "c1", []holdguard.Commit{{SHA: "c1", Author: "strategist-bot"}}) {
				t.Fatal("test setup: snapshot must record")
			}

			held := heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "c1", BaseSHA: "b1", BaseRef: "main", MergeableState: "behind"})
			_ = enforceHoldGuard(context.Background(), cfg, client, client, held, discardLogger())

			fake.mu.Lock()
			calls := fake.updateBranchCalls[7]
			fake.mu.Unlock()
			if calls != 1 {
				t.Fatalf("update-branch calls for PR 7 = %d, want 1", calls)
			}
			rec, ok := store.Recorded("acme/widgets", 7)
			if !ok || rec.HeadSHA != "c1" {
				t.Fatalf("baseline = %+v ok=%v, want it left pinned at c1: c2 is not proven to be the hive's merge", rec, ok)
			}
			if drift := holdguard.Diff(rec, "c2", nil); !drift.Moved {
				t.Fatal("the unproven head must still read as drift at lift time")
			}
		})
	}
}

// #14534 review: update-branch merges the base tip at call time, which may be
// newer than the enumerated BaseSHA, and the polling enumeration may not carry
// a BaseSHA at all. The hive's own merge must still advance the baseline when
// the live base branch contains the merged parent.
func TestMaintainHeldPRMergeabilityAdvancesWhenMergedBaseIsNewerThanEnumerated(t *testing.T) {
	tests := []struct {
		name    string
		baseSHA string
	}{
		{"base moved after enumeration", "b1"},
		{"base SHA not enumerated", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestHoldGuardStore(t)
			fake := &holdGuardServer{
				commits: map[int][]map[string]any{
					7: {ghCommit("c1", "strategist-bot", "docs: plan"), ghMergeCommit("c2", "hive-bot", "Merge branch 'main' into feature", "c1", "b2")},
				},
				baseContains: map[string]bool{"b1": true, "b2": true},
			}
			client := newHoldGuardClient(t, fake)
			cfg := escalationTestConfig()
			if !store.Snapshot("acme/widgets", 7, "c1", []holdguard.Commit{{SHA: "c1", Author: "strategist-bot"}}) {
				t.Fatal("test setup: snapshot must record")
			}

			held := heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "c1", BaseSHA: tt.baseSHA, BaseRef: "main", MergeableState: "behind"})
			if got := enforceHoldGuard(context.Background(), cfg, client, client, held, discardLogger()); len(got) != 0 {
				t.Fatalf("keeping a held PR mergeable must not flag it for re-review, got %v", got)
			}
			rec, ok := store.Recorded("acme/widgets", 7)
			if !ok || rec.HeadSHA != "c2" {
				t.Fatalf("baseline = %+v ok=%v, want it advanced to the hive's merge c2", rec, ok)
			}
			fake.mu.Lock()
			compares := fake.compareCalls
			fake.mu.Unlock()
			if compares != 1 {
				t.Fatalf("compare calls = %d, want 1 live base-membership check", compares)
			}
			if len(fake.comments) != 0 {
				t.Fatalf("a successful hygiene push must not comment, saw %v", fake.comments)
			}
		})
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
	fake.commits[7] = append(fake.commits[7], ghMergeCommit("c2", "hive-bot", "Merge branch 'main' into feature", "c1", "b1"))
	fake.mu.Unlock()
	heldResolved := heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "c1", BaseSHA: "b1", MergeableState: "behind"})
	_ = enforceHoldGuard(context.Background(), cfg, client, client, heldResolved, discardLogger())
	rec, ok = store.Recorded("acme/widgets", 7)
	if !ok || rec.ConflictNoted {
		t.Fatalf("snapshot = %+v ok=%v, want ConflictNoted cleared after the branch merges clean", rec, ok)
	}
	if rec.HeadSHA != "c2" {
		t.Fatalf("baseline = %q, want it advanced to c2 once the conflict clears", rec.HeadSHA)
	}
}

// #14537: the update is pinned to the enumerated head, so a contributor push
// landing between enumeration and the update makes GitHub refuse it instead of
// stacking the hive's merge on unevaluated commits. That refusal is a moved
// head, not a merge conflict: no conflict notice, baseline untouched.
func TestMaintainHeldPRMergeabilityRefusedWhenHeadMovedSinceEnumeration(t *testing.T) {
	for _, state := range []string{"behind", "dirty"} {
		t.Run(state, func(t *testing.T) {
			store := newTestHoldGuardStore(t)
			fake := &holdGuardServer{
				commits:  map[int][]map[string]any{7: {ghCommit("c1", "strategist-bot", "docs: plan"), ghCommit("c9", "contributor", "feat: late push")}},
				liveHead: map[int]string{7: "c9"},
			}
			client := newHoldGuardClient(t, fake)
			cfg := escalationTestConfig()
			store.Snapshot("acme/widgets", 7, "c1", []holdguard.Commit{{SHA: "c1", Author: "strategist-bot"}})

			held := heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "c1", BaseSHA: "b1", MergeableState: state})
			_ = enforceHoldGuard(context.Background(), cfg, client, client, held, discardLogger())

			fake.mu.Lock()
			calls := fake.updateBranchCalls[7]
			comments := append([]string(nil), fake.comments...)
			fake.mu.Unlock()
			if calls != 1 {
				t.Fatalf("update-branch calls for PR 7 = %d, want 1", calls)
			}
			for _, c := range comments {
				if strings.Contains(c, "conflict") {
					t.Fatalf("a moved head must not be reported as a merge conflict, saw %q", c)
				}
			}
			rec, ok := store.Recorded("acme/widgets", 7)
			if !ok || rec.HeadSHA != "c1" || rec.ConflictNoted {
				t.Fatalf("snapshot = %+v ok=%v, want baseline pinned at c1 and no conflict noted", rec, ok)
			}
		})
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

// projectbluefin/utah#1097: Renovate stops rebasing a branch once another
// identity commits to it, so the hive's merge-from-base on a held Renovate PR
// stranded it behind its base. Branches a dependency bot owns are left to the
// bot (no update, no conflict notice, baseline pinned); an ordinary held PR
// still gets the base merged in.
func TestMaintainHeldPRMergeabilityLeavesDependencyBotBranchesToTheBot(t *testing.T) {
	for _, tt := range []struct {
		name      string
		headRef   string
		author    string
		state     string
		wantCalls int
		wantHead  string
	}{
		{name: "renovate head ref behind", headRef: "renovate/actions-checkout-6.x", author: "projectbluefin-deps[bot]", state: "behind", wantHead: "c1"},
		{name: "renovate head ref dirty", headRef: "renovate/actions-checkout-6.x", author: "projectbluefin-deps[bot]", state: "dirty", wantHead: "c1"},
		{name: "renovate author", headRef: "deps/bump", author: "renovate[bot]", state: "behind", wantHead: "c1"},
		{name: "mergeraptor author", headRef: "deps/bump", author: "mergeraptor[bot]", state: "behind", wantHead: "c1"},
		{name: "ordinary PR", headRef: "feat/renovate-config", author: "castrojo", state: "behind", wantCalls: 1, wantHead: "c2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestHoldGuardStore(t)
			fake := &holdGuardServer{
				failUpdateBranch: tt.state == "dirty",
				commits: map[int][]map[string]any{
					7: {ghCommit("c1", tt.author, "chore(deps): update"), ghMergeCommit("c2", "hive-bot", "Merge branch 'main' into feature", "c1", "b1")},
				},
			}
			client := newHoldGuardClient(t, fake)
			cfg := escalationTestConfig()
			store.Snapshot("acme/widgets", 7, "c1", []holdguard.Commit{{SHA: "c1", Author: tt.author}})

			held := heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, Author: tt.author, HeadRef: tt.headRef, HeadSHA: "c1", BaseSHA: "b1", MergeableState: tt.state})
			if got := enforceHoldGuard(context.Background(), cfg, client, client, held, discardLogger()); len(got) != 0 {
				t.Fatalf("held-PR upkeep must not flag it for re-review, got %v", got)
			}

			fake.mu.Lock()
			calls := fake.updateBranchCalls[7]
			comments := append([]string(nil), fake.comments...)
			fake.mu.Unlock()
			if calls != tt.wantCalls {
				t.Fatalf("update-branch calls for PR 7 = %d, want %d", calls, tt.wantCalls)
			}
			if len(comments) != 0 {
				t.Fatalf("held-PR upkeep must not comment here, saw %v", comments)
			}
			rec, ok := store.Recorded("acme/widgets", 7)
			if !ok || rec.HeadSHA != tt.wantHead || rec.ConflictNoted {
				t.Fatalf("snapshot = %+v ok=%v, want baseline %s and no conflict noted", rec, ok, tt.wantHead)
			}
		})
	}
}
