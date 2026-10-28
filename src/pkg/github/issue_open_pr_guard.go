package github

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// HasOpenPRLabel marks an issue the open-PR claim guard refused: an open pull
// request already links it with a closing keyword (hivecommons/hive#14804).
const HasOpenPRLabel = "has-open-pr"

// ClaimBlockReasonOpenPRPrefix prefixes the refusal the open-PR claim guard
// returns: "open PR already linked: #14775".
const ClaimBlockReasonOpenPRPrefix = "open PR already linked: #"

// openLinkedPRCacheTTL bounds how long one open-PR listing answers for a
// repo. It spans one claim sweep — a contributor queue pass, a burst of
// start signals, a relay batch — so the guard costs one listing per repo per
// sweep instead of one call per issue, while a PR opened since the last
// listing is seen within the minute rather than at the next eval cycle's
// FetchClaimScan.
const openLinkedPRCacheTTL = time.Minute

// openLinkedPRCache is the per-sweep open-PR listing behind the guard, plus
// the issues it has labelled HasOpenPRLabel this process and not yet cleared.
type openLinkedPRCache struct {
	mu       sync.Mutex
	now      func() time.Time
	byRepo   map[string]openLinkedPRSnapshot
	labelled map[string]bool
}

type openLinkedPRSnapshot struct {
	at    time.Time
	links map[int]IssueClaim
}

func (p *openLinkedPRCache) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// SetSkipIssuesWithOpenPR installs scanner.skip_issues_with_open_pr. Off (the
// zero value, and what every test constructs) leaves every claim path exactly
// as it was before the guard existed.
func (c *Client) SetSkipIssuesWithOpenPR(on bool) {
	if c == nil {
		return
	}
	c.skipIssuesWithOpenPR.Store(on)
}

// openLinkedPRs returns, for owner/repo, the open pull requests whose title or
// body closes an issue in that repository (Fixes/Closes/Resolves #N, the
// keywords ParseClaimedIssues reads), keyed by issue. When two open PRs close
// the same issue the earlier-numbered one is kept. One listing is reused for
// openLinkedPRCacheTTL.
func (c *Client) openLinkedPRs(ctx context.Context, owner, repo string) (map[int]IssueClaim, error) {
	full := owner + "/" + repo
	key := strings.ToLower(full)
	cache := &c.openLinkedPRCache
	cache.mu.Lock()
	now := cache.clock()
	if snap, ok := cache.byRepo[key]; ok && now.Sub(snap.at) < openLinkedPRCacheTTL {
		cache.mu.Unlock()
		return snap.links, nil
	}
	cache.mu.Unlock()

	links := map[int]IssueClaim{}
	opts := &gh.PullRequestListOptions{
		State:       "open",
		ListOptions: gh.ListOptions{PerPage: claimSearchPerPage},
	}
	for page := 0; page < claimSearchMaxPages; page++ {
		prs, resp, err := c.client.PullRequests.List(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing open PRs for %s: %w", full, err)
		}
		for _, pr := range prs {
			if pr == nil {
				continue
			}
			for _, issue := range ClosingIssueRefs(pr.GetTitle()+"\n"+pr.GetBody(), full) {
				if prev, ok := links[issue]; ok && prev.PRNumber < pr.GetNumber() {
					continue
				}
				links[issue] = IssueClaim{
					Repo:       full,
					Issue:      issue,
					PRNumber:   pr.GetNumber(),
					PRRepo:     full,
					PRURL:      pr.GetHTMLURL(),
					PRAuthor:   safeGetLogin(pr.GetUser()),
					PRState:    PRStateOpen,
					PRHead:     pr.GetHead().GetSHA(),
					ObservedAt: now,
					Closing:    true,
				}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	cache.mu.Lock()
	if cache.byRepo == nil {
		cache.byRepo = map[string]openLinkedPRSnapshot{}
	}
	cache.byRepo[key] = openLinkedPRSnapshot{at: now, links: links}
	cache.mu.Unlock()
	return links, nil
}

// IssueOpenPRClaim is the live pre-dispatch guard of
// scanner.skip_issues_with_open_pr (hivecommons/hive#14804). The duplicate-PR
// claim ledger is refreshed only by the eval cycle's FetchClaimScan, so a PR
// opened between scans is invisible to every claim gate until the next cycle;
// this reads the repo's open PRs live (one cached listing per sweep) and
// reports the open PR that closes repo#number.
//
// On a hit it labels the issue HasOpenPRLabel (once, creating the label if
// missing) and logs "claim skipped: open PR already linked". When it later
// readmits an issue it labelled — the linked PR merged or closed — it removes
// the label again, so HasOpenPRLabel tracks the live refusal rather than
// lingering. It reports nothing while the setting is off, and fails open on a
// listing error: the ledger's next scan still covers the issue, and an API
// hiccup must not stop every claim.
func (c *Client) IssueOpenPRClaim(ctx context.Context, repo string, number int) (IssueClaim, bool) {
	if c == nil || c.client == nil || number <= 0 || !c.skipIssuesWithOpenPR.Load() {
		return IssueClaim{}, false
	}
	owner, name := c.splitRepo(repo)
	links, err := c.openLinkedPRs(ctx, owner, name)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("open-PR claim guard: listing failed, claim gates fall back to the claim ledger",
				"repo", owner+"/"+name, "issue", number, "error", err)
		}
		return IssueClaim{}, false
	}
	claim, ok := links[number]
	if !ok {
		c.unlabelHasOpenPR(ctx, owner+"/"+name, number)
		return IssueClaim{}, false
	}
	if c.logger != nil {
		c.logger.Info("claim skipped: open PR already linked",
			"repo", owner+"/"+name, "issue", number, "pr", claim.PRNumber, "pr_url", claim.PRURL, "pr_author", claim.PRAuthor)
	}
	c.labelHasOpenPR(ctx, owner+"/"+name, number)
	return claim, true
}

// IssueOpenPRClaimBlockReason is IssueOpenPRClaim as a claim refusal reason
// ("" admits), for the ledger admission and the `claim` relay request.
func (c *Client) IssueOpenPRClaimBlockReason(ctx context.Context, repo string, number int) string {
	claim, ok := c.IssueOpenPRClaim(ctx, repo, number)
	if !ok {
		return ""
	}
	return ClaimBlockReasonOpenPRPrefix + strconv.Itoa(claim.PRNumber)
}

// labelHasOpenPR applies HasOpenPRLabel at most once per issue per process
// (until unlabelHasOpenPR clears it); a failed write is logged and retried on
// the next refusal.
func (c *Client) labelHasOpenPR(ctx context.Context, repo string, number int) {
	key := strings.ToLower(repo) + "#" + strconv.Itoa(number)
	cache := &c.openLinkedPRCache
	cache.mu.Lock()
	done := cache.labelled[key]
	cache.mu.Unlock()
	if done {
		return
	}
	err := c.EnsureIssueLabel(ctx, repo, HasOpenPRLabel, "", "An open pull request already links this issue; automated claims are refused")
	if err == nil {
		err = c.AddLabels(ctx, repo, number, []string{HasOpenPRLabel})
	}
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("open-PR claim guard: could not label issue", "repo", repo, "issue", number, "label", HasOpenPRLabel, "error", err)
		}
		return
	}
	cache.mu.Lock()
	if cache.labelled == nil {
		cache.labelled = map[string]bool{}
	}
	cache.labelled[key] = true
	cache.mu.Unlock()
}

// unlabelHasOpenPR removes HasOpenPRLabel from an issue this process labelled
// once the guard admits it again: the linked PR has merged or closed, so the
// label would otherwise go stale. A failed removal is logged and retried on
// the next admission.
func (c *Client) unlabelHasOpenPR(ctx context.Context, repo string, number int) {
	key := strings.ToLower(repo) + "#" + strconv.Itoa(number)
	cache := &c.openLinkedPRCache
	cache.mu.Lock()
	labelled := cache.labelled[key]
	cache.mu.Unlock()
	if !labelled {
		return
	}
	if err := c.RemoveLabel(ctx, repo, number, HasOpenPRLabel); err != nil {
		if c.logger != nil {
			c.logger.Warn("open-PR claim guard: could not clear issue label", "repo", repo, "issue", number, "label", HasOpenPRLabel, "error", err)
		}
		return
	}
	if c.logger != nil {
		c.logger.Info("open-PR claim guard: linked PR no longer open, label cleared", "repo", repo, "issue", number, "label", HasOpenPRLabel)
	}
	cache.mu.Lock()
	delete(cache.labelled, key)
	cache.mu.Unlock()
}
