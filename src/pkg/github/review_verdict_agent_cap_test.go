package github

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/review"
)

// The three summaries the reviewer wrote on projectbluefin/utah-packages#959
// at head d09267d (2026-11-09), after the proxy refused its GitHub reads at
// its 300/h cap (01:33Z). None says "could not read", so the wording match
// (review.UnreadableReviewReason) let them through and the PR parked at 085.
var capStoppedSummaries959 = map[review.Perspective]string{
	review.PerspectiveCorrectness:  "Previous review had no findings to re-check. Read the full PR diff and 11-file delta; commit history identifies the delta as a merge of main containing #957. No concrete new correctness defect found. GitHub reads stopped at the user's request after the hourly cap; head-file surroundings, source-lock consumers, build behavior and head-specific tests could not be verified. Requires completion of those checks before correctness approval.",
	review.PerspectiveDocsCurrency: "Previous review had no findings to re-check. Delta documentation distinguishes recipe import from build, publication and runtime proof, consistent with the backlog record. No verified documentation defect found. Claims about HTTP rejection, source fallback behavior and build-root dependencies could not be checked against their consumers after GitHub reads stopped; documentation assessment remains incomplete.",
	review.PerspectiveSecurity:     "Previous review had no findings to re-check. Delta adds pinned firmware sources and a driver options file through a merge of main. No verified secret, injection or permission regression found in the diff. Source verification consumers and driver-option behavior could not be inspected after GitHub reads were stopped at the user's request; security assessment remains incomplete, not evidence of a vulnerability.",
}

type capRefusalRecord struct {
	agent string
	at    time.Time
	asked []time.Time
}

func (r *capRefusalRecord) refusedSince(agent string, since time.Time) (time.Time, bool) {
	r.asked = append(r.asked, since)
	if agent != r.agent || r.at.Before(since) {
		return time.Time{}, false
	}
	return r.at.Add(10 * time.Minute), true
}

func withAgentGitHubRefusals(t *testing.T, fn review.AgentGitHubRefusal) {
	t.Helper()
	old := AgentGitHubRefusalFunc()
	SetAgentGitHubRefusalFunc(fn)
	t.Cleanup(func() { SetAgentGitHubRefusalFunc(old) })
}

func capStoppedReport(t *testing.T, repo string, num int, head string, perspectives []review.Perspective, verdict string) string {
	t.Helper()
	var elems []string
	for _, p := range perspectives {
		elems = append(elems, verdictJSONWithSummary(t, repo, num, p, verdict, head, capStoppedSummaries959[p]))
	}
	return "[" + strings.Join(elems, ",") + "]"
}

var capStoppedPerspectives = []review.Perspective{review.PerspectiveCorrectness, review.PerspectiveDocsCurrency, review.PerspectiveSecurity}

func capStoppedDispatch(t *testing.T, repo string, num int, head string, dispatched time.Time) {
	t.Helper()
	var pending []review.PendingReview
	for _, p := range capStoppedPerspectives {
		pending = append(pending, review.PendingReview{Repo: repo, Number: num, HeadSHA: head, Perspective: p, Agent: "reviewer", Dispatched: dispatched})
	}
	withVerdictDispatchState(t, review.DispatchState{Pending: pending})
}

// projectbluefin/utah-packages#959: the proxy refused the reviewer's reads
// after the review was dispatched, and the reviewer said so in its own
// words. The relay must take the proxy's word for it, not the reviewer's:
// nothing recorded, no requires_human, the perspectives asked again after
// the pending TTL.
func TestRecordReviewVerdictCapStoppedSessionIsNoVerdict(t *testing.T) {
	const (
		repo = "projectbluefin/utah-packages"
		num  = 959
		head = "d09267d5f3b0c2a1e4d6b8a9c7e5f3a1b2c4d6e8"
	)
	for p, summary := range capStoppedSummaries959 {
		r := review.PerspectiveReport{Perspective: p, Verdict: review.VerdictRequiresHuman}
		r.Summary = summary
		if _, ok := review.UnreadableReviewReason(r); ok {
			t.Fatalf("test premise: the wording match already recognises %s: %q", p, summary)
		}
	}
	now := time.Now().UTC()
	dispatched := now.Add(-30 * time.Minute)
	capStoppedDispatch(t, repo, num, head, dispatched)
	dir := t.TempDir()
	refusals := &capRefusalRecord{agent: "reviewer", at: now.Add(-20 * time.Minute)}
	c := &Client{logger: testLogger()}
	withAgentGitHubRefusals(t, refusals.refusedSince)

	recorded := c.recordReviewVerdict(context.Background(), ReviewRequest{
		Repo: repo, Number: num, Agent: "reviewer", Event: ReviewEventRecordVerdict,
		Report: capStoppedReport(t, repo, num, head, capStoppedPerspectives, "requires_human"),
	}, dir)

	if len(recorded) != 0 {
		t.Fatalf("recorded %d verdicts from a session the proxy cut short: %+v", len(recorded), recorded)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, review.ReviewReportFilePrefix+"*")); len(matches) != 0 {
		t.Fatalf("cut-short review was written where the collector reads verdicts: %v", matches)
	}
	if len(refusals.asked) == 0 || !refusals.asked[0].Equal(dispatched) {
		t.Fatalf("refusal window started at %v, want the dispatch time %v", refusals.asked, dispatched)
	}

	state, err := review.LoadDispatchState("")
	if err != nil {
		t.Fatalf("load dispatch state: %v", err)
	}
	artifact, err := review.Collect(dir, review.AggregateOptions{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	pr := review.PullRequest{Repo: repo, Number: num, Title: "firmware sources", Author: "hanthor", HeadSHA: head}
	opts := review.DispatchOptions{
		RequireApproval:      true,
		FanOut:               true,
		CombinedPerspectives: true,
		MaxParallelReviews:   5,
		Agents:               []review.AgentCapability{{Name: "reviewer", Enabled: true, UsesKick: true, Role: "reviewer"}},
		Now:                  dispatched.Add(review.DefaultPendingReviewTTL + time.Minute),
	}
	retry := review.PlanDispatch([]review.PullRequest{pr}, artifact, state, opts)
	if len(retry.NewHuman) != 0 || len(retry.State.Human) != 0 {
		t.Fatalf("a cut-short review asked a person: new=%+v state=%+v", retry.NewHuman, retry.State.Human)
	}
	if len(retry.ReviewKicks) != 1 {
		t.Fatalf("review kicks after the TTL = %d, want one retry", len(retry.ReviewKicks))
	}
}

// The same requires_human is a verdict when the proxy refused nothing during
// the session: a refusal before the dispatch belongs to an earlier session,
// one for another agent is not this reviewer's, and an approve is a
// judgement whatever the proxy did.
func TestRecordReviewVerdictKeepsRequiresHumanWithoutARefusalInTheSession(t *testing.T) {
	const (
		repo = "o/r"
		num  = 11
		head = "abc123"
	)
	now := time.Now().UTC()
	dispatched := now.Add(-30 * time.Minute)
	cases := []struct {
		name     string
		refusals *capRefusalRecord
		verdict  string
	}{
		{"no proxy wired", nil, "requires_human"},
		{"refused before the dispatch", &capRefusalRecord{agent: "reviewer", at: dispatched.Add(-time.Minute)}, "requires_human"},
		{"another agent refused", &capRefusalRecord{agent: "scanner", at: now.Add(-time.Minute)}, "requires_human"},
		{"approve is a judgement", &capRefusalRecord{agent: "reviewer", at: now.Add(-time.Minute)}, "approve"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capStoppedDispatch(t, repo, num, head, dispatched)
			dir := t.TempDir()
			c := &Client{logger: testLogger()}
			if tc.refusals != nil {
				withAgentGitHubRefusals(t, tc.refusals.refusedSince)
			} else {
				withAgentGitHubRefusals(t, nil)
			}
			recorded := c.recordReviewVerdict(context.Background(), ReviewRequest{
				Repo: repo, Number: num, Agent: "reviewer",
				Report: capStoppedReport(t, repo, num, head, capStoppedPerspectives, tc.verdict),
			}, dir)
			if len(recorded) != len(capStoppedPerspectives) {
				t.Fatalf("recorded %d of %d verdicts: %+v", len(recorded), len(capStoppedPerspectives), recorded)
			}
		})
	}
}

// One cap-stopped session is fine; the gap was what came after. The relay
// set each requires_human aside, the row expired, and every expiry spent one
// of review.max_redispatch's three tries, so a reviewer capped for about
// 2h15m on projectbluefin/utah-packages#959's head still parked it as
// review-dispatch-exhausted and asked a person. Four capped sessions in a row
// now leave the head unheld and asked again; the same four sessions set
// aside on wording alone, with no proxy refusal behind them, still exhaust
// after three.
func TestCapStoppedSessionsInARowDoNotExhaustTheRedispatchBudget(t *testing.T) {
	const (
		repo = "projectbluefin/utah-packages"
		num  = 959
		head = "d09267d5f3b0c2a1e4d6b8a9c7e5f3a1b2c4d6e8"
	)
	pr := review.PullRequest{Repo: repo, Number: num, Title: "firmware sources", Author: "hanthor", HeadSHA: head}
	cases := []struct {
		name      string
		summaries map[review.Perspective]string
		refused   bool
		wantHold  int // the session after which the head is held; 0 = never within the run
	}{
		{"proxy refused every session", capStoppedSummaries959, true, 0},
		{"set aside on wording, no proxy refusal", rateLimitedSummaries1116, false, review.DefaultMaxRedispatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Start far enough back that every session in the run is in
			// the past, as the relay's lookback expects.
			dispatched := time.Now().UTC().Add(-6 * time.Hour)
			capStoppedDispatch(t, repo, num, head, dispatched)
			var lastRefusal time.Time
			refusal := func(agent string, since time.Time) (time.Time, bool) {
				if !tc.refused || agent != "reviewer" || lastRefusal.Before(since) {
					return time.Time{}, false
				}
				return lastRefusal.Add(10 * time.Minute), true
			}
			withAgentGitHubRefusals(t, refusal)
			c := &Client{logger: testLogger()}
			dir := t.TempDir()
			for session := 1; session <= review.DefaultMaxRedispatch+1; session++ {
				// The proxy refuses the session ten minutes in, and the
				// session reports requires_human in its own words.
				lastRefusal = dispatched.Add(10 * time.Minute)
				var elems []string
				for _, p := range capStoppedPerspectives {
					elems = append(elems, verdictJSONWithSummary(t, repo, num, p, "requires_human", head, tc.summaries[p]))
				}
				recorded := c.recordReviewVerdict(context.Background(), ReviewRequest{
					Repo: repo, Number: num, Agent: "reviewer", Event: ReviewEventRecordVerdict,
					Report: "[" + strings.Join(elems, ",") + "]",
				}, dir)
				if len(recorded) != 0 {
					t.Fatalf("session %d: recorded %d verdicts from a set-aside session", session, len(recorded))
				}

				state, err := review.LoadDispatchState("")
				if err != nil {
					t.Fatalf("load dispatch state: %v", err)
				}
				artifact, err := review.Collect(dir, review.AggregateOptions{})
				if err != nil {
					t.Fatalf("Collect: %v", err)
				}
				now := dispatched.Add(review.DefaultPendingReviewTTL + time.Minute)
				plan := review.PlanDispatch([]review.PullRequest{pr}, artifact, state, review.DispatchOptions{
					RequireApproval:      true,
					FanOut:               true,
					CombinedPerspectives: true,
					MaxParallelReviews:   5,
					Agents:               []review.AgentCapability{{Name: "reviewer", Enabled: true, UsesKick: true, Role: "reviewer"}},
					AgentGitHubRefusal:   AgentGitHubRefusalFunc(),
					Now:                  now,
				})
				held := len(plan.Exhausted) > 0
				if tc.wantHold == session {
					if !held || len(plan.NewHuman) != 1 {
						t.Fatalf("session %d: want the review-dispatch-exhausted hold, got exhausted=%+v new=%+v", session, plan.Exhausted, plan.NewHuman)
					}
					return
				}
				if held || len(plan.NewHuman) != 0 || len(plan.State.Human) != 0 {
					t.Fatalf("session %d: a person was asked: exhausted=%+v human=%+v", session, plan.Exhausted, plan.State.Human)
				}
				if len(plan.ReviewKicks) != 1 {
					t.Fatalf("session %d: review kicks = %d, want the head asked again", session, len(plan.ReviewKicks))
				}
				if err := review.WriteDispatchState("", plan.State); err != nil {
					t.Fatalf("write dispatch state: %v", err)
				}
				dispatched = now
			}
			if tc.wantHold != 0 {
				t.Fatalf("never reached the hold expected after session %d", tc.wantHold)
			}
		})
	}
}
