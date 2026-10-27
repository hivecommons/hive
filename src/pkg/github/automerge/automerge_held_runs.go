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
// `/ok-to-test`; the repository-local self-contained workflow approves only same-repo
// heads, never fork heads. The one human exception lives in
// automerge_contributor_held_runs.go (#14519): a contributor the operator
// placed in this hive's registry at a tier the contributor lane accepts for
// merging (require_tiers) gets their `action_required` runs approved once per
// head with the App token — fork head included — when the lane itself stops
// at `required-check-missing:<ctx>`. Claim-marker, auto-promotion and
// association admissions, sentinel/exclude labels and `.github/workflows/**`
// diffs stay a person's decision, and the toggle is
// auto_merge.contributor_lane.approve_held_runs_for_registry_trusted.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/oktotest"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

const (
	// requiredCheckMissingPrefix is the commit-CI reason this file refines.
	requiredCheckMissingPrefix = "required-check-missing:"
	// workflowApprovalRequiredPrefix: the head's workflow runs exist but GitHub
	// is waiting for maintainer approval before starting them.
	workflowApprovalRequiredPrefix = prstage.WorkflowApprovalRequiredPrefix
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
	// workflowApprovalUntrustedPrefix: the repository's merge bot (Prow) owns
	// `/ok-to-test` and only honours it from its trusted users, which a
	// GitHub App login never is. Posting again cannot succeed; a person must
	// issue the command (hivecommons/hive homebrew-hive#110).
	workflowApprovalUntrustedPrefix = "workflow-approval-required:untrusted-commenter:"
	workflowApprovalCommand         = "/ok-to-test"
	workflowApprovalGrace           = 10 * time.Minute
	// workflowApprovalCommandMarkerPrefix tags the hive's own command comment
	// with the head it was posted for, so a restarted hive (or another
	// replica) finds it instead of posting a second time for the same head.
	// It sits on its own line: Prow's trigger plugin and approve.sh both
	// match the command per line, so the marker does not hide it.
	workflowApprovalCommandMarkerPrefix = "<!-- hive-ok-to-test:"
	// workflowApprovalRejectedMarker stamps the one needs-human note posted
	// after the merge bot rejected the App's command.
	workflowApprovalRejectedMarker = "<!-- hive-ok-to-test-rejected -->"
	// prowOkToTestRejection is the reply Prow's trigger plugin posts when the
	// commenter is not one of its trusted users.
	prowOkToTestRejection = "Cannot trigger testing until a trusted user"
	// prowApprovalNotifier appears in every comment Prow's approve plugin
	// posts; its presence means `/ok-to-test` on this PR is Prow's command.
	prowApprovalNotifier = "[APPROVALNOTIFIER]"
	// workflowApprovalRecheck bounds how often one process re-reads a held
	// PR's comments for the merge bot's reply.
	workflowApprovalRecheck = 5 * time.Minute
	// okToTestWorkflowCheckTTL bounds how long a repository's hive-ok-to-test
	// workflow presence is remembered before being read again.
	okToTestWorkflowCheckTTL = time.Hour
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
	if c.workflowApprovalUntrustedKnown(owner, repo, number) {
		return workflowApprovalUntrustedPrefix + contextName
	}
	firstSeen, first := c.markWorkflowApprovalCommandSeen(owner, repo, number, headSHA)
	state, scanned := c.workflowApprovalCommentState(ctx, displayRepo, owner, repo, number, headSHA, first)
	if scanned && (state.rejected || state.prowManaged) && !c.okToTestWorkflowInstalled(ctx, displayRepo, owner, repo) {
		// Prow owns /ok-to-test here and the App is not one of its trusted
		// users: every further post gets the same rejection, so stop and
		// hand the command to a person once.
		c.markWorkflowApprovalUntrusted(owner, repo, number)
		if !state.rejectionNoted {
			c.postWorkflowApprovalUntrustedNote(ctx, displayRepo, owner, repo, number, headSHA, state.rejected)
		}
		c.warn("automerge workflow approval cannot be requested by the App; merge bot trusts only its own users",
			"repo", displayRepo, "pr", number, "sha", headSHA, "held", held, "context", contextName, "rejected", state.rejected, "prow", state.prowManaged)
		return workflowApprovalUntrustedPrefix + contextName
	}
	if first && !state.postedForHead {
		c.postWorkflowApprovalCommand(ctx, displayRepo, owner, repo, number, headSHA)
	}
	if !firstSeen.IsZero() && headOlderThan(c.now, firstSeen, workflowApprovalGrace) {
		c.warn("automerge workflow approval still required after ok-to-test grace; install the self-contained hive ok-to-test workflow",
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
	now := c.clock()
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

func (c *Engine) clock() time.Time {
	if c != nil && c.now != nil {
		return c.now()
	}
	return time.Now()
}

// workflowApprovalUntrustedKnown reports whether this process already
// learned that the merge bot refuses the App's command on this PR.
func (c *Engine) workflowApprovalUntrustedKnown(owner, repo string, number int) bool {
	c.workflowApprovalCommandsMu.Lock()
	defer c.workflowApprovalCommandsMu.Unlock()
	return c.workflowApprovalUntrusted[c.evaluatedHeadKey(owner, repo, number)]
}

func (c *Engine) markWorkflowApprovalUntrusted(owner, repo string, number int) {
	c.workflowApprovalCommandsMu.Lock()
	defer c.workflowApprovalCommandsMu.Unlock()
	if c.workflowApprovalUntrusted == nil {
		c.workflowApprovalUntrusted = make(map[string]bool)
	}
	c.workflowApprovalUntrusted[c.evaluatedHeadKey(owner, repo, number)] = true
}

// workflowApprovalScanDue reports whether the PR's comments should be read
// this tick: always for a head this process has not seen, otherwise at most
// once per workflowApprovalRecheck so the merge bot's reply (posted seconds
// after the command) is noticed on the next sweep, not the next head.
func (c *Engine) workflowApprovalScanDue(owner, repo string, number int, first bool) bool {
	key := c.evaluatedHeadKey(owner, repo, number)
	now := c.clock()
	c.workflowApprovalCommandsMu.Lock()
	defer c.workflowApprovalCommandsMu.Unlock()
	if c.workflowApprovalScans == nil {
		c.workflowApprovalScans = make(map[string]time.Time)
	}
	if last, ok := c.workflowApprovalScans[key]; ok && !first && now.Sub(last) < workflowApprovalRecheck {
		return false
	}
	c.workflowApprovalScans[key] = now
	return true
}

// workflowApprovalComments is what a PR's comment thread says about the
// hive's /ok-to-test command.
type workflowApprovalComments struct {
	// postedForHead: the App already posted the command for this head.
	postedForHead bool
	// rejected: the merge bot replied that the commenter is not trusted.
	rejected bool
	// rejectionNoted: the App already posted its needs-human note.
	rejectionNoted bool
	// prowManaged: Prow's approve plugin comments on this PR.
	prowManaged bool
}

// workflowApprovalCommentState reads the PR's comments when a scan is due.
// The second result is false when nothing was read (not due, or the API
// failed), in which case the caller keeps its in-process knowledge.
func (c *Engine) workflowApprovalCommentState(ctx context.Context, displayRepo, owner, repo string, number int, headSHA string, first bool) (workflowApprovalComments, bool) {
	var state workflowApprovalComments
	if !c.workflowApprovalScanDue(owner, repo, number, first) {
		return state, false
	}
	appLogin := ""
	if c.transport != nil {
		appLogin = strings.TrimSpace(c.transport.AppBotLogin())
	}
	marker := workflowApprovalCommandMarkerPrefix + strings.ToLower(headSHA) + " -->"
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, number, opts)
		if err != nil {
			c.warn("automerge could not list PR comments for ok-to-test state", "repo", displayRepo, "pr", number, "sha", headSHA, "error", err)
			return workflowApprovalComments{}, false
		}
		for _, comment := range comments {
			if comment == nil {
				continue
			}
			body := comment.GetBody()
			login := hgithub.SafeGetLogin(comment.GetUser())
			if appLogin != "" && strings.EqualFold(login, appLogin) {
				if strings.Contains(body, marker) {
					state.postedForHead = true
				}
				if strings.Contains(body, workflowApprovalRejectedMarker) {
					state.rejectionNoted = true
				}
				continue
			}
			if strings.Contains(body, prowApprovalNotifier) {
				state.prowManaged = true
			}
			if strings.Contains(body, prowOkToTestRejection) && (appLogin == "" || strings.Contains(strings.ToLower(body), "@"+strings.ToLower(appLogin))) {
				state.rejected = true
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return state, true
}

// okToTestWorkflowCheck remembers one repository's hive-ok-to-test lookup.
type okToTestWorkflowCheck struct {
	at        time.Time
	installed bool
}

// okToTestWorkflowInstalled reports whether the repository carries the
// self-contained hive-ok-to-test workflow, which approves held runs on the
// App's command whatever Prow thinks of the commenter. A lookup failure
// other than 404 reports true so the existing post-once behaviour is kept.
func (c *Engine) okToTestWorkflowInstalled(ctx context.Context, displayRepo, owner, repo string) bool {
	key := strings.ToLower(owner + "/" + repo)
	now := c.clock()
	c.workflowApprovalCommandsMu.Lock()
	cached, ok := c.workflowApprovalWorkflowChecks[key]
	c.workflowApprovalCommandsMu.Unlock()
	if ok && now.Sub(cached.at) < okToTestWorkflowCheckTTL {
		return cached.installed
	}
	installed := true
	_, _, resp, err := c.gh.Repositories.GetContents(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, oktotest.WorkflowPath, nil)
	switch {
	case err == nil:
		installed = true
	case resp != nil && resp.StatusCode == http.StatusNotFound:
		installed = false
	default:
		c.warn("automerge could not check for the hive ok-to-test workflow", "repo", displayRepo, "path", oktotest.WorkflowPath, "error", err)
	}
	c.workflowApprovalCommandsMu.Lock()
	if c.workflowApprovalWorkflowChecks == nil {
		c.workflowApprovalWorkflowChecks = make(map[string]okToTestWorkflowCheck)
	}
	c.workflowApprovalWorkflowChecks[key] = okToTestWorkflowCheck{at: now, installed: installed}
	c.workflowApprovalCommandsMu.Unlock()
	return installed
}

// workflowApprovalCommandBody is the command plus the per-head marker line.
func workflowApprovalCommandBody(headSHA string) string {
	return workflowApprovalCommand + "\n" + workflowApprovalCommandMarkerPrefix + strings.ToLower(headSHA) + " -->"
}

func (c *Engine) postWorkflowApprovalCommand(ctx context.Context, displayRepo, owner, repo string, number int, headSHA string) {
	body := workflowApprovalCommandBody(headSHA)
	_, err := effects.Execute(ctx, c.mutation, effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindIssueComment,
		Target: strconv.Itoa(number),
		Actor:  "automerge",
		Inputs: map[string]string{"head_sha": headSHA, "body": effects.StableDigest(workflowApprovalCommand)},
	}, func(ctx context.Context) (effects.Result, error) {
		comment, _, apiErr := c.gh.Issues.CreateComment(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)})
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

// workflowApprovalUntrustedNote is the one needs-human comment posted when
// the merge bot refuses the App's command.
func workflowApprovalUntrustedNote(appLogin string, rejected bool) string {
	if appLogin == "" {
		appLogin = "the Hive App"
	}
	var b strings.Builder
	b.WriteString(workflowApprovalRejectedMarker)
	b.WriteString("\nneeds-human: GitHub Actions is holding this PR's workflow runs for approval, and Hive cannot start them. `")
	b.WriteString(workflowApprovalCommand)
	b.WriteString("` on this repository belongs to Prow, which only accepts it from its trusted users")
	if rejected {
		b.WriteString("; it rejected `")
		b.WriteString(appLogin)
		b.WriteString("` (\"")
		b.WriteString(prowOkToTestRejection)
		b.WriteString("\")")
	} else {
		b.WriteString(", and `")
		b.WriteString(appLogin)
		b.WriteString("` is not one")
	}
	b.WriteString(". Hive will not post the command again on this PR.\n\nA maintainer can comment `")
	b.WriteString(workflowApprovalCommand)
	b.WriteString("` (plus `/lgtm` and `/approve` for tide), or install `")
	b.WriteString(oktotest.WorkflowPath)
	b.WriteString("` (`hivectl ok-to-test-template`) so the repository's own GITHUB_TOKEN approves trusted same-repo bot runs on Hive's command.")
	return b.String()
}

func (c *Engine) postWorkflowApprovalUntrustedNote(ctx context.Context, displayRepo, owner, repo string, number int, headSHA string, rejected bool) {
	appLogin := ""
	if c.transport != nil {
		appLogin = strings.TrimSpace(c.transport.AppBotLogin())
	}
	body := workflowApprovalUntrustedNote(appLogin, rejected)
	_, err := effects.Execute(ctx, c.mutation, effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindIssueComment,
		Target: strconv.Itoa(number),
		Actor:  "automerge",
		Inputs: map[string]string{"body": effects.StableDigest(workflowApprovalRejectedMarker)},
	}, func(ctx context.Context) (effects.Result, error) {
		comment, _, apiErr := c.gh.Issues.CreateComment(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)})
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		c.noteOwnStageComment(owner, repo, number)
		return effects.Result{Provenance: comment.GetHTMLURL()}, nil
	})
	if err != nil {
		c.warn("automerge failed to post ok-to-test needs-human note", "repo", displayRepo, "pr", number, "sha", headSHA, "error", err)
		return
	}
	c.info("automerge posted ok-to-test needs-human note", "repo", displayRepo, "pr", number, "sha", headSHA, "rejected", rejected)
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

// reportTerminalHold logs a terminal hold once per PR head and reason, and
// tells OnTerminalHold on that same first report so the caller can hand the PR
// to a person (#12379). Later ticks on the same head report nothing.
func (c *Engine) reportTerminalHold(repo, owner, repoName string, number int, headSHA, reason string) {
	if !c.markTerminalHoldLogged(owner, repoName, number, headSHA, reason) {
		return
	}
	c.info("automerge hold", "repo", repo, "pr", number, "sha", headSHA, "reason", reason)
	if c.onTerminalHold != nil {
		c.onTerminalHold(repo, number, headSHA, reason)
	}
}
