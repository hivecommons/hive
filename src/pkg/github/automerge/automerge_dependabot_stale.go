package automerge

// Stale Dependabot PRs (hivecommons/hive#18441).
//
// Dependabot only honours `@dependabot rebase` / `recreate` from users with
// push access, which the hive's GitHub App never is ("Sorry, only users with
// push access can use that command"), and it refuses `rebase` from anyone
// once another identity has pushed to the branch (the NOTICE autofix commit:
// "this PR has been edited by someone other than Dependabot"). Posting either
// command therefore cannot move the PR, and re-posting it every pass is noise.
// The hive never posts them. Instead the trusted-bot lane decides:
//
//   - behind (no conflicts): GitHub's update-branch API, which the App can
//     call. Dependabot, unlike Renovate, does not rebase a branch that is
//     merely behind, so leaving it to the bot strands it under an up-to-date
//     rule (hgithub.DependencyBotBranch still covers Renovate and the
//     merge-forward / merge-train paths).
//   - conflicting, only Dependabot's commits: leave it; Dependabot rebases
//     conflicting branches itself.
//   - conflicting, edited by others: Dependabot will never rebase it, and the
//     App cannot resolve conflicts, so the PR is closed once with a note.
//     Dependabot opens a fresh PR for the next release; a maintainer can
//     comment `@dependabot recreate` to restore this one now.

import (
	"context"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// dependabotStaleDecision is what the trusted-bot lane does with a stale
// Dependabot PR.
type dependabotStaleDecision int

const (
	// dependabotStaleNone: the PR is neither behind nor conflicting.
	dependabotStaleNone dependabotStaleDecision = iota
	// dependabotStaleUpdateBranch: behind only; merge the base in with the
	// update-branch API.
	dependabotStaleUpdateBranch
	// dependabotStaleLeaveToBot: conflicting, but Dependabot still owns every
	// commit and rebases the branch itself.
	dependabotStaleLeaveToBot
	// dependabotStaleClose: conflicting and edited by someone other than
	// Dependabot; nothing the App may do can rebase it.
	dependabotStaleClose
)

const (
	// dependabotStaleClosedReason is the lane reason on the tick that closed
	// a stale Dependabot PR.
	dependabotStaleClosedReason = "closed"
	// dependabotStaleCloseKey keys the once-per-head close attempt in
	// terminalHoldLogged.
	dependabotStaleCloseKey = "dependabot-stale-close"
	// dependabotStaleCloseMarker tags the hive's closing note.
	dependabotStaleCloseMarker = "<!-- hive-dependabot-stale-close -->"
	dependabotCommitsPageSize  = 100
)

// decideDependabotStale maps a Dependabot PR's mergeable_state and whether
// anyone other than Dependabot committed to the branch onto the lane's action.
func decideDependabotStale(mergeableState string, editedByOthers bool) dependabotStaleDecision {
	switch strings.ToLower(strings.TrimSpace(mergeableState)) {
	case "behind":
		return dependabotStaleUpdateBranch
	case "dirty", "conflicting":
		if editedByOthers {
			return dependabotStaleClose
		}
		return dependabotStaleLeaveToBot
	}
	return dependabotStaleNone
}

// isDependabotPR reports whether pr was opened by Dependabot on a branch the
// hive may update through the App (never a renovate/ head ref).
func isDependabotPR(pr *gh.PullRequest) bool {
	if pr == nil {
		return false
	}
	if strings.HasPrefix(strings.TrimSpace(pr.GetHead().GetRef()), "renovate/") {
		return false
	}
	return strings.EqualFold(hgithub.SafeGetLogin(pr.GetUser()), dependabotLogin)
}

// dependabotUpdateBranchAllowed reports whether a base-sync step may update a
// Dependabot branch with the App instead of leaving it to the bot: only the
// update-branch steps, never a merge-forward or merge-train rebase.
func dependabotUpdateBranchAllowed(pr *gh.PullRequest, step string) bool {
	return (step == "update-branch" || step == "current-base") && isDependabotPR(pr)
}

// isDependabotCommit reports whether a PR commit was authored by Dependabot.
func isDependabotCommit(commit *gh.RepositoryCommit) bool {
	if commit == nil {
		return false
	}
	if strings.EqualFold(hgithub.SafeGetLogin(commit.GetAuthor()), dependabotLogin) {
		return true
	}
	return strings.EqualFold(commit.GetCommit().GetAuthor().GetName(), dependabotLogin)
}

// dependabotEditedByOthers reports whether any commit on the PR was authored
// by someone other than Dependabot (an autofix push, an App update-branch
// merge, a human), which makes Dependabot refuse to rebase it.
func (c *Engine) dependabotEditedByOthers(ctx context.Context, owner, repo string, number int) (bool, error) {
	opts := &gh.ListOptions{PerPage: dependabotCommitsPageSize}
	for {
		commits, resp, err := c.gh.PullRequests.ListCommits(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, number, opts)
		if err != nil {
			return false, err
		}
		for _, commit := range commits {
			if !isDependabotCommit(commit) {
				return true, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return false, nil
		}
		opts.Page = resp.NextPage
	}
}

// resolveConflictingDependabotPR handles a conflicting Dependabot PR on the
// trusted-bot lane. It returns the lane reason and true when pr is a
// Dependabot PR; false leaves the caller's "conflicting" hold in place.
func (c *Engine) resolveConflictingDependabotPR(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest, number int, headSHA string) (string, bool) {
	if c == nil || c.gh == nil || !isDependabotPR(pr) {
		return "", false
	}
	edited, err := c.dependabotEditedByOthers(ctx, owner, repo, number)
	if err != nil {
		c.warn("automerge could not list Dependabot PR commits", "repo", displayRepo, "pr", number, "sha", headSHA, "error", err)
		return "conflicting", true
	}
	if decideDependabotStale(pr.GetMergeableState(), edited) != dependabotStaleClose {
		return "conflicting", true
	}
	if !c.markTerminalHoldLogged(owner, repo, number, headSHA, dependabotStaleCloseKey) {
		// Already attempted for this head; never repeat the note.
		return "conflicting", true
	}
	if c.closeStaleDependabotPR(ctx, displayRepo, owner, repo, number, headSHA) {
		return dependabotStaleClosedReason, true
	}
	return "conflicting", true
}

func dependabotStaleCloseNote() string {
	return dependabotStaleCloseMarker + "\n" +
		"Closing: this Dependabot PR conflicts with its base, and its branch has commits not authored by Dependabot, so Dependabot will not rebase it " +
		"(and it ignores `@dependabot` commands from the hive App). Dependabot opens a fresh PR for the next release; " +
		"a maintainer can comment `@dependabot recreate` to restore this one now."
}

// closeStaleDependabotPR posts the closing note and closes the PR. It reports
// whether the close went through. Both writes are solicited: the operator
// listed dependabot[bot] as a trusted bot, handing its PRs to this lane.
func (c *Engine) closeStaleDependabotPR(ctx context.Context, displayRepo, owner, repo string, number int, headSHA string) bool {
	restCtx := hgithub.WithRESTCaller(ctx, "hive:automerge_sweep")
	note := dependabotStaleCloseNote()
	_, err := effects.Execute(ctx, c.mutation, effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindIssueComment,
		Target: strconv.Itoa(number),
		Actor:  "automerge",
		Inputs: map[string]string{"head_sha": headSHA, "body": effects.StableDigest(note)},
	}, func(ctx context.Context) (effects.Result, error) {
		posted, _, apiErr := c.gh.Issues.CreateComment(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, number, &gh.IssueComment{Body: gh.Ptr(note)}) // writeback:solicited trusted-bot lane
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		c.noteOwnStageComment(owner, repo, number)
		return effects.Result{Provenance: posted.GetHTMLURL()}, nil
	})
	if err != nil {
		c.warn("automerge failed to post stale Dependabot close note", "repo", displayRepo, "pr", number, "sha", headSHA, "error", err)
	}
	if _, _, err := c.gh.PullRequests.Edit(restCtx, owner, repo, number, &gh.PullRequest{State: gh.Ptr("closed")}); err != nil { // writeback:solicited trusted-bot lane
		c.warn("automerge failed to close stale Dependabot PR", "repo", displayRepo, "pr", number, "sha", headSHA, "error", err)
		return false
	}
	c.info("automerge closed conflicting Dependabot PR edited by others; Dependabot opens a fresh one",
		"repo", displayRepo, "pr", number, "sha", headSHA)
	return true
}
