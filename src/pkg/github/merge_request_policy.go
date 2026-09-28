package github

import (
	"log/slog"
	"strings"
)

func (c *Client) SetMergeRequestAllowUnprotectedBaseRepos(repos map[string]bool) {
	if c == nil {
		return
	}
	c.mergePolicyMu.Lock()
	defer c.mergePolicyMu.Unlock()
	// Deprecated no-op: unprotected bases are no longer refused. Keep accepting
	// and normalizing the config so existing hive.yaml overlays keep loading.
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
