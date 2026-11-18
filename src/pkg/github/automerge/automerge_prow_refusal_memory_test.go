package automerge

// Repo-level memory that Prow refuses the App's /ok-to-test
// (hivecommons/hive#18608): once Prow refused the App anywhere in a
// repository, a later fork PR there gets the needs-human note without the
// first command being posted.

import (
	"context"
	"strings"
	"testing"
	"time"
)

const prowRefusalReply = "@" + testHiveAppBotLogin + ": Cannot trigger testing until a trusted user reviews the PR and leaves an `/ok-to-test` message."

// refusalOnEarlierPR is the repository-wide listing after Prow refused the App
// on some other PR.
func refusalOnEarlierPR() []map[string]any {
	return []map[string]any{prowComment(90, prowRefusalReply)}
}

// runProwRefusalEngines sweeps PR 7 with one fresh engine per entry in
// restarts, so each is a restarted process with an empty in-process memory.
func runProwRefusalEngines(t *testing.T, fx *contributorLaneFixture, restarts int) {
	t.Helper()
	api := newContributorLaneAPI(t, fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	for run := 0; run < restarts; run++ {
		engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock)})
		if _, _, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy); err != nil {
			t.Fatalf("run %d err = %v", run, err)
		}
		clock.advance(workflowApprovalRecheck + time.Minute)
	}
}

func countCommand(fx *contributorLaneFixture) int {
	n := 0
	for _, body := range fx.allComments {
		if body == workflowApprovalCommandBody("sha7") {
			n++
		}
	}
	return n
}

func TestKnownRefusingRepoGetsNoFirstOkToTest(t *testing.T) {
	fx := prowOkToTestFixture()
	fx.repoComments = refusalOnEarlierPR()
	runProwRefusalEngines(t, &fx, 1)
	if got := countCommand(&fx); got != 0 {
		t.Fatalf("ok-to-test posts = %d (%q), want none in a repository Prow already refused the App in", got, fx.allComments)
	}
	if fx.comments != 1 {
		t.Fatalf("comments = %d %q, want exactly the one needs-human note", fx.comments, fx.allComments)
	}
	note := fx.allComments[0]
	if !strings.HasPrefix(note, workflowApprovalRejectedMarker) || !strings.Contains(note, "needs-human") || !strings.Contains(note, testHiveAppBotLogin) {
		t.Fatalf("note = %q, want the rejected marker and a needs-human note naming the App", note)
	}
	if strings.Contains(note, prowOkToTestRejection) {
		t.Fatalf("note = %q quotes Prow's refusal, which would renew the memory forever", note)
	}
	for _, line := range strings.Split(note, "\n") {
		if strings.TrimSpace(line) == workflowApprovalCommand {
			t.Fatalf("note = %q carries a bare %s line Prow would act on", note, workflowApprovalCommand)
		}
	}
}

func TestUnknownRepoStillPostsOkToTestOnce(t *testing.T) {
	fx := prowOkToTestFixture()
	runProwRefusalEngines(t, &fx, 2)
	if got := countCommand(&fx); got != 1 {
		t.Fatalf("ok-to-test posts = %d (%q), want exactly one in a repository with no refusal on record", got, fx.allComments)
	}
	if fx.repoCommentLists == 0 {
		t.Fatalf("repository comments were never read, so the memory was not consulted")
	}
}

// The memory is derived from the repository's comments, so a restarted hive
// (empty in-process state) still skips the command, and posts the note once.
func TestProwRefusalMemorySurvivesRestart(t *testing.T) {
	fx := prowOkToTestFixture()
	fx.repoComments = refusalOnEarlierPR()
	runProwRefusalEngines(t, &fx, 3)
	if got := countCommand(&fx); got != 0 {
		t.Fatalf("ok-to-test posts = %d (%q), want none across restarts", got, fx.allComments)
	}
	if fx.comments != 1 {
		t.Fatalf("comments = %d %q, want one note across three restarts", fx.comments, fx.allComments)
	}
}

// The hive's own needs-human note from another PR is evidence too.
func TestProwRefusalMemoryFromHiveNote(t *testing.T) {
	fx := prowOkToTestFixture()
	fx.repoComments = []map[string]any{appComment(91, workflowApprovalUntrustedNote(testHiveAppBotLogin, true))}
	runProwRefusalEngines(t, &fx, 1)
	if got := countCommand(&fx); got != 0 {
		t.Fatalf("ok-to-test posts = %d, want none", got)
	}
}

// A refusal addressed to someone else is not a refusal of the App.
func TestProwRefusalForAnotherUserIsNotMemory(t *testing.T) {
	fx := prowOkToTestFixture()
	fx.repoComments = []map[string]any{prowComment(92, "@someone-else: Cannot trigger testing until a trusted user reviews the PR.")}
	runProwRefusalEngines(t, &fx, 1)
	if got := countCommand(&fx); got != 1 {
		t.Fatalf("ok-to-test posts = %d, want 1", got)
	}
}

// Once the repository carries the self-contained hive-ok-to-test workflow the
// memory clears and the lane behaves as before.
func TestProwRefusalMemoryClearsWhenWorkflowInstalled(t *testing.T) {
	fx := prowOkToTestFixture()
	fx.repoComments = refusalOnEarlierPR()
	fx.workflowInstalled = true
	runProwRefusalEngines(t, &fx, 1)
	if got := countCommand(&fx); got != 1 {
		t.Fatalf("ok-to-test posts = %d (%q), want 1 once the workflow is installed", got, fx.allComments)
	}
}

func TestProwRefusalMemoryIsReadOncePerTTL(t *testing.T) {
	fx := prowOkToTestFixture()
	fx.repoComments = refusalOnEarlierPR()
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock)})
	for i := 0; i < 3; i++ {
		engine.prowRefusesApp(context.Background(), "acme/widget", "acme", "widget")
	}
	if fx.repoCommentLists != 1 {
		t.Fatalf("repository comment reads = %d, want 1 within the TTL", fx.repoCommentLists)
	}
	clock.advance(okToTestWorkflowCheckTTL + time.Minute)
	engine.prowRefusesApp(context.Background(), "acme/widget", "acme", "widget")
	if fx.repoCommentLists != 2 {
		t.Fatalf("repository comment reads = %d, want 2 after the TTL", fx.repoCommentLists)
	}
}

// No behaviour change for a repository without Prow: no Prow signal on the PR
// means nothing is posted, with or without a refusal elsewhere.
func TestProwRefusalMemoryNoChangeForNonProwRepo(t *testing.T) {
	for _, refused := range []bool{false, true} {
		fx := prowOkToTestFixture()
		fx.labels = nil
		if refused {
			fx.repoComments = refusalOnEarlierPR()
		}
		runProwRefusalEngines(t, &fx, 2)
		if fx.comments != 0 {
			t.Fatalf("refused=%v comments = %d %q, want none on a PR with no Prow signal", refused, fx.comments, fx.allComments)
		}
	}
}
