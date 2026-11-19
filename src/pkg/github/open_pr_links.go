package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// Pre-dispatch guard against re-dispatching an issue whose fix is already
// open (hivecommons/hive#14799). Every automated claim producer runs
// IssueClaimBlockReason first; that gate used to see only the eval cycle's
// claim ledger, which lags a PR a person opened by hand until the next scan.
// #14766 and #14763 were each handed to a relay contributor six minutes
// after their fix PR opened, and the resulting pair of same-issue PRs ended
// up at stage/085 for the owner to pick between. The index here answers the
// same question live — one open-PR listing per repo per sweep, parsed with
// the claim ledger's own closing-keyword grammar — so the guard sees the PR
// the moment it exists.

const (
	// HasOpenPRLabel marks an issue the guard refused to dispatch because an
	// open pull request already links it with a closing keyword. It is a
	// signal for the tracker, not a park: the label comes off with nothing
	// — the stage engine re-derives the issue's stage from the PR itself.
	HasOpenPRLabel            = "has-open-pr"
	hasOpenPRLabelColor       = "0e8a16"
	hasOpenPRLabelDescription = "An open PR already links this issue with Fixes/Closes/Resolves; the hive will not dispatch it again"

	// ClaimBlockReasonOpenPRPrefix prefixes the IssueClaimBlockReason for an
	// issue an open PR already links: "open PR already linked: #14775".
	ClaimBlockReasonOpenPRPrefix = "open PR already linked: #"

	// openPRLinkCacheTTL bounds how long one repo's open-PR listing answers
	// the guard. A sweep (contributor selectTask pass, eval-cycle claim
	// burst) finishes well inside it, so the listing is paid once per sweep;
	// a PR opened after the listing is seen on the next one.
	openPRLinkCacheTTL = time.Minute

	// openPRLinkListTimeout bounds one full open-PR listing for the guard.
	openPRLinkListTimeout = 45 * time.Second
	// openPRLinkFailureTTL is how long a failed listing answers the guard
	// for its repo: a surge of claims fails open at once instead of each one
	// re-running a listing that just timed out and re-logging the warning.
	openPRLinkFailureTTL = time.Minute

	// openPRLabelTimeout bounds the has-open-pr label write on its own
	// budget. It used to share the caller's deadline (15s from the
	// contribute lane) with the open-PR listing ahead of it, so a slow
	// listing of a large repo left the label write an already-spent
	// context and every guard pass failed with `context deadline exceeded`
	// (hivecommons/hive#18589).
	openPRLabelTimeout = 10 * time.Second
	// openPRLabelRetryBackoff is how long a transient label failure
	// (timeout, rate limit, 5xx) keeps the guard from re-trying that issue:
	// the label is a tracker signal, and a claim surge on the same issue
	// must not turn one slow write into a WARN per pass.
	openPRLabelRetryBackoff = 15 * time.Minute
	// openPRLabelRefusedBackoff is the same for a refusal that will not fix
	// itself (no write access, issue gone or locked).
	openPRLabelRefusedBackoff = 6 * time.Hour
)

// errOpenPRListingBackedOff marks an error replayed from the failure cache.
var errOpenPRListingBackedOff = errors.New("open-PR listing failed recently")

type openPRLinkFailure struct {
	err   error
	until time.Time
}

// OpenPRLink is one open pull request that links an issue with a closing
// keyword, as the guard found it.
type OpenPRLink struct {
	// Repo is the issue's repository in the configured spelling.
	Repo string
	// Issue is the linked issue number.
	Issue int
	// PRRepo / PRNumber / PRURL / PRAuthor identify the linking PR.
	PRRepo   string
	PRNumber int
	PRURL    string
	PRAuthor string
	// CreatedAt is when the PR was opened; with several open PRs linking
	// one issue the earliest is reported.
	CreatedAt time.Time
}

// openPRLinkIndex is one repo's open PRs indexed by the issue they close.
type openPRLinkIndex struct {
	fetchedAt time.Time
	// byIssue maps claimKey(issueRepo, issue) to every open PR closing it,
	// sorted by creation time (earliest first).
	byIssue map[string][]OpenPRLink
	// prs is the raw listing, kept for the duplicate-fix resolver so it does
	// not list the repo a second time in the same sweep.
	prs []*gh.PullRequest
}

// openPRLinkCache is the per-client, per-repo cache behind the guard.
type openPRLinkCache struct {
	mu    sync.Mutex
	repos map[string]*openPRLinkIndex
	// failures holds each repo's last listing failure for openPRLinkFailureTTL.
	failures map[string]openPRLinkFailure
	// labelBackoff holds, per claimKey, when the guard may next try to
	// label that issue after a failed has-open-pr write.
	labelBackoff map[string]time.Time
	// disabled is scanner.skip_issues_with_open_pr: false.
	disabled bool
	now      func() time.Time
}

func (c *Client) openPRLinks() *openPRLinkCache {
	c.openPRLinkOnce.Do(func() {
		c.openPRLinkState = &openPRLinkCache{repos: map[string]*openPRLinkIndex{}, failures: map[string]openPRLinkFailure{}, labelBackoff: map[string]time.Time{}, now: time.Now}
	})
	return c.openPRLinkState
}

// SetSkipIssuesWithOpenPR installs scanner.skip_issues_with_open_pr. The
// guard is on until told otherwise.
func (c *Client) SetSkipIssuesWithOpenPR(enabled bool) {
	if c == nil {
		return
	}
	cache := c.openPRLinks()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.disabled = !enabled
}

// SkipIssuesWithOpenPR reports whether the open-PR guard is on.
func (c *Client) SkipIssuesWithOpenPR() bool {
	if c == nil {
		return false
	}
	cache := c.openPRLinks()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return !cache.disabled
}

// InvalidateOpenPRLinks drops the cached listing for repo (every repo when
// repo is ""), so the next guard call lists again. The PR-opened watcher
// path calls it when the hive itself opens a PR.
func (c *Client) InvalidateOpenPRLinks(repo string) {
	if c == nil {
		return
	}
	cache := c.openPRLinks()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if repo == "" {
		cache.repos = map[string]*openPRLinkIndex{}
		cache.failures = map[string]openPRLinkFailure{}
		return
	}
	delete(cache.repos, c.canonicalRepo(repo))
	delete(cache.failures, c.canonicalRepo(repo))
}

// canonicalRepo maps repo onto the configured spelling, so a caller naming
// the repo bare or org-qualified shares one cache entry and one claim key.
func (c *Client) canonicalRepo(repo string) string {
	return canonicalConfigRepo(repo, c.getRepos())
}

// openPRIndexFor returns the (possibly cached) open-PR link index for repo.
func (c *Client) openPRIndexFor(ctx context.Context, repo string) (*openPRLinkIndex, error) {
	repo = c.canonicalRepo(repo)
	cache := c.openPRLinks()
	cache.mu.Lock()
	now := cache.now()
	if idx, ok := cache.repos[repo]; ok && now.Sub(idx.fetchedAt) < openPRLinkCacheTTL {
		cache.mu.Unlock()
		return idx, nil
	}
	if f, ok := cache.failures[repo]; ok && now.Before(f.until) {
		cache.mu.Unlock()
		return nil, fmt.Errorf("%w: %w", errOpenPRListingBackedOff, f.err)
	}
	cache.mu.Unlock()

	owner, repoName := c.splitRepo(repo)
	listCtx, cancel := context.WithTimeout(WithRESTCaller(ctx, "hive:open_pr_links"), openPRLinkListTimeout)
	defer cancel()
	prs, err := c.listOpenPRsForSupersessionSweep(listCtx, owner, repoName)
	if err != nil {
		cache.mu.Lock()
		cache.failures[repo] = openPRLinkFailure{err: err, until: cache.now().Add(openPRLinkFailureTTL)}
		cache.mu.Unlock()
		return nil, err
	}
	identity := c.getHiveIdentity()
	if identity.AppLogin == "" {
		identity.AppLogin = c.appBotLogin
	}
	idx := buildOpenPRLinkIndex(prs, repo, identity, now)
	cache.mu.Lock()
	cache.repos[repo] = idx
	delete(cache.failures, repo)
	cache.mu.Unlock()
	return idx, nil
}

// buildOpenPRLinkIndex indexes prs by the issues their title/body close.
// Only closing-keyword claims count: a `Refs #N` reference or a branch-name
// heuristic is work-in-the-area, not "this PR is the fix".
func buildOpenPRLinkIndex(prs []*gh.PullRequest, repo string, identity HiveIdentity, now time.Time) *openPRLinkIndex {
	idx := &openPRLinkIndex{fetchedAt: now, byIssue: map[string][]OpenPRLink{}, prs: prs}
	for _, pr := range prs {
		if pr == nil || pr.GetNumber() <= 0 || (pr.GetState() != "" && !strings.EqualFold(pr.GetState(), PRStateOpen)) {
			continue
		}
		for _, claim := range claimsFromPR(pr, repo, identity, now) {
			if !claim.Closing {
				continue
			}
			key := claimKey(claim.Repo, claim.Issue)
			idx.byIssue[key] = append(idx.byIssue[key], OpenPRLink{
				Repo:      claim.Repo,
				Issue:     claim.Issue,
				PRRepo:    repo,
				PRNumber:  pr.GetNumber(),
				PRURL:     pr.GetHTMLURL(),
				PRAuthor:  claim.PRAuthor,
				CreatedAt: pr.GetCreatedAt().Time,
			})
		}
	}
	for key := range idx.byIssue {
		links := idx.byIssue[key]
		sort.SliceStable(links, func(i, j int) bool {
			if !links[i].CreatedAt.Equal(links[j].CreatedAt) {
				return links[i].CreatedAt.Before(links[j].CreatedAt)
			}
			return links[i].PRNumber < links[j].PRNumber
		})
	}
	return idx
}

// OpenPRLinkingIssue reports the earliest open pull request in repo that
// links issue number with a closing keyword. ok is false when none does.
// A listing failure is returned so the caller decides how to fail.
func (c *Client) OpenPRLinkingIssue(ctx context.Context, repo string, number int) (OpenPRLink, bool, error) {
	if c == nil || c.client == nil {
		return OpenPRLink{}, false, ErrNoGitHubClient
	}
	idx, err := c.openPRIndexFor(ctx, repo)
	if err != nil {
		return OpenPRLink{}, false, fmt.Errorf("listing open PRs linking %s#%d: %w", repo, number, err)
	}
	links := idx.byIssue[claimKey(c.canonicalRepo(repo), number)]
	if len(links) == 0 {
		return OpenPRLink{}, false, nil
	}
	return links[0], true, nil
}

// OpenPRLinkGuard is the pre-dispatch guard every automated claim producer
// runs: it reports the earliest open PR already linking the issue, labels
// the issue HasOpenPRLabel when it does not carry it yet (label created on
// first use) and logs `claim skipped: open PR already linked`. ok is false
// when the guard is off (scanner.skip_issues_with_open_pr: false), no open
// PR links the issue, or the listing failed — a failure fails OPEN with a
// warning: the ledger-backed guards downstream still stand, and a flaky
// listing must not stall every claim in the hive. labels are the issue's
// current labels as the caller read them (nil when unknown).
func (c *Client) OpenPRLinkGuard(ctx context.Context, repo string, number int, labels []string) (OpenPRLink, bool) {
	if c == nil || !c.SkipIssuesWithOpenPR() {
		return OpenPRLink{}, false
	}
	link, ok, err := c.OpenPRLinkingIssue(ctx, repo, number)
	if err != nil {
		// A replayed failure was already warned about when it happened.
		if c.logger != nil && !errors.Is(err, errOpenPRListingBackedOff) {
			reason := "open-pr-listing-failed"
			if errors.Is(err, context.DeadlineExceeded) {
				reason = "open-pr-listing-timeout"
			}
			c.logger.Warn("open-PR guard: could not list open PRs, admitting the claim on ledger evidence alone",
				slog.String("repo", repo), slog.Int("issue", number),
				slog.String("reason", reason), slog.String("error", err.Error()))
		}
		return OpenPRLink{}, false
	}
	if !ok {
		return OpenPRLink{}, false
	}
	if c.logger != nil {
		c.logger.Info("claim skipped: open PR already linked",
			slog.String("repo", repo), slog.Int("issue", number),
			slog.Int("pr", link.PRNumber), slog.String("pr_url", link.PRURL), slog.String("pr_author", link.PRAuthor))
	}
	c.markIssueHasOpenPR(ctx, repo, number, labels, link)
	return link, true
}

// openPRLinkBlockReason is the IssueClaimBlockReason step for the guard:
// the refusal reason for an issue an open PR already links, "" otherwise.
func (c *Client) openPRLinkBlockReason(ctx context.Context, repo string, number int, labels []string) string {
	link, ok := c.OpenPRLinkGuard(ctx, repo, number, labels)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s%d", ClaimBlockReasonOpenPRPrefix, link.PRNumber)
}

// markIssueHasOpenPR applies HasOpenPRLabel once. Idempotent on the labels
// the caller just read; a label failure is logged, never fatal — the claim
// is refused either way. The write runs on its own budget, and a failed
// write is remembered per issue so later passes skip it quietly until the
// backoff lapses (hivecommons/hive#18589). A refusal that cannot fix itself
// logs at INFO; a transient failure logs one WARN per backoff window.
func (c *Client) markIssueHasOpenPR(ctx context.Context, repo string, number int, labels []string, link OpenPRLink) {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), HasOpenPRLabel) {
			return
		}
	}
	key := claimKey(c.canonicalRepo(repo), number)
	cache := c.openPRLinks()
	cache.mu.Lock()
	until, backedOff := cache.labelBackoff[key]
	backedOff = backedOff && cache.now().Before(until)
	cache.mu.Unlock()
	if backedOff {
		return
	}

	labelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openPRLabelTimeout)
	defer cancel()
	step := "could not ensure label"
	err := c.EnsureIssueLabel(labelCtx, repo, HasOpenPRLabel, hasOpenPRLabelColor, hasOpenPRLabelDescription)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		// A failed lookup or create does not mean the label is missing:
		// the add below still lands when it exists (or the forge creates
		// it), so only a spent budget skips it.
		if addErr := c.AddLabels(labelCtx, repo, number, []string{HasOpenPRLabel}); addErr != nil {
			step, err = "could not label issue", addErr
		} else {
			err = nil
		}
	}

	cache.mu.Lock()
	now := cache.now()
	for k, t := range cache.labelBackoff {
		if !now.Before(t) {
			delete(cache.labelBackoff, k)
		}
	}
	if err == nil {
		delete(cache.labelBackoff, key)
		cache.mu.Unlock()
		return
	}
	reason, refused := openPRLabelFailureReason(err)
	backoff := openPRLabelRetryBackoff
	if refused {
		backoff = openPRLabelRefusedBackoff
	}
	cache.labelBackoff[key] = now.Add(backoff)
	cache.mu.Unlock()

	if c.logger == nil {
		return
	}
	attrs := []any{slog.String("label", HasOpenPRLabel), slog.String("repo", repo), slog.Int("issue", number),
		slog.Int("pr", link.PRNumber), slog.String("reason", reason), slog.Duration("retry_after", backoff), slog.String("error", err.Error())}
	if refused {
		c.logger.Info("open-PR guard: "+step+" (refused; not retrying this issue for a while)", attrs...)
		return
	}
	c.logger.Warn("open-PR guard: "+step, attrs...)
}

// openPRLabelFailureReason names a has-open-pr write failure for the log and
// reports whether it is a refusal that will not fix itself on retry: no
// write access (403), the repo or issue is gone (404/410), or the forge
// rejected the write — a locked issue answers 422.
func openPRLabelFailureReason(err error) (reason string, refused bool) {
	switch {
	case isRateLimitErr(err):
		return "rate-limited", false
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", false
	case githubStatusError(err, http.StatusForbidden):
		return "forbidden", true
	case githubStatusError(err, http.StatusNotFound), githubStatusError(err, http.StatusGone):
		return "not-found", true
	case githubStatusError(err, http.StatusUnprocessableEntity):
		return "unprocessable", true
	default:
		return "error", false
	}
}
