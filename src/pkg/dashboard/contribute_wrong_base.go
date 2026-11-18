package dashboard

import (
	"context"
	"fmt"
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

const (
	wrongBaseValidationTimeout = 15 * time.Second
	wrongBaseParkMarkerPrefix  = "<!-- hive:wrong-base-park"
)

// wrongBaseNoTargetReason is the park reason when the verdict named no `vN`
// branch. It does not say "release branch was supplied": a repository on
// main/testing/stable has no release branch to supply (projectbluefin/utah#1272).
const wrongBaseNoTargetReason = "wrong_base: the agent named no `vN` release branch, the only kind Hive can re-dispatch to"

// wrongBaseParkMarker tags the park note for one task, so a re-delivered
// completion posts nothing twice while a later task's park still gets its own
// note with that agent's reason.
func wrongBaseParkMarker(taskID string) string {
	if id := sanitizeParkTaskID(taskID); id != "" {
		return wrongBaseParkMarkerPrefix + " task=" + id + " -->"
	}
	return wrongBaseParkMarkerPrefix + " -->"
}

// wrongBaseParkReason is the reason a parked wrong_base verdict is recorded
// and logged with: the hub's finding plus the agent's own words, which are
// the only thing a maintainer can act on. agentReason is already sanitized.
func wrongBaseParkReason(detail, agentReason string) string {
	if agentReason == "" {
		return detail
	}
	return detail + "; the agent said: " + agentReason
}

// normalizeWrongBaseTarget accepts the release-line vocabulary contributor
// dispatch can route. Keeping this identical to declared issue targets prevents
// a relay-supplied ref expression from becoming a git argument in a later prompt.
func normalizeWrongBaseTarget(target string) string {
	target = strings.ToLower(strings.TrimSpace(target))
	if !isReleaseLine(target) {
		return ""
	}
	return target
}

// resolveWrongBaseVerdict validates the relay's proposed replacement branch.
// A valid branch remains wrong_base and becomes durable routing memory. Missing,
// malformed, or unverifiable targets become needs_decision so they can never be
// mistaken for already-done work or silently redirected to a guessed base.
func (h *ContributeWSHub) resolveWrongBaseVerdict(task *WSTaskAssign, verdict, proposed string) (resolved, target, reason string, park bool) {
	if verdict != completionVerdictWrongBase {
		return verdict, "", "", false
	}
	target = normalizeWrongBaseTarget(proposed)
	if task == nil || task.Repo == "" || target == "" {
		return completionVerdictNeedsDecision, target, wrongBaseNoTargetReason, true
	}

	ctx, cancel := context.WithTimeout(context.Background(), wrongBaseValidationTimeout)
	defer cancel()
	exists, err := h.wrongBaseBranchExists(ctx, task.Repo, target)
	switch {
	case err != nil:
		return completionVerdictNeedsDecision, target,
			fmt.Sprintf("wrong_base: target branch %s could not be validated", target), true
	case !exists:
		return completionVerdictNeedsDecision, target,
			fmt.Sprintf("wrong_base: target branch %s does not exist", target), true
	default:
		return completionVerdictWrongBase, target, target, false
	}
}

func (h *ContributeWSHub) wrongBaseBranchExists(ctx context.Context, repo, branch string) (bool, error) {
	if h != nil && h.wrongBaseBranchChecker != nil {
		return h.wrongBaseBranchChecker(ctx, repo, branch)
	}
	if h == nil || h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
		return false, ghpkg.ErrNoGitHubClient
	}
	return h.server.deps.GHClient.BranchExists(ctx, repo, branch)
}

// wrongBaseParkComment is the one note a parked wrong_base verdict posts; it
// is also the park's question, so the generic needs-decision question is not
// posted beside it. agentReason is the agent's own text, sanitized by the
// caller (no mentions, backticks, HTML comments or line breaks), quoted as
// untrusted text and never used as a branch.
func wrongBaseParkComment(taskID, reporter, target, detail, agentReason, label string) string {
	who := "a contributor's agent"
	if reporter = strings.TrimSpace(reporter); reporter != "" {
		who = "@" + reporter + "'s agent"
	}
	var line string
	if target != "" {
		line = fmt.Sprintf("Hive could not re-dispatch %s wrong-base verdict to `%s` (%s), so `%s` remains until a maintainer corrects the target branch.",
			who, target, detail, label)
	} else {
		line = fmt.Sprintf("Hive could not re-dispatch %s wrong-base verdict: it named no `vN` release branch, the only kind Hive re-dispatches to, so `%s` remains until a maintainer decides where this work belongs.",
			who, label)
	}
	said := "The agent gave no reason."
	if agentReason != "" {
		said = "The agent said: \u201c" + agentReason + "\u201d"
	}
	return fmt.Sprintf("%s %s %s Reply `/hive decision <your instructions>` to send the issue back with direction.",
		wrongBaseParkMarker(taskID), line, said)
}

// parkWrongBaseIssue records the fallback required when the proposed branch is
// absent or GitHub cannot validate it. Runs detached from the WebSocket loop.
func (h *ContributeWSHub) parkWrongBaseIssue(task *WSTaskAssign, taskID, reporter, target, detail, agentReason string) {
	if h == nil || task == nil || task.Kind != "issue" || task.Repo == "" || task.Number <= 0 || task.ExternalID != "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), gateParkReleaseTimeout)
	defer cancel()
	body := wrongBaseParkComment(taskID, reporter, target, detail, agentReason, h.configuredNeedsDecisionLabel())
	if err := h.parkWrongBase(ctx, task.Repo, task.Number, wrongBaseParkMarker(taskID), body); err != nil {
		h.logger.Warn("[contribute-ws] wrong-base verdict: could not park invalid target",
			"repo", task.Repo, "number", task.Number, "target_branch", target, "error", err)
	}
}

func (h *ContributeWSHub) parkWrongBase(ctx context.Context, repo string, number int, marker, body string) error {
	if h.wrongBaseParker != nil {
		return h.wrongBaseParker(ctx, repo, number, marker, body)
	}
	if h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
		return ghpkg.ErrNoGitHubClient
	}
	client := h.server.deps.GHClient
	posted, err := client.IssueCommentsContain(ctx, repo, number, marker)
	if err != nil {
		return err
	}
	label := h.configuredNeedsDecisionLabel()
	facts := client.StageFacts()
	if facts == nil {
		return ghpkg.ErrNoGitHubClient
	}
	if err := facts.Parked(ctx, client.IssueStageRef(repo, number), label); err != nil {
		return err
	}
	if posted {
		return nil
	}
	return client.CreateIssueComment(ctx, repo, number, body)
}
