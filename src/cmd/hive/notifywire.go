package main

import (
	"log/slog"
	"sync"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

var allowUnprotectedBaseDeprecatedLogOnce sync.Once

func logDeprecatedAllowUnprotectedBase(cfg *config.Config, logger *slog.Logger) {
	if cfg == nil || len(cfg.AutoMerge.AllowUnprotectedBase) == 0 {
		return
	}
	allowUnprotectedBaseDeprecatedLogOnce.Do(func() {
		if logger != nil {
			logger.Warn("auto_merge.allow_unprotected_base is deprecated and no longer needed; merge requests may target any protected or unprotected branch the GitHub App can write, while the CI-evidence gate still blocks red, pending, or unverified CI")
		}
	})
}

func syncAutoMergePolicyToGitHubClient(cfg *config.Config, ghClient *github.Client) (map[string]bool, bool) {
	if cfg == nil || ghClient == nil {
		return nil, false
	}
	set, ok := cfg.AutoMerge.RequiredCheckSet()
	// The merge-request watcher's pre-merge CI gate (#6173) names required
	// checks that have not reported yet, so it needs the same declared set the
	// sweep gates on. Passing nil clears stale values after config reload.
	ghClient.SetRequiredChecks(set)
	ghClient.SetMergeRequestAllowUnprotectedBaseRepos(cfg.AutoMerge.AllowUnprotectedBaseSet())
	ghClient.SetMergeRequestNoCIAllowedRepos(cfg.AutoMerge.NoCIOKSet())
	ghClient.SetAutoMergeMinHeadAge(cfg.AutoMerge.EffectiveMinHeadAge())
	return set, ok
}
