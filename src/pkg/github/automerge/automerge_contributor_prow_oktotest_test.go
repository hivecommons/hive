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

// cncf/prow-github-actions answers in its own words and names the App with no
// @ (projectbluefin/chairlift#802: five posts, five refusals, no note). That
// reply stops the lane exactly like Prow's, and the note quotes it.
func TestContributorLaneStopsProwOkToTestAfterProwActionsRefusal(t *testing.T) {
	fx := prowOkToTestFixture()
	fx.prComments = []map[string]any{
		appComment(2, workflowApprovalCommandBody("oldhead")),
		prowComment(3, "Cannot /ok-to-test because Error: "+testHiveAppBotLogin+" is not a reviewer or approver for any changed file"),
	}
	runProwOkToTestTicks(t, &fx, 3)
	runProwOkToTestTicks(t, &fx, 2)
	if fx.comments != 1 {
		t.Fatalf("comments = %d %q, want only the one needs-human note", fx.comments, fx.allComments)
	}
	note := fx.allComments[0]
	if !strings.HasPrefix(note, workflowApprovalRejectedMarker) || !strings.Contains(note, prowActionsOkToTestRefusal) || strings.Contains(note, prowOkToTestRejection) {
		t.Fatalf("note = %q, want the rejected marker quoting the action's wording, not Prow's", note)
	}
}

// commentAt stamps a fixture comment with its created_at.
func commentAt(comment map[string]any, at time.Time) map[string]any {
	comment["created_at"] = at.Format(time.RFC3339)
	return comment
}

// Whatever a merge bot replies (or if it says nothing), a command posted for
// an earlier head that is past the grace, left no ok-to-test label and drew
// no Prow presubmit on that head did nothing: the lane does not post again
// on a new head and leaves the one note. A command that did start
// presubmits, or one still inside the grace, is not treated as refused.
func TestContributorLaneTreatsUnansweredProwOkToTestAsRefused(t *testing.T) {
	clockStart := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	command := workflowApprovalCommandBody("sha7")
	for _, tc := range []struct {
		name     string
		postedAt time.Time
		mutate   func(fx *contributorLaneFixture)
		ticks    int
		wantPost bool
		wantNote bool
	}{
		{name: "earlier post past the grace with no effect", postedAt: clockStart.Add(-30 * time.Minute), ticks: 3, wantNote: true},
		{name: "earlier post still inside the grace", postedAt: clockStart.Add(-2 * time.Minute), ticks: 1, wantPost: true},
		{
			name:     "earlier post that drew a Prow presubmit on its head",
			postedAt: clockStart.Add(-30 * time.Minute),
			mutate: func(fx *contributorLaneFixture) {
				fx.earlierStatuses = map[string][]map[string]any{"oldhead": {
					prowStatus(prowTideContext, prowOkToTestTestBot),
					prowStatus("pull-widget-unit", prowOkToTestTestBot),
				}}
			},
			ticks:    3,
			wantPost: true,
		},
		{
			// The trigger plugin's label persists across pushes: a command
			// that took effect is never mistaken for a refusal.
			name:     "ok-to-test label present",
			postedAt: clockStart.Add(-30 * time.Minute),
			mutate:   func(fx *contributorLaneFixture) { fx.labels = []string{"size/S", "ok-to-test"} },
			ticks:    3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := prowOkToTestFixture()
			fx.prComments = []map[string]any{commentAt(appComment(2, workflowApprovalCommandBody("oldhead")), tc.postedAt)}
			if tc.mutate != nil {
				tc.mutate(&fx)
			}
			runProwOkToTestTicks(t, &fx, tc.ticks)
			posted, notes := 0, 0
			for _, body := range fx.allComments {
				switch {
				case body == command:
					posted++
				case strings.HasPrefix(body, workflowApprovalRejectedMarker):
					notes++
					if strings.Contains(body, prowOkToTestRejection) || !strings.Contains(body, "earlier head") {
						t.Fatalf("note = %q, want the no-effect wording, not a quoted refusal", body)
					}
				}
			}
			if (posted == 1) != tc.wantPost || posted > 1 || (notes == 1) != tc.wantNote || notes > 1 {
				t.Fatalf("posts=%d notes=%d (comments %q), want post=%v note=%v", posted, notes, fx.allComments, tc.wantPost, tc.wantNote)
			}
		})
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

// An observe-only pass (a repo whose auto-merge is off) posts no /ok-to-test
// and logs nothing at WARN: its mutation boundary refuses the comment by
// design, and each pass runs on a fresh engine, so the refused attempt was
// logged on every re-evaluation of the head (projectbluefin/server#690). The
// live sweep on the same fixture posts once (TestContributorLanePostsProwOkToTestForNonMemberFork).
func TestContributorLaneObserveOnlyPassDoesNotPostProwOkToTest(t *testing.T) {
	fx := prowOkToTestFixture()
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock)})
	var logs bytes.Buffer
	for pass := 0; pass < 3; pass++ {
		oc := engine.observeOnlyEngine()
		oc.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
		if _, _, err := oc.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy); err != nil {
			t.Fatalf("pass %d err = %v", pass, err)
		}
		clock.advance(workflowApprovalRecheck + time.Minute)
	}
	if fx.comments != 0 {
		t.Fatalf("comments = %d %q, want none from an observe-only pass", fx.comments, fx.allComments)
	}
	if strings.Contains(logs.String(), "failed to post ok-to-test") {
		t.Fatalf("observe-only pass logged the refused /ok-to-test at WARN: %s", logs.String())
	}
}
