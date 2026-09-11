package github

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// Content-identity duplicate guard (kubestellar/hive#5111).
//
// CreatePR has always been idempotent for the SAME head branch: re-requesting a
// PR for a branch that already has one returns the existing PR instead of
// opening a second. That covers a retried request; it cannot see the incident
// class this file exists for — an agent copying one change forward onto a NEW
// branch and filing it again. tuna-os/tromso collected five such PRs (#170,
// #212, #225, #241, #244) sharing the same blobs, two of them (#241, #212)
// carrying an identical tree, so `git diff` between their tips was empty.
//
// The discriminator is the commit's TREE SHA. Two commits with the same tree
// have byte-identical content, so two open PRs on the same base whose head
// commits share a tree propose exactly the same change — whatever their branch
// names, commit messages, or authorship. That is an equality test, not a
// heuristic: it cannot fire on two changes that merely look alike, which is why
// it is safe to act on automatically. The near-duplicate class in the same
// issue (same topic, different bytes) is deliberately NOT addressed here; it
// has no exact test and belongs in policy text, not in a gate that silently
// swallows work.
//
// Comparing only against PRs with the SAME base matters. An identical tree on a
// different base is a different diff, so it is not a duplicate.

// maxDuplicateTreeCandidates bounds how many open PRs one guard pass inspects.
// Each uncached candidate costs a commit lookup, and PR creation must not turn
// into an unbounded fan-out on a repository with hundreds of open PRs — the
// same secondary-rate-limit exposure that made the request watcher adopt
// backoff. Reaching the cap is logged rather than passed over in silence: a
// truncated pass can miss a duplicate, and an operator reading "no duplicate
// found" deserves to know the search was partial.
const maxDuplicateTreeCandidates = 50

// findOpenPRWithIdenticalTree returns an open PR against base whose head commit
// points at the same tree as head's tip, or nil when there is none.
//
// Errors are for the CALLER to decide about. This returns them rather than
// swallowing them because a dedupe lookup that failed is not the same fact as a
// dedupe lookup that found nothing, and CreatePR deliberately treats the former
// as "proceed and open the PR" — refusing to publish work because an auxiliary
// read failed would be a worse bug than the duplicate it is guarding.
func (c *Client) findOpenPRWithIdenticalTree(ctx context.Context, owner, repo, head, base string) (*gh.PullRequest, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	headSHA, err := c.LatestCommitHash(ctx, owner, repo, head)
	if err != nil {
		return nil, err
	}
	headTree, err := c.commitTreeSHA(ctx, owner, repo, headSHA)
	if err != nil {
		return nil, err
	}
	if headTree == "" {
		return nil, fmt.Errorf("duplicate-tree guard: commit %s carries no tree SHA", headSHA)
	}

	prs, _, err := c.client.PullRequests.List(ctx, owner, repo, &gh.PullRequestListOptions{
		State:       "open",
		Base:        base,
		ListOptions: gh.ListOptions{PerPage: 100},
	})
	if err != nil {
		return nil, err
	}

	inspected := 0
	for _, pr := range prs {
		if pr == nil {
			continue
		}
		candidateSHA := strings.TrimSpace(pr.GetHead().GetSHA())
		if candidateSHA == "" {
			continue
		}
		// The same commit is trivially the same tree, and costs no lookup to
		// recognise. This also covers the branch-renamed-but-not-rebuilt case.
		if candidateSHA == headSHA {
			return pr, nil
		}
		if inspected >= maxDuplicateTreeCandidates {
			c.logger.Info("duplicate-tree guard: candidate cap reached, remaining open PRs not inspected",
				slog.String("repo", owner+"/"+repo), slog.String("base", base),
				slog.Int("inspected", inspected), slog.Int("open_prs", len(prs)))
			break
		}
		inspected++
		candidateTree, terr := c.commitTreeSHA(ctx, owner, repo, candidateSHA)
		if terr != nil {
			if isRetryableGitHubError(terr) {
				return nil, terr
			}
			// One definitively unreadable candidate must not blind the guard to
			// the rest. A PR whose head commit was force-pushed away between the
			// list and the lookup is the ordinary cause, and it is not a
			// duplicate of anything by then.
			c.logger.Warn("duplicate-tree guard: could not read a candidate's tree, skipping it",
				slog.String("repo", owner+"/"+repo), slog.Int("number", pr.GetNumber()),
				slog.String("error", terr.Error()))
			continue
		}
		if candidateTree == headTree {
			return pr, nil
		}
	}
	return nil, nil
}

// commitTreeSHA resolves a commit SHA to the SHA of the tree it points at.
//
// The mapping is immutable — a commit's tree can never change — so the result
// is cached for the life of the process without any staleness risk. That
// matters more than it looks: the open PRs on a repository are re-inspected on
// every PR creation, so without the cache the guard's cost would grow with the
// product of open PRs and PRs opened, and with it the steady-state cost is one
// lookup for the new head alone.
func (c *Client) commitTreeSHA(ctx context.Context, owner, repo, sha string) (string, error) {
	if c == nil || c.client == nil {
		return "", ErrNoGitHubClient
	}
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return "", fmt.Errorf("commitTreeSHA: a commit SHA is required")
	}
	key := owner + "/" + repo + "@" + sha
	if tree, ok := c.cachedCommitTree(key); ok {
		return tree, nil
	}
	commit, _, err := c.client.Git.GetCommit(ctx, owner, repo, sha)
	if err != nil {
		return "", fmt.Errorf("fetching commit %s/%s@%s: %w", owner, repo, sha, err)
	}
	tree := strings.TrimSpace(commit.GetTree().GetSHA())
	if tree == "" {
		// Do not cache an empty answer: it would make a one-off malformed
		// response permanent for the life of the process.
		return "", nil
	}
	c.storeCommitTree(key, tree)
	return tree, nil
}

func (c *Client) cachedCommitTree(key string) (string, bool) {
	c.commitTreeMu.RLock()
	defer c.commitTreeMu.RUnlock()
	tree, ok := c.commitTrees[key]
	return tree, ok
}

// maxCommitTreeCacheEntries bounds the commit->tree cache. The mapping is
// immutable so nothing here goes stale; the bound exists only so a long-lived
// process working many repositories cannot grow the map without limit. At the
// bound the cache is dropped whole rather than evicted entry by entry — this is
// a cost optimisation, not a correctness mechanism, so the simplest possible
// reclaim is the right one and a rebuilt cache costs only lookups.
const maxCommitTreeCacheEntries = 4096

func (c *Client) storeCommitTree(key, tree string) {
	c.commitTreeMu.Lock()
	defer c.commitTreeMu.Unlock()
	if c.commitTrees == nil {
		c.commitTrees = map[string]string{}
	}
	if len(c.commitTrees) >= maxCommitTreeCacheEntries {
		c.commitTrees = map[string]string{}
	}
	c.commitTrees[key] = tree
}
