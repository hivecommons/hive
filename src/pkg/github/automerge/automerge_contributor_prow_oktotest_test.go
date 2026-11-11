package automerge

// Prow presubmits on non-member fork PRs (hivecommons/hive#17474). #17359
// sat green on GitHub Actions for hours because Prow never ran its
// presubmits for a non-member fork head and nobody posted /ok-to-test. These
// pin that the contributor lane posts the command once per head on exactly
// that shape, never otherwise, and stops for good once Prow rejects the App.

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

const (
	prowOkToTestTestContext = "verify PR contents"
	prowOkToTestTestBot     = "kubestellar-prow[bot]"
	prowOkToTestTestFork    = "karibhou/widget"
)

// prowStatus is a commit status as GET commits/{sha}/statuses serves it.
func prowStatus(name, creator string) map[string]any {
	return map[string]any{"context": name, "state": "pending", "creator": map[string]string{"login": creator}}
}

// prowOkToTestFixture is an admitted contributor-lane fork PR by a non-member
// on a Prow-managed repository whose Prow presubmit is a required check that
// never appeared on the head.
func prowOkToTestFixture() contributorLaneFixture {
	fx := validContributorFixture()
	fx.baseRef = "main"
	fx.requiredContexts = []string{"ci/build", prowOkToTestTestContext}
	fx.headRepo = prowOkToTestTestFork
	fx.labels = []string{"size/S", "needs-ok-to-test"}
	fx.headStatuses = []map[string]any{prowStatus(prowTideContext, prowOkToTestTestBot)}
	return fx
}

func runProwOkToTestTicks(t *testing.T, fx *contributorLaneFixture, ticks int) {
	t.Helper()
	api := newContributorLaneAPI(t, fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock)})
	for tick := 1; tick <= ticks; tick++ {
		if _, _, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy); err != nil {
			t.Fatalf("tick %d err = %v", tick, err)
		}
		// Past the comment re-scan interval, so a later tick reads the
		// thread again and would post again if nothing deduped it.
		clock.advance(workflowApprovalRecheck + time.Minute)
	}
	if fx.merges != 0 {
		t.Fatalf("merges = %d, want 0 while the Prow presubmit is missing", fx.merges)
	}
}

func TestContributorLanePostsProwOkToTestForNonMemberFork(t *testing.T) {
	command := workflowApprovalCommandBody("sha7")
	for _, tc := range []struct {
		name     string
		mutate   func(fx *contributorLaneFixture)
		wantPost bool
	}{
		{name: "non-member fork on a Prow repo posts once", wantPost: true},
		{name: "first-time contributor fork posts once", mutate: func(fx *contributorLaneFixture) { fx.association = "FIRST_TIME_CONTRIBUTOR" }, wantPost: true},
		{
			name: "approve plugin comment alone marks the repo as Prow-managed",
			mutate: func(fx *contributorLaneFixture) {
				fx.labels = nil
				fx.prComments = []map[string]any{prowComment(3, "[APPROVALNOTIFIER] This PR is **NOT APPROVED**")}
			},
			wantPost: true,
		},
		{
			name: "a status from another account is not a presubmit",
			mutate: func(fx *contributorLaneFixture) {
				fx.headStatuses = append(fx.headStatuses, prowStatus("hive/queue", testHiveAppBotLogin))
			},
			wantPost: true,
		},
		{name: "no Tide status means no presubmit evidence", mutate: func(fx *contributorLaneFixture) { fx.headStatuses = nil }, wantPost: true},
		{name: "same-repo head is not a fork", mutate: func(fx *contributorLaneFixture) { fx.headRepo = "" }},
		{name: "org member is trusted by Prow", mutate: func(fx *contributorLaneFixture) { fx.association = "MEMBER" }},
		{name: "collaborator is trusted by Prow", mutate: func(fx *contributorLaneFixture) { fx.association = "COLLABORATOR" }},
		{name: "ok-to-test label already released the presubmits", mutate: func(fx *contributorLaneFixture) { fx.labels = []string{"size/S", "ok-to-test"} }},
		{name: "repository without Prow", mutate: func(fx *contributorLaneFixture) { fx.labels = nil }},
		{
			name: "Prow already reported a presubmit on the head",
			mutate: func(fx *contributorLaneFixture) {
				fx.headStatuses = append(fx.headStatuses, prowStatus("native-queue-policy", prowOkToTestTestBot))
			},
		},
		{
			name:   "command already on the thread for this head",
			mutate: func(fx *contributorLaneFixture) { fx.prComments = []map[string]any{appComment(3, command)} },
		},
		{
			// Without a verified claim or registry tier the hive does not
			// vouch for the author, so it must not release their code to CI.
			name: "author the lane did not admit",
			mutate: func(fx *contributorLaneFixture) {
				fx.association = "FIRST_TIME_CONTRIBUTOR"
				fx.issueComments = nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := prowOkToTestFixture()
			if tc.mutate != nil {
				tc.mutate(&fx)
			}
			runProwOkToTestTicks(t, &fx, 3)
			posted := 0
			for _, body := range fx.allComments {
				if body == command {
					posted++
				}
			}
			want := 0
			if tc.wantPost {
				want = 1
			}
			if posted != want {
				t.Fatalf("ok-to-test posts = %d (comments %q), want %d across three ticks", posted, fx.allComments, want)
			}
		})
	}
}

// A restarted hive finds its own command for the head on the thread and does
// not post it a second time.
func TestContributorLaneProwOkToTestOncePerHeadAcrossRestart(t *testing.T) {
	fx := prowOkToTestFixture()
	runProwOkToTestTicks(t, &fx, 1)
	runProwOkToTestTicks(t, &fx, 1)
	if fx.comments != 1 || fx.lastComment != workflowApprovalCommandBody("sha7") {
		t.Fatalf("comments = %d last=%q, want the one command across a restart", fx.comments, fx.lastComment)
	}
}

// Prow answered the App's command with "Cannot trigger testing until a
// trusted user…": the lane leaves one needs-human note, posts no further
// command (not even for a new head), and a restarted hive stays quiet.
func TestContributorLaneStopsProwOkToTestAfterRejection(t *testing.T) {
	fx := prowOkToTestFixture()
	fx.prComments = []map[string]any{
		appComment(2, workflowApprovalCommandBody("oldhead")),
		prowComment(3, "@"+testHiveAppBotLogin+": Cannot trigger testing until a trusted user reviews the PR and leaves an `/ok-to-test` message."),
	}
	runProwOkToTestTicks(t, &fx, 3)
	runProwOkToTestTicks(t, &fx, 2)
	if fx.comments != 1 {
		t.Fatalf("comments = %d %q, want only the one needs-human note", fx.comments, fx.allComments)
	}
	note := fx.allComments[0]
	if !strings.HasPrefix(note, workflowApprovalRejectedMarker) || !strings.Contains(note, "needs-human") || !strings.Contains(note, prowOkToTestRejection) || !strings.Contains(note, testHiveAppBotLogin) {
		t.Fatalf("note = %q, want the rejected marker, a needs-human note naming the App and Prow's reply", note)
	}
	for _, line := range strings.Split(note, "\n") {
		if strings.TrimSpace(line) == workflowApprovalCommand {
			t.Fatalf("note = %q carries a bare %s line Prow would act on", note, workflowApprovalCommand)
		}
	}
}

// The merge-request relay evaluates the lane read-only and posts nothing.
func TestContributorLaneRelayEvaluationDoesNotPostProwOkToTest(t *testing.T) {
	fx := prowOkToTestFixture()
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock)})
	if _, _, err := engine.evaluateContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy, true); err != nil {
		t.Fatal(err)
	}
	if fx.comments != 0 {
		t.Fatalf("comments = %d %q, want none from a relay evaluation", fx.comments, fx.allComments)
	}
}

// A rejection the PR thread already records is a handled state: a restarted
// hive must not log it at WARN again.
func TestContributorLaneProwRejectionAlreadyNotedLogsBelowWarn(t *testing.T) {
	fx := prowOkToTestFixture()
	fx.prComments = []map[string]any{
		appComment(2, workflowApprovalCommandBody("oldhead")),
		prowComment(3, "@"+testHiveAppBotLogin+": Cannot trigger testing until a trusted user reviews the PR and leaves an `/ok-to-test` message."),
	}
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	var logs bytes.Buffer
	for run := 0; run < 2; run++ {
		engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock)})
		engine.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
		if _, _, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy); err != nil {
			t.Fatalf("run %d err = %v", run, err)
		}
	}
	if got := strings.Count(logs.String(), "ok-to-test not posted again"); got != 1 {
		t.Fatalf("WARN lines = %d in %q, want 1 (only the run that left the note)", got, logs.String())
	}
}
