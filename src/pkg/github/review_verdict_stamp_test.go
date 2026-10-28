package github

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/review"
)

const (
	stampHead   = "16708857eba9c1f0a6b2d3e4f5061728394a5b6c"
	stampDigest = "d2a28e0f58ca4b1e9f3c7a6d5e8b0c1f2a3b4c5d6e7f8091a2b3c4d5e6f70819"
)

func stampPending(perspective review.Perspective) review.PendingReview {
	p := pendingVerdict("o/r", 2341, perspective, "reviewer")
	p.HeadSHA = stampHead
	p.BodyDigest = stampDigest
	return p
}

// A dispatched verdict is grouped under the body digest its dispatch record
// names, whatever the reviewer copied back. Every miscopy seen in production
// (hivecommons/hive#14361) used to orphan the report in its own aggregate,
// so the planner's AggregateForBody lookup never saw that perspective
// delivered and the pending entry stayed forever.
func TestRecordReviewVerdictStampsDispatchBodyDigest(t *testing.T) {
	for name, reported := range map[string]string{
		"prose":         "Adds one new test file covering the never-raises arm; no production change.",
		"sha256 prefix": "sha256:" + stampDigest,
		"wrong hash":    "f0e2907ae8e2b1c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c",
		"missing":       "",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			c := &Client{logger: testLogger()}
			withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{
				stampPending(review.PerspectiveSecurity),
				stampPending(review.PerspectiveStyle),
			}})
			good := validVerdictJSONWithHeadAndBodyDigest(t, "o/r", 2341, "security", "approve", stampHead, stampDigest)
			var bad string
			if reported == "" {
				bad = validVerdictJSONWithHead(t, "o/r", 2341, "style", "approve", stampHead)
			} else {
				bad = validVerdictJSONWithHeadAndBodyDigest(t, "o/r", 2341, "style", "approve", stampHead, reported)
			}

			c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 2341, Agent: "reviewer", Report: good}, dir)
			recorded := c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 2341, Agent: "reviewer", Report: bad}, dir)

			if len(recorded) != 1 || recorded[0].BodyDigest != stampDigest {
				t.Fatalf("recorded = %+v, want the verdict stamped with the dispatch digest", recorded)
			}
			artifact, err := review.Collect(dir, review.AggregateOptions{})
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if len(artifact.Items) != 1 {
				t.Fatalf("want one aggregate for the dispatched body, got %+v", artifact.Items)
			}
			agg, ok := artifact.AggregateForBody("o/r", 2341, stampHead, stampDigest)
			if !ok || agg.BodyDigest != stampDigest {
				t.Fatalf("AggregateForBody = %+v, %v; want the dispatch digest", agg, ok)
			}
			for _, p := range []review.Perspective{review.PerspectiveSecurity, review.PerspectiveStyle} {
				if agg.Perspectives[p] == "" {
					t.Fatalf("perspective %s missing from the dispatched body's aggregate, so its pending entry would never clear: %+v", p, agg.Perspectives)
				}
			}
		})
	}
}

// A head_sha that names no revision (prose, the schema placeholder) still
// binds to the dispatch, and the report is stored under the dispatch head.
func TestRecordReviewVerdictStampsDispatchHead(t *testing.T) {
	for name, reported := range map[string]string{
		"prose":       "Tests-only PR; no production change.",
		"placeholder": "<head commit sha>",
		"abbreviated": stampHead[:7],
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			c := &Client{logger: testLogger()}
			withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{
				stampPending(review.PerspectiveCorrectness),
			}})
			raw := validVerdictJSONWithHeadAndBodyDigest(t, "o/r", 2341, "correctness", "approve", reported, stampDigest)

			recorded := c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 2341, Agent: "reviewer", Report: raw}, dir)

			if len(recorded) != 1 || recorded[0].HeadSHA != stampHead || recorded[0].BodyDigest != stampDigest {
				t.Fatalf("recorded = %+v, want head %s and digest %s from the dispatch", recorded, stampHead, stampDigest)
			}
		})
	}
}

// A real but different head still does not bind to the dispatch: stamping
// only replaces values that the dispatch match already accepted.
func TestRecordReviewVerdictDoesNotStampOtherHead(t *testing.T) {
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{
		stampPending(review.PerspectiveCorrectness),
	}})
	other := "0123456789abcdef0123456789abcdef01234567"
	raw := validVerdictJSONWithHeadAndBodyDigest(t, "o/r", 2341, "correctness", "approve", other, "body-x")

	recorded := c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 2341, Agent: "reviewer", Report: raw}, dir)

	if len(recorded) != 1 || recorded[0].HeadSHA != other || recorded[0].BodyDigest != "body-x" {
		t.Fatalf("recorded = %+v, want an advisory verdict kept as written", recorded)
	}
}

// An advisory verdict has no dispatch record to stamp from and keeps the
// head and digest it reported.
func TestRecordReviewVerdictAdvisoryKeepsReportedDigest(t *testing.T) {
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	withVerdictDispatchState(t, review.DispatchState{})
	raw := validVerdictJSONWithHeadAndBodyDigest(t, "o/r", 2341, "correctness", "approve", stampHead, "body-x")

	recorded := c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 2341, Agent: "reviewer", Report: raw}, dir)

	if len(recorded) != 1 || recorded[0].HeadSHA != stampHead || recorded[0].BodyDigest != "body-x" {
		t.Fatalf("recorded = %+v, want the advisory verdict kept as written", recorded)
	}
}

// A dispatch record with no digest (a held PR dispatched before its poll
// record carried one) stamps an empty digest over whatever each head wrote,
// so all heads for the PR and head SHA land in one aggregate
// (hivecommons/hive#14594).
func TestRecordReviewVerdictStampsEmptyDispatchBodyDigest(t *testing.T) {
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	security := stampPending(review.PerspectiveSecurity)
	security.BodyDigest = ""
	style := stampPending(review.PerspectiveStyle)
	style.BodyDigest = ""
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{security, style}})

	first := validVerdictJSONWithHeadAndBodyDigest(t, "o/r", 2341, "security", "approve", stampHead, "f0e2907ae8e2b1c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c")
	second := validVerdictJSONWithHeadAndBodyDigest(t, "o/r", 2341, "style", "approve", stampHead, "sha256:"+stampDigest)
	for _, raw := range []string{first, second} {
		recorded := c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 2341, Agent: "reviewer", Report: raw}, dir)
		if len(recorded) != 1 || recorded[0].BodyDigest != "" {
			t.Fatalf("recorded = %+v, want the verdict stamped with the empty dispatch digest", recorded)
		}
	}

	artifact, err := review.Collect(dir, review.AggregateOptions{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(artifact.Items) != 1 {
		t.Fatalf("want one aggregate for the PR and head, got %+v", artifact.Items)
	}
	agg, ok := artifact.AggregateForBody("o/r", 2341, stampHead, "")
	if !ok {
		t.Fatal("AggregateForBody found no aggregate for the dispatched head")
	}
	for _, p := range []review.Perspective{review.PerspectiveSecurity, review.PerspectiveStyle} {
		if agg.Perspectives[p] == "" {
			t.Fatalf("perspective %s missing from the shared aggregate: %+v", p, agg.Perspectives)
		}
	}
}
