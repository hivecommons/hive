package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
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
	collect := func(b *evidence.Bundle) {
		c.captureReviewEvidenceMergeContext(ctx, owner, name, b, merge.At)
	}
	b := c.completeReviewEvidenceMerge(full, number, head, merge, settings, collect)
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
// collect, when non-nil, completes the rest of the bundle (CI, human actions)
// before it is sealed; it runs only when the merge is new.
func (c *Client) completeReviewEvidenceMerge(repo string, number int, head string, merge evidence.MergeEvent, settings ReviewEvidenceSettings, collect func(*evidence.Bundle)) *evidence.Bundle {
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
	if collect != nil {
		collect(b)
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

// Human action kinds recorded in Bundle.HumanActions.
const (
	ReviewEvidenceActionApproval     = "approval"
	ReviewEvidenceActionLabelAdded   = "label_added"
	ReviewEvidenceActionLabelRemoved = "label_removed"
	ReviewEvidenceActionHoldLift     = "hold_lift"
)

// captureReviewEvidenceMergeContext fills b's CI summary and human actions
// from GitHub at merge time. Each half is best-effort: a fetch that fails is
// logged and leaves that half as it was, and never blocks the merge record.
func (c *Client) captureReviewEvidenceMergeContext(ctx context.Context, owner, name string, b *evidence.Bundle, now time.Time) {
	if c == nil || c.client == nil || b == nil {
		return
	}
	ctx = WithRESTCaller(ctx, "hive:review_evidence_merge")
	ci, err := reviewEvidenceCI(ctx, c.client, owner, name, b.HeadSHA, now)
	if err != nil {
		c.logger.Warn("review evidence: could not capture CI for merged head",
			slog.String("id", b.ID), slog.String("error", err.Error()))
	} else {
		b.CI = ci
	}
	actions, err := reviewEvidenceHumanActions(ctx, c.client, owner, name, b.Number, b.HeadSHA, b.CreatedAt)
	if err != nil {
		c.logger.Warn("review evidence: could not capture human actions for merged PR",
			slog.String("id", b.ID), slog.String("error", err.Error()))
		return
	}
	for _, a := range actions {
		if !containsEvidenceAction(b.HumanActions, a) {
			b.HumanActions = append(b.HumanActions, a)
		}
	}
	sort.SliceStable(b.HumanActions, func(i, j int) bool { return b.HumanActions[i].At.Before(b.HumanActions[j].At) })
}

// reviewEvidenceCI is the check-run summary for head: the latest run of each
// check (per name and app, as the merge CI gate sees them), sorted by name. A
// run that has not completed reports its status as its conclusion, so the
// bundle states it was still running rather than dropping it.
func reviewEvidenceCI(ctx context.Context, client *gh.Client, owner, name, head string, now time.Time) (evidence.CI, error) {
	if strings.TrimSpace(head) == "" {
		return evidence.CI{}, errors.New("head SHA unknown")
	}
	opts := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	var runs []*gh.CheckRun
	for {
		page, resp, err := client.Checks.ListCheckRunsForRef(ctx, owner, name, head, opts)
		if err != nil {
			return evidence.CI{}, fmt.Errorf("listing check runs: %w", err)
		}
		if page != nil {
			runs = append(runs, page.CheckRuns...)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	ci := evidence.CI{CapturedAt: now.UTC()}
	for _, cr := range latestCheckRunsByNameAndApp(runs) {
		checkName := strings.TrimSpace(cr.GetName())
		if checkName == "" {
			continue
		}
		conclusion := strings.TrimSpace(cr.GetConclusion())
		if conclusion == "" {
			conclusion = strings.TrimSpace(cr.GetStatus())
		}
		if conclusion == "" {
			conclusion = "unknown"
		}
		ci.Checks = append(ci.Checks, evidence.Check{Name: checkName, Conclusion: conclusion, URL: cr.GetHTMLURL()})
	}
	return ci, nil
}

// reviewEvidenceHumanActions lists the human actions on the PR that the bundle
// for head should carry: approvals of head or submitted since the bundle was
// started, and label adds and removes since then (removing a hold label is a
// hold lift). Bot accounts, including Hive's own App, are not humans and are
// left out; their writes are already in the hive audit log.
func reviewEvidenceHumanActions(ctx context.Context, client *gh.Client, owner, name string, number int, head string, since time.Time) ([]evidence.Action, error) {
	var actions []evidence.Action
	reviewOpts := &gh.ListOptions{PerPage: 100}
	for {
		reviews, resp, err := client.PullRequests.ListReviews(ctx, owner, name, number, reviewOpts)
		if err != nil {
			return nil, fmt.Errorf("listing reviews: %w", err)
		}
		for _, r := range reviews {
			if !strings.EqualFold(r.GetState(), "APPROVED") || !evidenceHumanUser(r.GetUser()) {
				continue
			}
			at := r.GetSubmittedAt().UTC()
			if r.GetCommitID() != head && at.Before(since) {
				continue
			}
			detail := strings.TrimSpace(r.GetHTMLURL())
			if detail == "" && r.GetCommitID() != "" {
				detail = "commit " + r.GetCommitID()
			}
			actions = append(actions, evidence.Action{Actor: r.GetUser().GetLogin(), Kind: ReviewEvidenceActionApproval, Detail: detail, At: at})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		reviewOpts.Page = resp.NextPage
	}
	eventOpts := &gh.ListOptions{PerPage: 100}
	for {
		events, resp, err := client.Issues.ListIssueEvents(ctx, owner, name, number, eventOpts)
		if err != nil {
			return nil, fmt.Errorf("listing issue events: %w", err)
		}
		for _, e := range events {
			label := strings.TrimSpace(e.GetLabel().GetName())
			if label == "" || !evidenceHumanUser(e.GetActor()) {
				continue
			}
			at := e.GetCreatedAt().UTC()
			if at.Before(since) {
				continue
			}
			var kind string
			switch e.GetEvent() {
			case "labeled":
				kind = ReviewEvidenceActionLabelAdded
			case "unlabeled":
				kind = ReviewEvidenceActionLabelRemoved
				if HasHoldLabel([]string{label}) {
					kind = ReviewEvidenceActionHoldLift
				}
			default:
				continue
			}
			actions = append(actions, evidence.Action{Actor: e.GetActor().GetLogin(), Kind: kind, Detail: label, At: at})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		eventOpts.Page = resp.NextPage
	}
	return actions, nil
}

// evidenceHumanUser reports whether u is a person rather than a bot or App,
// using the same test evidenceAuthor uses for the PR author.
func evidenceHumanUser(u *gh.User) bool {
	login := strings.TrimSpace(u.GetLogin())
	return login != "" && !strings.EqualFold(u.GetType(), "Bot") && !strings.HasSuffix(strings.ToLower(login), "[bot]")
}

func containsEvidenceAction(have []evidence.Action, a evidence.Action) bool {
	for _, h := range have {
		if h.Actor == a.Actor && h.Kind == a.Kind && h.Detail == a.Detail && h.At.Equal(a.At) {
			return true
		}
	}
	return false
}
