package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/evidence"
)

// ReviewEvidenceMarker stamps the merge comment that points a PR at its
// evidence bundle. One per PR: the marker carries no head, so a re-run of the
// sweep, a dedup replay or a second merge path finds it and posts nothing
// (same idempotency pattern as SentinelMarker; hivecommons/hive#11062).
const ReviewEvidenceMarker = "<!-- hive-review-evidence -->"

// reviewEvidenceMergeTimeout bounds the post-merge evidence work. It runs on
// the merge path after GitHub has already merged, so it must never hold that
// path for long.
const reviewEvidenceMergeTimeout = 30 * time.Second

// ReviewEvidenceComment is the body of the merge pointer comment.
func ReviewEvidenceComment(repo string, number int, id, hash string) string {
	return fmt.Sprintf("%s Evidence bundle `%s` sha256:%s — download from the dashboard or `hivectl review evidence %s#%d`.",
		ReviewEvidenceMarker, id, hash, strings.TrimSpace(repo), number)
}

// recordReviewEvidenceMerge completes the merged head's bundle with the merge
// event and posts the one marker comment pointing at it. Called from
// RecordPRMergedAudit, which every merge path (relay, sweep, queue) calls once
// GitHub has merged. Like the relay's writes, every failure is logged and
// swallowed: the merge already happened and must not be reported as failed.
func (c *Client) recordReviewEvidenceMerge(repo string, number int, method, sha, path string) {
	settings, ok := c.reviewEvidenceSettings(repo)
	if !ok || number <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), reviewEvidenceMergeTimeout)
	defer cancel()
	owner, name := c.splitRepo(strings.TrimSpace(repo))
	full := owner + "/" + name

	var head, actor string
	if c.client != nil {
		pr, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:review_evidence_merge"), owner, name, number)
		if err != nil {
			c.logger.Warn("review evidence: could not fetch merged PR",
				slog.String("repo", full), slog.Int("number", number), slog.String("error", err.Error()))
		} else {
			head = pr.GetHead().GetSHA()
			actor = pr.GetMergedBy().GetLogin()
		}
	}
	if actor == "" {
		actor = "hive:" + strings.TrimSpace(path)
	}
	if method = strings.TrimSpace(method); method == "" {
		method = "squash"
	}
	if sha = strings.TrimSpace(sha); sha == "" {
		sha = head
	}
	merge := evidence.MergeEvent{Actor: actor, Method: method, At: time.Now().UTC(), SHA: sha}
	b := c.completeReviewEvidenceMerge(full, number, head, merge, settings)
	if b == nil || c.client == nil {
		return
	}
	posted, err := hasMarkerComment(ctx, c.client, owner, name, number, ReviewEvidenceMarker)
	if err != nil {
		c.logger.Warn("review evidence: could not check for merge comment",
			slog.String("repo", full), slog.Int("number", number), slog.String("error", err.Error()))
		return
	}
	if posted {
		return
	}
	body := ReviewEvidenceComment(full, number, b.ID, b.Hash)
	if _, _, err := c.client.Issues.CreateComment(ctx, owner, name, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		c.logger.Warn("review evidence: could not post merge comment",
			slog.String("repo", full), slog.Int("number", number), slog.String("error", err.Error()))
	}
}

// completeReviewEvidenceMerge records merge on the bundle for head (the newest
// bundle when head is unknown or has none) and returns the sealed bundle, or
// nil when there is no bundle to point at. A bundle that already records a
// merge is returned untouched, so a replayed merge never churns its hash.
func (c *Client) completeReviewEvidenceMerge(repo string, number int, head string, merge evidence.MergeEvent, settings ReviewEvidenceSettings) *evidence.Bundle {
	reviewEvidenceMu.Lock()
	defer reviewEvidenceMu.Unlock()

	var (
		b    *evidence.Bundle
		path string
	)
	if head != "" {
		path = ReviewEvidencePath("", repo, number, head)
		got, err := LoadReviewEvidence(path)
		switch {
		case err == nil:
			b = got
		case !errors.Is(err, os.ErrNotExist):
			c.logger.Warn("review evidence: merged head's bundle unreadable, left untouched",
				slog.String("path", path), slog.String("error", err.Error()))
			return nil
		}
	}
	if b == nil {
		found, err := FindReviewEvidence("", repo, number, "")
		if err != nil || found.Entry == nil {
			c.logger.Info("review evidence: no bundle for merged PR, no merge comment posted",
				slog.String("repo", repo), slog.Int("number", number), slog.String("head", head))
			return nil
		}
		b, path = found.Entry.Bundle, found.Entry.Path
	}
	if b.Merge != nil {
		return b
	}
	if merge.SHA == "" {
		merge.SHA = b.HeadSHA
	}
	b.Merge = &merge
	b.UpdatedAt = merge.At
	if err := b.Validate(); err != nil {
		c.logger.Warn("review evidence: merged bundle invalid, not written",
			slog.String("path", path), slog.String("error", err.Error()))
		return nil
	}
	key, err := evidence.LoadSigningKey(settings.SigningKeyFile)
	if err != nil {
		c.logger.Warn("review evidence: signing key unusable, writing unsigned",
			slog.String("error", err.Error()))
		key = nil
	}
	if err := evidence.Seal(b, key); err != nil {
		c.logger.Warn("review evidence: could not seal merged bundle",
			slog.String("path", path), slog.String("error", err.Error()))
		return nil
	}
	if err := writeReviewEvidence(path, b); err != nil {
		c.logger.Warn("review evidence: could not write merged bundle",
			slog.String("path", path), slog.String("error", err.Error()))
		return nil
	}
	c.logger.Info("review evidence: merge recorded",
		slog.String("id", b.ID), slog.String("merge_sha", merge.SHA), slog.Bool("signed", b.Signed))
	return b
}
