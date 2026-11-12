package main

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// A held contributor PR whose head conflicts with its base goes back to its
// contributor through the stage route (072, the nudge and the fix queue),
// so the hold guard neither tries the merge-from-base nor posts its
// "someone must rebase it by hand" note. The guard runs before the stage
// pass writes 072 for a newly dirty head, so it reads the author kind the
// stage engine stamped, not the 072 label. The hive's own held PR, a
// person's own PR and a PR a person parked keep the note.
func TestMaintainHeldPRMergeabilityLeavesAContributorConflictToTheStageRoute(t *testing.T) {
	cases := []struct {
		name     string
		pr       github.PullRequest
		wantNote bool
	}{
		{"contributor PR held by a sentinel alert", github.PullRequest{Author: "mallory", Labels: []string{"sentinel-alert", "author/clanker", "stage/090-ready-for-human-merge"}}, false},
		{"contributor PR held by a hold label", github.PullRequest{Author: "mallory", Labels: []string{"hold", "author/clanker"}}, false},
		{"the hive's own held PR", github.PullRequest{Author: "hive-app[bot]", AppAuthored: true, Labels: []string{"hold", "author/hive"}}, true},
		{"a person's own held PR", github.PullRequest{Author: "alice", Labels: []string{"hold", "author/human"}}, true},
		{"contributor PR a person parked with needs-human", github.PullRequest{Author: "mallory", Labels: []string{"hold", "needs-human", "author/clanker"}}, true},
		{"held PR whose author kind is not known yet", github.PullRequest{Author: "mallory", Labels: []string{"hold"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestHoldGuardStore(t)
			fake := &holdGuardServer{
				failUpdateBranch: true,
				commits:          map[int][]map[string]any{7: {ghCommit("c1", "mallory", "fix: widget")}},
			}
			client := newHoldGuardClient(t, fake)
			cfg := escalationTestConfig()
			store.Snapshot("acme/widgets", 7, "c1", nil)

			pr := tc.pr
			pr.Repo, pr.Number, pr.HeadSHA, pr.BaseSHA, pr.MergeableState = "widgets", 7, "c1", "b1", "dirty"
			_ = enforceHoldGuard(context.Background(), cfg, client, client, heldPRsActionable(pr), discardLogger())

			fake.mu.Lock()
			calls := fake.updateBranchCalls[7]
			comments := append([]string(nil), fake.comments...)
			fake.mu.Unlock()
			if !tc.wantNote {
				if calls != 0 || len(comments) != 0 {
					t.Fatalf("update-branch calls = %d, comments = %v; want the conflict left to the stage route", calls, comments)
				}
				rec, ok := store.Recorded("acme/widgets", 7)
				if !ok || rec.ConflictNoted {
					t.Fatalf("snapshot = %+v ok=%v, want it kept and no conflict noted", rec, ok)
				}
				return
			}
			if len(comments) != 1 || !strings.Contains(comments[0], "conflict") {
				t.Fatalf("comments = %v, want the guard's one conflict notice", comments)
			}
		})
	}
}

// A contributor PR that is only behind its base still gets the guard's
// merge-from-base: the stage route sends a conflict back, not a stale
// branch.
func TestMaintainHeldPRMergeabilityStillUpdatesABehindContributorPR(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{
		7: {ghCommit("c1", "mallory", "fix: widget"), ghMergeCommit("c2", "hive-bot", "Merge branch 'main' into feature", "c1", "b1")},
	}}
	client := newHoldGuardClient(t, fake)
	cfg := escalationTestConfig()
	store.Snapshot("acme/widgets", 7, "c1", nil)

	pr := github.PullRequest{Repo: "widgets", Number: 7, Author: "mallory", HeadSHA: "c1", BaseSHA: "b1", MergeableState: "behind", Labels: []string{"sentinel-alert", "author/clanker"}}
	_ = enforceHoldGuard(context.Background(), cfg, client, client, heldPRsActionable(pr), discardLogger())

	fake.mu.Lock()
	calls := fake.updateBranchCalls[7]
	fake.mu.Unlock()
	if calls != 1 {
		t.Fatalf("update-branch calls = %d, want 1 for a behind contributor PR", calls)
	}
}
