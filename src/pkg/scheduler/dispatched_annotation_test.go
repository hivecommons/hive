package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// A head the review swarm has asked for, with no verdict yet, is marked so
// the queue-mode reviewer skips it instead of reviewing it side by side with
// the swarm (projectbluefin/dakota-iso#376, projectbluefin/bonedigger#116).
func TestDispatchedAnnotation(t *testing.T) {
	head := "48b9e51d43185ed8a038e2cec2b379240e3c7e14"
	pending := []review.PendingReview{{Repo: "projectbluefin/dakota-iso", Number: 376, HeadSHA: head, Perspective: review.PerspectiveCorrectness, Agent: "reviewer-2"}}
	cases := []struct {
		name string
		pr   github.PullRequest
		want string
	}{
		{"bare repo, same head", github.PullRequest{Repo: "dakota-iso", Number: 376, HeadSHA: head}, " [hive-reviewed: dispatched@48b9e51]"},
		{"qualified repo, same head", github.PullRequest{Repo: "projectbluefin/dakota-iso", Number: 376, HeadSHA: head}, " [hive-reviewed: dispatched@48b9e51]"},
		{"new head", github.PullRequest{Repo: "dakota-iso", Number: 376, HeadSHA: "1111111111111111111111111111111111111111"}, ""},
		{"other PR", github.PullRequest{Repo: "dakota-iso", Number: 377, HeadSHA: head}, ""},
		{"no head known", github.PullRequest{Repo: "dakota-iso", Number: 376}, ""},
	}
	for _, tc := range cases {
		if got := dispatchedAnnotation(pending, tc.pr, "projectbluefin"); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
	if got := dispatchedAnnotation(nil, cases[0].pr, "projectbluefin"); got != "" {
		t.Errorf("no pending rows must annotate nothing, got %q", got)
	}
}

// The eval cycle's pending rows reach ${PR_LIST}; a verdict mark wins over
// the dispatched one.
func TestFormatPRList_CarriesDispatchedAnnotation(t *testing.T) {
	s := newSchedulerWithIoscan(false)
	s.cfg.Project.Org = "projectbluefin"
	dir := t.TempDir()
	orig := review.ReviewVerdictsPath
	review.ReviewVerdictsPath = dir + "/review-verdicts.json"
	t.Cleanup(func() { review.ReviewVerdictsPath = orig })
	judged := "fc92b3aa4c71a5b68f64f089c03a07f8af290981"
	inFlight := "48b9e51d43185ed8a038e2cec2b379240e3c7e14"
	if err := review.WriteArtifact(review.ReviewVerdictsPath, review.Artifact{Items: []review.Aggregate{{Repo: "projectbluefin/actions", Number: 548, HeadSHA: judged, Verdict: review.VerdictRequiresHuman}}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.SetReviewDispatchPending([]review.PendingReview{
		{Repo: "projectbluefin/actions", Number: 548, HeadSHA: judged, Perspective: review.PerspectiveSecurity, Agent: "reviewer", Dispatched: now},
		{Repo: "projectbluefin/dakota-iso", Number: 376, HeadSHA: inFlight, Perspective: review.PerspectiveCorrectness, Agent: "reviewer-2", Dispatched: now},
	}, time.Hour)
	actionable := &github.ActionableResult{}
	actionable.PRs.Items = []github.PullRequest{
		{Repo: "actions", Number: 548, Title: "gate", Author: "a", HeadSHA: judged},
		{Repo: "dakota-iso", Number: 376, Title: "renovate", Author: "b", HeadSHA: inFlight},
		{Repo: "dakota-iso", Number: 368, Title: "other", Author: "c", HeadSHA: "2222222222222222222222222222222222222222"},
	}
	out, _ := s.formatPRListWithPolicyForAgent(actionable, "reviewer")
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(line, "#548"):
			if !strings.Contains(line, "[hive-reviewed: requires_human@fc92b3a]") || strings.Contains(line, "dispatched@") {
				t.Fatalf("judged head must carry the verdict mark only: %q", line)
			}
		case strings.Contains(line, "#376"):
			if !strings.Contains(line, "[hive-reviewed: dispatched@48b9e51]") {
				t.Fatalf("in-flight head not marked: %q", line)
			}
		case strings.Contains(line, "#368"):
			if strings.Contains(line, "hive-reviewed") {
				t.Fatalf("unreviewed head must not be marked: %q", line)
			}
		}
	}
	if !strings.Contains(out, "#376") {
		t.Fatalf("PR list missing #376: %q", out)
	}
}

// A row older than the pending TTL stops marking its head even if no later
// eval cycle replaces the set, and nil (review dispatch off) clears it.
func TestReviewDispatchPendingAgesOutAndClears(t *testing.T) {
	s := &Scheduler{}
	now := time.Date(2026, 11, 21, 19, 0, 0, 0, time.UTC)
	fresh := review.PendingReview{Repo: "o/r", Number: 1, HeadSHA: "aaaaaaa", Agent: "reviewer", Dispatched: now.Add(-10 * time.Minute)}
	stale := review.PendingReview{Repo: "o/r", Number: 2, HeadSHA: "bbbbbbb", Agent: "reviewer", Dispatched: now.Add(-2 * time.Hour)}
	undated := review.PendingReview{Repo: "o/r", Number: 3, HeadSHA: "ccccccc", Agent: "reviewer"}
	s.SetReviewDispatchPending([]review.PendingReview{fresh, stale, undated}, 45*time.Minute)
	got := s.reviewDispatchPending(now)
	if len(got) != 1 || got[0].Number != 1 {
		t.Fatalf("live rows = %+v, want only the fresh one", got)
	}
	s.SetReviewDispatchPending(nil, 0)
	if got := s.reviewDispatchPending(now); len(got) != 0 {
		t.Fatalf("cleared set still marks %+v", got)
	}
}
