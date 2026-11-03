package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

const (
	CloseOnMergeMarker                  = "<!-- hive-close-on-merge -->"
	closeOnMergeAwaitingMarker          = "<!-- hive-close-on-merge: awaiting-confirmation -->"
	closeOnMergeCommentPageSize         = 100
	closeOnMergeOpenIssueBatchThreshold = 2
	// reopenedAfterMergeMarkerFormat marks an issue a person reopened after
	// the given PR merged. It is PR-specific and must not contain
	// CloseOnMergeMarker, which is matched issue-wide and would also block a
	// later close for a different PR.
	reopenedAfterMergeMarkerFormat = "<!-- hive-reopened-after-merge pr=%d -->"
)

var coveredByPRCommentPattern = regexp.MustCompile(`(?i)\bcovered\s+by\s+(?:open\s+)?(?:pr|pull request)\s+#(\d+)\b`)

type TrustedPRAuthorFunc func(repo, login string) bool

// DefaultCloseOnMergeRecheck is how long a settled ledger claim
// (IssueClaim.CloseOnMergeSettledAt) is skipped by the ledger sweep before
// it is re-verified against the API once more. It mirrors the config
// default so a zero CloseOnMergeOptions behaves like an unset config.
const DefaultCloseOnMergeRecheck = config.DefaultCloseOnMergeRecheck

type CloseOnMergeOptions struct {
	Identity      HiveIdentity
	TrustedAuthor TrustedPRAuthorFunc
	Logger        *slog.Logger
	// RecheckAfter bounds how long a settled claim is skipped without an API
	// call; zero means DefaultCloseOnMergeRecheck.
	RecheckAfter time.Duration
	// MergedAfter, when set, restricts the ledger sweep to claims whose PR
	// merged at or after that instant: the between-sweeps path, which must
	// still close an issue promptly for a PR that just merged without paying
	// for the whole ledger.
	MergedAfter time.Time
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// OnlyPRRepo/OnlyPRNumber, when OnlyPRNumber is set, restrict the ledger
	// sweep to the claims one pull request makes: the webhook path, which
	// closes a just-merged PR's issues on the delivery instead of waiting for
	// the reconcile pass (whose sweep a cached cycle skips). OnlyPRRepo is
	// matched by bare repository name; empty matches any.
	OnlyPRRepo   string
	OnlyPRNumber int
	// LabelDriven reports a repo that opted in to label-driven triage. nil
	// means no repo opts in.
	LabelDriven func(repo string) bool
	// AdvisoryAutocloseOff disables the advisory-checklist sweep
	// (dedupe.advisory_autoclose: false): checklists are then neither
	// ticked nor closed by the backfill (hivecommons/hive#15754).
	AdvisoryAutocloseOff bool
	// RepoOnGitHub reports whether a repo lives on GitHub (its forge.Kind, via
	// config.RepoOnGitHub). Both sweeps read and close through the GitHub API,
	// so a claim or repo on GitLab or Gitea is skipped rather than looked up on
	// GitHub under its slug (hivecommons/hive#14150). nil means every repo is
	// on GitHub.
	RepoOnGitHub func(repo string) bool
}

func (o CloseOnMergeOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o CloseOnMergeOptions) onGitHub(repo string) bool {
	return o.RepoOnGitHub == nil || o.RepoOnGitHub(repo)
}

// wantsPR reports whether the OnlyPR filter admits a claim by (repo, number).
func (o CloseOnMergeOptions) wantsPR(repo string, number int) bool {
	if o.OnlyPRNumber <= 0 {
		return true
	}
	if number != o.OnlyPRNumber {
		return false
	}
	return o.OnlyPRRepo == "" || strings.EqualFold(bareRepo(o.OnlyPRRepo), bareRepo(repo))
}

// bareRepo strips an owner/ prefix.
func bareRepo(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

func (o CloseOnMergeOptions) recheckAfter() time.Duration {
	if o.RecheckAfter <= 0 {
		return DefaultCloseOnMergeRecheck
	}
	return o.RecheckAfter
}

// CloseOnMergeSweep is one ledger pass: the per-claim results plus the
// claims the pass did not spend an API call on.
type CloseOnMergeSweep struct {
	Results []CloseOnMergeResult
	// SkippedSettled counts claims skipped on their CloseOnMergeSettledAt
	// marker.
	SkippedSettled int
	// SkippedNotFresh counts claims older than Options.MergedAfter.
	SkippedNotFresh int
	// SkippedReopened counts merged claims whose issue a person reopened
	// after the merge (IssueClaim.ReopenedAfterMerge); they are never
	// re-closed for that PR and cost no API call.
	SkippedReopened int
	// SkippedOtherForge counts claims whose issue or PR repo lives on GitLab
	// or Gitea (Options.RepoOnGitHub); this GitHub sweep cannot observe them.
	SkippedOtherForge int
	// Marked counts claims whose settled marker this pass set or refreshed.
	Marked int
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
	PRRepo      string // set when the PR lives in a different repo than the issue
	Action      CloseOnMergeAction
	Reason      string
	WrongRefs   bool
	MergeCommit string
	// Ticked is how many advisory-checklist items this sweep marked
	// resolved in the issue body (hivecommons/hive#15754).
	Ticked int
}

// closeOnMergeTransientReasons are the result reasons that describe a failed
// or deferred API step rather than a verdict: the next sweep must retry them,
// so they never settle a ledger claim.
var closeOnMergeTransientReasons = map[string]bool{
	"issue_read_failed":       true,
	"timeline_read_failed":    true,
	"awaiting_comment_failed": true,
	"close_comment_failed":    true,
	"close_failed":            true,
	"body_edit_failed":        true,
}

// Settles reports whether the result is a final verdict for its PR/issue
// pair — one that re-running the sweep could not change without new
// evidence — so the ledger can skip the claim on later passes.
func (r CloseOnMergeResult) Settles() bool {
	if r.Action == "" || closeOnMergeTransientReasons[r.Reason] {
		return false
	}
	switch r.Action {
	case CloseOnMergeClosed, CloseOnMergeAlreadyDone, CloseOnMergeAwaiting, CloseOnMergeSkipped, CloseOnMergeNoop:
		return true
	}
	return false
}

// closeOnMergeClaimSettled reports whether claim's settled marker is still
// within the recheck window at now.
func closeOnMergeClaimSettled(claim IssueClaim, now time.Time, recheck time.Duration) bool {
	if claim.CloseOnMergeSettledAt.IsZero() {
		return false
	}
	return now.Sub(claim.CloseOnMergeSettledAt) < recheck
}

// closeOnMergeClaimMergedAt is the instant the sweep treats as the claim's
// merge time for MergedAfter filtering: MergedAt, else the first observation.
func closeOnMergeClaimMergedAt(claim IssueClaim) time.Time {
	if !claim.MergedAt.IsZero() {
		return claim.MergedAt
	}
	if !claim.FirstObservedAt.IsZero() {
		return claim.FirstObservedAt
	}
	return claim.ObservedAt
}

// CloseOnMergeForLedger runs CloseOnMergeLedgerSweep and returns only its
// per-claim results.
func (c *Client) CloseOnMergeForLedger(ctx context.Context, ledger *ClaimLedger, opts CloseOnMergeOptions) []CloseOnMergeResult {
	return c.CloseOnMergeLedgerSweep(ctx, ledger, opts).Results
}

// CloseOnMergeLedgerSweep applies close-on-merge to the ledger's merged
// strong claims. A claim whose previous verdict settled within
// opts.RecheckAfter is skipped without an API call; with opts.MergedAfter set
// only claims merged since then are considered. Every settling verdict is
// stamped on the ledger and the ledger is saved once if anything changed.
func (c *Client) CloseOnMergeLedgerSweep(ctx context.Context, ledger *ClaimLedger, opts CloseOnMergeOptions) CloseOnMergeSweep {
	var sweep CloseOnMergeSweep
	if c == nil || c.client == nil || ledger == nil {
		return sweep
	}
	now := opts.now()
	recheck := opts.recheckAfter()
	claims := ledger.Claims()
	sweep.Results = make([]CloseOnMergeResult, 0, len(claims))
	candidate := func(claim IssueClaim) bool {
		return claim.MergedPR && !claim.Reference && claim.PRNumber > 0 && claim.Issue > 0 && strings.TrimSpace(claim.Repo) != ""
	}
	// #11538: list each claimed repo's open issues once and only spend
	// per-claim reads on claims whose issue is still open. Merged claims stay
	// in the ledger for settledClaimRetention, so reading every one of them on
	// every pass exhausts the App quota after a restart empties the caches.
	openIssues := map[string]map[int]*gh.Issue{}
	openIssueListFailed := map[string]bool{}
	eligibleByRepo := map[string]int{}
	for _, claim := range claims {
		if !candidate(claim) || !opts.wantsPR(claim.PRRepo, claim.PRNumber) || claim.ReopenedAfterMerge() || !claim.MergedAt.IsZero() || closeOnMergeClaimSettled(claim, now, recheck) || (!opts.MergedAfter.IsZero() && closeOnMergeClaimMergedAt(claim).Before(opts.MergedAfter)) {
			continue
		}
		eligibleByRepo[c.closeOnMergeRepoKey(claim.Repo)]++
	}
	for _, claim := range claims {
		if !candidate(claim) || !opts.wantsPR(claim.PRRepo, claim.PRNumber) {
			continue
		}
		if !opts.onGitHub(claim.Repo) || (claim.PRRepo != "" && !opts.onGitHub(claim.PRRepo)) {
			sweep.SkippedOtherForge++
			continue
		}
		// A person reopened the issue after this PR merged: never close it
		// for this PR again, and spend no API call finding that out.
		if claim.ReopenedAfterMerge() {
			sweep.SkippedReopened++
			sweep.Results = append(sweep.Results, CloseOnMergeResult{Repo: claim.Repo, Issue: claim.Issue, PR: claim.PRNumber, Action: CloseOnMergeSkipped, Reason: "reopened_after_merge"})
			continue
		}
		if closeOnMergeClaimSettled(claim, now, recheck) {
			sweep.SkippedSettled++
			continue
		}
		if !opts.MergedAfter.IsZero() && closeOnMergeClaimMergedAt(claim).Before(opts.MergedAfter) {
			sweep.SkippedNotFresh++
			continue
		}
		var issue *gh.Issue
		key := c.closeOnMergeRepoKey(claim.Repo)
		batchOpenIssues := eligibleByRepo[key] >= closeOnMergeOpenIssueBatchThreshold
		if batchOpenIssues {
			if _, seen := openIssues[key]; !seen && !openIssueListFailed[key] {
				open, err := c.closeOnMergeOpenIssues(ctx, claim.Repo)
				if err != nil {
					if opts.Logger != nil {
						opts.Logger.Warn("close-on-merge: listing open issues failed", "repo", claim.Repo, "error", err)
					}
					openIssueListFailed[key] = true
				} else {
					openIssues[key] = open
				}
			}
		}
		if batchOpenIssues && openIssueListFailed[key] {
			continue
		}
		if open, ok := openIssues[key]; ok {
			if issue = open[claim.Issue]; issue == nil {
				if ledger.MarkCloseOnMergeSettled(claim.Repo, claim.Issue, claim.PRNumber, now) {
					sweep.Marked++
				}
				continue
			}
		}
		pr, err := c.getPullRequestForCloseOnMerge(ctx, claim.PRRepo, claim.PRNumber)
		if err != nil {
			if opts.Logger != nil {
				opts.Logger.Warn("close-on-merge: could not read merged PR", "repo", claim.PRRepo, "pr", claim.PRNumber, "error", err)
			}
			continue
		}
		res := c.closeIssueForMergedPRClaimWithIssue(ctx, claim.Repo, claim.Issue, pr, true, opts, issue)
		sweep.Results = append(sweep.Results, res)
		if res.Settles() && ledger.MarkCloseOnMergeSettled(claim.Repo, claim.Issue, claim.PRNumber, now) {
			sweep.Marked++
		}
	}
	if sweep.Marked > 0 {
		if err := ledger.Save(); err != nil && opts.Logger != nil {
			opts.Logger.Warn("close-on-merge: failed to persist settled markers", "error", err)
		}
	}
	return sweep
}

func (c *Client) closeOnMergeRepoKey(repo string) string {
	owner, repoName := c.splitRepo(strings.TrimSpace(repo))
	return strings.ToLower(owner + "/" + repoName)
}

// closeOnMergeOpenIssues lists repo's open issues (pull requests excluded),
// keyed by number. Any list error is returned so the caller skips the repo
// for this pass instead of falling back to per-claim reads.
func (c *Client) closeOnMergeOpenIssues(ctx context.Context, repo string) (map[int]*gh.Issue, error) {
	owner, repoName := c.splitRepo(strings.TrimSpace(repo))
	out := map[int]*gh.Issue{}
	issueOpts := &gh.IssueListByRepoOptions{
		State:       "open",
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	for page := 0; page < claimSearchMaxPages; page++ {
		issues, resp, err := c.client.Issues.ListByRepo(WithRESTCaller(ctx, "hive:close_on_merge"), owner, repoName, issueOpts)
		if err != nil {
			return nil, err
		}
		for _, issue := range issues {
			if issue == nil || issue.IsPullRequest() || issue.GetNumber() <= 0 || !strings.EqualFold(issue.GetState(), "open") {
				continue
			}
			out[issue.GetNumber()] = issue
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		issueOpts.ListOptions.Page = resp.NextPage
	}
	return out, nil
}

func (c *Client) CloseOnMergeBackfill(ctx context.Context, opts CloseOnMergeOptions) []CloseOnMergeResult {
	if c == nil || c.client == nil {
		return nil
	}
	var out []CloseOnMergeResult
	watched := map[string]bool{}
	for _, repo := range c.activeRepos() {
		o, n := c.splitRepo(repo)
		watched[strings.ToLower(o+"/"+n)] = true
	}
	for _, repo := range c.activeRepos() {
		if !opts.onGitHub(repo) {
			continue
		}
		owner, repoName := c.splitRepo(repo)
		issueOpts := &gh.IssueListByRepoOptions{
			State:       "open",
			ListOptions: gh.ListOptions{PerPage: 100},
		}
		for page := 0; page < claimSearchMaxPages; page++ {
			issues, resp, err := c.client.Issues.ListByRepo(WithRESTCaller(ctx, "hive:close_on_merge"), owner, repoName, issueOpts)
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
				if res, ok := c.sweepDedupeAdvisoryChecklist(ctx, repo, issue, opts); ok {
					out = append(out, res)
					continue
				}
				if !closeOnMergeBackfillCandidate(issue) {
					continue
				}
				issueRepo := owner + "/" + repoName
				prs := c.closeOnMergeLinkedPRs(ctx, owner, repoName, issue)
				for _, ref := range prs {
					prNum := ref.Number
					prRepo := ""
					sameRepo := strings.EqualFold(ref.Repo, issueRepo)
					if !sameRepo {
						prRepo = ref.Repo
						if !watched[strings.ToLower(ref.Repo)] {
							// Not a repo this hive watches: reading it can only fail,
							// and the answer never changes between sweeps.
							out = append(out, CloseOnMergeResult{Repo: repo, Issue: issue.GetNumber(), PR: prNum, PRRepo: prRepo, Action: CloseOnMergeSkipped, Reason: "pr_repo_not_watched"})
							if opts.Logger != nil {
								opts.Logger.Info("close-on-merge backfill: linked PR is in an unwatched repo; skipping", "repo", repo, "issue", issue.GetNumber(), "pr_repo", ref.Repo, "pr", prNum)
							}
							continue
						}
					}
					pr, err := c.getPullRequestForCloseOnMerge(ctx, ref.Repo, prNum)
					if err != nil {
						if opts.Logger != nil {
							opts.Logger.Warn("close-on-merge backfill: reading linked PR failed", "repo", repo, "issue", issue.GetNumber(), "pr_repo", ref.Repo, "pr", prNum, "error", err)
						}
						continue
					}
					if pr.GetMergedAt().Time.IsZero() && !pr.GetMerged() {
						out = append(out, CloseOnMergeResult{Repo: repo, Issue: issue.GetNumber(), PR: prNum, PRRepo: prRepo, Action: CloseOnMergeNoop, Reason: "pr_not_merged"})
						continue
					}
					claimed := closeOnMergePRClaimsIssue(pr, ref.Repo, issueRepo, issue.GetNumber())
					if !claimed && sameRepo && closeOnMergeIssueCommentsLinkPR(ctx, c, owner, repoName, issue.GetNumber(), prNum) {
						claimed = true
					}
					if !claimed {
						out = append(out, CloseOnMergeResult{Repo: repo, Issue: issue.GetNumber(), PR: prNum, PRRepo: prRepo, Action: CloseOnMergeSkipped, Reason: "no_closing_or_claim_metadata"})
						continue
					}
					res := c.closeIssueForMergedPRClaimWithIssue(ctx, repo, issue.GetNumber(), pr, true, opts, issue)
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
	if _, tracking := isTrackingIssue(issue); tracking {
		return false
	}
	labels := issueLabelNames(issue.Labels)
	return HasCoveredByPRLabel(labels) ||
		labelPresent(labels, LikelyDoneLabel) ||
		labelPresent(labels, claims.LabelClaimed) ||
		labelPresent(labels, claims.LegacyLabelClaimed)
}

// closeOnMergePRRef names a linked PR together with the repo it lives in,
// which differs from the issue's repo for cross-repo references.
type closeOnMergePRRef struct {
	Repo   string
	Number int
}

func (c *Client) closeOnMergeLinkedPRs(ctx context.Context, owner, repo string, issue *gh.Issue) []closeOnMergePRRef {
	issueRepo := owner + "/" + repo
	seen := map[closeOnMergePRRef]bool{}
	var out []closeOnMergePRRef
	add := func(prRepo string, n int) {
		if prRepo == "" {
			prRepo = issueRepo
		}
		ref := closeOnMergePRRef{Repo: prRepo, Number: n}
		key := closeOnMergePRRef{Repo: strings.ToLower(prRepo), Number: n}
		if n <= 0 || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, ref)
	}
	commentOpts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: closeOnMergeCommentPageSize}}
	for {
		comments, resp, err := c.client.Issues.ListComments(WithRESTCaller(ctx, "hive:close_on_merge"), owner, repo, issue.GetNumber(), commentOpts)
		if err != nil {
			break
		}
		for _, comment := range comments {
			for _, n := range parseCoveredByPRComments(comment.GetBody()) {
				add(issueRepo, n)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		commentOpts.Page = resp.NextPage
	}
	timelineOpts := &gh.ListOptions{PerPage: closeOnMergeCommentPageSize}
	for {
		events, resp, err := c.client.Issues.ListIssueTimeline(WithRESTCaller(ctx, "hive:close_on_merge"), owner, repo, issue.GetNumber(), timelineOpts)
		if err != nil {
			break
		}
		for _, event := range events {
			if event == nil || !strings.EqualFold(event.GetEvent(), "cross-referenced") {
				continue
			}
			if src := event.GetSource(); src != nil && src.Issue != nil && src.Issue.IsPullRequest() {
				add(src.Issue.GetRepository().GetFullName(), src.Issue.GetNumber())
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		timelineOpts.Page = resp.NextPage
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Number < out[j].Number
	})
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
	fullRepo := owner + "/" + repoName
	if pr, ok := c.cachedPRDetailAnyAllowUnknown(fullRepo, number); ok && closeOnMergeCachedPRUsable(pr) {
		return pr, nil
	}
	pr, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:close_on_merge"), owner, repoName, number)
	if err == nil {
		c.storePRDetail(fullRepo, number, pr)
	}
	return pr, err
}

func closeOnMergeCachedPRUsable(pr *gh.PullRequest) bool {
	return pr != nil && strings.TrimSpace(pr.GetBody()) != "" && pr.GetUser().GetLogin() != "" && pr.GetNumber() > 0
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

// closeOnMergeIssueNotFound reports whether reading the referenced issue
// failed because GitHub does not know it (404), as opposed to a transient
// API error worth retrying on the next sweep.
func closeOnMergeIssueNotFound(err error) bool {
	var ghErr *gh.ErrorResponse
	return errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound
}

func (c *Client) closeIssueForMergedPRClaim(ctx context.Context, issueRepo string, issueNumber int, pr *gh.PullRequest, claimMetadata bool, opts CloseOnMergeOptions) CloseOnMergeResult {
	return c.closeIssueForMergedPRClaimWithIssue(ctx, issueRepo, issueNumber, pr, claimMetadata, opts, nil)
}

func (c *Client) closeIssueForMergedPRClaimWithIssue(ctx context.Context, issueRepo string, issueNumber int, pr *gh.PullRequest, claimMetadata bool, opts CloseOnMergeOptions, issue *gh.Issue) CloseOnMergeResult {
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
		// Merging is the hive's acceptance of an external contributor's PR.
		// List/cache entries can omit merged_by; resolve the terminal actor
		// before rejecting it, especially on non-default release branches
		// where GitHub does not apply closing keywords itself (#15148).
		owner, repoName := c.splitRepo(prRepo)
		if !opts.Identity.Matches(c.prTerminalMergedBy(ctx, owner, repoName, pr)) {
			res.Action, res.Reason = CloseOnMergeSkipped, "untrusted_author"
			return res
		}
	}
	if !claimMetadata && !closeOnMergePRClaimsIssue(pr, prRepo, issueRepo, issueNumber) {
		res.Action, res.Reason = CloseOnMergeSkipped, "no_closing_or_claim_metadata"
		return res
	}
	if claimMetadata && !closeOnMergePRClaimsIssue(pr, prRepo, issueRepo, issueNumber) && closeOnMergePRRefsIssue(pr, prRepo, issueRepo, issueNumber) {
		res.WrongRefs = true
		if opts.Logger != nil {
			// This is PR metadata, not a failed runtime operation. WARN would
			// turn repeated sweeps into an unclassified fleet self-report.
			opts.Logger.Info("close-on-merge: PR used non-closing Refs for its claimed issue",
				"repo", issueRepo, "issue", issueNumber, "pr", pr.GetNumber(), "author", author)
		}
	}
	owner, repoName := c.splitRepo(issueRepo)
	if issue == nil {
		var err error
		issue, _, err = c.client.Issues.Get(WithRESTCaller(ctx, "hive:close_on_merge"), owner, repoName, issueNumber)
		if err != nil {
			if closeOnMergeIssueNotFound(err) {
				// The referenced issue does not exist (or is not visible to the
				// App): a verdict, not a failed step. Re-reading it every sweep
				// cannot change the answer, and WARN would turn the repeats
				// into an unclassified fleet self-report.
				res.Action, res.Reason = CloseOnMergeSkipped, "issue_not_found"
				if opts.Logger != nil {
					opts.Logger.Info("close-on-merge: referenced issue not found; skipping", "repo", issueRepo, "issue", issueNumber, "pr", pr.GetNumber())
				}
				return res
			}
			res.Action, res.Reason = CloseOnMergeNoop, "issue_read_failed"
			if opts.Logger != nil {
				opts.Logger.Warn("close-on-merge: reading issue failed", "repo", issueRepo, "issue", issueNumber, "error", err)
			}
			return res
		}
	}
	if issue.IsPullRequest() {
		// The Issues API also returns pull requests; a cross-repo mention can
		// claim one. A verdict, not a failed step, so INFO rather than WARN.
		res.Action, res.Reason = CloseOnMergeSkipped, "target_is_pull_request"
		if opts.Logger != nil {
			opts.Logger.Info("close-on-merge: claimed target is a pull request, not an issue; skipping", "repo", issueRepo, "number", issueNumber, "pr", pr.GetNumber())
		}
		return res
	}
	if !strings.EqualFold(issue.GetState(), "open") {
		res.Action, res.Reason = CloseOnMergeAlreadyDone, "issue_not_open"
		return res
	}
	if why, ok := isTrackingIssue(issue); ok {
		// A tracking/archive issue (hivecommons/hive#14613) is never
		// "fixed" by a merge: the keyword that claimed it is ignored. A
		// verdict, not a failed step, so INFO; Skipped settles the claim so
		// the sweep stops re-reading it.
		res.Action, res.Reason = CloseOnMergeSkipped, "tracking_issue"
		if opts.Logger != nil {
			opts.Logger.Info("close-on-merge: claimed issue is a tracking/archive issue; leaving it open", "repo", issueRepo, "issue", issueNumber, "pr", pr.GetNumber(), "why", why)
		}
		return res
	}
	closedMarker, reopenedMarker := c.closeOnMergeMarkers(ctx, owner, repoName, issueNumber, pr.GetNumber())
	if closedMarker {
		res.Action, res.Reason = CloseOnMergeAlreadyDone, "marker_present"
		return res
	}
	if reopenedMarker {
		res.Action, res.Reason = CloseOnMergeSkipped, "reopened_after_merge"
		return res
	}
	events, err := c.IssueTimelineSince(WithRESTCaller(ctx, "hive:close_on_merge"), issueRepo, issueNumber, pr.GetMergedAt().Time)
	if err != nil {
		res.Action, res.Reason = CloseOnMergeSkipped, "timeline_read_failed"
		if opts.Logger != nil {
			opts.Logger.Warn("close-on-merge: reading issue timeline failed; will retry", "repo", issueRepo, "issue", issueNumber, "pr", pr.GetNumber(), "error", err)
		}
		return res
	}
	if closeOnMergeReopenedAfter(events, opts.Identity) {
		res.Action, res.Reason = CloseOnMergeSkipped, "reopened_after_merge"
		if _, _, err := c.client.Issues.CreateComment(WithRESTCaller(ctx, "hive:close_on_merge"), owner, repoName, issueNumber, &gh.IssueComment{Body: gh.Ptr(closeOnMergeReopenedComment(pr))}); err != nil && opts.Logger != nil {
			opts.Logger.Warn("close-on-merge: posting reopened-after-merge marker failed", "repo", issueRepo, "issue", issueNumber, "pr", pr.GetNumber(), "error", err)
		}
		return res
	}
	if c.reporterConfirmationCloseGateReason(issue) != "" || closeOnMergeNeedsConfirmation(issue) {
		if cov := c.closeOnMergeCoverage(ctx, prRepo, issueRepo, issueNumber, pr, issue, opts); cov.partial() {
			c.markPartiallyAddressed(ctx, issueRepo, issueNumber, pr.GetNumber(), issue, cov, opts)
			res.Action, res.Reason = CloseOnMergeSkipped, "partially_addressed"
			return res
		}
		body := closeOnMergeAwaitingComment(pr)
		if _, _, err := c.client.Issues.CreateComment(WithRESTCaller(ctx, "hive:close_on_merge"), owner, repoName, issueNumber, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
			res.Action, res.Reason = CloseOnMergeNoop, "awaiting_comment_failed"
			return res
		}
		if err := c.EnsureIssueLabel(ctx, issueRepo, LikelyDoneLabel, LikelyDoneLabelColor, LikelyDoneLabelDescription); err != nil && opts.Logger != nil {
			opts.Logger.Warn("close-on-merge: ensuring likely-done label failed", "repo", issueRepo, "issue", issueNumber, "error", err)
		} else if err := c.AddLabels(ctx, issueRepo, issueNumber, []string{LikelyDoneLabel}); err != nil && opts.Logger != nil {
			opts.Logger.Warn("close-on-merge: adding likely-done label failed", "repo", issueRepo, "issue", issueNumber, "error", err)
		}
		if labelPresent(issueLabelNames(issue.Labels), PartiallyAddressedLabel) {
			if err := c.RemoveLabel(ctx, issueRepo, issueNumber, PartiallyAddressedLabel); err != nil && opts.Logger != nil {
				opts.Logger.Warn("close-on-merge: removing partially-addressed label failed", "repo", issueRepo, "issue", issueNumber, "error", err)
			}
		}
		res.Action, res.Reason = CloseOnMergeAwaiting, "needs_confirmation_or_human"
		return res
	}
	if cov := c.closeOnMergeCoverage(ctx, prRepo, issueRepo, issueNumber, pr, issue, opts); cov.partialForDirectClose(closeOnMergePRRefsIssue(pr, prRepo, issueRepo, issueNumber)) {
		c.markPartiallyAddressed(ctx, issueRepo, issueNumber, pr.GetNumber(), issue, cov, opts)
		res.Action, res.Reason = CloseOnMergeSkipped, "partially_addressed"
		return res
	}
	if _, _, err := c.client.Issues.CreateComment(WithRESTCaller(ctx, "hive:close_on_merge"), owner, repoName, issueNumber, &gh.IssueComment{Body: gh.Ptr(closeOnMergeCloseComment(pr))}); err != nil {
		res.Action, res.Reason = CloseOnMergeNoop, "close_comment_failed"
		return res
	}
	err = c.CloseIssue(WithRESTCaller(ctx, "hive:close_on_merge"), issueRepo, issueNumber, IssueCloseOptions{
		OverrideReason:          fmt.Sprintf("fixed by merged PR #%d", pr.GetNumber()),
		SuppressOverrideComment: true,
		StateReason:             IssueStateReasonCompleted,
	})
	if err != nil {
		res.Action, res.Reason = CloseOnMergeNoop, "close_failed"
		return res
	}
	res.Action, res.Reason = CloseOnMergeClosed, "closed_completed"
	c.stripStageLabelsOnClose(ctx, issueRepo, issueNumber, issueLabelNames(issue.Labels), opts.Logger)
	return res
}

// stripStageLabelsOnClose removes the hive-owned stage/author labels from an
// issue the hive just closed (hivecommons/hive#12436): a closed item is out
// of the pipeline and the enumeration sweep will never see it again to
// reconcile. Labels only; best-effort.
func (c *Client) stripStageLabelsOnClose(ctx context.Context, repo string, number int, labels []string, logger *slog.Logger) {
	for _, l := range labels {
		if !stagelabels.Owned(l) {
			continue
		}
		if err := c.RemoveLabel(WithRESTCaller(ctx, "hive:close_on_merge"), repo, number, l); err != nil && logger != nil {
			logger.Warn("close-on-merge: removing stage label on close failed", "repo", repo, "issue", number, "label", l, "error", err)
		}
	}
}

// closeOnMergeCoverage is how much of an issue a merged PR claims to cover
// (#11931). hive/likely-done needs a closing keyword for the issue, no
// remainder/out-of-scope paragraph in the PR body, and — when the issue cites
// file paths — at least one cited path among the PR's changed files.
type closeOnMergeCoverage struct {
	Closing   bool
	Remainder bool
	Cited     []string
	// FilesKnown is false when the changed-file list was not fetched or the
	// fetch failed; the path rule is then not applied.
	FilesKnown bool
	Untouched  []string
}

func (cov closeOnMergeCoverage) partial() bool {
	if !cov.Closing || cov.Remainder {
		return true
	}
	return cov.FilesKnown && len(cov.Cited) > 0 && len(cov.Untouched) == len(cov.Cited)
}

// partialForDirectClose is the guard for the direct close path, where the
// claim may come from the branch name, claim metadata or a comment link rather
// than a closing keyword. A missing keyword alone is therefore not partial
// coverage; a textual reference without a closing keyword (`Refs #N`) is.
func (cov closeOnMergeCoverage) partialForDirectClose(refsIssue bool) bool {
	if cov.Remainder {
		return true
	}
	if !cov.Closing {
		return refsIssue
	}
	return cov.FilesKnown && len(cov.Cited) > 0 && len(cov.Untouched) == len(cov.Cited)
}

func (c *Client) closeOnMergeCoverage(ctx context.Context, prRepo, issueRepo string, issueNumber int, pr *gh.PullRequest, issue *gh.Issue, opts CloseOnMergeOptions) closeOnMergeCoverage {
	_, closing := FindClosingKeywordReference(pr.GetTitle()+"\n"+pr.GetBody(), prRepo, issueRepo, issueNumber)
	cov := closeOnMergeCoverage{
		Closing:   closing,
		Remainder: prBodyHasRemainderSignal(pr.GetBody()),
		Cited:     issueCitedPaths(issue.GetBody()),
	}
	if !cov.Closing || cov.Remainder || len(cov.Cited) == 0 {
		return cov
	}
	owner, repoName := c.splitRepo(prRepo)
	files, err := ListPRChangedFiles(WithRESTCaller(ctx, "hive:close_on_merge"), c.client, owner, repoName, pr)
	if err != nil {
		if opts.Logger != nil {
			opts.Logger.Warn("close-on-merge: listing PR changed files failed; skipping cited-path check", "repo", prRepo, "pr", pr.GetNumber(), "issue", issueNumber, "error", err)
		}
		return cov
	}
	changed := make([]string, 0, len(files))
	for _, f := range files {
		changed = append(changed, f.Filename)
	}
	cov.FilesKnown = true
	cov.Untouched = untouchedCitedPaths(cov.Cited, changed)
	return cov
}

// markPartiallyAddressed labels the issue hive/partially-addressed and takes
// hive/likely-done off. It posts no awaiting-confirmation comment: the issue
// stays in the actionable pool for the remainder.
func (c *Client) markPartiallyAddressed(ctx context.Context, issueRepo string, issueNumber, prNumber int, issue *gh.Issue, cov closeOnMergeCoverage, opts CloseOnMergeOptions) {
	if opts.Logger != nil {
		opts.Logger.Info("close-on-merge: merged PR covers only part of the issue; labelling partially-addressed",
			"repo", issueRepo, "issue", issueNumber, "pr", prNumber,
			"closing_keyword", cov.Closing, "remainder", cov.Remainder, "untouched", cov.Untouched)
	}
	if err := c.EnsureIssueLabel(ctx, issueRepo, PartiallyAddressedLabel, PartiallyAddressedLabelColor, PartiallyAddressedLabelDescription); err != nil && opts.Logger != nil {
		opts.Logger.Warn("close-on-merge: ensuring partially-addressed label failed", "repo", issueRepo, "issue", issueNumber, "error", err)
	} else if err := c.AddLabels(ctx, issueRepo, issueNumber, []string{PartiallyAddressedLabel}); err != nil && opts.Logger != nil {
		opts.Logger.Warn("close-on-merge: adding partially-addressed label failed", "repo", issueRepo, "issue", issueNumber, "error", err)
	}
	if labelPresent(issueLabelNames(issue.Labels), LikelyDoneLabel) {
		if err := c.RemoveLabel(ctx, issueRepo, issueNumber, LikelyDoneLabel); err != nil && opts.Logger != nil {
			opts.Logger.Warn("close-on-merge: removing likely-done label failed", "repo", issueRepo, "issue", issueNumber, "error", err)
		}
	}
}

// prRemainderSignalPattern matches PR-body prose that says the PR left part
// of the work undone.
var prRemainderSignalPattern = regexp.MustCompile(`(?i)\b(?:remainder|out[- ]of[- ]scope|this agent tier cannot|left undone|what remains|not (?:yet )?addressed)\b`)

func prBodyHasRemainderSignal(body string) bool {
	if extractRemainderSection(body) != "" {
		return true
	}
	return prRemainderSignalPattern.MatchString(stripClosingFalsePositiveText(body))
}

var citedPathTokenPattern = regexp.MustCompile("`([^`\n]+)`")

var citedPathExtensions = map[string]bool{
	"go": true, "mod": true, "sum": true, "md": true, "mdx": true, "yml": true, "yaml": true,
	"json": true, "js": true, "mjs": true, "cjs": true, "ts": true, "tsx": true, "jsx": true,
	"py": true, "sh": true, "bash": true, "toml": true, "txt": true, "rs": true, "java": true,
	"kt": true, "rb": true, "php": true, "c": true, "h": true, "cc": true, "cpp": true, "hpp": true,
	"cs": true, "swift": true, "css": true, "scss": true, "html": true, "sql": true, "proto": true,
	"tf": true, "xml": true, "ini": true, "cfg": true, "conf": true, "lock": true, "vue": true,
	"svelte": true, "tpl": true, "tmpl": true, "gradle": true, "ps1": true, "dockerfile": true,
}

var citedPathBasenames = map[string]bool{
	"Makefile": true, "Dockerfile": true, "Containerfile": true, "CODEOWNERS": true, "Gemfile": true,
}

// issueCitedPaths returns the backtick-quoted tokens in an issue body that
// look like repository file paths (a known file extension or basename, or a
// directory with a trailing slash), normalised and de-duplicated in order.
// Line suffixes such as `:48` or `#L48` are dropped.
func issueCitedPaths(body string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range citedPathTokenPattern.FindAllStringSubmatch(body, -1) {
		p, ok := normalizeCitedPath(m[1])
		if !ok || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func normalizeCitedPath(tok string) (string, bool) {
	tok = strings.TrimSpace(tok)
	if tok == "" || strings.Contains(tok, "://") || strings.ContainsAny(tok, " \t()<>{}[]=$*?,;\"'|!") {
		return "", false
	}
	if i := strings.IndexAny(tok, ":#"); i >= 0 {
		tok = tok[:i]
	}
	tok = strings.TrimPrefix(tok, "./")
	tok = strings.TrimPrefix(tok, "/")
	if tok == "" || strings.HasPrefix(tok, "-") || strings.HasPrefix(tok, "@") || strings.Contains(tok, "..") {
		return "", false
	}
	if strings.HasSuffix(tok, "/") {
		return tok, len(tok) > 1
	}
	base := path.Base(tok)
	if citedPathBasenames[base] {
		return tok, true
	}
	ext := strings.TrimPrefix(path.Ext(base), ".")
	if ext == "" || !citedPathExtensions[strings.ToLower(ext)] {
		return "", false
	}
	return tok, true
}

// untouchedCitedPaths returns the cited paths no changed file matches. A
// cited directory matches any file under it; a cited path matches a changed
// file when either is a slash-bounded suffix of the other, so a bare basename
// or a path relative to a different root still counts.
func untouchedCitedPaths(cited, changed []string) []string {
	var out []string
	for _, p := range cited {
		if !citedPathTouched(p, changed) {
			out = append(out, p)
		}
	}
	return out
}

func citedPathTouched(cited string, changed []string) bool {
	for _, f := range changed {
		f = strings.TrimPrefix(strings.TrimSpace(f), "/")
		if f == "" {
			continue
		}
		if strings.HasSuffix(cited, "/") {
			if strings.HasPrefix(f, cited) || strings.Contains(f, "/"+cited) {
				return true
			}
			continue
		}
		if f == cited || strings.HasSuffix(f, "/"+cited) || strings.HasSuffix(cited, "/"+f) {
			return true
		}
	}
	return false
}

func closeOnMergeNeedsConfirmation(issue *gh.Issue) bool {
	for _, l := range issue.Labels {
		name := strings.ToLower(strings.TrimSpace(l.GetName()))
		switch name {
		case "needs-reporter-confirmation", "epic", "needs-human", "needs-decision", "needs-direction":
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

// closeOnMergeReopenedComment records that the issue was reopened after pr
// merged, so later sweeps skip pr without another timeline read.
func closeOnMergeReopenedComment(pr *gh.PullRequest) string {
	return fmt.Sprintf("%s\nReopened by a person after #%d merged. Hive will not close this issue for #%d again; close it by hand or with a new PR.",
		closeOnMergeReopenedMarker(pr.GetNumber()), pr.GetNumber(), pr.GetNumber())
}

func closeOnMergeReopenedMarker(pr int) string {
	return fmt.Sprintf(reopenedAfterMergeMarkerFormat, pr)
}

// closeOnMergeReopenedAfter reports a reopened event among events (already
// limited to after the PR merged) by any actor, bots such as the /reopen
// workflow's github-actions[bot] included, except the hive itself.
func closeOnMergeReopenedAfter(events []IssueEvent, identity HiveIdentity) bool {
	for _, e := range events {
		if e.Event == "reopened" && !identity.Matches(e.Actor) {
			return true
		}
	}
	return false
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
	closed, _ := c.closeOnMergeMarkers(ctx, owner, repo, number, 0)
	return closed
}

// closeOnMergeMarkers reports, in one pass over the issue's comments, whether
// Hive's close-on-merge marker is present and whether the reopened-after-merge
// marker for pr is present.
func (c *Client) closeOnMergeMarkers(ctx context.Context, owner, repo string, number, pr int) (closed, reopened bool) {
	reopenedMarker := ""
	if pr > 0 {
		reopenedMarker = closeOnMergeReopenedMarker(pr)
	}
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: closeOnMergeCommentPageSize}}
	for {
		comments, resp, err := c.client.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return closed, reopened
		}
		for _, comment := range comments {
			body := comment.GetBody()
			if strings.Contains(body, CloseOnMergeMarker) {
				return true, reopened
			}
			if reopenedMarker != "" && strings.Contains(body, reopenedMarker) {
				reopened = true
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return closed, reopened
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
