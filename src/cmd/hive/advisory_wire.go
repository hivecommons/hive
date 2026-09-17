package main

import (
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// This file is the thin wiring layer between runEvalCycle and the advisory
// posting policy, which now lives in pkg/advisory (#7238 stage 2). The policy
// package cannot accept a *github.Client — pkg/github imports pkg/advisory, so
// that parameter would close an import cycle — hence the nil checks happen
// here, on this side of the boundary.

func primaryAdvisoryRepo(cfg *config.Config) string {
	return advisory.PrimaryRepo(cfg)
}

func advisoryIssueUnresolved(advisoryIssues map[string]int, repo string) bool {
	return advisory.IssueUnresolved(advisoryIssues, repo)
}

func advisoryIssueNumber(advisoryIssues map[string]int, repo string) (int, bool) {
	return advisory.IssueNumber(advisoryIssues, repo)
}

func shouldBuildAdvisoryDigest(beadStores map[string]*beads.Store, ghClient *github.Client, hasExistingPinnedIssue bool) bool {
	return advisory.ShouldBuildDigest(beadStores, ghClient != nil, hasExistingPinnedIssue)
}

func shouldPostAdvisoryDigest(digest *advisory.Digest, ghClient *github.Client, hasPinnedIssue bool) bool {
	return advisory.ShouldPostDigest(digest, ghClient != nil, hasPinnedIssue)
}

func advisoryPostDue(advCfg config.AdvisoryConfig, repo string, now time.Time, logger *slog.Logger) bool {
	return advisory.PostDue(advCfg, repo, now, logger)
}

func recordAdvisoryPostSuccess(repo string, now time.Time) {
	advisory.RecordPostSuccess(repo, now)
}
