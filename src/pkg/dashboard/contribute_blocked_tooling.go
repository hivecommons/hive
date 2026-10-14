package dashboard

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// A blocked verdict (#7924) means nothing in the task's repository can change
// until something OUTSIDE it lands, and the relay backs it with the repo's
// `blocked` label — a gate only a human lifts. hivecommons/hive#11775/#11776
// showed the failure mode of trusting that verdict verbatim: a contributor's
// agent could not push to its fork (the task token lacked the `workflow`
// scope), printed `HIVE_VERDICT: blocked — the push to my fork is rejected…`,
// and the relay dutifully labelled both issues `blocked`. Nothing outside the
// repository was in the way; the contributor's own tooling was. Every hive in
// the fleet then withheld the issues (the `blocked` label is a workflow gate
// for the spoke's scanner too) with no PR behind them and no release coming.
//
// The hub is the authority on what a completion means, so it reclassifies a
// blocked verdict whose reason describes the CONTRIBUTOR'S OWN tooling —
// push rejected, token scope, credentials, permission, the gh/git wrappers,
// a stale fork — as an ordinary idle completion (short cooldown, re-offered
// to another contributor), lifts the `blocked` label the relay just applied,
// and says so once on the issue so the next reader knows why.

// blockedVerdictToolingFailurePattern matches the vocabulary of a contributor
// environment failing — not of an external dependency being absent. Each
// alternative is anchored on a word that does not appear in a genuine
// "waiting on another repo's release" reason.
var blockedVerdictToolingFailurePattern = regexp.MustCompile(`(?i)(?:` +
	`\bpush(?:ed|ing)?\b.{0,80}\b(?:rejected|denied|refused|failed|forbidden|not permitted)\b|` +
	`\b(?:rejected|denied|refused|failed|cannot|can't|could not|unable to)\b.{0,40}\bpush\b|` +
	`\b(?:token|credential|PAT)s?\b.{0,60}\b(?:lacks?|lacking|missing|without|does not (?:have|carry|include|grant)|doesn't (?:have|carry|include|grant)|no|insufficient)\b.{0,40}\b(?:scope|permission)s?\b|` +
	`\b(?:workflow|repo|write)\s+scope\b|` +
	`\bHTTP\s*40[13]\b|\b40[13]\s+forbidden\b|\b401\s+unauthorized\b|` +
	`\bpermission denied\b|` +
	`\bauthentication failed\b|` +
	`\b(?:gh|go|git)[\s-]wrapper\b|` +
	`\bfork(?:'s)?\b.{0,80}\b(?:behind|out[\s-]of[\s-]date|stale|sync(?:ed|ing)?|diverged)\b|` +
	`\bsync(?:ing)?\b.{0,40}\bfork\b` +
	`)`)

// toolingBlockedReleaseTimeout bounds the detached GitHub work the release
// does: one label removal, one comment search, at most one comment.
const toolingBlockedReleaseTimeout = 30 * time.Second

// toolingBlockedReleaseMarker tags the release comment so it is posted once
// per issue and a later reader (human or hive) can find it.
const toolingBlockedReleaseMarker = "<!-- hive:blocked-released kind=tooling -->"

// isBlockedVerdictToolingFailure reports whether a blocked verdict's reason
// describes the contributor's own tooling failing rather than an external
// dependency.
func isBlockedVerdictToolingFailure(reason string) bool {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return false
	}
	return blockedVerdictToolingFailurePattern.MatchString(reason)
}

// reclassifyToolingBlockedVerdict demotes a blocked verdict whose reason is a
// contributor-side tooling failure to idle. The second return reports whether
// it did so; every other verdict passes through unchanged.
func reclassifyToolingBlockedVerdict(verdict, reason string) (string, bool) {
	if verdict != completionVerdictBlocked || !isBlockedVerdictToolingFailure(reason) {
		return verdict, false
	}
	return completionVerdictIdle, true
}

// toolingBlockedReleaseComment is the one comment the hub leaves when it lifts
// a tooling-failure `blocked` label. The marker makes the post idempotent.
func toolingBlockedReleaseComment(reporter, reason string) string {
	reporter = strings.TrimSpace(reporter)
	if len(reason) > noWorkReasonMaxLen {
		reason = reason[:noWorkReasonMaxLen]
	}
	reason = strings.TrimSpace(reason)
	who := "a contributor's agent"
	if reporter != "" {
		who = "@" + reporter + "'s agent"
	}
	body := fmt.Sprintf("%s\nHive: %s reported this issue blocked, but the reason describes the contributor's own tooling rather than an external dependency, so the `%s` label it applied has been removed and the issue stays actionable for other contributors. Reported reason: %s",
		toolingBlockedReleaseMarker, who, blockedWorkflowLabel, reason)
	if reason == "" {
		body = strings.TrimSuffix(body, " Reported reason: ")
	}
	return body
}

// releaseToolingBlockedIssue lifts the `blocked` label a relay applied for a
// tooling-failure verdict and leaves the release comment once. Runs detached
// from the read loop; callers `go` it. A missing GitHub client or an API
// error is logged — the verdict has already been booked as idle, so the
// cooldown still re-offers the issue; only the label gate would then need a
// human.
func (h *ContributeWSHub) releaseToolingBlockedIssue(task *WSTaskAssign, reporter, reason string) {
	if h == nil || task == nil || task.Kind != "issue" || task.Repo == "" || task.Number <= 0 || task.ExternalID != "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), toolingBlockedReleaseTimeout)
	defer cancel()
	err := h.releaseToolingBlockedLabel(ctx, task.Repo, task.Number, reporter, reason)
	if err != nil {
		h.logger.Warn("[contribute-ws] tooling-failure blocked verdict: could not release the blocked label",
			"repo", task.Repo, "number", task.Number, "reporter", reporter, "error", err)
		return
	}
	h.logger.Info("[contribute-ws] tooling-failure blocked verdict: released the blocked label",
		"repo", task.Repo, "number", task.Number, "reporter", reporter)
}

func (h *ContributeWSHub) releaseToolingBlockedLabel(ctx context.Context, repo string, number int, reporter, reason string) error {
	if h.toolingBlockedReleaser != nil {
		return h.toolingBlockedReleaser(ctx, repo, number, reporter, reason)
	}
	if h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
		return ghpkg.ErrNoGitHubClient
	}
	client := h.server.deps.GHClient
	if err := client.RemoveLabel(ctx, repo, number, blockedWorkflowLabel); err != nil {
		return err
	}
	posted, err := client.IssueCommentsContain(ctx, repo, number, toolingBlockedReleaseMarker)
	if err != nil {
		return err
	}
	if posted {
		return nil
	}
	return client.CreateIssueComment(ctx, repo, number, toolingBlockedReleaseComment(reporter, reason))
}
