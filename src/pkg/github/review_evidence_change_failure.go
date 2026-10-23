package github

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
)

// Change-failure recording (hivecommons/hive#13241). Two signals that a
// merged PR did not stick are written onto the merged PR's evidence bundle so
// the Outcome card can report a change-failure rate per author kind:
//
//   - a revert: a later merged PR whose title or body names this PR
//     (`Reverts #N`, GitHub's Revert button) or its commit (`This reverts
//     commit <sha>`). The reverting PR need not have a bundle of its own;
//     the link is written as reverted_by on the original, exactly as the
//     merge-time linker does for hive-merged reverts. Detection uses only
//     text already in hand — a pull_request webhook payload, the PR detail
//     the review-outcome ledger fetched, or a merged-PR listing — never a
//     GitHub call of its own.
//   - an issue reopen: the duplicate-PR claim ledger voided a merged claim
//     because a person reopened the issue the PR closed (webhook or poll).
//
// Both are record-only and bounded by evidence.LinkWindow (14 days after the
// merge). Every failure is logged and swallowed.

// MergedPRText is what revert detection needs from a merged PR.
type MergedPRText struct {
	// Repo is owner/name.
	Repo     string
	Number   int
	HeadSHA  string
	Title    string
	Body     string
	MergedAt time.Time
}

// reviewEvidenceMaxRevertSHAScan bounds how many PR directories one
// SHA-named revert walks looking for the merged bundle it undid.
const reviewEvidenceMaxRevertSHAScan = 2000

// RecordReviewEvidenceRevert links pr, a merged PR whose text claims to
// revert something, to the bundles of the same-repo PRs it undid, writing
// reverted_by on each. A PR whose text does not look like a revert costs
// nothing beyond two regexp matches.
func (c *Client) RecordReviewEvidenceRevert(pr MergedPRText) {
	if c == nil || pr.Number <= 0 || pr.MergedAt.IsZero() {
		return
	}
	if !evidence.LooksLikeRevert(pr.Title, pr.Body) {
		return
	}
	settings, ok := c.reviewEvidenceSettings(pr.Repo)
	if !ok {
		return
	}
	owner, name := c.splitRepo(strings.TrimSpace(pr.Repo))
	full := owner + "/" + name
	targets := evidence.ParseRevertTargets(full, pr.Number, pr.Title, pr.Body)
	if len(targets.PRs)+len(targets.SHAs) == 0 {
		return
	}
	ids := reviewEvidenceRevertTargetIDs(full, targets, pr.MergedAt)
	if len(ids) == 0 {
		c.logger.Debug("review evidence: revert names no merged bundle in the 14 d window",
			slog.String("repo", full), slog.Int("number", pr.Number))
		return
	}
	key, err := evidence.LoadSigningKey(settings.SigningKeyFile)
	if err != nil {
		c.logger.Warn("review evidence: signing key unusable, writing unsigned",
			slog.String("error", err.Error()))
		key = nil
	}
	from := evidence.RevertBundleID(full, pr.Number, pr.HeadSHA)
	reviewEvidenceMu.Lock()
	defer reviewEvidenceMu.Unlock()
	for _, id := range ids {
		c.addReviewEvidenceBacklink(id, from, true, key, pr.MergedAt.UTC())
	}
}

// reviewEvidenceRevertTargetIDs resolves the PR numbers and commit SHAs a
// revert names to the ids of bundles recording a merge within LinkWindow
// before revertedAt, in first-mention order and without duplicates.
func reviewEvidenceRevertTargetIDs(repo string, targets evidence.RevertTargets, revertedAt time.Time) []string {
	var ids []string
	add := func(id string) {
		if id == "" {
			return
		}
		for _, have := range ids {
			if have == id {
				return
			}
		}
		ids = append(ids, id)
	}
	looked := 0
	for _, n := range targets.PRs {
		if looked >= reviewEvidenceMaxLinkRefs {
			break
		}
		looked++
		add(mergedReviewEvidenceID(repo, n, revertedAt))
	}
	if len(targets.SHAs) > 0 {
		for _, id := range mergedReviewEvidenceIDsBySHA(repo, targets.SHAs, revertedAt) {
			add(id)
		}
	}
	return ids
}

// mergedReviewEvidenceIDsBySHA walks repo's bundle directories for merged
// bundles whose merge commit or head one of shas names, within LinkWindow
// before revertedAt. Reverts are rare and the walk is bounded, so the cost is
// acceptable where it happens.
func mergedReviewEvidenceIDsBySHA(repo string, shas []string, revertedAt time.Time) []string {
	dir := filepath.Dir(ReviewEvidenceDir("", repo, 1))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var ids []string
	scanned := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		number, err := strconv.Atoi(e.Name())
		if err != nil || number <= 0 {
			continue
		}
		if scanned >= reviewEvidenceMaxRevertSHAScan {
			break
		}
		scanned++
		bundles, err := ListReviewEvidence("", repo, number)
		if err != nil {
			continue
		}
		for i := len(bundles) - 1; i >= 0; i-- {
			b := bundles[i].Bundle
			if b.Merge == nil {
				continue
			}
			for _, sha := range shas {
				if evidence.RevertsMergedBundle(b, sha, revertedAt) {
					ids = append(ids, b.ID)
					break
				}
			}
			break
		}
	}
	return ids
}

// RecordReviewEvidenceIssueReopen stamps the merged PR that claimed issue
// (repo, number) with an issue_reopens entry when a person reopened it at
// `at`. claim is the duplicate-PR claim the
// ledger just voided; its PRRepo/PRNumber name the closing PR and MergedAt
// its merge. Nothing is written for a PR with no bundle or whose merge the
// hive never recorded. Legacy bundles without issue closures retain the
// 14-day limit; captured closures keep later reopens for the 30-day rate.
func (c *Client) RecordReviewEvidenceIssueReopen(claim IssueClaim, at time.Time) {
	if c == nil || claim.PRNumber <= 0 || claim.Issue <= 0 || at.IsZero() {
		return
	}
	prRepo := strings.TrimSpace(claim.PRRepo)
	if prRepo == "" {
		prRepo = strings.TrimSpace(claim.Repo)
	}
	settings, ok := c.reviewEvidenceSettings(prRepo)
	if !ok {
		return
	}
	owner, name := c.splitRepo(prRepo)
	full := owner + "/" + name
	issueRepo := ""
	if iOwner, iName := c.splitRepo(strings.TrimSpace(claim.Repo)); !strings.EqualFold(iOwner+"/"+iName, full) {
		issueRepo = iOwner + "/" + iName
	}
	key, err := evidence.LoadSigningKey(settings.SigningKeyFile)
	if err != nil {
		c.logger.Warn("review evidence: signing key unusable, writing unsigned",
			slog.String("error", err.Error()))
		key = nil
	}

	reviewEvidenceMu.Lock()
	defer reviewEvidenceMu.Unlock()

	entries, err := ListVerifiedReviewEvidence("", full, claim.PRNumber, reviewEvidenceVerifierForKey(key))
	if err != nil || len(entries) == 0 {
		return
	}
	// Prefer the record that carries the merge; fall back to the newest.
	entry := entries[len(entries)-1]
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Bundle.Merge != nil {
			entry = entries[i]
			break
		}
	}
	b, path := entry.Bundle, entry.Path
	mergedAt := claim.MergedAt
	if b.Merge != nil {
		mergedAt = b.Merge.At
	}
	closureRepo := issueRepo
	if closureRepo == "" {
		closureRepo = full
	}
	knownClosure := false
	for _, closure := range b.IssueClosures {
		if closure.Issue == claim.Issue && strings.EqualFold(closure.Repo, closureRepo) && at.After(closure.At) {
			knownClosure = true
			break
		}
	}
	if mergedAt.IsZero() || (!knownClosure && !evidence.WithinLinkWindow(mergedAt, at)) {
		return
	}
	if !b.AddIssueReopen(evidence.IssueReopen{Issue: claim.Issue, Repo: issueRepo, At: at.UTC(), Source: evidence.IssueReopenSourceClaimLedger}) {
		return
	}
	b.UpdatedAt = at.UTC()
	if err := b.Validate(); err != nil {
		c.logger.Warn("review evidence: reopen-stamped bundle invalid, not written",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}
	if err := evidence.Seal(b, key); err != nil {
		c.logger.Warn("review evidence: could not seal reopen-stamped bundle",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}
	if err := writeReviewEvidence(path, b); err != nil {
		c.logger.Warn("review evidence: could not write reopen-stamped bundle",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}
	c.logger.Info("review evidence: issue reopen recorded as change failure",
		slog.String("id", b.ID), slog.Int("issue", claim.Issue), slog.String("issue_repo", issueRepo), slog.Time("reopened_at", at))
}

// IssueReopenRecorder is told, with the voided claim and the reopen instant,
// that ClaimLedger.MarkIssueReopened voided a merged claim. The GitHub
// client registers RecordReviewEvidenceIssueReopen so the signal reaches the
// evidence bundle from both the webhook and the enumeration path.
type IssueReopenRecorder func(claim IssueClaim, at time.Time)

var (
	issueReopenRecorderMu sync.RWMutex
	issueReopenRecorderFn IssueReopenRecorder
)

// SetIssueReopenRecorder installs (or, with nil, removes) the process-wide
// IssueReopenRecorder.
func SetIssueReopenRecorder(fn IssueReopenRecorder) {
	issueReopenRecorderMu.Lock()
	issueReopenRecorderFn = fn
	issueReopenRecorderMu.Unlock()
}

func issueReopenRecorder() IssueReopenRecorder {
	issueReopenRecorderMu.RLock()
	defer issueReopenRecorderMu.RUnlock()
	return issueReopenRecorderFn
}

// webhookMergedPR extracts the merged PR from a pull_request closed delivery;
// ok is false for any other event or an unmerged close.
func webhookMergedPR(event string, p webhookEventPayload) (MergedPRText, bool) {
	if event != "pull_request" || p.Action != "closed" || p.PullRequest == nil || !p.PullRequest.Merged || p.PullRequest.Number <= 0 {
		return MergedPRText{}, false
	}
	out := MergedPRText{
		Repo:    p.Repository.FullName,
		Number:  p.PullRequest.Number,
		HeadSHA: p.PullRequest.Head.SHA,
		Title:   p.PullRequest.Title,
		Body:    p.PullRequest.Body,
	}
	if p.PullRequest.MergedAt != nil {
		out.MergedAt = p.PullRequest.MergedAt.UTC()
	}
	if out.MergedAt.IsZero() {
		out.MergedAt = sharedWebhookTracker.now().UTC()
	}
	return out, true
}
