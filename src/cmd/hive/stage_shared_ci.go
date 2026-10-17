package main

import (
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/hivecommons/hive/pkg/github"
)

// stageSharedCIEntry is one red PR's shared-CI deferral as of the last
// CI-enriched enumeration.
type stageSharedCIEntry struct {
	head     string
	incident int
}

// stageSharedCIIndex holds, per "repo#number", the open [shared-ci] incident
// each red PR was deferred to (github.PullRequest.SharedCIIncident) in the
// latest CI-enriched actionable snapshot. The stage reconciler reads it
// through StageLabelPolicy.SharedCIIncident so a base/shared-CI red PR is not
// returned to its contributor or nudged (hivecommons/hive#12606).
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
				if pr.SharedCIIncident > 0 {
					idx[stageSharedCIKey(pr.Repo, pr.Number)] = stageSharedCIEntry{head: pr.HeadSHA, incident: pr.SharedCIIncident}
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
	idx := stageSharedCIIndex.Load()
	if idx == nil {
		return 0
	}
	e, ok := (*idx)[stageSharedCIKey(repo, number)]
	if !ok {
		if i := strings.LastIndex(repo, "/"); i >= 0 {
			e, ok = (*idx)[stageSharedCIKey(repo[i+1:], number)]
		}
	}
	if !ok || (head != "" && e.head != "" && e.head != head) {
		return 0
	}
	return e.incident
}
