package main

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

type recordedAuditAt struct {
	ts, user, action, detail, agent, repo string
	target                                int
}

type fakeAuditAtSink struct{ got []recordedAuditAt }

func (f *fakeAuditAtSink) AuditLogRecordAt(ts, user, action, detail, agent, repo string, target int) {
	f.got = append(f.got, recordedAuditAt{ts, user, action, detail, agent, repo, target})
}

// #16116: a review the settle scan observed is recorded as pr_review_observed
// at the review's own submitted_at, with the detail the throughput counters
// de-duplicate (review_id) and attribute (actor_class) on.
func TestRecordObservedReview(t *testing.T) {
	sink := &fakeAuditAtSink{}
	submitted := time.Date(2026, 10, 31, 9, 30, 0, 0, time.FixedZone("CET", 3600))
	recordObservedReview(sink, github.PRReviewObservation{
		Repo: "acme/widgets", Number: 41, ReviewID: 9001, Reviewer: "alice",
		Attribution: "human", State: "APPROVED", SubmittedAt: submitted,
	})

	if len(sink.got) != 1 {
		t.Fatalf("recorded %d entries, want 1: %+v", len(sink.got), sink.got)
	}
	want := recordedAuditAt{
		ts:     "2026-10-31T08:30:00Z",
		user:   "system",
		action: github.AuditActionPRReviewObserved,
		detail: "repo=acme/widgets, number=41, review_id=9001, actor=alice, actor_class=human",
		agent:  github.AttributionAgentGovernor,
		repo:   "acme/widgets",
		target: 41,
	}
	if sink.got[0] != want {
		t.Fatalf("recorded %+v, want %+v", sink.got[0], want)
	}

	// Nothing to de-duplicate on, or no instant to bucket it at: not recorded.
	for _, bad := range []github.PRReviewObservation{
		{Repo: "acme/widgets", Number: 41, Reviewer: "alice", Attribution: "human", SubmittedAt: submitted},
		{Repo: "acme/widgets", Number: 41, ReviewID: 1, Reviewer: "alice", Attribution: "human"},
	} {
		recordObservedReview(sink, bad)
	}
	if len(sink.got) != 1 {
		t.Fatalf("observations with no review_id or submitted_at were recorded: %+v", sink.got[1:])
	}
	recordObservedReview(nil, github.PRReviewObservation{ReviewID: 1, SubmittedAt: submitted}) // must not panic
}
