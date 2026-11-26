package dashboard

import (
	"strings"

	"github.com/hivecommons/hive/pkg/github"
	stageengineprstage "github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// An adopted-in-place PR is the hive's own fixer's to repair only while the
// hive can push its branch (projectbluefin/utah#919,
// projectbluefin/testsuite#1037). The facts and the refusal rule live in
// prstage.AdoptedPushFacts, shared with the PR-decision relay
// (github.Client.hiveOwnsAdoptedBranch), so the two never disagree.

// OwnershipReasonAdoptedPushRefused: a person answered the review on an
// adopted PR's head, but the hive cannot push that branch, so the answer
// cannot be carried out by its fixer.
const OwnershipReasonAdoptedPushRefused = "adopted_push_refused"

func init() {
	ownershipReasonVerbs[OwnershipReasonAdoptedPushRefused] = OwnershipVerbDecide
}

// adoptedPushFacts is what says whether the hive can push an adopted PR's
// branch; Refusal names why not.
type adoptedPushFacts = stageengineprstage.AdoptedPushFacts

// recordedAdoptedPush fills what the stage store and the blocker ledger hold
// about pushing repo#number at head.
func recordedAdoptedPush(store *stagelabels.Store, repo string, number int, head string, f *adoptedPushFacts) {
	if store != nil {
		if rec, ok := store.Get(stagelabels.Key(repo, number)); ok {
			f.FromRecord(rec, head)
		}
	}
	if e, ok := blockerLedger().Get(repo, number); ok && e.Blocks() && e.Blocker.Kind.IsCapability() {
		f.Capability = e.Blocker.Kind.Short()
	}
}

// adoptedPushStore is the stage store the ownership pass reads; tests swap it.
var adoptedPushStore = stagelabels.Shared

// adoptedPushRefusalFor is the ownership pass's check on a listed PR. It
// applies only to a PR the hive took on at its head: on any other PR the
// author's own agent is the one fixing. The listing carries no file names,
// so a fork's workflow-file diff counts only once the adoption or the fixer
// recorded it.
// A short repo name is qualified with org, as the decision-answer lookup does.
func adoptedPushRefusalFor(org string) func(pr github.PullRequest) string {
	return func(pr github.PullRequest) string {
		repo := strings.TrimSpace(pr.Repo)
		if !strings.Contains(repo, "/") && strings.TrimSpace(org) != "" {
			repo = strings.TrimSpace(org) + "/" + repo
		}
		f := adoptedPushFacts{Fork: pr.FromFork, MaintainerCanModify: pr.MaintainerCanModify}
		recordedAdoptedPush(adoptedPushStore(), repo, pr.Number, strings.TrimSpace(pr.HeadSHA), &f)
		if !f.Adopted {
			return ""
		}
		return f.Refusal()
	}
}
