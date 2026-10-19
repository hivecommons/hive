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
// first-time contributor's held run is never released by automation. Only
// runs whose head repository is the base repository are approved — a fork head
// is never released here even for a trusted login.

import (
	"context"
	"fmt"
	"net/http"
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
	// requiredCheckHeldPrefix: the head's workflow runs exist but are held for
	// approval and the sweep could not (or already did) release them.
	requiredCheckHeldPrefix = "required-check-held:"
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
	// approvedHeldWorkflowRunsReason is returned on the tick that released
	// held runs; the next tick re-evaluates the (now running) checks.
	approvedHeldWorkflowRunsReason = "approved-held-workflow-runs"
	// workflowRunConclusionActionRequired is GitHub's conclusion for a run
	// waiting on a person's approval.
	workflowRunConclusionActionRequired = "action_required"
	// heldRunNeverStartedGrace bounds how long a head with zero workflow runs
	// is still treated as "GitHub has not scheduled them yet".
	heldRunNeverStartedGrace = 30 * time.Minute
	workflowRunsPageSize     = 100
)

// resolveMissingRequiredCheck refines a `required-check-missing:<ctx>` skip
// for a trusted-lane PR. It returns the reason the caller should log: the
// original reason when nothing is known, a held/never-started reason when
// polling cannot progress, or approvedHeldWorkflowRunsReason after releasing
// held runs. It never returns an error: inability to inspect or approve runs
// degrades to the original reason so the sweep keeps its existing behavior.
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
	if !c.workflowApprovalTrustedBotsEnabled() {
		return requiredCheckHeldPrefix + missing
	}
	if !c.markHeldRunApprovalAttempted(owner, repo, number, headSHA) {
		// Already released (or refused) for this head; the runs are either
		// starting or need a person. Do not spend another approve call.
		return requiredCheckHeldPrefix + missing
	}
	approved := 0
	for _, run := range held {
		if err := c.approveWorkflowRunEffect(ctx, owner, repo, number, headSHA, run); err != nil {
			if isGitHubStatus(err, http.StatusForbidden) {
				c.warn("automerge cannot approve held workflow runs; the App needs Actions: write — approve them in the Actions tab or `gh run approve`",
					"repo", displayRepo, "pr", number, "sha", headSHA, "run", run.GetID(), "workflow", run.GetName(), "held", len(held))
				return requiredCheckHeldPrefix + missing
			}
			c.warn("automerge failed to approve held workflow run", "repo", displayRepo, "pr", number, "run", run.GetID(), "workflow", run.GetName(), "error", err)
			continue
		}
		approved++
	}
	if approved == 0 {
		return requiredCheckHeldPrefix + missing
	}
	c.info("automerge approved held workflow runs", "repo", displayRepo, "pr", number, "sha", headSHA, "approved", approved, "held", len(held), "missing", missing)
	return approvedHeldWorkflowRunsReason
}

// resolveActionRequiredWorkflowRuns releases same-repo workflow runs GitHub is
// holding for approval on trusted lanes even when the required context already
// exists and commitGreen reported `required-check-pending:` rather than
// `required-check-missing:`. It returns an empty string when this shape does not
// apply, or a skip reason for the current sweep tick after an approval/refusal.
func (c *Engine) resolveActionRequiredWorkflowRuns(ctx context.Context, displayRepo, owner, repo, lane string, pr *gh.PullRequest, number int, headSHA, reason string) string {
	if c == nil || c.gh == nil || !c.workflowApprovalTrustedBotsEnabled() || c.isForkPR(pr) {
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
	return c.approveActionRequiredWorkflowRuns(ctx, displayRepo, owner, repo, number, headSHA, strings.TrimPrefix(reason, requiredCheckPendingPrefix))
}

const requiredCheckPendingPrefix = "required-check-pending:"

func (c *Engine) approveActionRequiredWorkflowRuns(ctx context.Context, displayRepo, owner, repo string, number int, headSHA, missing string) string {
	runs, _, err := c.gh.Actions.ListRepositoryWorkflowRuns(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, &gh.ListWorkflowRunsOptions{
		HeadSHA:     headSHA,
		Event:       "pull_request",
		ListOptions: gh.ListOptions{PerPage: workflowRunsPageSize},
	})
	if err != nil || runs == nil {
		c.warn("automerge could not list workflow runs for approval", "repo", displayRepo, "pr", number, "sha", headSHA, "error", err)
		return ""
	}
	held := heldSameRepoWorkflowRuns(runs.WorkflowRuns, owner+"/"+repo)
	if len(held) == 0 {
		return ""
	}
	if missing == "" {
		missing = held[0].GetName()
	}
	if !c.markHeldRunApprovalAttempted(owner, repo, number, headSHA) {
		return requiredCheckHeldPrefix + missing
	}
	approved := 0
	for _, run := range held {
		if err := c.approveWorkflowRunEffect(ctx, owner, repo, number, headSHA, run); err != nil {
			if isGitHubStatus(err, http.StatusForbidden) {
				c.warn("automerge cannot approve action_required workflow runs; the App needs Actions: write — approve them in the Actions tab or `gh run approve`",
					"repo", displayRepo, "pr", number, "sha", headSHA, "run", run.GetID(), "workflow", run.GetName(), "held", len(held))
				return requiredCheckHeldPrefix + missing
			}
			c.warn("automerge failed to approve action_required workflow run", "repo", displayRepo, "pr", number, "run", run.GetID(), "workflow", run.GetName(), "error", err)
			continue
		}
		approved++
	}
	if approved == 0 {
		return requiredCheckHeldPrefix + missing
	}
	c.info("automerge approved action_required workflow runs", "repo", displayRepo, "pr", number, "sha", headSHA, "approved", approved, "held", len(held), "reason", missing)
	return approvedHeldWorkflowRunsReason
}

// approveTrustedRegistryClankerWorkflowRuns releases held runs for the
// contributor registry lane without admitting the PR to the self-authored merge
// path. It is intentionally narrow: same-repo head, author/clanker +
// role/trusted labels, no sentinel label, and trusted-bot approval enabled.
func (c *Engine) approveTrustedRegistryClankerWorkflowRuns(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest) string {
	if c == nil || !c.workflowApprovalTrustedBotsEnabled() || pr == nil || c.isForkPR(pr) {
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
	return c.approveActionRequiredWorkflowRuns(ctx, displayRepo, owner, repo, pr.GetNumber(), sha, "workflow-approval")
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
// repository is fullName. A run without a head repository is never released.
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

// markHeldRunApprovalAttempted records that approval was attempted for this
// PR head and reports whether this is the first attempt.
func (c *Engine) markHeldRunApprovalAttempted(owner, repo string, number int, headSHA string) bool {
	key := c.evaluatedHeadKey(owner, repo, number) + "@" + strings.ToLower(headSHA)
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

// isTerminalRequiredCheckHold reports whether reason is a hold polling cannot
// cure (held for approval, runs never created, or never-ran after a nudge).
func isTerminalRequiredCheckHold(reason string) bool {
	return strings.HasPrefix(reason, requiredCheckHeldPrefix) || strings.HasPrefix(reason, requiredCheckNeverStartedPrefix) || strings.HasPrefix(reason, requiredCheckNeverRanPrefix)
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

// approveWorkflowRunEffect routes POST /actions/runs/{id}/approve through the
// mutation boundary. go-github v72 has no binding for this endpoint.
func (c *Engine) approveWorkflowRunEffect(ctx context.Context, owner, repo string, number int, headSHA string, run *gh.WorkflowRun) error {
	_, err := effects.Execute(ctx, c.mutation, effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindWorkflowRunApprove,
		Target: strconv.FormatInt(run.GetID(), 10),
		Actor:  "automerge",
		Inputs: map[string]string{"pr": strconv.Itoa(number), "head_sha": headSHA, "workflow": run.GetName()},
	}, func(ctx context.Context) (effects.Result, error) {
		path := fmt.Sprintf("repos/%s/%s/actions/runs/%d/approve", owner, repo, run.GetID())
		req, err := c.gh.NewRequest(http.MethodPost, path, nil)
		if err != nil {
			return effects.Result{}, effects.NotApplied(err)
		}
		if _, err := c.gh.Do(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), req, nil); err != nil {
			return effects.Result{}, effects.NotApplied(err)
		}
		return effects.Result{Provenance: path}, nil
	})
	return err
}
