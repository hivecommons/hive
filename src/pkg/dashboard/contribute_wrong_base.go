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
	wrongBaseParkMarker        = "<!-- hive:wrong-base-park -->"
)

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
		return completionVerdictNeedsDecision, target,
			"wrong_base: no valid target release branch was supplied", true
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

func wrongBaseParkComment(reporter, target, detail, label string) string {
	who := "a contributor's agent"
	if reporter = strings.TrimSpace(reporter); reporter != "" {
		who = "@" + reporter + "'s agent"
	}
	branch := "no valid branch"
	if target != "" {
		branch = "`" + target + "`"
	}
	return fmt.Sprintf("%s Hive could not re-dispatch %s wrong-base verdict to %s (%s), so `%s` remains until a maintainer corrects the target branch.",
		wrongBaseParkMarker, who, branch, detail, label)
}

// parkWrongBaseIssue records the fallback required when the proposed branch is
// absent or GitHub cannot validate it. Runs detached from the WebSocket loop.
func (h *ContributeWSHub) parkWrongBaseIssue(task *WSTaskAssign, reporter, target, detail string) {
	if h == nil || task == nil || task.Kind != "issue" || task.Repo == "" || task.Number <= 0 || task.ExternalID != "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), gateParkReleaseTimeout)
	defer cancel()
	if err := h.parkWrongBase(ctx, task.Repo, task.Number, reporter, target, detail); err != nil {
		h.logger.Warn("[contribute-ws] wrong-base verdict: could not park invalid target",
			"repo", task.Repo, "number", task.Number, "target_branch", target, "error", err)
	}
}

func (h *ContributeWSHub) parkWrongBase(ctx context.Context, repo string, number int, reporter, target, detail string) error {
	if h.wrongBaseParker != nil {
		return h.wrongBaseParker(ctx, repo, number, reporter, target, detail)
	}
	if h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
		return ghpkg.ErrNoGitHubClient
	}
	client := h.server.deps.GHClient
	posted, err := client.IssueCommentsContain(ctx, repo, number, wrongBaseParkMarker)
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
	return client.CreateIssueComment(ctx, repo, number,
		wrongBaseParkComment(reporter, target, detail, label))
}
