package hub

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// ============================================================================
// HUB UPGRADE TARGET WALKBACK — "the tip has no hub image; what is the newest
// commit on this branch that does?"
// ============================================================================
//
// fetchBranchSHA only ever admitted the branch TIP into latestHubSHAByBranch,
// and only when the tip's hive-hub image was already on GHCR. Two ordinary
// events break that:
//
//   - A release-bump commit (`release: vX.Y.Z`) is merged by github-actions[bot]
//     with GITHUB_TOKEN, and GitHub fires no `push` workflows for such pushes,
//     so docker.yml never builds an image for the release commit at all.
//   - A commit is tip for less than one poll cycle (two merges two minutes
//     apart), so the poller never sees it as tip once its image has landed.
//
// Either way the hub's target froze at whatever tip it last verified. On
// 2026-09-09 that left the hub on bd68d95 for 13 hours while d566f97 (built,
// published, one commit newer) sat unreachable behind the image-less v4.20.1
// release commit; the manual Upgrade button failed because the target never
// advanced. Spokes got the analogous fix in #6294 (reachableUpgradeTarget);
// this is the hub side.
//
// When the tip's hub image is absent, walk the branch's recent commits
// newest-first and take the first one whose hub image IS published, stopping
// at the currently recorded target (nothing newer is published, so keep it).
// Advancement stays monotonic: everything before `current` in a newest-first
// listing of a linear branch is a descendant of it.

// hubTargetWalkbackDepth bounds how many recent commits are inspected when
// the tip has no hub image. Each candidate costs one GHCR manifest probe; the
// walk stops early at the current target, so in the steady state (image-less
// release commit on top of a verified one) it probes exactly one commit.
const hubTargetWalkbackDepth = 10

// listRecentBranchCommits returns up to n commits reachable from branch,
// newest first (the tip is index 0), as short SHA + first-line message. A nil
// slice means the listing failed; the caller must then leave the target alone.
func listRecentBranchCommits(client *http.Client, branch string, n int, logger *slog.Logger) []branchSHAInfo {
	commitsURL := fmt.Sprintf("%s/repos/hivecommons/hive/commits?sha=%s&per_page=%d", githubAPIBase, branch, n)
	req, _ := http.NewRequest("GET", commitsURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		logger.Warn("SHA poll: commit list request failed", "branch", branch, "error", err)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		logger.Warn("SHA poll: commit list non-200", "branch", branch, "status", resp.StatusCode)
		return nil
	}
	var result []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		logger.Warn("SHA poll: commit list decode failed", "branch", branch, "error", err)
		return nil
	}
	out := make([]branchSHAInfo, 0, len(result))
	for _, c := range result {
		if len(c.SHA) < StandardSHALen {
			continue
		}
		msg := c.Commit.Message
		if idx := strings.Index(msg, "\n"); idx >= 0 {
			msg = msg[:idx]
		}
		out = append(out, branchSHAInfo{SHA: shortSHA(c.SHA), Message: msg})
	}
	return out
}

// newestPublishedHubAncestor finds the newest commit on branch, older than the
// (image-less) tip and newer than current, whose hive-hub image is published on
// GHCR. ok is false when there is none — including when current is already the
// newest published commit, or the listing failed.
func newestPublishedHubAncestor(client *http.Client, branch, tip, current string, logger *slog.Logger) (info branchSHAInfo, ok bool) {
	commits := listRecentBranchCommits(client, branch, hubTargetWalkbackDepth, logger)
	for _, c := range commits {
		if sameCommit(c.SHA, tip) {
			continue // the caller already probed the tip and found no image
		}
		if sameCommit(c.SHA, current) {
			return branchSHAInfo{}, false // nothing newer is published; keep the current target
		}
		if ghcrTagExists(client, ghcrRepoHub, c.SHA, logger) {
			return c, true
		}
	}
	return branchSHAInfo{}, false
}
