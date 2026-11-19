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
	// heldRuns names the workflow runs GitHub holds for approval on the head
	// that the hive did not approve (PullRequest.HeldWorkflowRunIDs).
	heldRuns []int64
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
				e.heldRuns = pr.HeldWorkflowRunIDs()
				if e.incident > 0 || len(e.baseRed) > 0 || len(e.forkGuard) > 0 || len(e.heldRuns) > 0 {
					idx[stageSharedCIKey(pr.Repo, pr.Number)] = e
				}
			}
		}
	}
	stageSharedCIIndex.Store(&idx)
}

// stageBaseRedMemory holds the github client's remembered base-branch red
// checks (github.Client.BaseRedSnapshot), keyed by
// github.BaseRedRememberedKey, as of the last CI-enriched enumeration. The
// stage reconciler reads it through StageLabelPolicy.BaseRedRemembered so a
// PR head the index has not read yet, red only on its base's checks, is not
// returned to its author before the next enumeration
// (projectbluefin/chairlift#811).
var stageBaseRedMemory atomic.Pointer[map[string][]string]

// publishStageBaseRedMemory replaces the base-red memory snapshot.
func publishStageBaseRedMemory(snap map[string][]string) {
	if snap == nil {
		snap = map[string][]string{}
	}
	stageBaseRedMemory.Store(&snap)
}

// stageBaseRedRemembered answers StageLabelPolicy.BaseRedRemembered: the
// checks remembered red on repo's ("owner/name") base branch baseRef.
func stageBaseRedRemembered(repo, baseRef string) []string {
	m := stageBaseRedMemory.Load()
	if m == nil {
		return nil
	}
	return append([]string(nil), (*m)[github.BaseRedRememberedKey(repo, baseRef)]...)
}

// stageBaseRedRememberedReady answers
// StageLabelPolicy.BaseRedRememberedReady: whether a CI-enriched
// enumeration has published the memory since the hive started. Until then
// the stage reconciler reads base tips itself (projectbluefin/common#1689).
func stageBaseRedRememberedReady() bool {
	return stageBaseRedMemory.Load() != nil
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

// stageHeldWorkflowRuns answers StageLabelPolicy.HeldWorkflowRuns with the
// same repo/head matching as stageSharedCIIncident: the workflow runs GitHub
// holds for approval on the head that the hive did not approve. A new head
// is not covered until the next enumeration probes it.
func stageHeldWorkflowRuns(repo string, number int, head string) []int64 {
	e, ok := stageSharedCILookup(repo, number, head)
	if !ok || len(e.heldRuns) == 0 {
		return nil
	}
	return append([]int64(nil), e.heldRuns...)
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
