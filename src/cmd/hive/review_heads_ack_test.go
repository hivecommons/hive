package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/review"
)

type fakeReviewCommentPoster struct {
	posted []string
	fail   bool
}

func (f *fakeReviewCommentPoster) CreateIssueCommentBudgeted(_ context.Context, repo string, number int, body string) error {
	if f.fail {
		return errors.New("boom")
	}
	f.posted = append(f.posted, repo+"#"+body)
	return nil
}

// The review-heads skip rule settles a clean PR head with no decision
// session, so nothing else posts its acknowledgement (hivecommons/hive#13542).
func TestPostReviewAcknowledgementsPostsEachPlannedLine(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	poster := &fakeReviewCommentPoster{}
	plan := review.DispatchPlan{Acknowledgements: []review.ReviewAcknowledgement{
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Body: "**Reviewed** — no findings from correctness."},
	}}
	postReviewAcknowledgements(context.Background(), poster, plan, logger)
	if len(poster.posted) != 1 || poster.posted[0] != "acme/hive#**Reviewed** — no findings from correctness." {
		t.Fatalf("posted = %v", poster.posted)
	}

	failing := &fakeReviewCommentPoster{fail: true}
	postReviewAcknowledgements(context.Background(), failing, plan, logger) // logged, not fatal
	postReviewAcknowledgements(context.Background(), poster, review.DispatchPlan{}, logger)
	if len(poster.posted) != 1 {
		t.Fatalf("posted with nothing planned: %v", poster.posted)
	}
}
