package automerge

// Tests for the review verdict gate (#12374). As with the human-merge-path
// gate, the invariant is that the MERGE DOES NOT HAPPEN while a verdict holds
// the PR: every refusal asserts zero PUT .../merge calls.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

var reviewHoldVerdictAt = time.Date(2026, 10, 14, 11, 6, 42, 0, time.UTC)

type reviewHoldActivity struct {
	login     string
	userType  string
	state     string // review state; empty for a comment
	body      string
	createdAt time.Time
}

// reviewHoldFront serves PR 7's reviews and comments and proxies everything
// else to the lane fixture.
type reviewHoldFront struct {
	mu           sync.Mutex
	reviews      []reviewHoldActivity
	comments     []reviewHoldActivity
	reviewStatus int
	hits         int
}

func newReviewHoldFront(t *testing.T, backend string, f *reviewHoldFront) *httptest.Server {
	t.Helper()
	target, err := url.Parse(backend)
	if err != nil {
		t.Fatalf("parse backend url: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	user := func(a reviewHoldActivity) map[string]string {
		return map[string]string{"login": a.login, "type": nonEmptyString(a.userType, "User")}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/reviews":
			f.hits++
			if f.reviewStatus != 0 {
				w.WriteHeader(f.reviewStatus)
				return
			}
			out := []map[string]any{}
			for _, a := range f.reviews {
				out = append(out, map[string]any{"user": user(a), "state": a.state, "submitted_at": a.createdAt.Format(time.RFC3339)})
			}
			json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/7/comments":
			f.hits++
			out := []map[string]any{}
			for _, a := range f.comments {
				out = append(out, map[string]any{"user": user(a), "body": a.body, "created_at": a.createdAt.Format(time.RFC3339)})
			}
			json.NewEncoder(w).Encode(out)
		default:
			proxy.ServeHTTP(w, r)
		}
	}))
}

func nonEmptyString(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func verdictHoldFor(repo string, hold ReviewVerdictHold) ReviewVerdictHoldFunc {
	return func(r string, number int) ReviewVerdictHold {
		if strings.EqualFold(r, repo) && number == 7 {
			return hold
		}
		return ReviewVerdictHold{}
	}
}

func requiresHumanHold(head string) ReviewVerdictHold {
	return ReviewVerdictHold{Held: true, Verdict: "requires_human", HeadSHA: head, RecordedAt: reviewHoldVerdictAt}
}

// A requires_human or reject verdict holds the App's own PR with
// review.require_approval off (the sweep never reads that setting), on the
// verdict's head and after a fix push moved the head, for the App and for a
// trusted bot author alike.
func TestSelfAuthoredSweepReviewVerdictHolds(t *testing.T) {
	for _, tc := range []struct {
		name       string
		author     string
		hold       ReviewVerdictHold
		wantReason string
	}{
		{name: "requires_human on the current head", hold: requiresHumanHold("sha7"), wantReason: autoMergeReasonReviewRequiresHuman},
		{name: "requires_human on an earlier head after a fix push", hold: requiresHumanHold("081022e"), wantReason: autoMergeReasonReviewRequiresHuman},
		{name: "reject verdict", hold: ReviewVerdictHold{Held: true, Verdict: "reject", HeadSHA: "sha7", RecordedAt: reviewHoldVerdictAt}, wantReason: autoMergeReasonReviewRejected},
		{name: "trusted bot author", author: "renovate[bot]", hold: requiresHumanHold("5d72aaf"), wantReason: autoMergeReasonReviewRequiresHuman},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merges := 0
			api := newSelfSweepGuardAPI(t, selfSweepFixture{author: tc.author, mergeApplied: true, mergeCalls: &merges})
			defer api.Close()
			front := &reviewHoldFront{
				// Neither a bot's approval, nor a person's approval from before
				// the verdict, nor a plain person comment releases the hold.
				reviews: []reviewHoldActivity{
					{login: testHiveAppBotLogin, userType: "Bot", state: "APPROVED", createdAt: reviewHoldVerdictAt.Add(time.Hour)},
					{login: "owner", state: "APPROVED", createdAt: reviewHoldVerdictAt.Add(-time.Hour)},
					{login: "owner", state: "COMMENTED", createdAt: reviewHoldVerdictAt.Add(time.Hour)},
				},
				comments: []reviewHoldActivity{
					{login: "owner", body: "please do not merge this yet", createdAt: reviewHoldVerdictAt.Add(time.Hour)},
					{login: "renovate[bot]", userType: "Bot", body: "/hive approve", createdAt: reviewHoldVerdictAt.Add(time.Hour)},
				},
			}
			srv := newReviewHoldFront(t, api.URL, front)
			defer srv.Close()

			c := newAutoMergeSweepClient(srv.URL)
			c.trustedBotAuthors = func() map[string]bool { return map[string]bool{"renovate[bot]": true} }
			c.reviewVerdictHold = verdictHoldFor("acme/widget", tc.hold)
			for tick := 0; tick < 2; tick++ {
				event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
				if err != nil {
					t.Fatalf("tick %d: err = %v, want nil", tick, err)
				}
				if reason != tc.wantReason {
					t.Fatalf("tick %d: reason = %q, want %q", tick, reason, tc.wantReason)
				}
				if event.MergeSHA != "" || merges != 0 {
					t.Fatalf("tick %d: event = %+v, merges = %d; want no merge", tick, event, merges)
				}
			}
		})
	}
}

// A person releases the hold with an approving review or a `/hive approve`
// reply posted after the verdict; with no hold the gate costs no API call.
func TestSelfAuthoredSweepReviewVerdictReleases(t *testing.T) {
	after := reviewHoldVerdictAt.Add(90 * time.Minute)
	for _, tc := range []struct {
		name     string
		hold     ReviewVerdictHold
		reviews  []reviewHoldActivity
		comments []reviewHoldActivity
		wantHits int
	}{
		{name: "no verdict hold", wantHits: 0},
		{name: "person approved after the verdict", hold: requiresHumanHold("081022e"),
			reviews: []reviewHoldActivity{{login: "owner", state: "APPROVED", createdAt: after}}, wantHits: 1},
		{name: "person replied /hive approve after the verdict", hold: requiresHumanHold("081022e"),
			comments: []reviewHoldActivity{{login: "owner", body: "Looked at the ruleset change.\n/hive approve", createdAt: after}}, wantHits: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merges := 0
			api := newSelfSweepGuardAPI(t, selfSweepFixture{mergeApplied: true, mergeCalls: &merges})
			defer api.Close()
			front := &reviewHoldFront{reviews: tc.reviews, comments: tc.comments}
			srv := newReviewHoldFront(t, api.URL, front)
			defer srv.Close()

			c := newAutoMergeSweepClient(srv.URL)
			if tc.hold.Held {
				c.reviewVerdictHold = verdictHoldFor("acme/widget", tc.hold)
			}
			event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
			if err != nil || reason != "" {
				t.Fatalf("reason = %q, err = %v; want clean merge", reason, err)
			}
			if event.MergeSHA != "merge7" || merges != 1 {
				t.Fatalf("event = %+v, merges = %d; want one merge", event, merges)
			}
			if front.hits != tc.wantHits {
				t.Fatalf("reviews/comments API hits = %d, want %d", front.hits, tc.wantHits)
			}
		})
	}
}

// When the reviews cannot be listed the gate cannot tell whether a person
// released the hold, so it fails closed.
func TestSelfAuthoredSweepReviewVerdictEvidenceErrorFailsClosed(t *testing.T) {
	merges := 0
	api := newSelfSweepGuardAPI(t, selfSweepFixture{mergeApplied: true, mergeCalls: &merges})
	defer api.Close()
	front := &reviewHoldFront{reviewStatus: http.StatusInternalServerError}
	srv := newReviewHoldFront(t, api.URL, front)
	defer srv.Close()

	c := New(newAutoMergeSweepClient(srv.URL).transport, Options{ReviewVerdictHold: verdictHoldFor("acme/widget", requiresHumanHold("sha7"))})
	_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
	if err == nil || reason != autoMergeReasonReviewHoldEvidence || merges != 0 {
		t.Fatalf("reason = %q, err = %v, merges = %d; want evidence failure with no merge", reason, err, merges)
	}
}

func TestIsReviewHoldReleaseCommand(t *testing.T) {
	for body, want := range map[string]bool{
		"/hive approve":                  true,
		"  /HIVE   Approve  please":      true,
		"lgtm\n/hive approve\n":          true,
		"I would not /hive approve this": false,
		"/hive decision A":               false,
		"looks fine":                     false,
	} {
		if got := isReviewHoldReleaseCommand(body); got != want {
			t.Errorf("isReviewHoldReleaseCommand(%q) = %v, want %v", body, got, want)
		}
	}
}
