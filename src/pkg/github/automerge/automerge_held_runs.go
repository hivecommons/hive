package automerge

// Held and never-started required checks on trusted-lane PRs.
//
// A required context can be missing from a PR head for two reasons that
// polling will never cure (hivecommons/hive#11552, #11553, #11877):
//
//  1. GitHub created the `pull_request` workflow runs but holds them for
//     approval (`conclusion=action_required`). This happens when the head was
//     pushed by `github-actions[bot]` — e.g. a GITHUB_TOKEN autofix commit onto
//     a Dependabot branch — under the repository's "require approval for
//     first-time contributors" policy. The run exists, the required check does
//     not, and nothing will start it until a person clicks "Approve and run".
//  2. GitHub created no `pull_request` runs for the head at all, so the
//     required check can never appear.
//  3. The head was pushed by `github-actions[bot]` onto a Dependabot branch
//     (notice-autofix.yml with GITHUB_TOKEN) and the missing context comes
//     from a `pull_request_target`-only workflow (`verify PR contents`). Such
//     pushes never trigger that workflow, so the context is absent forever
//     (#11923). Dependabot's own re-push restores normal triggers, so the
//     sweep asks for one `@dependabot rebase` per head instead of polling.
//  4. The PR itself was opened by `github-actions[bot]` with GITHUB_TOKEN, so
//     no `pull_request` workflow ever runs for it. The bot cannot be asked to
//     re-push, so the sweep re-authors the PR as the App once per head
//     (automerge_bot_takeover.go, #12003).
//
// The App's own PRs and configured trusted bots are the only lanes this file
// acts for (trySweepSelfAuthoredPR); human-authored lanes are untouched so a
// first-time contributor's held run is never nudged by automation. Hive posts
// `/ok-to-test`; the repository-local caller workflow approves only same-repo
// heads, never fork heads.

import (
	"context"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

const (
	// requiredCheckMissingPrefix is the commit-CI reason this file refines.
	requiredCheckMissingPrefix = "required-check-missing:"
	// workflowApprovalRequiredPrefix: the head's workflow runs exist but GitHub
	// is waiting for maintainer approval before starting them.
	workflowApprovalRequiredPrefix = "workflow-approval-required:"
	// requiredCheckNeverStartedPrefix: no `pull_request` run exists for a head
	// older than heldRunNeverStartedGrace; polling cannot make one appear.
	requiredCheckNeverStartedPrefix = "required-check-never-started:"
	// requiredCheckNeverRanPrefix: a trusted-bot head pushed by
	// github-actions[bot] is missing a context no held run will produce; the
	// sweep nudged the bot to rebase (#11923).
	requiredCheckNeverRanPrefix = "required-check-never-ran:"
	// requiredCheckSkippedByDesignPrefix logs a required context counted as
	// satisfied because its workflow concluded `skipped` on a trusted bot's head.
	requiredCheckSkippedByDesignPrefix = "required-check-skipped-by-design:"
	workflowRunConclusionSkipped       = "skipped"
	// githubActionsBotLogin authors commits pushed with a workflow GITHUB_TOKEN.
	githubActionsBotLogin = "github-actions[bot]"
	// dependabotLogin is the only trusted bot that understands the rebase nudge.
	dependabotLogin = "dependabot[bot]"
	// dependabotRebaseComment asks Dependabot to re-push the branch itself.
	dependabotRebaseComment = "@dependabot rebase"
	// workflowRunConclusionActionRequired is GitHub's conclusion for a run
	// waiting on a person's approval.
	workflowRunConclusionActionRequired   = "action_required"
	workflowApprovalWorkflowMissingPrefix = "workflow-approval-required:missing-workflow:"
	workflowApprovalCommand               = "/ok-to-test"
	workflowApprovalGrace                 = 10 * time.Minute
	// heldRunNeverStartedGrace bounds how long a head with zero workflow runs
	// is still treated as "GitHub has not scheduled them yet".
	heldRunNeverStartedGrace = 30 * time.Minute
	workflowRunsPageSize     = 100
)

// resolveMissingRequiredCheck refines a `required-check-missing:<ctx>` skip
// for a trusted-lane PR. It returns the original reason when nothing is known,
// a workflow-approval-required reason when GitHub is waiting for approval and
// Hive has posted `/ok-to-test`, or a never-started/never-ran reason when
// polling cannot progress. It never approves a run with the App token.
func (c *Engine) resolveMissingRequiredCheck(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest, number int, headSHA, reason string, headPushedAt time.Time) string {
	if c == nil || c.gh == nil || !strings.HasPrefix(reason, requiredCheckMissingPrefix) {
		return reason
	}
	missing := strings.TrimPrefix(reason, requiredCheckMissingPrefix)
	if c.isForkPR(pr) {
		return reason
	}
	runs, _, err := c.gh.Actions.ListRepositoryWorkflowRuns(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, &gh.ListWorkflowRunsOptions{
		HeadSHA:     headSHA,
		Event:       "pull_request",
		ListOptions: gh.ListOptions{PerPage: workflowRunsPageSize},
	})
	if err != nil || runs == nil {
		c.warn("automerge could not list workflow runs for head", "repo", displayRepo, "pr", number, "sha", headSHA, "error", err)
		return reason
	}
	held := heldSameRepoWorkflowRuns(runs.WorkflowRuns, owner+"/"+repo)
	if len(held) == 0 {
		if c.rebaseNudged(owner, repo, number, headSHA) || c.botTakeoverAttempted(owner, repo, number, headSHA) {
			return requiredCheckNeverRanPrefix + missing
		}
		if headOlderThan(c.now, headPushedAt, heldRunNeverStartedGrace) && c.isAutofixPushedDependabotHead(ctx, displayRepo, owner, repo, pr, number, headSHA) {
			c.nudgeDependabotRebase(ctx, displayRepo, owner, repo, number, headSHA, missing)
			return requiredCheckNeverRanPrefix + missing
		}
		if c.botTakeoverEligible(pr, len(runs.WorkflowRuns), headPushedAt) {
			return c.takeOverBotPR(ctx, displayRepo, owner, repo, pr, number, headSHA, missing)
		}
		if len(runs.WorkflowRuns) == 0 && headOlderThan(c.now, headPushedAt, heldRunNeverStartedGrace) {
			return requiredCheckNeverStartedPrefix + missing
		}
		return reason
	}
	if c.sentinelLabelPresent(labelNames(pr.Labels)) {
		return "sentinel"
	}
	return c.workflowApprovalAttention(ctx, displayRepo, owner, repo, number, headSHA, missing, len(held))
}

// detectActionRequiredWorkflowRuns names same-repo workflow runs GitHub is
// holding for approval on trusted lanes even when the required context already
// exists and commitGreen reported `required-check-pending:` rather than
// `required-check-missing:`. It returns an empty string when this shape does not
// apply.
func (c *Engine) detectActionRequiredWorkflowRuns(ctx context.Context, displayRepo, owner, repo, lane string, pr *gh.PullRequest, number int, headSHA, reason string) string {
	if c == nil || c.gh == nil || c.isForkPR(pr) {
		return ""
	}
	labels := labelNames(pr.Labels)
	if c.sentinelLabelPresent(labels) {
		return ""
	}
	if lane != "self-authored" && lane != "trusted-bot" && !trustedRegistryClanker(labels) {
		return ""
	}
	if !strings.HasPrefix(reason, requiredCheckPendingPrefix) && reason != "check-pending" && reason != "ci-unverified" {
		return ""
	}
	return c.workflowApprovalRequiredReason(ctx, displayRepo, owner, repo, number, headSHA, strings.TrimPrefix(reason, requiredCheckPendingPrefix))
}

const requiredCheckPendingPrefix = "required-check-pending:"

func (c *Engine) workflowApprovalRequiredReason(ctx context.Context, displayRepo, owner, repo string, number int, headSHA, contextName string) string {
	runs, _, err := c.gh.Actions.ListRepositoryWorkflowRuns(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, &gh.ListWorkflowRunsOptions{
		HeadSHA:     headSHA,
		Event:       "pull_request",
		ListOptions: gh.ListOptions{PerPage: workflowRunsPageSize},
	})
	if err != nil || runs == nil {
		c.warn("automerge could not list workflow runs for approval detection", "repo", displayRepo, "pr", number, "sha", headSHA, "error", err)
		return ""
	}
	held := heldSameRepoWorkflowRuns(runs.WorkflowRuns, owner+"/"+repo)
	if len(held) == 0 {
		return ""
	}
	if contextName == "" {
		contextName = held[0].GetName()
	}
	return c.workflowApprovalAttention(ctx, displayRepo, owner, repo, number, headSHA, contextName, len(held))
}

func (c *Engine) workflowApprovalAttention(ctx context.Context, displayRepo, owner, repo string, number int, headSHA, contextName string, held int) string {
	firstSeen, first := c.markWorkflowApprovalCommandSeen(owner, repo, number, headSHA)
	if first {
		c.postWorkflowApprovalCommand(ctx, displayRepo, owner, repo, number, headSHA)
	}
	if !firstSeen.IsZero() && headOlderThan(c.now, firstSeen, workflowApprovalGrace) {
		c.warn("automerge workflow approval still required after ok-to-test grace; install the hive ok-to-test workflow caller",
			"repo", displayRepo, "pr", number, "sha", headSHA, "held", held, "context", contextName, "command", workflowApprovalCommand)
		return workflowApprovalWorkflowMissingPrefix + contextName
	}
	c.info("automerge workflow approval required; posted ok-to-test command if needed", "repo", displayRepo, "pr", number, "sha", headSHA, "held", held, "context", contextName, "command", workflowApprovalCommand)
	return workflowApprovalRequiredPrefix + contextName
}

func (c *Engine) workflowApprovalCommandKey(owner, repo string, number int, headSHA string) string {
	return "workflow-ok-to-test:" + c.evaluatedHeadKey(owner, repo, number) + "@" + strings.ToLower(headSHA)
}

func (c *Engine) markWorkflowApprovalCommandSeen(owner, repo string, number int, headSHA string) (time.Time, bool) {
	key := c.workflowApprovalCommandKey(owner, repo, number, headSHA)
	now := time.Now()
	if c != nil && c.now != nil {
		now = c.now()
	}
	c.workflowApprovalCommandsMu.Lock()
	defer c.workflowApprovalCommandsMu.Unlock()
	if c.workflowApprovalCommands == nil {
		c.workflowApprovalCommands = make(map[string]time.Time)
	}
	if first, ok := c.workflowApprovalCommands[key]; ok {
		return first, false
	}
	c.workflowApprovalCommands[key] = now
	return now, true
}

func (c *Engine) postWorkflowApprovalCommand(ctx context.Context, displayRepo, owner, repo string, number int, headSHA string) {
	_, err := effects.Execute(ctx, c.mutation, effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindIssueComment,
		Target: strconv.Itoa(number),
		Actor:  "automerge",
		Inputs: map[string]string{"head_sha": headSHA, "body": effects.StableDigest(workflowApprovalCommand)},
	}, func(ctx context.Context) (effects.Result, error) {
		comment, _, apiErr := c.gh.Issues.CreateComment(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, number, &gh.IssueComment{Body: gh.Ptr(workflowApprovalCommand)})
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		c.noteOwnStageComment(owner, repo, number)
		return effects.Result{Provenance: comment.GetHTMLURL()}, nil
	})
	if err != nil {
		c.warn("automerge failed to post ok-to-test workflow approval command", "repo", displayRepo, "pr", number, "sha", headSHA, "command", workflowApprovalCommand, "error", err)
		return
	}
	c.info("automerge posted ok-to-test workflow approval command", "repo", displayRepo, "pr", number, "sha", headSHA, "command", workflowApprovalCommand)
}

// detectTrustedRegistryClankerWorkflowApprovalRequired surfaces held runs for
// the contributor registry lane without admitting the PR to the self-authored
// merge path. It is intentionally narrow: same-repo head, author/clanker +
// role/trusted labels, no sentinel label.
func (c *Engine) detectTrustedRegistryClankerWorkflowApprovalRequired(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest) string {
	if c == nil || pr == nil || c.isForkPR(pr) {
		return ""
	}
	labels := labelNames(pr.Labels)
	if !trustedRegistryClanker(labels) {
		return ""
	}
	if c.sentinelLabelPresent(labels) {
		return ""
	}
	sha := ""
	if pr.GetHead() != nil {
		sha = pr.GetHead().GetSHA()
	}
	if sha == "" {
		return ""
	}
	return c.workflowApprovalRequiredReason(ctx, displayRepo, owner, repo, pr.GetNumber(), sha, "workflow-approval")
}

// recheckSkippedByDesign re-evaluates a `required-check-missing:` head for a
// trusted-lane bot author, treating required contexts whose same-repo
// workflow run concluded `skipped` as satisfied (#12004). A context with no
// run at all ("never scheduled") stays missing. It returns the original
// verdict unchanged when the shape does not apply.
func (c *Engine) recheckSkippedByDesign(ctx context.Context, displayRepo, owner, repo, branch string, pr *gh.PullRequest, number int, headSHA string, headPushedAt time.Time, reason string) (bool, string, error) {
	if c == nil || c.gh == nil || !strings.HasPrefix(reason, requiredCheckMissingPrefix) || c.isForkPR(pr) {
		return false, reason, nil
	}
	if lane, ok := c.sweepLaneForAuthor(hgithub.SafeGetLogin(pr.GetUser())); !ok || lane != "trusted-bot" {
		return false, reason, nil
	}
	runs, _, err := c.gh.Actions.ListRepositoryWorkflowRuns(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, &gh.ListWorkflowRunsOptions{
		HeadSHA:     headSHA,
		Event:       "pull_request",
		ListOptions: gh.ListOptions{PerPage: workflowRunsPageSize},
	})
	if err != nil || runs == nil {
		return false, reason, nil
	}
	skipped := skippedWorkflowRunNames(runs.WorkflowRuns, owner+"/"+repo)
	if len(skipped) == 0 {
		return false, reason, nil
	}
	green, newReason, err := c.commitGreenForPRSkipping(ctx, owner, repo, branch, headSHA, number, headPushedAt, expectedCheckCacheFromContext(ctx), skipped)
	if err != nil || !green {
		if err == nil && strings.HasPrefix(newReason, requiredCheckMissingPrefix) {
			return false, reason, nil
		}
		return green, newReason, err
	}
	c.info("automerge required check skipped by design", "repo", displayRepo, "pr", number, "sha", headSHA, "reason", requiredCheckSkippedByDesignPrefix+strings.TrimPrefix(reason, requiredCheckMissingPrefix))
	return true, "", nil
}

// skippedWorkflowRunNames returns the set of workflow
// names whose same-repo run concluded `skipped`.
func skippedWorkflowRunNames(runs []*gh.WorkflowRun, fullName string) map[string]bool {
	skipped := make(map[string]bool)
	for _, run := range runs {
		if run == nil || !strings.EqualFold(run.GetConclusion(), workflowRunConclusionSkipped) {
			continue
		}
		if run.GetHeadRepository() == nil || !strings.EqualFold(run.GetHeadRepository().GetFullName(), fullName) {
			continue
		}
		skipped[run.GetName()] = true
	}
	return skipped
}

// heldSameRepoWorkflowRuns returns the runs waiting on approval whose head
// repository is fullName. A run without a head repository is not same-repo evidence.
func heldSameRepoWorkflowRuns(runs []*gh.WorkflowRun, fullName string) []*gh.WorkflowRun {
	var held []*gh.WorkflowRun
	for _, run := range runs {
		if run == nil || !strings.EqualFold(run.GetConclusion(), workflowRunConclusionActionRequired) {
			continue
		}
		if run.GetHeadRepository() == nil || !strings.EqualFold(run.GetHeadRepository().GetFullName(), fullName) {
			continue
		}
		held = append(held, run)
	}
	return held
}

func headOlderThan(now func() time.Time, headPushedAt time.Time, grace time.Duration) bool {
	if headPushedAt.IsZero() {
		return false
	}
	if now == nil {
		now = time.Now
	}
	return now().Sub(headPushedAt) > grace
}

// isTerminalRequiredCheckHold reports whether reason is a hold polling cannot
// cure (held for approval, runs never created, or never-ran after a nudge).
func isTerminalRequiredCheckHold(reason string) bool {
	return strings.HasPrefix(reason, workflowApprovalRequiredPrefix) || strings.HasPrefix(reason, requiredCheckNeverStartedPrefix) || strings.HasPrefix(reason, requiredCheckNeverRanPrefix)
}

// isAutofixPushedDependabotHead reports whether pr is a trusted-lane
// Dependabot PR whose head commit was authored by github-actions[bot] — the
// shape a GITHUB_TOKEN autofix push leaves behind (#11923). Lookup failures
// report false so the sweep keeps its existing behavior.
func (c *Engine) isAutofixPushedDependabotHead(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest, number int, headSHA string) bool {
	author := hgithub.SafeGetLogin(pr.GetUser())
	if !strings.EqualFold(author, dependabotLogin) {
		return false
	}
	if lane, ok := c.sweepLaneForAuthor(author); !ok || lane != "trusted-bot" {
		return false
	}
	commit, _, err := c.gh.Repositories.GetCommit(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, headSHA, nil)
	if err != nil || commit == nil {
		c.warn("automerge could not fetch head commit", "repo", displayRepo, "pr", number, "sha", headSHA, "error", err)
		return false
	}
	if strings.EqualFold(hgithub.SafeGetLogin(commit.GetAuthor()), githubActionsBotLogin) {
		return true
	}
	return strings.EqualFold(commit.GetCommit().GetAuthor().GetName(), githubActionsBotLogin)
}

// nudgeDependabotRebase logs one WARN naming the missing context and posts
// `@dependabot rebase` once per PR head. A failed post is not retried for the
// same head; the hold reason still surfaces in the sweep log.
func (c *Engine) nudgeDependabotRebase(ctx context.Context, displayRepo, owner, repo string, number int, headSHA, missing string) {
	if !c.markRebaseNudged(owner, repo, number, headSHA) {
		return
	}
	c.warn("automerge required check never ran on autofix-pushed Dependabot head; asking Dependabot to rebase",
		"repo", displayRepo, "pr", number, "sha", headSHA, "context", missing)
	_, err := effects.Execute(ctx, c.mutation, effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindIssueComment,
		Target: strconv.Itoa(number),
		Actor:  "automerge",
		Inputs: map[string]string{"head_sha": headSHA, "body": effects.StableDigest(dependabotRebaseComment)},
	}, func(ctx context.Context) (effects.Result, error) {
		comment, _, apiErr := c.gh.Issues.CreateComment(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, number, &gh.IssueComment{Body: gh.Ptr(dependabotRebaseComment)})
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		c.noteOwnStageComment(owner, repo, number)
		return effects.Result{Provenance: comment.GetHTMLURL()}, nil
	})
	if err != nil {
		c.warn("automerge failed to post Dependabot rebase nudge", "repo", displayRepo, "pr", number, "sha", headSHA, "error", err)
	}
}

// rebaseNudged reports whether the nudge was already attempted for this PR
// head, so later ticks skip the head-commit lookup.
func (c *Engine) rebaseNudged(owner, repo string, number int, headSHA string) bool {
	c.heldRunApprovalsMu.Lock()
	defer c.heldRunApprovalsMu.Unlock()
	return c.heldRunApprovals[rebaseNudgeKey(c.evaluatedHeadKey(owner, repo, number), headSHA)]
}

func rebaseNudgeKey(prKey, headSHA string) string {
	return "rebase-nudge:" + prKey + "@" + strings.ToLower(headSHA)
}

// markRebaseNudged records that the rebase nudge was attempted for this PR
// head and reports whether this is the first attempt.
func (c *Engine) markRebaseNudged(owner, repo string, number int, headSHA string) bool {
	key := rebaseNudgeKey(c.evaluatedHeadKey(owner, repo, number), headSHA)
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

// markTerminalHoldLogged records that the hold was reported for this PR head
// and reports whether this is the first report.
func (c *Engine) markTerminalHoldLogged(owner, repo string, number int, headSHA, reason string) bool {
	key := c.evaluatedHeadKey(owner, repo, number) + "@" + strings.ToLower(headSHA) + "@" + reason
	c.heldRunApprovalsMu.Lock()
	defer c.heldRunApprovalsMu.Unlock()
	if c.terminalHoldsLogged == nil {
		c.terminalHoldsLogged = make(map[string]bool)
	}
	if c.terminalHoldsLogged[key] {
		return false
	}
	c.terminalHoldsLogged[key] = true
	return true
}
