package github

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

const (
	CloseOnMergeMarker          = "<!-- hive-close-on-merge -->"
	closeOnMergeAwaitingMarker  = "<!-- hive-close-on-merge: awaiting-confirmation -->"
	closeOnMergeCommentPageSize = 100
)

var coveredByPRCommentPattern = regexp.MustCompile(`(?i)\bcovered\s+by\s+(?:open\s+)?(?:pr|pull request)\s+#(\d+)\b`)

type TrustedPRAuthorFunc func(repo, login string) bool

type CloseOnMergeOptions struct {
	Identity      HiveIdentity
	TrustedAuthor TrustedPRAuthorFunc
	Logger        *slog.Logger
}

type CloseOnMergeAction string

const (
	CloseOnMergeNoop        CloseOnMergeAction = "noop"
	CloseOnMergeClosed      CloseOnMergeAction = "closed"
	CloseOnMergeAwaiting    CloseOnMergeAction = "awaiting_confirmation"
	CloseOnMergeSkipped     CloseOnMergeAction = "skipped"
	CloseOnMergeAlreadyDone CloseOnMergeAction = "already_done"
)

type CloseOnMergeResult struct {
	Repo        string
	Issue       int
	PR          int
	Action      CloseOnMergeAction
	Reason      string
	WrongRefs   bool
	MergeCommit string
}

func (c *Client) CloseOnMergeForLedger(ctx context.Context, ledger *ClaimLedger, opts CloseOnMergeOptions) []CloseOnMergeResult {
	if c == nil || c.client == nil || ledger == nil {
		return nil
	}
	claims := ledger.Claims()
	out := make([]CloseOnMergeResult, 0, len(claims))
	for _, claim := range claims {
		if !claim.MergedPR || claim.PRNumber <= 0 || claim.Issue <= 0 || strings.TrimSpace(claim.Repo) == "" {
			continue
		}
		if claim.Reference {
			continue
		}
		pr, err := c.getPullRequestForCloseOnMerge(ctx, claim.PRRepo, claim.PRNumber)
		if err != nil {
			if opts.Logger != nil {
				opts.Logger.Warn("close-on-merge: could not read merged PR", "repo", claim.PRRepo, "pr", claim.PRNumber, "error", err)
			}
			continue
		}
		res := c.closeIssueForMergedPRClaim(ctx, claim.Repo, claim.Issue, pr, true, opts)
		out = append(out, res)
	}
	return out
}

func (c *Client) CloseOnMergeBackfill(ctx context.Context, opts CloseOnMergeOptions) []CloseOnMergeResult {
	if c == nil || c.client == nil {
		return nil
	}
	var out []CloseOnMergeResult
	for _, repo := range c.activeRepos() {
		owner, repoName := c.splitRepo(repo)
		issueOpts := &gh.IssueListByRepoOptions{
			State:       "open",
			ListOptions: gh.ListOptions{PerPage: 100},
		}
		for page := 0; page < claimSearchMaxPages; page++ {
			issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repoName, issueOpts)
			if err != nil {
				if opts.Logger != nil {
					opts.Logger.Warn("close-on-merge backfill: listing open issues failed", "repo", repo, "error", err)
				}
				break
			}
			for _, issue := range issues {
				if issue == nil || issue.IsPullRequest() || !strings.EqualFold(issue.GetState(), "open") {
					continue
				}
				if !closeOnMergeBackfillCandidate(issue) {
					continue
				}
				prs := c.closeOnMergeLinkedPRs(ctx, owner, repoName, issue)
				for _, prNum := range prs {
					pr, err := c.getPullRequestForCloseOnMerge(ctx, repo, prNum)
					if err != nil {
						if opts.Logger != nil {
							opts.Logger.Warn("close-on-merge backfill: reading linked PR failed", "repo", repo, "issue", issue.GetNumber(), "pr", prNum, "error", err)
						}
						continue
					}
					if pr.GetMergedAt().Time.IsZero() && !pr.GetMerged() {
						out = append(out, CloseOnMergeResult{Repo: repo, Issue: issue.GetNumber(), PR: prNum, Action: CloseOnMergeNoop, Reason: "pr_not_merged"})
						continue
					}
					claimed := closeOnMergePRClaimsIssue(pr, repo, repo, issue.GetNumber())
					if !claimed && closeOnMergeIssueCommentsLinkPR(ctx, c, owner, repoName, issue.GetNumber(), prNum) {
						claimed = true
					}
					if !claimed {
						out = append(out, CloseOnMergeResult{Repo: repo, Issue: issue.GetNumber(), PR: prNum, Action: CloseOnMergeSkipped, Reason: "no_closing_or_claim_metadata"})
						continue
					}
					res := c.closeIssueForMergedPRClaim(ctx, repo, issue.GetNumber(), pr, true, opts)
					out = append(out, res)
					if res.Action == CloseOnMergeClosed || res.Action == CloseOnMergeAwaiting || res.Action == CloseOnMergeAlreadyDone {
						break
					}
				}
			}
			if resp == nil || resp.NextPage == 0 {
				break
			}
			issueOpts.ListOptions.Page = resp.NextPage
		}
	}
	return out
}

func closeOnMergeBackfillCandidate(issue *gh.Issue) bool {
	labels := issueLabelNames(issue.Labels)
	return labelPresent(labels, CoveredByPRLabel) ||
		labelPresent(labels, LikelyDoneLabel) ||
		labelPresent(labels, "claimed")
}

func (c *Client) closeOnMergeLinkedPRs(ctx context.Context, owner, repo string, issue *gh.Issue) []int {
	seen := map[int]bool{}
	var out []int
	add := func(n int) {
		if n <= 0 || seen[n] {
			return
		}
		seen[n] = true
		out = append(out, n)
	}
	commentOpts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: closeOnMergeCommentPageSize}}
	for {
		comments, resp, err := c.client.Issues.ListComments(ctx, owner, repo, issue.GetNumber(), commentOpts)
		if err != nil {
			break
		}
		for _, comment := range comments {
			for _, n := range parseCoveredByPRComments(comment.GetBody()) {
				add(n)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		commentOpts.Page = resp.NextPage
	}
	timelineOpts := &gh.ListOptions{PerPage: closeOnMergeCommentPageSize}
	for {
		events, resp, err := c.client.Issues.ListIssueTimeline(ctx, owner, repo, issue.GetNumber(), timelineOpts)
		if err != nil {
			break
		}
		for _, event := range events {
			if event == nil || !strings.EqualFold(event.GetEvent(), "cross-referenced") {
				continue
			}
			if src := event.GetSource(); src != nil && src.Issue != nil && src.Issue.IsPullRequest() {
				add(src.Issue.GetNumber())
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		timelineOpts.Page = resp.NextPage
	}
	sort.Ints(out)
	return out
}

func parseCoveredByPRComments(body string) []int {
	matches := coveredByPRCommentPattern.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil
	}
	var out []int
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		n, err := strconvAtoi(m[1])
		if err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

func (c *Client) getPullRequestForCloseOnMerge(ctx context.Context, repo string, number int) (*gh.PullRequest, error) {
	owner, repoName := c.splitRepo(repo)
	if owner == "" || repoName == "" || number <= 0 {
		return nil, fmt.Errorf("invalid PR ref %s#%d", repo, number)
	}
	pr, _, err := c.client.PullRequests.Get(ctx, owner, repoName, number)
	return pr, err
}

func closeOnMergePRClaimsIssue(pr *gh.PullRequest, prRepo, issueRepo string, issue int) bool {
	if pr == nil || issue <= 0 {
		return false
	}
	text := pr.GetTitle() + "\n" + pr.GetBody()
	if _, ok := FindClosingKeywordReference(text, prRepo, issueRepo, issue); ok {
		return true
	}
	if n, ok := issueFromBranchName(headRef(pr)); ok && n == issue && strings.EqualFold(prRepo, issueRepo) {
		return true
	}
	return false
}

func closeOnMergePRRefsIssue(pr *gh.PullRequest, prRepo, issueRepo string, issue int) bool {
	if pr == nil || issue <= 0 {
		return false
	}
	for _, ref := range ParseReferencedIssues(pr.GetTitle()+"\n"+pr.GetBody(), prRepo) {
		if ref.Issue == issue && strings.EqualFold(ref.Repo, issueRepo) {
			return true
		}
	}
	return false
}

func (c *Client) closeIssueForMergedPRClaim(ctx context.Context, issueRepo string, issueNumber int, pr *gh.PullRequest, claimMetadata bool, opts CloseOnMergeOptions) CloseOnMergeResult {
	prRepo := issueRepo
	if pr != nil && pr.GetBase() != nil && pr.GetBase().GetRepo() != nil && pr.GetBase().GetRepo().GetFullName() != "" {
		prRepo = pr.GetBase().GetRepo().GetFullName()
	}
	res := CloseOnMergeResult{Repo: issueRepo, Issue: issueNumber}
	if pr != nil {
		res.PR = pr.GetNumber()
		res.MergeCommit = pr.GetMergeCommitSHA()
	}
	if pr == nil || issueNumber <= 0 {
		res.Action, res.Reason = CloseOnMergeNoop, "missing_pr_or_issue"
		return res
	}
	if pr.GetMergedAt().Time.IsZero() && !pr.GetMerged() {
		res.Action, res.Reason = CloseOnMergeNoop, "pr_not_merged"
		return res
	}
	author := safeGetLogin(pr.GetUser())
	if !opts.Identity.Matches(author) && (opts.TrustedAuthor == nil || !opts.TrustedAuthor(prRepo, author)) {
		res.Action, res.Reason = CloseOnMergeSkipped, "untrusted_author"
		return res
	}
	if !claimMetadata && !closeOnMergePRClaimsIssue(pr, prRepo, issueRepo, issueNumber) {
		res.Action, res.Reason = CloseOnMergeSkipped, "no_closing_or_claim_metadata"
		return res
	}
	if claimMetadata && !closeOnMergePRClaimsIssue(pr, prRepo, issueRepo, issueNumber) && closeOnMergePRRefsIssue(pr, prRepo, issueRepo, issueNumber) {
		res.WrongRefs = true
		if opts.Logger != nil {
			opts.Logger.Warn("close-on-merge: PR used non-closing Refs for its claimed issue",
				"repo", issueRepo, "issue", issueNumber, "pr", pr.GetNumber(), "author", author)
		}
	}
	owner, repoName := c.splitRepo(issueRepo)
	issue, _, err := c.client.Issues.Get(ctx, owner, repoName, issueNumber)
	if err != nil {
		res.Action, res.Reason = CloseOnMergeNoop, "issue_read_failed"
		if opts.Logger != nil {
			opts.Logger.Warn("close-on-merge: reading issue failed", "repo", issueRepo, "issue", issueNumber, "error", err)
		}
		return res
	}
	if !strings.EqualFold(issue.GetState(), "open") {
		res.Action, res.Reason = CloseOnMergeAlreadyDone, "issue_not_open"
		return res
	}
	if c.hasCloseOnMergeMarker(ctx, owner, repoName, issueNumber) {
		res.Action, res.Reason = CloseOnMergeAlreadyDone, "marker_present"
		return res
	}
	if closeOnMergeNeedsConfirmation(issue) {
		body := closeOnMergeAwaitingComment(pr)
		if _, _, err := c.client.Issues.CreateComment(ctx, owner, repoName, issueNumber, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
			res.Action, res.Reason = CloseOnMergeNoop, "awaiting_comment_failed"
			return res
		}
		if err := c.EnsureIssueLabel(ctx, issueRepo, LikelyDoneLabel, "0e8a16", "Hive verified that a merged PR references or claims this issue; pending confirmation"); err != nil && opts.Logger != nil {
			opts.Logger.Warn("close-on-merge: ensuring likely-done label failed", "repo", issueRepo, "issue", issueNumber, "error", err)
		} else if err := c.AddLabels(ctx, issueRepo, issueNumber, []string{LikelyDoneLabel}); err != nil && opts.Logger != nil {
			opts.Logger.Warn("close-on-merge: adding likely-done label failed", "repo", issueRepo, "issue", issueNumber, "error", err)
		}
		res.Action, res.Reason = CloseOnMergeAwaiting, "needs_confirmation_or_human"
		return res
	}
	if _, _, err := c.client.Issues.CreateComment(ctx, owner, repoName, issueNumber, &gh.IssueComment{Body: gh.Ptr(closeOnMergeCloseComment(pr))}); err != nil {
		res.Action, res.Reason = CloseOnMergeNoop, "close_comment_failed"
		return res
	}
	err = c.CloseIssue(ctx, issueRepo, issueNumber, IssueCloseOptions{
		OverrideReason:          fmt.Sprintf("fixed by merged PR #%d", pr.GetNumber()),
		SuppressOverrideComment: true,
		StateReason:             IssueStateReasonCompleted,
	})
	if err != nil {
		res.Action, res.Reason = CloseOnMergeNoop, "close_failed"
		return res
	}
	res.Action, res.Reason = CloseOnMergeClosed, "closed_completed"
	return res
}

func closeOnMergeNeedsConfirmation(issue *gh.Issue) bool {
	for _, l := range issue.Labels {
		name := strings.ToLower(strings.TrimSpace(l.GetName()))
		switch name {
		case "needs-reporter-confirmation", "epic", "needs-human":
			return true
		}
	}
	return false
}

func closeOnMergeCloseComment(pr *gh.PullRequest) string {
	return fmt.Sprintf("%s\nFixed by #%d (merged %s). Closing. Reply /reopen if the problem persists.",
		CloseOnMergeMarker, pr.GetNumber(), closeOnMergeShortSHA(pr.GetMergeCommitSHA()))
}

func closeOnMergeAwaitingComment(pr *gh.PullRequest) string {
	return fmt.Sprintf("%s\n%s\nFix merged in #%d; awaiting confirmation. Reply /fixed when verified, or /reopen if the problem persists.",
		CloseOnMergeMarker, closeOnMergeAwaitingMarker, pr.GetNumber())
}

func closeOnMergeShortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "unknown"
	}
	return sha
}

func (c *Client) hasCloseOnMergeMarker(ctx context.Context, owner, repo string, number int) bool {
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: closeOnMergeCommentPageSize}}
	for {
		comments, resp, err := c.client.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return false
		}
		for _, comment := range comments {
			if strings.Contains(comment.GetBody(), CloseOnMergeMarker) {
				return true
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return false
		}
		opts.Page = resp.NextPage
	}
}

func closeOnMergeIssueCommentsLinkPR(ctx context.Context, c *Client, owner, repo string, issue, pr int) bool {
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: closeOnMergeCommentPageSize}}
	for {
		comments, resp, err := c.client.Issues.ListComments(ctx, owner, repo, issue, opts)
		if err != nil {
			return false
		}
		for _, comment := range comments {
			for _, n := range parseCoveredByPRComments(comment.GetBody()) {
				if n == pr {
					return true
				}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return false
		}
		opts.Page = resp.NextPage
	}
}

func strconvAtoi(s string) (int, error) {
	var n int
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("non-digit")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}
