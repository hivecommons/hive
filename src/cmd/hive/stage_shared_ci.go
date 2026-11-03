package main

import (
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/hivecommons/hive/pkg/github"
)

// stageSharedCIEntry is one red PR's shared-CI deferral and base-branch-red
// classification as of the last CI-enriched enumeration.
type stageSharedCIEntry struct {
	head     string
	incident int
	// baseRed names the failing checks that are also red on the base branch
	// tip; set only when every failing check is (PullRequest.AllFailuresBaseRed).
	baseRed []string
	// forkGuard names the failing checks that refuse the PR only because it
	// is from a fork; set only when every failing check does
	// (PullRequest.ForkGuardChecks).
	forkGuard []string
}

// stageSharedCIIndex holds, per "repo#number", the open [shared-ci] incident
// each red PR was deferred to (github.PullRequest.SharedCIIncident) and the
// base-branch-red checks (github.PullRequest.BaseRedChecks) in the latest
// CI-enriched actionable snapshot. The stage reconciler reads it through
// StageLabelPolicy.SharedCIIncident / BaseBranchRed so a base/shared-CI red
// PR is not returned to its contributor or nudged (hivecommons/hive#12606).
var stageSharedCIIndex atomic.Pointer[map[string]stageSharedCIEntry]

func stageSharedCIKey(repo string, number int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
}

// publishStageSharedCIIncidents replaces the index from a freshly
// CI-enriched actionable result (PRs.Items and PRs.Held).
func publishStageSharedCIIncidents(actionable *github.ActionableResult) {
	idx := map[string]stageSharedCIEntry{}
	if actionable != nil {
		for _, list := range [][]github.PullRequest{actionable.PRs.Items, actionable.PRs.Held} {
			for _, pr := range list {
				e := stageSharedCIEntry{head: pr.HeadSHA, incident: pr.SharedCIIncident}
				if pr.AllFailuresBaseRed() {
					e.baseRed = append([]string(nil), pr.BaseRedChecks...)
				}
				e.forkGuard = pr.ForkGuardChecks()
				if e.incident > 0 || len(e.baseRed) > 0 || len(e.forkGuard) > 0 {
					idx[stageSharedCIKey(pr.Repo, pr.Number)] = e
				}
			}
		}
	}
	stageSharedCIIndex.Store(&idx)
}

// stageSharedCIIncident answers StageLabelPolicy.SharedCIIncident. repo is
// "owner/name"; the snapshot may key a PR by either form. A head pushed
// since the snapshot is not covered: the deferral was decided for the old
// head, and the next enrichment re-evaluates the new one.
func stageSharedCIIncident(repo string, number int, head string) int {
	e, ok := stageSharedCILookup(repo, number, head)
	if !ok {
		return 0
	}
	return e.incident
}

// stageBaseBranchRed answers StageLabelPolicy.BaseBranchRed with the same
// repo/head matching as stageSharedCIIncident: the checks red on both the
// PR head and its base branch, when every failing check is.
func stageBaseBranchRed(repo string, number int, head string) []string {
	e, ok := stageSharedCILookup(repo, number, head)
	if !ok || len(e.baseRed) == 0 {
		return nil
	}
	return append([]string(nil), e.baseRed...)
}

// stageForkGuard answers StageLabelPolicy.ForkGuard with the same repo/head
// matching as stageSharedCIIncident: the failing checks that refuse the PR
// only because it is from a fork, when every failing check does.
func stageForkGuard(repo string, number int, head string) []string {
	e, ok := stageSharedCILookup(repo, number, head)
	if !ok || len(e.forkGuard) == 0 {
		return nil
	}
	return append([]string(nil), e.forkGuard...)
}

func stageSharedCILookup(repo string, number int, head string) (stageSharedCIEntry, bool) {
	idx := stageSharedCIIndex.Load()
	if idx == nil {
		return stageSharedCIEntry{}, false
	}
	e, ok := (*idx)[stageSharedCIKey(repo, number)]
	if !ok {
		if i := strings.LastIndex(repo, "/"); i >= 0 {
			e, ok = (*idx)[stageSharedCIKey(repo[i+1:], number)]
		}
	}
	if !ok || (head != "" && e.head != "" && e.head != head) {
		return stageSharedCIEntry{}, false
	}
	return e, true
}
