package dashboard

import (
	"strings"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/review/pipeline"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// OwnershipReasonBotPRReviewChanges: another bot's PR whose current head the
// hive's review returned for changes, and whose fix the review dispatcher
// withheld because that bot owns the branch. Nobody in the hive will move it:
// the fixer never pushes to another bot's branch, and the bot only pushes
// when it regenerates the PR.
const OwnershipReasonBotPRReviewChanges = "bot_pr_review_changes"

// botPRReviewChangesReason is the decision's reason, written for the repo's
// maintainers rather than for one viewer.
const botPRReviewChangesReason = "Hive's review asked for changes on a bot's PR, and the hive will not push fixes to it. The repo's maintainers decide: close it, or wait for the bot's next version."

func init() {
	ownershipReasonVerbs[OwnershipReasonBotPRReviewChanges] = OwnershipVerbReview
}

// botPRReviewChangesOwnership is the your-turn decision for another bot's PR
// the hive's review returned for changes. It reads two structured records
// for the PR's current head: the review dispatcher's withheld fix with the
// other-bot exclusion (otherBotFixWithheld), which already accounts for
// review.bot_authors, and the head's newest review verdict, which must be
// changes_requested.
//
// A person's answer on the head or a hold release does not move it off the
// list. An instruction answer is never carried out on another bot's branch
// (the dispatcher withholds that fix too), a release applies only to a
// requires_human or reject hold, never to changes_requested, and the merge
// classifier's review gate reads neither: the head still waits for the
// repo's maintainers to close it or for the bot's next version.
func (e ownershipEnv) botPRReviewChangesOwnership(pr github.PullRequest) (Ownership, bool) {
	head := strings.TrimSpace(pr.HeadSHA)
	if head == "" || !e.otherBotFixWithheld(pr) {
		return Ownership{}, false
	}
	current, _ := pipeline.HeadVerdict(e.reviewVerdicts, github.ReviewQueueEntry{Repo: pr.Repo, Number: pr.Number, HeadSHA: head})
	if current == nil || !review.SameHeadSHA(current.HeadSHA, head) || current.Verdict != review.VerdictChangesRequested {
		return Ownership{}, false
	}
	return yourTurn(OwnershipReasonBotPRReviewChanges, botPRReviewChangesReason), true
}

// otherBotFixWithheld reports whether the review dispatch state holds a
// withheld fix for pr's current head because another bot owns the branch
// (review.FixDispatchExcludedOtherBotAuthor), matched by repo, number and
// head the way the review pipeline matches them, and nothing since says the
// hive owns the fix after all. A withheld record stays for its head until
// the head changes or the PR closes, so it does not count once the PR is
// adopted in place (`stage/074-adopted-by-hive`: the fixer pushes there) or
// while a fix for the head is pending in the dispatch state. A record
// written before the exclusion code was kept counts when it cites
// review.bot_authors, the setting only the other-bot refusal records.
func (e ownershipEnv) otherBotFixWithheld(pr github.PullRequest) bool {
	head := strings.TrimSpace(pr.HeadSHA)
	if head == "" || labelSet(pr.Labels)[strings.ToLower(stagelabels.AdoptedByHive.Label())] {
		return false
	}
	for _, f := range e.reviewSwarm.dispatch.Fixes {
		if f.Number == pr.Number && pipeline.SameRepo(f.Repo, pr.Repo) && review.SameHeadSHA(f.HeadSHA, head) {
			return false
		}
	}
	for _, w := range e.reviewSwarm.dispatch.Withheld {
		if w.Number != pr.Number || !pipeline.SameRepo(w.Repo, pr.Repo) || !review.SameHeadSHA(w.HeadSHA, head) {
			continue
		}
		switch strings.TrimSpace(w.Excluded) {
		case review.FixDispatchExcludedOtherBotAuthor:
			return true
		case "":
			if w.Setting == review.WithheldFixBotSetting {
				return true
			}
		}
	}
	return false
}
