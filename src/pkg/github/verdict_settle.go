package github

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// hivecommons/hive#7871: a `no_work_needed` verdict is the one place an agent
// reports WHAT settled the issue — "already merged via #532", "covered by
// merged PR #867", "already in upstream/main (e6d3de3)". The #6869 settle scan
// cannot see any of those: it only recovers claims from PRs that REFERENCE
// the issue, and a fix that lands without mentioning the issue is invisible to
// it. The hub used to discard the reason and apply the generic cooldown, so a
// fresh contributor re-derived the same conclusion every window.
//
// This file turns the cited reference into a verified ledger claim. The text
// alone is never trusted — the API check is what turns a claim into a fact,
// which also defuses a model fabricating "already fixed by #123".

// SettlingRef is one candidate reference parsed out of a verdict reason: a
// pull request (Repo + Number) or a commit (SHA). Exactly one of Number/SHA
// is set.
type SettlingRef struct {
	// Repo is the "owner/repo" the PR lives in. Bare `#N` references default
	// to the task's repo; `owner/repo#N` and pull URLs carry their own.
	Repo   string
	Number int
	SHA    string
}

// String renders the reference for logs.
func (r SettlingRef) String() string {
	if r.SHA != "" {
		return r.Repo + "@" + r.SHA
	}
	return r.Repo + "#" + strconv.Itoa(r.Number)
}

// maxSettlingRefs bounds how many candidates one verdict may fan out into API
// calls. Real reasons cite one or two; a reason that names dozens is prose
// about the repo, not a settling claim.
const maxSettlingRefs = 6

var (
	// settlePullURLPattern is prURLPattern with a trailing word boundary so
	// "pull/12" in a URL cannot also match as "pull/1".
	settlePullURLPattern = regexp.MustCompile(`([\w.-]+)/([\w.-]+)/pull/(\d+)\b`)
	// settleQualifiedPattern matches `owner/repo#N`.
	settleQualifiedPattern = regexp.MustCompile(`(?:^|[^\w/])([\w.-]+/[\w.-]+)#(\d+)\b`)
	// settleBarePattern matches `#N` (and the "PR 532" / "PR#532" spellings
	// agents produce) with a leading boundary so `owner/repo#N` is not
	// double-counted as a bare reference.
	settleBarePattern = regexp.MustCompile(`(?i)(?:^|[^\w/])(?:PR\s*)?#(\d+)\b`)
	// settleSHAPattern matches a 7–40 hex commit reference. At least one
	// letter is required by the filter below so a run of digits (an issue
	// number, a date, a port) is never mistaken for a SHA.
	settleSHAPattern = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
)

// ParseSettlingRefs extracts the PR and commit references a verdict reason
// cites, in first-seen order and de-duplicated: pull URLs, `owner/repo#N`,
// bare `#N` (defaulting to taskRepo) and 7–40 hex SHAs. taskIssue — the issue
// the task was FOR — is excluded, since a reason that says "#533 is already
// covered by #532" names the issue itself as well as the settling PR. The
// result is capped at maxSettlingRefs. An empty reason yields nil.
func ParseSettlingRefs(reason, taskRepo string, taskIssue int) []SettlingRef {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil
	}
	taskRepo = strings.Trim(strings.TrimSpace(taskRepo), "/")
	seen := make(map[string]bool)
	var refs []SettlingRef
	add := func(r SettlingRef) {
		if len(refs) >= maxSettlingRefs {
			return
		}
		if r.Number > 0 && strings.EqualFold(r.Repo, taskRepo) && r.Number == taskIssue {
			return
		}
		key := strings.ToLower(r.String())
		if seen[key] {
			return
		}
		seen[key] = true
		refs = append(refs, r)
	}
	for _, m := range settlePullURLPattern.FindAllStringSubmatch(reason, -1) {
		if n, err := strconv.Atoi(m[3]); err == nil && n > 0 {
			add(SettlingRef{Repo: m[1] + "/" + m[2], Number: n})
		}
	}
	// Strip the URLs before the looser patterns run, so "…/pull/532" is not
	// also read as a bare "#532" and "owner/repo/pull" is not read as a repo.
	stripped := settlePullURLPattern.ReplaceAllString(reason, " ")
	for _, m := range settleQualifiedPattern.FindAllStringSubmatch(stripped, -1) {
		if n, err := strconv.Atoi(m[2]); err == nil && n > 0 {
			add(SettlingRef{Repo: m[1], Number: n})
		}
	}
	stripped = settleQualifiedPattern.ReplaceAllString(stripped, " ")
	if taskRepo != "" {
		for _, m := range settleBarePattern.FindAllStringSubmatch(stripped, -1) {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				add(SettlingRef{Repo: taskRepo, Number: n})
			}
		}
	}
	if taskRepo != "" {
		for _, sha := range settleSHAPattern.FindAllString(stripped, -1) {
			if !strings.ContainsAny(sha, "abcdef") {
				continue
			}
			add(SettlingRef{Repo: taskRepo, SHA: sha})
		}
	}
	return refs
}

// SettleVerification is the outcome of checking one SettlingRef against
// GitHub. Settled is true only when the reference is a fact the ledger should
// record: a PR merged into the task repo before the task was dispatched, an
// open PR in the task repo by someone else, or a commit reachable from the
// task repo's default branch. Reason explains a negative for the log.
type SettleVerification struct {
	Settled bool
	Claim   IssueClaim
	Reason  string
}

// SettleVerifier is the API-check seam the contribute hub calls per candidate
// reference. It is a func type so a hub under test can substitute a fixture
// for the live client.
type SettleVerifier func(ctx context.Context, taskRepo string, taskIssue int, ref SettlingRef, dispatchedAt time.Time) (SettleVerification, error)

// VerifySettlingRef checks one cited reference against GitHub and, when it
// holds up, builds the ledger claim that records it (hivecommons/hive#7871).
//
//   - A PR that is MERGED into taskRepo with merged_at before dispatchedAt is a
//     strong merged claim — the issue was settled before this task even ran.
//     A PR merged after dispatch is not accepted from a no_work_needed verdict:
//     the settle scan will pick it up if it references the issue, and if it
//     does not, the next agent's verdict will cite it and pass this check.
//   - A PR that is still OPEN in taskRepo, by someone other than the reporter,
//     is a weak external claim: another author is on it, so the queue defers
//     rather than presenting the issue cold (the #808 / #277 rows).
//   - A commit reachable from taskRepo's default branch settles the issue as a
//     merged claim. When GitHub associates a merged PR with the commit, that
//     PR is recorded; otherwise the commit itself is (PRNumber 0, commit URL).
//
// PRs in another repo, closed-unmerged PRs, unreachable commits and API
// failures all return Settled=false; an API failure additionally returns the
// error so the caller can log it distinctly from a clean negative.
func (c *Client) VerifySettlingRef(ctx context.Context, taskRepo string, taskIssue int, ref SettlingRef, dispatchedAt time.Time) (SettleVerification, error) {
	if c == nil || c.client == nil {
		return SettleVerification{Reason: "no github client configured"}, ErrNoGitHubClient
	}
	taskOwner, taskName := splitRepo(taskRepo)
	if taskOwner == "" || taskName == "" {
		return SettleVerification{Reason: "task repo is not owner/repo"}, nil
	}
	refOwner, refName := splitRepo(ref.Repo)
	if refOwner == "" {
		refOwner = taskOwner
	}
	if refName == "" {
		refName = taskName
	}
	if !strings.EqualFold(refOwner, taskOwner) || !strings.EqualFold(refName, taskName) {
		// A reference into another repo cannot settle this repo's issue —
		// the ledger keys claims by the ISSUE repo, and a cross-repo
		// "already fixed" is exactly the shape a fabricated reason takes.
		return SettleVerification{Reason: fmt.Sprintf("reference %s is not in task repo %s", ref, taskRepo)}, nil
	}
	if ref.SHA != "" {
		return c.verifySettlingCommit(ctx, taskOwner, taskName, taskRepo, taskIssue, ref.SHA, dispatchedAt)
	}
	return c.verifySettlingPR(ctx, taskOwner, taskName, taskRepo, taskIssue, ref.Number, dispatchedAt)
}

func (c *Client) verifySettlingPR(ctx context.Context, owner, name, taskRepo string, taskIssue, number int, dispatchedAt time.Time) (SettleVerification, error) {
	pr, _, err := c.client.PullRequests.Get(ctx, owner, name, number)
	if err != nil {
		return SettleVerification{Reason: "github lookup failed"}, err
	}
	if pr == nil {
		return SettleVerification{Reason: "github returned no pull request"}, nil
	}
	base := pr.GetBase().GetRepo().GetFullName()
	if !prBaseRepoMatches(base, taskRepo) {
		return SettleVerification{Reason: fmt.Sprintf("PR base repo %q is not task repo %q", base, taskRepo)}, nil
	}
	claim := IssueClaim{
		Repo:       taskRepo,
		Issue:      taskIssue,
		PRNumber:   pr.GetNumber(),
		PRRepo:     taskRepo,
		PRURL:      pr.GetHTMLURL(),
		PRAuthor:   safeGetLogin(pr.GetUser()),
		ObservedAt: time.Now(),
		Source:     ClaimSourceVerdict,
	}
	switch {
	case pr.GetMerged():
		mergedAt := pr.GetMergedAt().Time
		if !dispatchedAt.IsZero() && !mergedAt.IsZero() && mergedAt.After(dispatchedAt) {
			return SettleVerification{Reason: fmt.Sprintf("PR #%d merged at %s, after the task was dispatched at %s",
				pr.GetNumber(), mergedAt.UTC().Format(time.RFC3339), dispatchedAt.UTC().Format(time.RFC3339))}, nil
		}
		claim.MergedPR = true
		claim.MergedAt = mergedAt
		return SettleVerification{Settled: true, Claim: claim}, nil
	case strings.EqualFold(pr.GetState(), "open"):
		claim.Reference = true
		claim.ExternalAuthor = true
		return SettleVerification{Settled: true, Claim: claim}, nil
	default:
		return SettleVerification{Reason: fmt.Sprintf("PR #%d is closed without merging", pr.GetNumber())}, nil
	}
}

func (c *Client) verifySettlingCommit(ctx context.Context, owner, name, taskRepo string, taskIssue int, sha string, dispatchedAt time.Time) (SettleVerification, error) {
	branch, err := c.DefaultBranch(ctx, owner, name)
	if err != nil {
		return SettleVerification{Reason: "default branch lookup failed"}, err
	}
	cmp, _, err := c.client.Repositories.CompareCommits(ctx, owner, name, branch, sha, nil)
	if err != nil {
		return SettleVerification{Reason: "commit compare failed"}, err
	}
	// Comparing base=default, head=sha: "behind" or "identical" means the
	// default branch already contains the commit; "ahead"/"diverged" means
	// it does not.
	switch cmp.GetStatus() {
	case "behind", "identical":
	default:
		return SettleVerification{Reason: fmt.Sprintf("commit %s is not reachable from %s (%s)", sha, branch, cmp.GetStatus())}, nil
	}
	commit, _, err := c.client.Repositories.GetCommit(ctx, owner, name, sha, nil)
	if err != nil {
		return SettleVerification{Reason: "commit lookup failed"}, err
	}
	committedAt := commit.GetCommit().GetCommitter().GetDate().Time
	sha = commit.GetSHA()
	if !dispatchedAt.IsZero() && !committedAt.IsZero() && committedAt.After(dispatchedAt) {
		return SettleVerification{Reason: fmt.Sprintf("commit %s landed after the task was dispatched", sha)}, nil
	}
	// Prefer the PR GitHub associates with the commit, so the ledger entry
	// carries a PR number and URL like every other claim.
	if prs, _, err := c.client.PullRequests.ListPullRequestsWithCommit(ctx, owner, name, sha, nil); err == nil {
		for _, pr := range prs {
			if pr == nil || (!pr.GetMerged() && pr.GetMergedAt().IsZero()) {
				continue
			}
			if !prBaseRepoMatches(pr.GetBase().GetRepo().GetFullName(), taskRepo) {
				continue
			}
			return SettleVerification{Settled: true, Claim: IssueClaim{
				Repo:       taskRepo,
				Issue:      taskIssue,
				PRNumber:   pr.GetNumber(),
				PRRepo:     taskRepo,
				PRURL:      pr.GetHTMLURL(),
				PRAuthor:   safeGetLogin(pr.GetUser()),
				ObservedAt: time.Now(),
				MergedPR:   true,
				MergedAt:   pr.GetMergedAt().Time,
				Source:     ClaimSourceVerdict,
			}}, nil
		}
	}
	return SettleVerification{Settled: true, Claim: IssueClaim{
		Repo:       taskRepo,
		Issue:      taskIssue,
		PRRepo:     taskRepo,
		PRURL:      fmt.Sprintf("https://github.com/%s/%s/commit/%s", owner, name, sha),
		ObservedAt: time.Now(),
		MergedPR:   true,
		MergedAt:   committedAt,
		Source:     ClaimSourceVerdict,
	}}, nil
}
