package automerge

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

const mergeConflictsTestMessage = "Pull Request has merge conflicts"

// projectbluefin/utah-packages#633, 2026-10-31 18:29Z: a pass's first read of
// the PR did not show the conflict, the read right before the merge did, and
// the sweep called PUT /merge anyway (405 "Pull Request has merge
// conflicts"). The re-check now skips it without the call.
func TestSweepSelfAuthoredSkipsAConflictFoundAtTheMergeTimeRead(t *testing.T) {
	for _, recheck := range []string{"dirty", "conflicting"} {
		t.Run(recheck, func(t *testing.T) {
			var merged []int
			var attempts int
			prs := []selfAuthoredPR{{
				number: 90, author: testHiveAppBotLogin, mergeableState: "clean", recheckMergeableState: recheck,
				statusState: "success", checkStatus: "completed", checkConclusion: "success", mergeAttempts: &attempts,
			}}
			api := newSelfAuthoredAutoMergeAPI(t, prs, &merged)
			defer api.Close()
			var logs bytes.Buffer
			c := newAutoMergeSweepClient(api.URL)
			c.logger = slog.New(slog.NewTextHandler(&logs, nil))

			result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
			if err != nil {
				t.Fatalf("SweepSelfAuthoredAutoMerges: %v", err)
			}
			if attempts != 0 || len(merged) != 0 || result.Skipped != 1 {
				t.Fatalf("merge attempts = %d, merged = %v, result = %+v; want a skip with no merge call", attempts, merged, result)
			}
			if !strings.Contains(logs.String(), "reason=conflicting") {
				t.Fatalf("logs = %q, want reason=conflicting", logs.String())
			}
		})
	}
}

// A clean head is still merged through the re-check.
func TestSweepSelfAuthoredStillMergesACleanHead(t *testing.T) {
	var merged []int
	var attempts int
	prs := []selfAuthoredPR{{
		number: 90, author: testHiveAppBotLogin, mergeableState: "clean",
		statusState: "success", checkStatus: "completed", checkConclusion: "success", mergeAttempts: &attempts,
	}}
	api := newSelfAuthoredAutoMergeAPI(t, prs, &merged)
	defer api.Close()
	result, err := newAutoMergeSweepClient(api.URL).SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil || attempts != 1 || len(merged) != 1 || len(result.Merged) != 1 {
		t.Fatalf("attempts = %d, merged = %v, result = %+v, err = %v; want one merge", attempts, merged, result, err)
	}
}

// When GitHub only learns of the conflict at the merge call, the 405 is the
// conflict it names, not merge-failed: that reason parked #633 at 080 for a
// pass and logged a WARN.
func TestSweepSelfAuthoredMergeConflict405IsConflicting(t *testing.T) {
	var merged []int
	var attempts int
	prs := []selfAuthoredPR{{
		number: 90, author: testHiveAppBotLogin, mergeableState: "clean",
		statusState: "success", checkStatus: "completed", checkConclusion: "success",
		mergeStatus: http.StatusMethodNotAllowed, mergeMessage: mergeConflictsTestMessage, mergeAttempts: &attempts,
	}}
	api := newSelfAuthoredAutoMergeAPI(t, prs, &merged)
	defer api.Close()
	var logs bytes.Buffer
	c := newAutoMergeSweepClient(api.URL)
	c.logger = slog.New(slog.NewTextHandler(&logs, nil))
	if _, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{}); err != nil {
		t.Fatalf("SweepSelfAuthoredAutoMerges: %v", err)
	}
	got := logs.String()
	if attempts != 1 || !strings.Contains(got, "reason=conflicting") || strings.Contains(got, "merge-failed") {
		t.Fatalf("attempts = %d, logs = %q; want one call reported as conflicting, not merge-failed", attempts, got)
	}
}

func TestMergeConflictMergeErrorIsOnlyThe405(t *testing.T) {
	resp := func(status int, msg string) error {
		return &gh.ErrorResponse{Response: &http.Response{StatusCode: status}, Message: msg}
	}
	if !mergeConflictMergeError(resp(http.StatusMethodNotAllowed, mergeConflictsTestMessage)) {
		t.Fatal("405 merge conflicts not recognised")
	}
	for _, err := range []error{resp(http.StatusConflict, mergeConflictsTestMessage), resp(http.StatusMethodNotAllowed, reviewRequiredTestMessage), resp(http.StatusMethodNotAllowed, "Pull Request is not mergeable")} {
		if mergeConflictMergeError(err) {
			t.Fatalf("%v read as a merge conflict", err)
		}
	}
}

func TestMergeCallConflictingReadsTheBoolOnlyWhileTheStateIsUnknown(t *testing.T) {
	for _, tc := range []struct {
		state     string
		mergeable *bool
		want      bool
	}{
		{"dirty", gh.Ptr(false), true},
		{"dirty", nil, true},
		{"unknown", gh.Ptr(false), true},
		{"", gh.Ptr(false), true},
		{"unknown", nil, false},
		{"unknown", gh.Ptr(true), false},
		{"clean", gh.Ptr(true), false},
		{"blocked", gh.Ptr(false), false},
		{"behind", gh.Ptr(false), false},
	} {
		pr := &gh.PullRequest{MergeableState: gh.Ptr(tc.state), Mergeable: tc.mergeable}
		if got := mergeCallConflicting(pr); got != tc.want {
			t.Fatalf("state %q mergeable %v: got %v, want %v", tc.state, tc.mergeable, got, tc.want)
		}
	}
}

// projectbluefin/utah-packages#633: @hanthor asked for changes at 1760767,
// the hive reworked the branch, and GitHub refused the merge for want of an
// approving review. The 085 ask named only the approval. It now names the
// standing change request, links what the hive pushed since and asks the
// reviewer to re-review; without one the text is unchanged.
func TestMergeRefusedReviewAskNamesTheStandingChangeRequest(t *testing.T) {
	const reviewed = "1760767603b4d1c6a3f1c6c4bb0ad5c20c7e2a11"
	const head = "sha90" // the fixture's head: the hive pushed past the review
	reviewURL := "https://github.com/acme/widget/pull/90#pullrequestreview-1"
	at := time.Date(2026, 10, 30, 4, 12, 6, 0, time.UTC)
	review := func(login, state, commit string, when time.Time) map[string]any {
		return map[string]any{
			"id": when.Unix(), "user": map[string]string{"login": login}, "state": state,
			"commit_id": commit, "submitted_at": when.Format(time.RFC3339), "html_url": reviewURL,
		}
	}
	var merged []int
	var attempts int
	prs := []selfAuthoredPR{{
		number: 90, author: testHiveAppBotLogin, mergeableState: "blocked",
		statusState: "success", checkStatus: "completed", checkConclusion: "success",
		mergeStatus: http.StatusMethodNotAllowed, mergeMessage: reviewRequiredTestMessage, mergeAttempts: &attempts,
		reviews: []map[string]any{
			review(testHiveAppBotLogin, "COMMENTED", reviewed, at.Add(-time.Hour)),
			review("hanthor", "CHANGES_REQUESTED", reviewed, at),
			// A request its reviewer later approved, one dismissed, and a
			// bot's: none of them stands.
			review("carol", "CHANGES_REQUESTED", reviewed, at.Add(time.Minute)),
			review("carol", "APPROVED", "sha90", at.Add(2*time.Minute)),
			review("dave", "CHANGES_REQUESTED", reviewed, at.Add(3*time.Minute)),
			review("dave", "DISMISSED", reviewed, at.Add(4*time.Minute)),
			review("renovate[bot]", "CHANGES_REQUESTED", reviewed, at.Add(5*time.Minute)),
		},
	}}
	api := newSelfAuthoredAutoMergeAPI(t, prs, &merged)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	c.stageLabelPolicy = func() StageLabelPolicy { return StageLabelPolicy{Enabled: true} }
	c.stageTracker = NewStageTracker()
	if _, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{}); err != nil {
		t.Fatalf("SweepSelfAuthoredAutoMerges: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("merge attempts = %d, want the one refused call", attempts)
	}
	var obs stageObservation
	for _, list := range c.stageTracker.drain() {
		for _, o := range list {
			if o.Number == 90 && o.Reason == autoMergeReasonMergeRefusedReview {
				obs = o
			}
		}
	}
	if obs.Number != 90 {
		t.Fatal("no merge-refused-review observation recorded for #90")
	}
	if got := obs.Detail.StandingChangeRequests; len(got) != 1 || got[0].Login != "hanthor" || got[0].CommitID != reviewed {
		t.Fatalf("StandingChangeRequests = %+v, want only @hanthor's at %s", got, reviewed)
	}

	// The 085 text is built from the drained observation.
	if obs.HeadSHA != head {
		t.Fatalf("observed head = %q, want %q", obs.HeadSHA, head)
	}
	key := stageKey(obs.Owner, obs.Repo, obs.Number)
	e := c.humanEvidence(context.Background(), c.stageTracker, key, obs, stagelabels.CauseDecision, at.Add(48*time.Hour))
	for _, want := range []string{
		"@hanthor requested changes on 2026-10-30 at `1760767`",
		"([review](" + reviewURL + "))",
		"the hive has pushed since ([changes since that review](https://github.com/acme/widget/compare/" + reviewed + "..." + head + "))",
		"@hanthor, please re-review this head",
		"a maintainer can dismiss the review",
	} {
		if !strings.Contains(e.CauseReason, want) {
			t.Fatalf("085 cause = %q\nwant it to contain %q", e.CauseReason, want)
		}
	}
	if strings.Contains(e.CauseReason, "A GitHub **Approve** review on this head") {
		t.Fatalf("085 cause still invites an approval over the change request: %q", e.CauseReason)
	}
	if !strings.Contains(stagelabels.DecisionComment(stagelabels.RouteHuman(e)), "@hanthor, please re-review this head") {
		t.Fatal("the decision comment does not carry the re-review ask")
	}

	// No standing request: the text is exactly today's.
	obs.Detail.StandingChangeRequests = nil
	plain := c.humanEvidence(context.Background(), c.stageTracker, key, obs, stagelabels.CauseDecision, at)
	if plain.CauseReason != decisionCause(autoMergeReasonMergeRefusedReview) {
		t.Fatalf("085 cause without a change request = %q, want the unchanged approval ask", plain.CauseReason)
	}
}

func TestMergeRefusedReviewChangeRequestCauseShapes(t *testing.T) {
	const head = "337fa030b73afe13dd9eb77cb6cadf94dcd63ec5"
	if got := mergeRefusedReviewChangeRequestCause(head, nil); got != "" {
		t.Fatalf("no requests: %q, want empty", got)
	}
	onHead := mergeRefusedReviewChangeRequestCause(head, []prstage.ChangeRequestRef{{Login: "hanthor", CommitID: head}})
	if !strings.Contains(onHead, "@hanthor requested changes on this head") || strings.Contains(onHead, "pushed since") {
		t.Fatalf("request on the head: %q, want no claim that the hive pushed since", onHead)
	}
	two := mergeRefusedReviewChangeRequestCause(head, []prstage.ChangeRequestRef{{Login: "hanthor", CommitID: "aaaa111"}, {Login: "castrojo", CommitID: "bbbb222"}})
	if !strings.Contains(two, "@hanthor and @castrojo, please re-review this head") || strings.Contains(two, "compare/") {
		t.Fatalf("two requests without URLs: %q", two)
	}
}
