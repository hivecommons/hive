package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

const baseBranchProtectionCacheTTL = 5 * time.Minute

type baseBranchProtectionCacheEntry struct {
	protected bool
	cachedAt  time.Time
}

func (c *Client) SetMergeRequestAllowUnprotectedBaseRepos(repos map[string]bool) {
	if c == nil {
		return
	}
	c.mergePolicyMu.Lock()
	defer c.mergePolicyMu.Unlock()
	c.allowUnprotectedBaseRepos = normalizeRepoSet(c.org, repos)
}

func (c *Client) SetMergeRequestNoCIAllowedRepos(repos map[string]bool) {
	if c == nil {
		return
	}
	c.mergePolicyMu.Lock()
	defer c.mergePolicyMu.Unlock()
	c.noCIAllowedRepos = normalizeRepoSet(c.org, repos)
}

func normalizeRepoSet(defaultOwner string, in map[string]bool) map[string]bool {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]bool, len(in))
	for repo, ok := range in {
		if !ok {
			continue
		}
		if key := repoPolicyKey(defaultOwner, repo); key != "" {
			out[key] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func repoPolicyKey(defaultOwner, repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return ""
	}
	if !strings.Contains(repo, "/") && strings.TrimSpace(defaultOwner) != "" {
		repo = strings.TrimSpace(defaultOwner) + "/" + repo
	}
	return strings.ToLower(repo)
}

func (c *Client) repoInMergePolicySet(repo string, set map[string]bool) bool {
	if c == nil || len(set) == 0 {
		return false
	}
	return set[repoPolicyKey(c.org, repo)]
}

func (c *Client) mergeRequestAllowsUnprotectedBase(repo string) bool {
	if c == nil {
		return false
	}
	c.mergePolicyMu.RLock()
	defer c.mergePolicyMu.RUnlock()
	return c.repoInMergePolicySet(repo, c.allowUnprotectedBaseRepos)
}

func (c *Client) mergeRequestAllowsNoCI(repo string) bool {
	if c == nil {
		return false
	}
	c.mergePolicyMu.RLock()
	defer c.mergePolicyMu.RUnlock()
	return c.repoInMergePolicySet(repo, c.noCIAllowedRepos)
}

func (c *Client) maybeDowngradeNoCIVerdict(req MergeRequest, verdict mergeCIVerdict, why string) (mergeCIVerdict, string) {
	if verdict != mergeCIUnverified || !c.mergeRequestAllowsNoCI(req.Repo) {
		return verdict, why
	}
	reason := why + "; auto_merge.no_ci_ok explicitly allows absent CI for this repo"
	if c.logger != nil {
		c.logger.Info("merge-request watcher: no-CI repo opt-in accepted unverified CI verdict",
			slog.String("repo", req.Repo), slog.Int("number", req.Number),
			slog.String("agent", req.Agent), slog.String("config", "auto_merge.no_ci_ok"))
	}
	return mergeCIGreen, reason
}

func (c *Client) verifyMergeRequestBaseProtected(ctx context.Context, repo string, number int) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	pr, _, err := c.client.PullRequests.Get(ctx, owner, name, number)
	if err != nil {
		return fmt.Errorf("base branch protection: fetching PR %s/%s#%d: %w", owner, name, number, err)
	}
	base := strings.TrimSpace(pr.GetBase().GetRef())
	if base == "" {
		return fmt.Errorf("base branch protection: PR %s/%s#%d has no base branch", owner, name, number)
	}
	key := repoPolicyKey(owner, name) + ":" + strings.ToLower(base)
	now := time.Now()
	c.mergePolicyMu.RLock()
	cached, ok := c.baseBranchProtectionCached[key]
	c.mergePolicyMu.RUnlock()
	if ok && now.Sub(cached.cachedAt) < baseBranchProtectionCacheTTL {
		if cached.protected {
			return nil
		}
		if c.mergeRequestAllowsUnprotectedBase(repo) {
			if c.logger != nil {
				c.logger.Info("merge-request watcher: cached unprotected base branch opt-in allows merge",
					slog.String("repo", repo), slog.Int("number", number),
					slog.String("base", base), slog.String("config", "auto_merge.allow_unprotected_base"))
			}
			return nil
		}
		return fmt.Errorf("base branch %q has no branch protection; set auto_merge.allow_unprotected_base for this repo to override", base)
	}

	_, _, err = c.client.Repositories.GetBranchProtection(ctx, owner, name, base)
	if err != nil {
		if isGitHubNotFound(err) {
			c.cacheBaseBranchProtection(key, false, now)
			if c.mergeRequestAllowsUnprotectedBase(repo) {
				if c.logger != nil {
					c.logger.Info("merge-request watcher: unprotected base branch opt-in allows merge",
						slog.String("repo", repo), slog.Int("number", number),
						slog.String("base", base), slog.String("config", "auto_merge.allow_unprotected_base"))
				}
				return nil
			}
			return fmt.Errorf("base branch %q has no branch protection; set auto_merge.allow_unprotected_base for this repo to override", base)
		}
		return fmt.Errorf("base branch protection: checking %s/%s@%s: %w", owner, name, base, err)
	}
	c.cacheBaseBranchProtection(key, true, now)
	return nil
}

func (c *Client) cacheBaseBranchProtection(key string, protected bool, now time.Time) {
	if c == nil || key == "" {
		return
	}
	c.mergePolicyMu.Lock()
	defer c.mergePolicyMu.Unlock()
	if c.baseBranchProtectionCached == nil {
		c.baseBranchProtectionCached = make(map[string]baseBranchProtectionCacheEntry)
	}
	c.baseBranchProtectionCached[key] = baseBranchProtectionCacheEntry{protected: protected, cachedAt: now}
}

func (c *Client) cachedBaseBranchProtection(owner, repo, branch string) (bool, bool) {
	if c == nil {
		return false, false
	}
	key := repoPolicyKey(owner, repo) + ":" + strings.ToLower(strings.TrimSpace(branch))
	now := time.Now()
	c.mergePolicyMu.RLock()
	cached, ok := c.baseBranchProtectionCached[key]
	c.mergePolicyMu.RUnlock()
	if !ok || now.Sub(cached.cachedAt) >= baseBranchProtectionCacheTTL {
		return false, false
	}
	return cached.protected, true
}

func isGitHubNotFound(err error) bool {
	if errors.Is(err, gh.ErrBranchNotProtected) {
		return true
	}
	var ghErr *gh.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound {
		return true
	}
	return false
}
