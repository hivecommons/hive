package github

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/writeback"
)

// ReviewBacklogCloser is an optional sink capability: a destination that can
// close an item as completed, with a note saying why. Sinks without it keep
// the lift-and-ask behaviour.
type ReviewBacklogCloser interface {
	CloseItem(ctx context.Context, repo string, ref ReviewBacklogRef, note string) error
}

// CloseItem posts note and closes the GitHub issue as completed.
func (s githubIssueBacklogSink) CloseItem(ctx context.Context, repo string, ref ReviewBacklogRef, note string) error {
	if ref.Number <= 0 {
		return fmt.Errorf("review backlog: GitHub item %q has no issue number", ref.ID)
	}
	// The item is the hive's own filing, so the gate only withholds the
	// close when the repo is paused or its write-back mode is off.
	item := writeback.Item{Number: ref.Number, HiveAuthored: true}
	if d := s.c.WritebackAllowed(repo, item, writeback.KindIssueClose, note); !d.Allowed {
		return fmt.Errorf("review backlog: close of %s#%d withheld: %s", repo, ref.Number, d.Reason)
	}
	if err := s.c.CreateIssueCommentBudgeted(ctx, repo, ref.Number, note); err != nil {
		return err
	}
	return s.c.CloseIssue(ctx, repo, ref.Number, IssueCloseOptions{StateReason: IssueStateReasonCompleted})
}

// reviewBacklogBaseVerdict is what the base-branch check could tell.
type reviewBacklogBaseVerdict int

const (
	reviewBacklogBaseUnknown reviewBacklogBaseVerdict = iota
	reviewBacklogBaseApplies
	reviewBacklogBaseGone
)

// reviewBacklogSelfVerify is the "does it still apply on base?" check the
// lift note used to ask the worker for (hivecommons/hive#15627). Once the
// source PR has merged it reads the finding's file on the base branch: a
// file that no longer exists, or that no longer contains any symbol the
// finding's title names, means the finding is gone and the item is closed as
// completed with a one-line note. Anything less certain — no file recorded,
// a title naming no symbol, a lookup error, or a sink that cannot close —
// reports not closed, and the caller lifts the block as before.
func (c *Client) reviewBacklogSelfVerify(ctx context.Context, sink ReviewBacklogSink, repo string, rec reviewBacklogFingerprintRecord, baseRef string, logger *slog.Logger) (bool, error) {
	closer, can := sink.(ReviewBacklogCloser)
	if !can || strings.TrimSpace(rec.File) == "" {
		return false, nil
	}
	verdict, why := c.reviewBacklogFindingOnBase(ctx, repo, baseRef, rec.File, rec.Title)
	if verdict != reviewBacklogBaseGone {
		return false, nil
	}
	note := fmt.Sprintf("%s\nClosed as completed: %s#%d merged and %s, so this finding no longer applies. Reopen if it does.", blockedUnblockedMarker, repo, rec.BlockedOnPR, why)
	if err := closer.CloseItem(ctx, repo, rec.ref(), note); err != nil {
		return false, err
	}
	if rec.ItemNumber > 0 {
		c.recordWriteAudit(AuditActionReviewBacklogIssueClosed, hiveWriteMeta(),
			WriteTarget{Repo: repo, Number: rec.ItemNumber},
			"pr", strconv.Itoa(rec.BlockedOnPR),
			"why", why)
	}
	if logger != nil {
		logger.Info("review backlog block sweep: finding gone from base, item closed", "item", rec.ItemKey, "pr", repo+"#"+strconv.Itoa(rec.BlockedOnPR), "why", why)
	}
	return true, nil
}

// reviewBacklogFindingOnBase reads file at baseRef ("" for the default
// branch) and reports whether the finding titled title can still be there.
func (c *Client) reviewBacklogFindingOnBase(ctx context.Context, repo, baseRef, file, title string) (reviewBacklogBaseVerdict, string) {
	if c == nil || c.client == nil {
		return reviewBacklogBaseUnknown, ""
	}
	file = normaliseFingerprintPath(file)
	if file == "" {
		return reviewBacklogBaseUnknown, ""
	}
	owner, name := c.splitRepo(repo)
	base := strings.TrimSpace(baseRef)
	if base == "" {
		base = "the default branch"
	}
	exists, err := c.PathExistsAtRef(ctx, owner, name, file, baseRef)
	if err != nil {
		return reviewBacklogBaseUnknown, ""
	}
	if !exists {
		return reviewBacklogBaseGone, fmt.Sprintf("`%s` no longer exists on %s", file, base)
	}
	symbols := reviewBacklogSymbols(reviewBacklogBareTitle(title))
	if len(symbols) == 0 {
		return reviewBacklogBaseUnknown, ""
	}
	content, err := c.GetFileContentRef(ctx, owner, name, file, baseRef)
	if err != nil {
		return reviewBacklogBaseUnknown, ""
	}
	names := make([]string, 0, len(symbols))
	for s := range symbols {
		if strings.Contains(content, s) {
			return reviewBacklogBaseApplies, ""
		}
		names = append(names, "`"+s+"`")
	}
	sort.Strings(names)
	return reviewBacklogBaseGone, fmt.Sprintf("%s is no longer in `%s` on %s", strings.Join(names, ", "), file, base)
}
