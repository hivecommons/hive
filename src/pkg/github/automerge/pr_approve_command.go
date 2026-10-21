package automerge

import (
	"context"
	"strings"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/intent"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// prApproveReleaseHint names what a trusted merger's `/hive approve` does on
// a pull request: the merge lanes read it, exactly like a GitHub Approve
// review on the head, as the release of a `requires_human` verdict
// (FindReviewHoldRelease, hivecommons/hive#13087).
const prApproveReleaseHint = "a trusted merger's `/hive approve` posted after the swarm's `requires_human` verdict releases that hold on the current head"

// prApproveNeedsHumanNote says whether a `needs-human` label still blocks the
// PR: the verdict release does not touch it (hivecommons/hive#13309).
func prApproveNeedsHumanNote(labels []string) string {
	if !hasLabelFold(labels, "needs-human") {
		return "No `needs-human` label blocks this PR."
	}
	return "`needs-human` still blocks this PR: on a hive-authored PR parked at `" + stagelabels.NeedsDecision.Label() + "` the un-park sweep lifts it when Hive added it, and a `needs-human` a person added stays until a person removes it."
}

// PRApproveReply describes what a `/hive approve` reply on a pull request
// means to the existing merge lanes (hivecommons/hive#13087). It is
// deliberately read-only: acknowledging a command is neither a merge
// authorization nor a label change. The lane still evaluates every gate on
// its next pass.
func (c *Engine) PRApproveReply(ctx context.Context, displayRepo string, number int, comment *gh.IssueComment) (string, error) {
	if !c.ready() {
		return "", hgithub.ErrNoGitHubClient
	}
	owner, repo := c.transport.SplitRepo(displayRepo)
	pr, _, err := c.gh.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return "", err
	}
	if pr.GetState() != "open" {
		return "`/hive approve` was not applied: this PR is no longer open.", nil
	}
	author := hgithub.SafeGetLogin(pr.GetUser())
	actor := hgithub.SafeGetLogin(comment.GetUser())
	head := strings.TrimSpace(pr.GetHead().GetSHA())
	_, self := c.sweepLaneForAuthor(author)
	trustedPolicy := c.currentTrustedAuthorPolicy()
	trustedAuthor := trustedPolicy.repoAllowed(displayRepo) && c.trustedAuthorDecision(author, trustedPolicy.RequireRole).Allowed
	contributor := !self && !trustedAuthor && c.currentContributorLanePolicy().repoAllowed(displayRepo)
	if !c.isPersonLogin(comment.GetUser()) {
		return "`/hive approve` was not applied: a bot cannot release a person's review hold.", nil
	}
	// Every lane releases a requires_human hold only on a trusted merger's
	// Approve review (mergeReviewGate), so an acknowledgement to anyone else
	// would be ineffective on any of them.
	if contributor || self || trustedAuthor {
		if trusted, configured := c.isTrustedMerger(actor); !configured || !trusted {
			if strings.EqualFold(actor, author) {
				return "`/hive approve` was not applied: a contributor cannot release their own PR unless they are a trusted merger. Ask the owner or another trusted merger to submit a GitHub **Approve** review for the current head.", nil
			}
			return "`/hive approve` was not applied: you are not a trusted merger for this hive. Ask the owner or a trusted merger to submit a GitHub **Approve** review for the current head.", nil
		}
	}
	if !c.repoAutoMergeAllowed(displayRepo) || (!self && !trustedAuthor && !contributor) {
		return "`/hive approve` cannot enable an automerge lane for this PR. A maintainer must merge it manually after checking the remaining requirements.", nil
	}
	if contributor && !verdictNeedsHuman(stageDetail{Verdict: c.contributorLaneVerdictFor(owner+"/"+repo, number, head), VerdictKnown: true}) {
		return "`/hive approve` cannot override this contributor-lane verdict: only a recorded `requires_human` decision can be released, and only by a trusted merger's GitHub **Approve** review. Resolve the review findings or ask a maintainer to review and merge manually.", nil
	}
	// Read paths only for a command that could matter to a hold. These are
	// the same manual-only rules as the lanes, without their label/comment
	// writes.
	patterns := c.currentHumanMergePaths(owner + "/" + repo)
	if contributor || len(patterns) > 0 {
		files, err := ListChangedFiles(ctx, c.gh, owner, repo, pr)
		if err != nil {
			return "", err
		}
		if len(intent.HumanMergePathMatches(files, patterns)) > 0 || (contributor && contributorLaneDiffReason(files) != "") {
			return "`/hive approve` acknowledged, but this PR changes files that its lane requires a person to merge manually. The command cannot override that rule; a maintainer must review and merge it.", nil
		}
	}
	return "`/hive approve` acknowledged: " + prApproveReleaseHint + " (`" + stagelabels.ShortSHA(head) + "`) on the lane's next pass, exactly as a GitHub **Approve** review for that head does. " + prApproveNeedsHumanNote(labelNames(pr.Labels)) + " Author admission, hold labels, CI, soak and all other merge gates still apply. This reply does not mean the PR has merged.", nil
}
