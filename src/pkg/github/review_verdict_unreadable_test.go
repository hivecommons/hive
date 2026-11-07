package github

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/review"
)

// The five summaries the reviewer wrote on projectbluefin/testsuite#1116 at
// head ae22434 (2026-11-07), when every GitHub read hit the agent API cap.
var rateLimitedSummaries1116 = map[review.Perspective]string{
	review.PerspectiveCorrectness:     "Could not read the delta d397ece..ae22434: every GitHub read returned HTTP 429 (hive agent API hourly cap, 181-211/400) and no local checkout of projectbluefin/testsuite exists on this host. Not reviewed; previous correctness verdict (approve) was for head d397ece only.",
	review.PerspectiveDocsCurrency:    "Delta touches docs/skills/test-authoring/suite-map/SKILL.md but could not be read (GitHub API 429 cap, no local checkout); could not confirm whether prior finding SKILL.md:170 (immutability checks still marked @pending) is fixed. Status unknown.",
	review.PerspectiveIntentAlignment: "Delta not readable (GitHub API 429 cap, no local checkout); could not re-check prior finding tests/common/features/common_immutable.feature:13 ('Closes #838' may over-claim scope) nor read issue #838. Status of prior finding unknown.",
	review.PerspectiveSecurity:        "Delta not readable (GitHub API 429 cap, no local checkout). Not reviewed at this head.",
	review.PerspectiveStyle:           "Delta not readable (GitHub API 429 cap, no local checkout). Not reviewed at this head.",
}

func verdictJSONWithSummary(t *testing.T, repo string, number int, perspective review.Perspective, verdict, head, summary string) string {
	t.Helper()
	var report map[string]any
	if err := json.Unmarshal([]byte(validVerdictJSONWithHead(t, repo, number, string(perspective), verdict, head)), &report); err != nil {
		t.Fatalf("unmarshal verdict fixture: %v", err)
	}
	report["summary"] = summary
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// projectbluefin/testsuite#1116: every GitHub read the reviewer made returned
// 429, so all five perspectives came back requires_human with "could not
// read … not reviewed". Recorded, that parked the PR at 085 and the
// delegate asked a person about "the security review withheld approval".
// It must be no verdict at all: nothing recorded, nobody asked, and the same
// perspectives asked again once the pending TTL has passed.
func TestRecordReviewVerdictRateLimitedReviewIsNoVerdictAndRetries(t *testing.T) {
	const (
		repo = "projectbluefin/testsuite"
		num  = 1116
		head = "ae22434cfe05ce9c119f2e508634d599201ab327"
	)
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	dispatched := time.Date(2026, 11, 7, 10, 30, 0, 0, time.UTC)
	var pending []review.PendingReview
	var elems []string
	for _, p := range review.DefaultPerspectives {
		pending = append(pending, review.PendingReview{Repo: repo, Number: num, HeadSHA: head, Perspective: p, Agent: "reviewer", Dispatched: dispatched})
		elems = append(elems, verdictJSONWithSummary(t, repo, num, p, "requires_human", head, rateLimitedSummaries1116[p]))
	}
	withVerdictDispatchState(t, review.DispatchState{Pending: pending})

	recorded := c.recordReviewVerdict(context.Background(), ReviewRequest{
		Repo: repo, Number: num, Agent: "reviewer", Event: ReviewEventRecordVerdict,
		Report: "[" + strings.Join(elems, ",") + "]",
	}, dir)

	if len(recorded) != 0 {
		t.Fatalf("recorded %d verdicts from a review that read nothing: %+v", len(recorded), recorded)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, review.ReviewReportFilePrefix+"*")); len(matches) != 0 {
		t.Fatalf("unreadable review was written where the collector reads verdicts: %v", matches)
	}
	artifact, err := review.Collect(dir, review.AggregateOptions{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, agg := range artifact.Items {
		if agg.RequiresHuman || agg.Verdict == review.VerdictRequiresHuman {
			t.Fatalf("unreadable review became a requires_human aggregate: %+v", agg)
		}
		if strings.Contains(agg.DecisionFinding, "security") {
			t.Fatalf("unreadable review claimed a security finding: %q", agg.DecisionFinding)
		}
	}

	state, err := review.LoadDispatchState("")
	if err != nil {
		t.Fatalf("load dispatch state: %v", err)
	}
	pr := review.PullRequest{Repo: repo, Number: num, Title: "enable immutability checks", Author: "Karibhou", HeadSHA: head}
	opts := review.DispatchOptions{
		RequireApproval:      true,
		FanOut:               true,
		CombinedPerspectives: true,
		MaxParallelReviews:   5,
		Agents:               []review.AgentCapability{{Name: "reviewer", Enabled: true, UsesKick: true, Role: "reviewer"}},
	}

	// Inside the TTL the dispatch is still open: no re-kick, no person.
	opts.Now = dispatched.Add(10 * time.Minute)
	early := review.PlanDispatch([]review.PullRequest{pr}, artifact, state, opts)
	if len(early.ReviewKicks) != 0 {
		t.Fatalf("re-kicked inside the pending TTL: %+v", early.ReviewKicks)
	}
	if len(early.NewHuman) != 0 || len(early.State.Human) != 0 {
		t.Fatalf("an unreadable review asked a person: new=%+v state=%+v", early.NewHuman, early.State.Human)
	}

	// Past the TTL the planner asks again for every perspective.
	opts.Now = dispatched.Add(review.DefaultPendingReviewTTL + time.Minute)
	retry := review.PlanDispatch([]review.PullRequest{pr}, artifact, state, opts)
	if len(retry.ReviewKicks) != 1 {
		t.Fatalf("review kicks after the TTL = %d, want one retry", len(retry.ReviewKicks))
	}
	if got := len(retry.ReviewKicks[0].Perspectives); got != len(review.DefaultPerspectives) {
		t.Fatalf("retry asked %d perspectives, want all %d", got, len(review.DefaultPerspectives))
	}
	if len(retry.NewHuman) != 0 || len(retry.State.Human) != 0 {
		t.Fatalf("the retry asked a person: new=%+v state=%+v", retry.NewHuman, retry.State.Human)
	}
}

// Only the rate-limited perspectives are dropped. A perspective that did read
// the PR is recorded as usual, and the head reads as under-asked
// (incomplete), not approved: an unread correctness review must not resolve
// hands-free to approve and make the head merge-eligible.
func TestRecordReviewVerdictKeepsReadPerspectivesAndDropsRateLimitedOnes(t *testing.T) {
	const (
		repo = "o/r"
		num  = 7
		head = "abc123"
	)
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{
		{Repo: repo, Number: num, HeadSHA: head, Perspective: review.PerspectiveCorrectness, Agent: "reviewer", Dispatched: time.Now().UTC()},
		{Repo: repo, Number: num, HeadSHA: head, Perspective: review.PerspectiveStyle, Agent: "reviewer", Dispatched: time.Now().UTC()},
	}})
	raw := "[" + strings.Join([]string{
		verdictJSONWithSummary(t, repo, num, review.PerspectiveCorrectness, "requires_human", head, rateLimitedSummaries1116[review.PerspectiveCorrectness]),
		verdictJSONWithSummary(t, repo, num, review.PerspectiveStyle, "approve", head, "Read the diff; naming is consistent."),
	}, ",") + "]"

	recorded := c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: repo, Number: num, Agent: "reviewer", Report: raw}, dir)

	if len(recorded) != 1 || recorded[0].Perspective != review.PerspectiveStyle {
		t.Fatalf("recorded = %+v, want only the style verdict", recorded)
	}
	artifact, err := review.Collect(dir, review.AggregateOptions{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(artifact.Items) != 1 {
		t.Fatalf("want one aggregate, got %+v", artifact.Items)
	}
	got := artifact.Items[0]
	if _, ok := got.Perspectives[review.PerspectiveCorrectness]; ok {
		t.Fatalf("unread correctness perspective was counted: %v", got.Perspectives)
	}
	if got.Verdict != review.VerdictIncomplete || got.RequiresHuman || got.MergeEligible {
		t.Fatalf("aggregate = %s (requires_human=%t merge_eligible=%t), want incomplete", got.Verdict, got.RequiresHuman, got.MergeEligible)
	}
}

// The rate-limited security report is dropped (no verdict), and correctness
// read the PR and asked a non-security question. On a contributor PR that
// question alone is hands-free, but the head has had no security review:
// it must read incomplete, never merge-eligible. Before the coverage check
// the hands-free rule approved it.
func TestRecordReviewVerdictDroppedSecurityLeavesHandsFreeHeadIncomplete(t *testing.T) {
	const (
		repo = "o/r"
		num  = 8
		head = "def456"
	)
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	var pending []review.PendingReview
	var elems []string
	for _, p := range review.DefaultPerspectives {
		pending = append(pending, review.PendingReview{Repo: repo, Number: num, HeadSHA: head, Perspective: p, Agent: "reviewer", Dispatched: time.Now().UTC()})
		switch p {
		case review.PerspectiveSecurity:
			elems = append(elems, verdictJSONWithSummary(t, repo, num, p, "requires_human", head, rateLimitedSummaries1116[p]))
		case review.PerspectiveCorrectness:
			elems = append(elems, verdictJSONWithSummary(t, repo, num, p, "requires_human", head, "Read the diff; unclear whether this groundwork should land before its consumer."))
		default:
			elems = append(elems, verdictJSONWithSummary(t, repo, num, p, "approve", head, "Read the diff; fine."))
		}
	}
	withVerdictDispatchState(t, review.DispatchState{Pending: pending})

	recorded := c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: repo, Number: num, Agent: "reviewer", Report: "[" + strings.Join(elems, ",") + "]"}, dir)

	if len(recorded) != len(review.DefaultPerspectives)-1 {
		t.Fatalf("recorded %d verdicts, want every perspective but the unread security one", len(recorded))
	}
	artifact, err := review.Collect(dir, review.AggregateOptions{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(artifact.Items) != 1 {
		t.Fatalf("want one aggregate, got %+v", artifact.Items)
	}
	got := artifact.Items[0]
	if _, ok := got.Perspectives[review.PerspectiveSecurity]; ok {
		t.Fatalf("unread security perspective was counted: %v", got.Perspectives)
	}
	if got.MergeEligible || got.Verdict != review.VerdictIncomplete || got.RequiresHuman {
		t.Fatalf("aggregate = %s (merge_eligible=%t requires_human=%t), want incomplete: the head has had no security review", got.Verdict, got.MergeEligible, got.RequiresHuman)
	}
	if !reasonsContain(got.Reasons, "review coverage incomplete: missing security") {
		t.Fatalf("Reasons = %v, want the missing security perspective named", got.Reasons)
	}
}
