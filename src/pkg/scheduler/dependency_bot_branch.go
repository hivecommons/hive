package scheduler

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/rebasemerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Dependency-bot guard for the PR lists.
//
// Every API path in the hive leaves a dependency bot's branch to the bot
// (github.DependencyBotBranch): the bot stops rebasing a branch once another
// identity has committed to it, so a single merge of the base strands the PR
// behind its base for good. The scanner could still do it with plain git: its
// shared-CI triage says MERGE_BASE for a red PR that is behind, and the work
// list carried nothing that said the branch was not the hive's. Once the busy
// window passed, a Renovate branch read as an ordinary push target, and one
// kick merged main into two of them (projectbluefin/utah#1471: Renovate then
// "will not automatically rebase this PR, because it does not recognize the
// last commit author").
//
// So a dependency-bot row is never a push target. ${PR_LIST} marks it inline,
// like a fork, and both repair queues (FIX-BEFORE-NEW and CI_FAILING) list it
// under its own comment-only heading instead of the push list, like a busy
// branch, whatever the age of its head. App-authored rows and in-place
// adoptions are never checked, as in the busy guard: the first are the hive's
// own lane branches, the second are owned by the hive while the adoption
// holds. An operator's armed "rebase, then merge as maintainer" counts as an
// adoption here, as it does in merge eligibility: the operator asked for the
// hive's rebase, and the merge follows it at once, so nothing is stranded.

// dependencyBotBranchRow reports a row whose head branch a dependency bot
// keeps current itself and the hive must not push to.
func dependencyBotBranchRow(headRef, author string, appAuthored, adopted bool) bool {
	return !appAuthored && !adopted && github.DependencyBotBranch(headRef, author)
}

// dependencyBotMarker is the inline ${PR_LIST} marker. It is short on purpose:
// a PR list is often mostly dependency bumps and every kick has a size budget
// (hivecommons/hive#7368); the policy text says what the marker means.
const dependencyBotMarker = " [dependency bot: do not push]"

// rebaseMergeArmed reports a live rebase-then-merge follow-through on
// repo#number (owner/name), the store merge eligibility reads for the same
// decision. A var so tests can swap it.
var rebaseMergeArmed = func(repo string, number int) bool {
	return rebasemerge.Shared().Armed(repo, number)
}

// dependencyBotAnnotation is the ${PR_LIST} marker for a dependency bot's
// branch, the counterpart of forkAnnotation's "cannot push". Empty for a fork
// PR (already comment-only), for one adopted in place or armed for
// rebase-then-merge, and for every other branch. org qualifies a bare repo
// name for the armed lookup.
func dependencyBotAnnotation(pr github.PullRequest, org string) string {
	if pr.FromFork || !dependencyBotBranchRow(pr.HeadRef, pr.Author, PRAppAuthored(pr), prHasLabel(pr.Labels, stagelabels.AdoptedByHive.Label())) {
		return ""
	}
	if rebaseMergeArmed(config.QualifyRepo(org, pr.Repo), pr.Number) {
		return ""
	}
	return dependencyBotMarker
}

// dependencyBotOwner names the bot on a repair-queue row: the PR author, or
// the head ref when the row carries no author.
func dependencyBotOwner(headRef, author string) string {
	if who := strings.TrimSpace(author); who != "" {
		return who
	}
	return strings.TrimSpace(headRef)
}

// dependencyBotBranchInstruction is what an agent does with a red
// dependency-bot branch instead of pushing.
const dependencyBotBranchInstruction = "Do NOT push, merge the base into, rebase or force-push these branches: a dependency bot owns each one and rebases it itself, and it stops doing that for good once any other identity commits to it. " +
	"A branch that is behind its base needs nothing from you: when it stalls, the hive asks the bot to rebase it or hands the PR to a person. " +
	"Still triage a red check here with hive-baseline-check.sh, and when you find the failure transient or upstream, stamp its transient_marker or external_marker as your policy says: the hive's dependency-bot red-CI hand-off reads them. " +
	"For a failure the bump itself causes, report the failing check and the fix in ONE comment per PR: if you already left a note on that PR, edit it rather than adding another."

// writeDependencyBotRedPRs renders the FIX-BEFORE-NEW companion block for
// dependency-bot rows, under a heading that says the branch is not theirs to
// push to. Why on each row names the bot (dependencyBotOwner).
func writeDependencyBotRedPRs(b *strings.Builder, n int, row func(i int) busyRedPR) {
	writeNoPushRedPRs(b, fmt.Sprintf("## ⏸ DEPENDENCY-BOT BRANCHES - red PRs you must NOT push to (%d)", n), dependencyBotBranchInstruction, " - ", n, func(i int) (busyRedPR, string) {
		pr := row(i)
		return pr, "dependency bot: " + pr.Why + " - comment only, do not push"
	})
}
