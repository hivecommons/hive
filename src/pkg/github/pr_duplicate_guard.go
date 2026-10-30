package github

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/dupsweep"
	"github.com/hivecommons/hive/pkg/prstatus"
)

// Duplicate reasons reported in CreatePRResult.DuplicateReason.
const (
	// DuplicateReasonClosesIssue: an open PR already carries Closes/Fixes for
	// an issue this PR also closes.
	DuplicateReasonClosesIssue = "closes-issue"
	// DuplicateReasonOverlappingFiles: an open App-bot PR edits an overlapping
	// file set (identical, or one set contained in the other).
	DuplicateReasonOverlappingFiles = "overlapping-files"
	// DuplicateReasonMergedFix: a merged PR on the same base already closed an
	// issue this PR also closes.
	DuplicateReasonMergedFix = "merged-fix"
)

// duplicateGuardMarker tags the redirect comment so a retried request does not
// stack a second one on the same PR.
const duplicateGuardMarker = "<!-- hive:create-pr-duplicate-guard -->"

// maxMergedFixCandidates bounds how many recently closed PRs the merged-fix
// check reads.
const maxMergedFixCandidates = 50

// guardHeadSentinelNumber stands in for the not-yet-opened head PR when it is
// clustered with dupsweep, which drops members with Number <= 0. No real PR
// number reaches it.
const guardHeadSentinelNumber = math.MaxInt32

var closingRefRe = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s*:?\s+(?:https://github\.com/([\w.-]+/[\w.-]+)/issues/|([\w.-]+/[\w.-]+)#|#)(\d+)`)

// closingIssueRefs returns the issue numbers body closes in owner/repo via a
// GitHub closing keyword. References into other repositories are ignored.
func closingIssueRefs(body, owner, repo string) map[int]bool {
	out := map[int]bool{}
	for _, m := range closingRefRe.FindAllStringSubmatch(body, -1) {
		target := m[1]
		if target == "" {
			target = m[2]
		}
		if target != "" && !strings.EqualFold(target, owner+"/"+repo) {
			continue
		}
		if n, err := strconv.Atoi(m[3]); err == nil {
			out[n] = true
		}
	}
	return out
}

func sharedIssue(a, b map[int]bool) (int, bool) {
	best := 0
	for n := range a {
		if b[n] && (best == 0 || n < best) {
			best = n
		}
	}
	return best, best != 0
}

type duplicateFinding struct {
	pr     *gh.PullRequest
	reason string
	issue  int
}

// findDuplicatePR is the PR-side admission check CreatePR runs after the
// head-branch and identical-tree checks. It returns the first existing PR that
// makes opening this one redundant, or nil. Retryable lookup errors are
// returned for the caller to queue; a candidate whose evidence cannot be read
// definitively is skipped rather than blocking the PR.
func (c *Client) findDuplicatePR(ctx context.Context, owner, repo, head, base, body string) (*duplicateFinding, error) {
	mine := closingIssueRefs(body, owner, repo)

	open, _, err := c.client.PullRequests.List(ctx, owner, repo, &gh.PullRequestListOptions{
		State:       "open",
		Base:        base,
		ListOptions: gh.ListOptions{PerPage: 100},
	})
	if err != nil {
		return nil, err
	}

	var candidates []*gh.PullRequest
	for _, pr := range open {
		if pr == nil || pr.GetHead().GetRef() == head || pr.GetDraft() {
			continue
		}
		if n, ok := sharedIssue(mine, closingIssueRefs(pr.GetBody(), owner, repo)); ok {
			return &duplicateFinding{pr: pr, reason: DuplicateReasonClosesIssue, issue: n}, nil
		}
		candidates = append(candidates, pr)
	}

	if f, err := c.findOverlappingBotPR(ctx, owner, repo, head, base, candidates); err != nil || f != nil {
		return f, err
	}

	if len(mine) == 0 {
		return nil, nil
	}
	closed, _, err := c.client.PullRequests.List(ctx, owner, repo, &gh.PullRequestListOptions{
		State:       "closed",
		Base:        base,
		Sort:        "updated",
		Direction:   "desc",
		ListOptions: gh.ListOptions{PerPage: maxMergedFixCandidates},
	})
	if err != nil {
		return nil, err
	}
	for _, pr := range closed {
		if pr == nil || pr.MergedAt == nil {
			continue
		}
		n, ok := sharedIssue(mine, closingIssueRefs(pr.GetBody(), owner, repo))
		if !ok {
			continue
		}
		// A merged Closes #N closed N; if N is open again it was reopened
		// (incomplete fix or revert) and a follow-up PR is exactly what is
		// wanted, so only a still-closed issue counts as already fixed.
		issue, _, err := c.client.Issues.Get(ctx, owner, repo, n)
		if err != nil {
			return nil, err
		}
		if issue.GetState() != "closed" {
			continue
		}
		return &duplicateFinding{pr: pr, reason: DuplicateReasonMergedFix, issue: n}, nil
	}
	return nil, nil
}

// findOverlappingBotPR runs the dupsweep file-set clustering synchronously over
// head's changed files and each open App-bot PR's. Overlap means identical sets
// or one set contained in the other (the #1434 ⊃ #1413 shape); a single shared
// path is not enough, because that is where the false positives live.
func (c *Client) findOverlappingBotPR(ctx context.Context, owner, repo, head, base string, candidates []*gh.PullRequest) (*duplicateFinding, error) {
	var bots []*gh.PullRequest
	for _, pr := range candidates {
		if !strings.HasSuffix(pr.GetUser().GetLogin(), "[bot]") {
			continue
		}
		if reason := c.unmergeableLabelReason(pr); reason != "" {
			c.logger.Info("duplicate guard: skipping held or stalled PR for the overlapping-files check",
				slog.String("repo", owner+"/"+repo), slog.Int("number", pr.GetNumber()), slog.String("reason", reason))
			continue
		}
		bots = append(bots, pr)
	}
	if len(bots) == 0 {
		return nil, nil
	}
	cmp, _, err := c.client.Repositories.CompareCommits(ctx, owner, repo, base, head, &gh.ListOptions{PerPage: 300})
	if err != nil {
		return nil, err
	}
	var headFiles []string
	for _, f := range cmp.Files {
		if f != nil {
			headFiles = append(headFiles, f.GetFilename())
		}
	}
	if len(headFiles) == 0 {
		return nil, nil
	}
	headSet := map[string]bool{}
	for _, f := range headFiles {
		headSet[f] = true
	}

	for i, pr := range bots {
		if i >= maxDuplicateTreeCandidates {
			c.logger.Info("duplicate guard: candidate cap reached, remaining open PRs not inspected",
				slog.String("repo", owner+"/"+repo), slog.Int("inspected", i), slog.Int("open_prs", len(bots)))
			break
		}
		if c.prIsDirty(ctx, owner, repo, pr.GetNumber()) {
			c.logger.Info("duplicate guard: skipping PR with merge conflicts for the overlapping-files check",
				slog.String("repo", owner+"/"+repo), slog.Int("number", pr.GetNumber()))
			continue
		}
		fp, ok := c.prChangedFiles(ctx, owner, repo, pr)
		if !ok {
			continue
		}
		cluster := dupsweep.Find([]dupsweep.PR{
			{Repo: owner + "/" + repo, Number: pr.GetNumber(), Files: fp.files},
			{Repo: owner + "/" + repo, Number: guardHeadSentinelNumber, Files: headFiles},
		}, dupsweep.Options{})
		if len(cluster) > 0 || fileSetContained(headSet, fp.files) {
			return &duplicateFinding{pr: pr, reason: DuplicateReasonOverlappingFiles}, nil
		}
	}
	return nil, nil
}

// unmergeableLabelReason returns the label that marks pr as not the place to
// continue new work, or "" when none is present. The hold class is the hive's
// canonical one (c.isHeld: HoldLabels plus operator-configured hold labels),
// so a PR the automerge sweep skips is skipped here too; do-not-merge,
// hive-pause/*, stage/stalled and needs-rebase are matched explicitly. Labels
// are carried by the list-pulls payload, so this runs before any per-PR fetch.
func (c *Client) unmergeableLabelReason(pr *gh.PullRequest) string {
	for _, l := range pr.Labels {
		name := l.GetName()
		lower := strings.ToLower(name)
		if lower == "do-not-merge" || strings.HasPrefix(lower, "hive-pause/") ||
			lower == "stage/stalled" || lower == "needs-rebase" || c.isHeld([]string{name}) {
			return name
		}
	}
	return ""
}

// prIsDirty reports whether the PR has merge conflicts. mergeable_state is only
// present on the single-PR GET, never in list responses, so it is fetched here
// for the capped set of candidates. Fetch errors and the transient "unknown"
// state are not treated as dirty.
func (c *Client) prIsDirty(ctx context.Context, owner, repo string, number int) bool {
	full, _, err := c.client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		c.logger.Info("duplicate guard: could not read mergeable state, treating PR as mergeable",
			slog.String("repo", owner+"/"+repo), slog.Int("number", number), slog.String("error", err.Error()))
		return false
	}
	return strings.EqualFold(full.GetMergeableState(), "dirty")
}

// fileSetContained reports whether either set contains the other entirely.
func fileSetContained(headSet map[string]bool, other []string) bool {
	if len(other) == 0 {
		return false
	}
	otherSet := map[string]bool{}
	inHead := 0
	for _, f := range other {
		otherSet[f] = true
		if headSet[f] {
			inHead++
		}
	}
	if inHead == len(otherSet) {
		return true
	}
	for f := range headSet {
		if !otherSet[f] {
			return false
		}
	}
	return true
}

func duplicateGuardComment(f *duplicateFinding, head string) string {
	var why string
	switch f.reason {
	case DuplicateReasonClosesIssue:
		why = fmt.Sprintf("this PR already closes #%d", f.issue)
	case DuplicateReasonMergedFix:
		why = fmt.Sprintf("this merged PR already closed #%d", f.issue)
	default:
		why = "this PR edits an overlapping set of files"
	}
	return fmt.Sprintf("%s\nAnother agent tried to open a PR from `%s`, but %s. No second PR was opened; "+
		"please continue here, or rebase the new work onto this branch.", duplicateGuardMarker, head, why)
}

// redirectToExistingPR posts the redirect comment on the existing PR, once.
// Best-effort: the refusal is the enforcement, the comment is courtesy. In
// quiet notification mode it is a section of the PR's hive status comment,
// edited in place, so a retried request or a second agent never emails the
// watchers again.
func (c *Client) redirectToExistingPR(ctx context.Context, owner, repo, head string, f *duplicateFinding) {
	if f.reason == DuplicateReasonMergedFix {
		return
	}
	if c.notificationsQuiet() {
		if _, err := c.UpsertPRStatusSection(ctx, owner+"/"+repo, f.pr.GetNumber(), prstatus.KeyDuplicateGuard, duplicateGuardComment(f, head)); err != nil {
			c.logger.Warn("duplicate guard: could not record the redirect in the status comment",
				slog.String("repo", repo), slog.Int("number", f.pr.GetNumber()), slog.String("error", err.Error()))
		}
		return
	}
	comments, _, err := c.client.Issues.ListComments(ctx, owner, repo, f.pr.GetNumber(), &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}})
	if err != nil {
		c.logger.Warn("duplicate guard: could not read comments on the existing PR, skipping redirect comment",
			slog.String("repo", repo), slog.Int("number", f.pr.GetNumber()), slog.String("error", err.Error()))
		return
	}
	for _, cm := range comments {
		if strings.Contains(cm.GetBody(), duplicateGuardMarker) && strings.Contains(cm.GetBody(), "`"+head+"`") {
			return
		}
	}
	if err := c.CreateIssueComment(ctx, owner+"/"+repo, f.pr.GetNumber(), duplicateGuardComment(f, head)); err != nil {
		c.logger.Warn("duplicate guard: could not post the redirect comment",
			slog.String("repo", repo), slog.Int("number", f.pr.GetNumber()), slog.String("error", err.Error()))
	}
}
