package automerge

import (
	"strings"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

// stageReasonSharedCIIncident prefixes the recorded reason when a red PR is
// deferred to an open [shared-ci] incident (`<!-- hive-shared-ci-<n> -->`,
// hivecommons/hive#10528). The base or a shared required check is what is
// red, so the author has nothing to fix: the PR stays out of 070/072 and gets
// no contributor nudge until the incident closes (hivecommons/hive#12606).
const stageReasonSharedCIIncident = prstage.StageReasonSharedCIIncident

// stageReasonBaseBranchRed prefixes the recorded reason when every failing
// check on a red PR head is also red on the base branch's tip and the PR
// touches no CI-owning path (hgithub.PullRequest.AllFailuresBaseRed). The
// checks follow, comma-separated. The base branch is what is red, so the
// author has nothing to fix: the PR stays out of 070/071/072, gets no
// contributor nudge and no fix kick, and is handed back as usual once the
// base recovers and the head is still red.
const stageReasonBaseBranchRed = prstage.StageReasonBaseBranchRed

// autoMergeReasonBehindBudgetExhausted is the self-authored sweep's reason
// for a `behind` PR it did not update only because this tick's branch-update
// budget (selfAuthoredSweepMaxBranchUpdates) was spent. The next tick retries
// update-branch; nothing is owed by the author (hivecommons/hive#12607).
const autoMergeReasonBehindBudgetExhausted = prstage.AutoMergeReasonBehindBudgetExhausted

// stageSharedCIIncident asks the policy which open shared-CI incident, if
// any, the PR's head is deferred to.
func stageSharedCIIncident(policy StageLabelPolicy, owner, repo string, number int, head string) int {
	if policy.SharedCIIncident == nil || number <= 0 {
		return 0
	}
	return policy.SharedCIIncident(owner+"/"+repo, number, head)
}

// stageBaseBranchRed asks the policy which failing checks on the PR's head
// are the base branch's; empty unless all of them are.
func stageBaseBranchRed(policy StageLabelPolicy, owner, repo string, number int, head string) []string {
	if policy.BaseBranchRed == nil || number <= 0 {
		return nil
	}
	return policy.BaseBranchRed(owner+"/"+repo, number, head)
}

func baseBranchRedReason(checks []string) string { return stageengine.BaseBranchRedReason(checks) }

// stageReasonWaitsOnHive reports reasons that leave the PR waiting on the
// hive rather than a person (no stall): a shared-CI incident being repaired,
// a red base branch, and a branch update deferred to the next sweep tick.
func stageReasonWaitsOnHive(reason string) bool {
	return reason == autoMergeReasonBehindBudgetExhausted || strings.HasPrefix(reason, stageReasonSharedCIIncident) ||
		strings.HasPrefix(reason, stageReasonBaseBranchRed)
}
