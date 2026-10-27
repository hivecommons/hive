package main

// Tests for the hold guard's keep-mergeable update when GitHub does not
// complete it: one attempt per head and base, a wait on an accepted (202)
// update, and a stop with a person-facing note on a refusal the same token
// would get again (projectbluefin/fsdk-containers#474,
// projectbluefin/ghostscript-printer-app#152, projectbluefin/ps-printer-app#136).

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/convergence/proof"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/holdguard"
)

func (s *holdGuardServer) updateCalls(number int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateBranchCalls[number]
}

func (s *holdGuardServer) commentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.comments)
}

func (s *holdGuardServer) setCommits(number int, commits ...map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commits[number] = commits
}

// withJournal installs the mutation journal a hive runs with by default
// (convergence mode shadow), which records every external effect and replays
// an identical one as "already applied".
func withJournal(t *testing.T, c *github.Client) {
	t.Helper()
	dir := t.TempDir()
	ledger, err := mutation.OpenLedger(filepath.Join(dir, "mutations.json"), 1)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := mutation.OpenJournal(filepath.Join(dir, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	c.SetMutationBoundary(&mutation.Boundary{Executor: mutation.Executor{Ledger: ledger, Journal: journal, Mode: proof.ModeShadow}, Holder: "test"})
}

// fsdk-containers#474: the first update landed, and every later one for the
// same PR replayed from the journal as "already applied" without reaching
// GitHub, so the branch stayed behind for hours. A new head or base is a new
// attempt and must reach GitHub.
func TestMaintainHeldPRMergeabilityAsksGitHubAgainAfterAJournaledUpdate(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{
		7: {ghCommit("c1", "contributor", "fix: scan"), ghCommit("m1", "hive-bot", "Merge branch 'main' into fix")},
	}}
	client := newHoldGuardClient(t, fake)
	withJournal(t, client)
	cfg := escalationTestConfig()
	store.Snapshot("acme/widgets", 7, "c1", []holdguard.Commit{{SHA: "c1", Author: "contributor"}})

	_ = enforceHoldGuard(context.Background(), cfg, client, client,
		heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "c1", BaseSHA: "b1", MergeableState: "behind"}), discardLogger())
	if rec, _ := store.Recorded("acme/widgets", 7); fake.updateCalls(7) != 1 || rec.HeadSHA != "m1" {
		t.Fatalf("first update: calls = %d, baseline = %q; want 1 and m1", fake.updateCalls(7), rec.HeadSHA)
	}

	// Main moves again; the PR is behind at the hive's own merge commit.
	fake.setCommits(7, ghCommit("c1", "contributor", "fix: scan"), ghCommit("m1", "hive-bot", "Merge branch 'main' into fix"),
		ghCommit("m2", "hive-bot", "Merge branch 'main' into fix"))
	_ = enforceHoldGuard(context.Background(), cfg, client, client,
		heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "m1", BaseSHA: "b2", MergeableState: "behind"}), discardLogger())
	if rec, _ := store.Recorded("acme/widgets", 7); fake.updateCalls(7) != 2 || rec.HeadSHA != "m2" {
		t.Fatalf("second update: calls = %d, baseline = %q; want GitHub asked again and the baseline at m2", fake.updateCalls(7), rec.HeadSHA)
	}
	if fake.commentCount() != 0 {
		t.Fatalf("successful updates must not comment, saw %v", fake.comments)
	}
}

// ghostscript-printer-app#152 / ps-printer-app#136: GitHub answered 202 and
// pushed the merge about two seconds later, after the guard had already read
// the commit list. An accepted update whose head is not visible yet is waited
// on, not re-sent; one that never lands stops with a note.
func TestMaintainHeldPRMergeabilityWaitsOnAnAcceptedUpdate(t *testing.T) {
	store := newTestHoldGuardStore(t)
	now := time.Date(2026, 10, 26, 18, 34, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return now })
	fake := &holdGuardServer{commits: map[int][]map[string]any{7: {ghCommit("c1", "renovate", "chore(deps): bump")}}}
	client := newHoldGuardClient(t, fake)
	cfg := escalationTestConfig()
	store.Snapshot("acme/widgets", 7, "c1", nil)
	behind := heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "c1", BaseSHA: "b1", MergeableState: "behind"})

	_ = enforceHoldGuard(context.Background(), cfg, client, client, behind, discardLogger())
	rec, _ := store.Recorded("acme/widgets", 7)
	if fake.updateCalls(7) != 1 || rec.UpdateRequestedHeadSHA != "c1" || rec.HeadSHA != "c1" {
		t.Fatalf("accepted update: calls = %d, record = %+v; want 1 call, a request at c1, the baseline unchanged", fake.updateCalls(7), rec)
	}

	// Still within the confirmation window: no new request, no note.
	now = now.Add(holdguard.UpdateConfirmWindow - time.Minute)
	_ = enforceHoldGuard(context.Background(), cfg, client, client, behind, discardLogger())
	if fake.updateCalls(7) != 1 || fake.commentCount() != 0 {
		t.Fatalf("while waiting: calls = %d, comments = %d; want 1 and 0", fake.updateCalls(7), fake.commentCount())
	}

	// The window passes with the head unmoved: stop and tell a person once.
	now = now.Add(2 * time.Minute)
	for i := 0; i < 3; i++ {
		_ = enforceHoldGuard(context.Background(), cfg, client, client, behind, discardLogger())
	}
	rec, _ = store.Recorded("acme/widgets", 7)
	if fake.updateCalls(7) != 1 || rec.UpdateStoppedReason != holdguard.UpdateStopNotLanded || rec.UpdateRequestedHeadSHA != "" {
		t.Fatalf("after the window: calls = %d, record = %+v; want no new call and a not-landed stop", fake.updateCalls(7), rec)
	}
	if fake.commentCount() != 1 || !strings.Contains(fake.comments[0], "had not moved") || !strings.Contains(fake.comments[0], "Update branch") {
		t.Fatalf("want one not-landed note naming the Update branch step, saw %v", fake.comments)
	}

	// Main moves: a new attempt.
	_ = enforceHoldGuard(context.Background(), cfg, client, client,
		heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "c1", BaseSHA: "b2", MergeableState: "behind"}), discardLogger())
	if fake.updateCalls(7) != 2 {
		t.Fatalf("after the base moved: calls = %d, want 2", fake.updateCalls(7))
	}
}

// The usual slow case: the merge lands between ticks. The request settles
// once the head moves, and the baseline is not advanced to a head the guard
// cannot prove is its own merge.
func TestMaintainHeldPRMergeabilitySettlesAnUpdateThatLandsLater(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{7: {ghCommit("c1", "renovate", "chore(deps): bump")}}}
	client := newHoldGuardClient(t, fake)
	cfg := escalationTestConfig()
	store.Snapshot("acme/widgets", 7, "c1", nil)

	_ = enforceHoldGuard(context.Background(), cfg, client, client,
		heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "c1", BaseSHA: "b1", MergeableState: "behind"}), discardLogger())
	if rec, _ := store.Recorded("acme/widgets", 7); rec.UpdateRequestedHeadSHA != "c1" {
		t.Fatalf("record = %+v, want a request at c1", rec)
	}

	_ = enforceHoldGuard(context.Background(), cfg, client, client,
		heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "m1", BaseSHA: "b1", MergeableState: "clean"}), discardLogger())
	rec, _ := store.Recorded("acme/widgets", 7)
	if rec.UpdateRequestedHeadSHA != "" || rec.HeadSHA != "c1" {
		t.Fatalf("record = %+v, want the request settled and the baseline left at c1", rec)
	}
	if fake.updateCalls(7) != 1 || fake.commentCount() != 0 {
		t.Fatalf("calls = %d, comments = %d; want 1 and 0", fake.updateCalls(7), fake.commentCount())
	}
}

// A fork PR the App cannot push to: known up front when "allow edits by
// maintainers" is off (no call at all), or from GitHub's refusal when it is
// on but the push is still refused. Either way the guard stops for that head
// and base and tells a person once.
func TestMaintainHeldPRMergeabilityStopsOnAForkItCannotPushTo(t *testing.T) {
	cases := []struct {
		name      string
		canModify bool
		status    int
		message   string
		wantCalls int
	}{
		{name: "maintainer edits off", canModify: false, wantCalls: 0},
		{name: "push refused (422)", canModify: true, status: http.StatusUnprocessableEntity, message: "You do not have permission to push to the head repository; maintainer edits may be disabled", wantCalls: 1},
		{name: "push refused (403)", canModify: true, status: http.StatusForbidden, message: "Resource not accessible by integration", wantCalls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestHoldGuardStore(t)
			fake := &holdGuardServer{
				refuseStatus:  tc.status,
				refuseMessage: tc.message,
				commits:       map[int][]map[string]any{7: {ghCommit("c1", "contributor", "fix")}},
			}
			client := newHoldGuardClient(t, fake)
			cfg := escalationTestConfig()
			store.Snapshot("acme/widgets", 7, "c1", nil)
			held := heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "c1", BaseSHA: "b1", MergeableState: "behind",
				FromFork: true, MaintainerCanModify: tc.canModify})

			for i := 0; i < 3; i++ {
				_ = enforceHoldGuard(context.Background(), cfg, client, client, held, discardLogger())
			}
			rec, _ := store.Recorded("acme/widgets", 7)
			if fake.updateCalls(7) != tc.wantCalls || rec.UpdateStoppedReason != holdguard.UpdateStopForkNotEditable {
				t.Fatalf("calls = %d, record = %+v; want %d calls and a fork-not-editable stop", fake.updateCalls(7), rec, tc.wantCalls)
			}
			if fake.commentCount() != 1 || !strings.Contains(fake.comments[0], "fork") {
				t.Fatalf("want one note naming the fork, saw %v", fake.comments)
			}
		})
	}
}

// A refused head is not asked again until the head or main moves, and a
// transient failure is retried on the next pass.
func TestMaintainHeldPRMergeabilityRetriesRefusedHeadOnlyAfterHeadOrBaseMoves(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{workflowRefused: true, commits: map[int][]map[string]any{7: {ghCommit("c1", "contributor", "fix")}}}
	client := newHoldGuardClient(t, fake)
	cfg := escalationTestConfig()
	store.Snapshot("acme/widgets", 7, "c1", nil)
	pass := func(head, base string) {
		_ = enforceHoldGuard(context.Background(), cfg, client, client,
			heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: head, BaseSHA: base, MergeableState: "behind"}), discardLogger())
	}

	for i := 0; i < 5; i++ {
		pass("c1", "b1")
	}
	if fake.updateCalls(7) != 1 {
		t.Fatalf("calls on an unchanged refused head and base = %d, want 1", fake.updateCalls(7))
	}
	pass("c1", "b2")
	pass("c1", "b2")
	if fake.updateCalls(7) != 2 {
		t.Fatalf("calls after main moved = %d, want 2", fake.updateCalls(7))
	}
	pass("c2", "b2")
	pass("c2", "b2")
	if fake.updateCalls(7) != 3 {
		t.Fatalf("calls after the head moved = %d, want 3", fake.updateCalls(7))
	}

	// A transient failure is not a stop.
	fake.mu.Lock()
	fake.workflowRefused = false
	fake.refuseStatus, fake.refuseMessage = http.StatusBadGateway, "Server Error"
	fake.mu.Unlock()
	pass("c3", "b2")
	pass("c3", "b2")
	if rec, _ := store.Recorded("acme/widgets", 7); fake.updateCalls(7) != 5 || rec.UpdateStoppedHeadSHA == "c3" {
		t.Fatalf("transient failure: calls = %d, record = %+v; want a retry each pass and no stop", fake.updateCalls(7), rec)
	}
}

// A record written by a build that only knew the workflows refusal keeps
// stopping on its head only while no base is known; a known base retries.
func TestMaintainHeldPRMergeabilityHonoursLegacyWorkflowRefusal(t *testing.T) {
	rec := holdguard.Entry{WorkflowRefusedHeadSHA: "c1"}
	if reason, ok := rec.UpdateStoppedFor("c1", ""); !ok || reason != holdguard.UpdateStopWorkflowPermission {
		t.Fatalf("UpdateStoppedFor(c1) = %q, %v; want the legacy workflows refusal", reason, ok)
	}
	if _, ok := rec.UpdateStoppedFor("c1", "b9"); ok {
		t.Fatal("a known base must re-evaluate a legacy record")
	}
	if _, ok := rec.UpdateStoppedFor("c2", "b9"); ok {
		t.Fatal("a moved head must not be stopped by the legacy record")
	}
}

// A PR returned to its contributor (072) is theirs to push to; the guard
// does not merge the base in underneath them.
func TestMaintainHeldPRMergeabilityLeavesAReturnedPRAlone(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{7: {ghCommit("c1", "contributor", "fix")}}}
	client := newHoldGuardClient(t, fake)
	cfg := escalationTestConfig()
	store.Snapshot("acme/widgets", 7, "c1", nil)

	_ = enforceHoldGuard(context.Background(), cfg, client, client,
		heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "c1", BaseSHA: "b1", MergeableState: "behind",
			FromFork: true, MaintainerCanModify: true, Labels: []string{"sentinel-alert", "stage/072-returned-to-contributor"}}), discardLogger())
	if fake.updateCalls(7) != 0 || fake.commentCount() != 0 {
		t.Fatalf("calls = %d, comments = %d; want the returned PR left alone", fake.updateCalls(7), fake.commentCount())
	}
}

// An accepted update that lands between ticks is settled without moving the
// baseline, so the hive's merge commit is not in the snapshot's commit set.
// It still never reads as drift at lift time when the hive saw it while the
// PR was held: the snapshot step records that head before the update
// settles, and a human lift on an observed head is accepted (#13603). Only
// a lift that lands before the hive sees the new head re-holds, which fails
// toward a fresh review.
func TestEnforceHoldGuardSettledUpdateIsNotDriftAtLift(t *testing.T) {
	lifted := []map[string]any{{
		"event":      "labeled",
		"label":      map[string]string{"name": "hold"},
		"actor":      map[string]string{"login": "kubestellar-hive[bot]", "type": "Bot"},
		"created_at": "2026-10-22T05:00:00Z",
	}, {
		"event":     "committed",
		"sha":       "m1",
		"committer": map[string]string{"name": "GitHub", "date": "2026-10-22T10:00:00Z"},
	}, {
		"event":      "unlabeled",
		"label":      map[string]string{"name": "hold"},
		"actor":      map[string]string{"login": "clubanderson", "type": "User"},
		"created_at": "2026-10-22T18:19:00Z",
	}}
	held := func(head, state string) *github.ActionableResult {
		a := heldActionable(github.HoldItem{Repo: "widgets", Number: 7, Type: "pr", HeadSHA: head})
		a.PRs.Held = []github.PullRequest{{Repo: "widgets", Number: 7, HeadSHA: head, BaseSHA: "b1", MergeableState: state}}
		return a
	}
	for _, tc := range []struct {
		name         string
		seenWhenHeld bool
	}{
		{"merge seen while held", true},
		{"lift before the merge was seen", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestHoldGuardStore(t)
			fake := &holdGuardServer{commits: map[int][]map[string]any{7: {ghCommit("c1", "renovate", "chore(deps): bump")}}}
			fake.timeline = map[int][]map[string]any{7: lifted}
			client := newHoldGuardClient(t, fake)
			client.SetAppBotLogin("kubestellar-hive[bot]")
			cfg := escalationTestConfig()

			_ = enforceHoldGuard(context.Background(), cfg, client, client, held("c1", "behind"), discardLogger())
			if rec, _ := store.Recorded("acme/widgets", 7); rec.UpdateRequestedHeadSHA != "c1" {
				t.Fatalf("record = %+v, want an accepted update awaited at c1", rec)
			}
			fake.setCommits(7, ghCommit("c1", "renovate", "chore(deps): bump"), ghCommit("m1", "kubestellar-hive[bot]", "Merge branch 'main' into deps"))
			if tc.seenWhenHeld {
				_ = enforceHoldGuard(context.Background(), cfg, client, client, held("m1", "clean"), discardLogger())
				if rec, _ := store.Recorded("acme/widgets", 7); rec.UpdateRequestedHeadSHA != "" || rec.HeadSHA != "c1" || rec.ObservedHeldHeadSHA != "m1" {
					t.Fatalf("record = %+v, want the update settled, baseline c1, observed head m1", rec)
				}
			}

			got := enforceHoldGuard(context.Background(), cfg, client, client,
				actionableWith(github.PullRequest{Repo: "widgets", Number: 7, Author: "renovate", HeadSHA: "m1"}), discardLogger())
			_, tracked := store.Recorded("acme/widgets", 7)
			if tc.seenWhenHeld {
				if len(got) != 0 || fake.commentCount() != 0 || len(fake.labels) != 0 || tracked {
					t.Fatalf("excluded=%v comments=%v labels=%v tracked=%v; a lift on the merge the hive saw while held must reopen the lanes quietly", got, fake.comments, fake.labels, tracked)
				}
				return
			}
			if len(got) == 0 || fake.commentCount() != 1 {
				t.Fatalf("excluded=%v comments=%v; a head the hive never saw while held must re-hold for review", got, fake.comments)
			}
		})
	}
}
