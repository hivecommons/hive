package automerge

// Bot-PR takeover for github-actions[bot] PRs (#12003).
//
// A PR opened by `github-actions[bot]` with a workflow GITHUB_TOKEN never
// triggers `pull_request` workflows (GitHub's anti-recursion rule). When the
// base branch's required contexts come from those workflows the PR sits at
// `required-check-missing:<ctx>` forever, even with github-actions[bot] in
// auto_merge.trusted_bot_authors. Unlike Dependabot (#11923) the bot does not
// understand a comment, so the sweep re-authors the change as the App: one
// signed-off commit carrying the bot head's tree on top of the merge base,
// pushed to `hive/takeover-<n>`, opened as an App PR that supersedes the bot
// PR, after which the bot PR is closed. App-pushed refs trigger workflows
// normally, and the App PR then flows through the self-authored sweep.
//
// The path is deliberately narrow: same-repo github-actions[bot] PRs in the
// trusted-bot lane only, no `pull_request` run at all for the head, head
// older than both MinHeadAge and heldRunNeverStartedGrace, the
// auto_merge.bot_takeover.enabled switch on, and one attempt per bot head SHA
// whatever the outcome.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

const (
	// botTakeoverBranchPrefix names the App branch that re-authors bot PR <n>.
	botTakeoverBranchPrefix = "hive/takeover-"
	// botPRTakenOverReason is returned on the tick that opened the App PR.
	botPRTakenOverReason = "bot-pr-taken-over"
)

// botTakeoverEnabled reports the live auto_merge.bot_takeover.enabled switch.
// No hook means off so engines built without config never re-author PRs.
func (c *Engine) botTakeoverEnabled() bool {
	return c != nil && c.botTakeoverEnabledFn != nil && c.botTakeoverEnabledFn()
}

// botTakeoverMinAge is how old a bot head must be before the sweep concludes
// its `pull_request` runs are never coming: the longer of MinHeadAge and the
// never-started grace.
func (c *Engine) botTakeoverMinAge() time.Duration {
	if c.minHeadAge > heldRunNeverStartedGrace {
		return c.minHeadAge
	}
	return heldRunNeverStartedGrace
}

// botTakeoverEligible is the trigger predicate: a same-repo trusted-lane
// github-actions[bot] PR whose head has no `pull_request` workflow run at all
// and is older than botTakeoverMinAge.
func (c *Engine) botTakeoverEligible(pr *gh.PullRequest, pullRequestRuns int, headPushedAt time.Time) bool {
	if !c.botTakeoverEnabled() || pr == nil || pullRequestRuns != 0 || c.isForkPR(pr) {
		return false
	}
	if pr.GetBase() == nil || strings.TrimSpace(pr.GetBase().GetRef()) == "" {
		return false
	}
	author := hgithub.SafeGetLogin(pr.GetUser())
	if !strings.EqualFold(author, githubActionsBotLogin) {
		return false
	}
	if lane, ok := c.sweepLaneForAuthor(author); !ok || lane != "trusted-bot" {
		return false
	}
	return headOlderThan(c.now, headPushedAt, c.botTakeoverMinAge())
}

func botTakeoverKey(prKey, headSHA string) string {
	return "bot-takeover:" + prKey + "@" + strings.ToLower(headSHA)
}

// botTakeoverAttempted reports whether a takeover was already attempted for
// this bot PR head.
func (c *Engine) botTakeoverAttempted(owner, repo string, number int, headSHA string) bool {
	c.heldRunApprovalsMu.Lock()
	defer c.heldRunApprovalsMu.Unlock()
	return c.heldRunApprovals[botTakeoverKey(c.evaluatedHeadKey(owner, repo, number), headSHA)]
}

// markBotTakeoverAttempted records the takeover attempt for this bot PR head
// and reports whether this is the first attempt.
func (c *Engine) markBotTakeoverAttempted(owner, repo string, number int, headSHA string) bool {
	key := botTakeoverKey(c.evaluatedHeadKey(owner, repo, number), headSHA)
	c.heldRunApprovalsMu.Lock()
	defer c.heldRunApprovalsMu.Unlock()
	if c.heldRunApprovals == nil {
		c.heldRunApprovals = make(map[string]bool)
	}
	if c.heldRunApprovals[key] {
		return false
	}
	c.heldRunApprovals[key] = true
	return true
}

// takeOverBotPR re-authors bot PR number as the App and closes it. It returns
// botPRTakenOverReason once the App PR exists, else the never-ran hold; it is
// attempted once per head SHA and never returns an error.
func (c *Engine) takeOverBotPR(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest, number int, headSHA, missing string) string {
	held := requiredCheckNeverRanPrefix + missing
	if !c.markBotTakeoverAttempted(owner, repo, number, headSHA) {
		return held
	}
	appLogin := strings.TrimSpace(c.transport.AppBotLogin())
	if appLogin == "" {
		c.warn("automerge cannot take over github-actions[bot] PR: App login unknown", "repo", displayRepo, "pr", number, "sha", headSHA)
		return held
	}
	baseRef := pr.GetBase().GetRef()
	branch := botTakeoverBranchPrefix + strconv.Itoa(number)
	c.warn("automerge required check never ran on github-actions[bot] PR; re-authoring it as the App",
		"repo", displayRepo, "pr", number, "sha", headSHA, "context", missing, "branch", branch)

	restCtx := hgithub.WithRESTCaller(ctx, "hive:automerge_sweep")
	cmp, _, err := c.gh.Repositories.CompareCommits(restCtx, owner, repo, baseRef, headSHA, &gh.ListOptions{PerPage: 1})
	mergeBase := ""
	if err == nil && cmp != nil {
		mergeBase = cmp.GetMergeBaseCommit().GetSHA()
	}
	if mergeBase == "" {
		c.warn("automerge bot takeover could not resolve merge base", "repo", displayRepo, "pr", number, "base", baseRef, "sha", headSHA, "error", err)
		return held
	}
	headCommit, _, err := c.gh.Git.GetCommit(restCtx, owner, repo, headSHA)
	if err != nil || headCommit.GetTree().GetSHA() == "" {
		c.warn("automerge bot takeover could not read head tree", "repo", displayRepo, "pr", number, "sha", headSHA, "error", err)
		return held
	}
	message := botTakeoverCommitMessage(pr, number, headSHA, appLogin)
	_, err = effects.Execute(ctx, c.mutation, effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindBranchPush,
		Target: branch,
		Actor:  "automerge",
		Inputs: map[string]string{"pr": strconv.Itoa(number), "head_sha": headSHA, "merge_base": mergeBase},
	}, func(ctx context.Context) (effects.Result, error) {
		commit, _, apiErr := c.gh.Git.CreateCommit(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, &gh.Commit{
			Message: gh.Ptr(message),
			Tree:    &gh.Tree{SHA: gh.Ptr(headCommit.GetTree().GetSHA())},
			Parents: []*gh.Commit{{SHA: gh.Ptr(mergeBase)}},
		}, nil)
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		if _, _, apiErr = c.gh.Git.CreateRef(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, &gh.Reference{
			Ref:    gh.Ptr("refs/heads/" + branch),
			Object: &gh.GitObject{SHA: gh.Ptr(commit.GetSHA())},
		}); apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		return effects.Result{Provenance: commit.GetSHA()}, nil
	})
	if err != nil {
		c.warn("automerge bot takeover failed to push branch", "repo", displayRepo, "pr", number, "branch", branch, "error", err)
		return held
	}

	var created *gh.PullRequest
	_, err = effects.Execute(ctx, c.mutation, effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindPullRequestCreate,
		Target: branch,
		Actor:  "automerge",
		Inputs: map[string]string{"base": baseRef, "supersedes": strconv.Itoa(number), "head_sha": headSHA},
	}, func(ctx context.Context) (effects.Result, error) {
		var apiErr error
		created, _, apiErr = c.gh.PullRequests.Create(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, &gh.NewPullRequest{
			Title: gh.Ptr(pr.GetTitle()),
			Head:  gh.Ptr(branch),
			Base:  gh.Ptr(baseRef),
			Body:  gh.Ptr(botTakeoverPRBody(pr, number, missing)),
		})
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		return effects.Result{Provenance: created.GetHTMLURL()}, nil
	})
	if err != nil || created == nil {
		c.warn("automerge bot takeover failed to open App PR", "repo", displayRepo, "pr", number, "branch", branch, "error", err)
		return held
	}
	newNumber := created.GetNumber()
	c.info("automerge took over github-actions[bot] PR", "repo", displayRepo, "pr", number, "sha", headSHA, "context", missing, "superseded_by", newNumber)

	comment := fmt.Sprintf("Superseded by #%d: re-authored by the hive App so the required `pull_request` checks can run.", newNumber)
	_, err = effects.Execute(ctx, c.mutation, effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindIssueComment,
		Target: strconv.Itoa(number),
		Actor:  "automerge",
		Inputs: map[string]string{"head_sha": headSHA, "body": effects.StableDigest(comment)},
	}, func(ctx context.Context) (effects.Result, error) {
		posted, _, apiErr := c.gh.Issues.CreateComment(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, number, &gh.IssueComment{Body: gh.Ptr(comment)})
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		return effects.Result{Provenance: posted.GetHTMLURL()}, nil
	})
	if err != nil {
		c.warn("automerge bot takeover failed to comment on superseded PR", "repo", displayRepo, "pr", number, "superseded_by", newNumber, "error", err)
	}
	if _, _, err := c.gh.PullRequests.Edit(restCtx, owner, repo, number, &gh.PullRequest{State: gh.Ptr("closed")}); err != nil {
		c.warn("automerge bot takeover failed to close superseded PR", "repo", displayRepo, "pr", number, "superseded_by", newNumber, "error", err)
	}
	return botPRTakenOverReason
}

// botTakeoverCommitMessage is the single re-authored commit's message: the bot
// PR title, the provenance line, and the App's DCO sign-off.
func botTakeoverCommitMessage(pr *gh.PullRequest, number int, headSHA, appLogin string) string {
	title := strings.TrimSpace(pr.GetTitle())
	if title == "" {
		title = fmt.Sprintf("Re-author #%d", number)
	}
	return fmt.Sprintf("%s\n\nRe-authored from #%d (head %s) by the hive App so pull_request workflows run.\n\n%s",
		title, number, headSHA, hgithub.BotSignedOffByTrailer(appLogin))
}

// botTakeoverPRBody keeps the bot PR's body and appends the supersession note.
func botTakeoverPRBody(pr *gh.PullRequest, number int, missing string) string {
	note := fmt.Sprintf("Supersedes #%d. `%s` opened it with the workflow `GITHUB_TOKEN`, so its `pull_request` workflows never ran and the required check `%s` could not report. The hive App re-authored the same change so CI runs normally.",
		number, githubActionsBotLogin, missing)
	if body := strings.TrimSpace(pr.GetBody()); body != "" {
		return body + "\n\n---\n\n" + note
	}
	return note
}
