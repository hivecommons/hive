package dashboard

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// The wrong_base contributor verdict (hivecommons/hive#13146).
//
// An agent dispatched against one branch that finds the issue's code lives
// on another used to have only already_done to say so, and the hub's #8477
// settlement then labelled the issue `hive/already-done` — and, where
// configured, closed it — for code that was simply elsewhere (#13090). A
// wrong_base verdict says "code not on <dispatched base>; it is on <branch>".
// It is a dispatch error, never a settlement: the hub never applies
// `hive/already-done` and never runs the completed-close path for it.
//
// Settlement is one of two things, decided by asking the repository:
//   - the named branch exists and differs from the dispatched base: the hub
//     records it as the item's target-branch override in the contribute
//     ledger (the next offer bases on it), clears the completion cooldown so
//     the item is offerable again, and leaves one comment saying so;
//   - the named branch does not exist (or is the base the agent was already
//     given): the hub parks the issue for a maintainer through the stage
//     engine's sanctioned Parked fact — the same path issue_request_watcher
//     uses — with the one-line reason "code not on <base>; agent named <b>
//     which does not exist", books the needs-decision hold, and comments.
//
// Stage labels stay derived: nothing here writes a `stage/*` label.

// verdictReasonKindWrongBase is the structured verdict_reason_kind a relay
// sends for this verdict; evidence.branch carries the branch.
const verdictReasonKindWrongBase = "wrong_base"

// wrongBaseReasonPattern recognises the sentinel's structured reason text
// (`wrong_base: <branch> — <why>`) from a relay that predates the structured
// kind. Keep in sync with NO_WORK_WRONG_BASE_REASON_RE in
// bin/contributor-relay.js.
var wrongBaseReasonPattern = regexp.MustCompile(`(?i)^\s*wrong[_ -]?base\s*[:\x{2014}\x{2013}-]\s*` + "`?" + `([A-Za-z0-9][A-Za-z0-9._/-]*)` + "`?")

// wrongBaseSettleTimeout bounds the GitHub round trips one settlement makes.
const wrongBaseSettleTimeout = 45 * time.Second

// wrongBaseMarker is the HTML marker on the comment each outcome leaves, so
// a re-run after a hub restart does not comment twice.
const wrongBaseMarker = "<!-- hive:wrong-base-verdict -->"

// wrongBaseVerdict reports whether a completion is a wrong_base verdict and,
// if so, which branch it names. The structured kind wins; the reason text is
// the fallback for older relays. Only a no_work_needed-family verdict can be
// wrong_base — a shipped PR is a PR.
func wrongBaseVerdict(verdict, reason, reasonKind string, evidence *VerdictEvidence) (branch string, ok bool) {
	if verdict == completionVerdictShipped {
		return "", false
	}
	kind := strings.ToLower(strings.TrimSpace(reasonKind))
	if kind == verdictReasonKindWrongBase {
		if evidence != nil {
			branch = strings.TrimSpace(evidence.Branch)
		}
		if branch == "" {
			if m := wrongBaseReasonPattern.FindStringSubmatch(reason); len(m) > 1 {
				branch = m[1]
			}
		}
		return branch, true
	}
	if kind != "" && kind != verdictReasonKindAlreadyDone {
		return "", false
	}
	if m := wrongBaseReasonPattern.FindStringSubmatch(reason); len(m) > 1 {
		return m[1], true
	}
	return "", false
}

// wrongBaseParkReason is the one-line reason the Parked fact and the hold
// carry when the named branch does not exist.
func wrongBaseParkReason(base, branch string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "the dispatched base"
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return fmt.Sprintf("code not on %s; agent named no branch it lives on", base)
	}
	return fmt.Sprintf("code not on %s; agent named %s which does not exist", base, branch)
}

func wrongBaseRequeueComment(reporter, base, branch string) string {
	return fmt.Sprintf("%s\nHive: %s reported this issue's code is not on %s (the branch it was dispatched against) but on `%s`, which exists in this repository. This is a dispatch error, not a finished issue: no already-done label or close applies. The issue is re-offered to contributors against `%s`.",
		wrongBaseMarker, wrongBaseWho(reporter), wrongBaseDisplay(base, "the repository's default branch"), branch, branch)
}

func wrongBaseParkComment(reporter, base, branch, label string) string {
	return fmt.Sprintf("%s\nHive: %s reported this issue's code is not on %s (the branch it was dispatched against) and named %s as where it lives, but this repository has no such branch. Parked for a maintainer under `%s`: %s. No already-done label or close applies.",
		wrongBaseMarker, wrongBaseWho(reporter), wrongBaseDisplay(base, "the repository's default branch"), wrongBaseDisplay(branch, "no branch"), label, wrongBaseParkReason(base, branch))
}

// wrongBaseDisplay renders a branch name in backticks, or the fallback
// phrase when the name is empty.
func wrongBaseDisplay(name, fallback string) string {
	if name = strings.TrimSpace(name); name == "" {
		return fallback
	}
	return "`" + name + "`"
}

func wrongBaseWho(reporter string) string {
	if reporter = strings.TrimSpace(reporter); reporter != "" {
		return "@" + reporter + "'s agent"
	}
	return "a contributor's agent"
}

// settleWrongBaseVerdict runs the settlement described at the top of this
// file. Detached from the read loop; callers `go` it.
func (h *ContributeWSHub) settleWrongBaseVerdict(task *WSTaskAssign, branch, reporter, reason string) {
	if h == nil || task == nil {
		return
	}
	h.wrongBaseSettlements.Add(1)
	defer h.wrongBaseSettlements.Done()
	if task.Kind != "issue" || task.Repo == "" || task.Number <= 0 || task.ExternalID != "" {
		return
	}
	key := task.identityKey()
	base := strings.TrimSpace(task.BaseBranch)
	branch = strings.TrimSpace(branch)
	ctx, cancel := context.WithTimeout(context.Background(), wrongBaseSettleTimeout)
	defer cancel()

	exists, checked := false, false
	if branch != "" && branch != base {
		exists, checked = h.branchExists(ctx, task.Repo, branch)
	}
	// An override already recorded for this very branch means a previous
	// wrong_base verdict re-queued the item there and the agent STILL could
	// not find the code: the second verdict parks it for a person.
	if prior, ok := h.targetBranchOverride(key); ok && prior.Branch == branch {
		h.logger.Warn("[contribute-ws] wrong_base verdict repeats the recorded target branch; parking for a maintainer",
			"repo", task.Repo, "number", task.Number, "branch", branch, "reporter", reporter)
		h.parkWrongBaseIssue(ctx, task, base, branch, reporter)
		return
	}
	if !checked && branch != "" && branch != base && h.canValidateBranches() {
		// The repository could not be consulted (API failure). Neither
		// outcome is safe to assert; leave the ordinary idle cooldown the
		// handler booked and let the next offer try again.
		h.logger.Warn("[contribute-ws] wrong_base verdict: could not confirm the named branch; leaving the issue on its ordinary cooldown",
			"repo", task.Repo, "number", task.Number, "branch", branch, "reporter", reporter)
		return
	}
	if exists {
		h.requeueWrongBaseIssue(ctx, task, base, branch, reporter)
		return
	}
	h.parkWrongBaseIssue(ctx, task, base, branch, reporter)
}

// requeueWrongBaseIssue records the override and makes the item offerable
// again against the named branch.
func (h *ContributeWSHub) requeueWrongBaseIssue(ctx context.Context, task *WSTaskAssign, base, branch, reporter string) {
	key := task.identityKey()
	h.recordTargetBranchOverride(key, targetBranchOverride{
		Branch:         branch,
		DispatchedBase: base,
		Reporter:       reporter,
		RecordedAt:     time.Now(),
	})
	h.completedMu.Lock()
	delete(h.completedTasks, key)
	delete(h.completedTaskCooldown, key)
	delete(h.completedTaskPRURL, key)
	delete(h.noPRStreaks, key)
	verdictCleared := false
	if _, had := h.noWorkVerdicts[key]; had {
		delete(h.noWorkVerdicts, key)
		verdictCleared = true
	}
	h.completedMu.Unlock()
	h.saveCompletedTasks()
	h.saveNoPRStreaks()
	if verdictCleared {
		h.saveNoWorkVerdicts()
	}
	h.logger.Info("[contribute-ws] wrong_base verdict: re-queued the issue against the branch the agent named",
		"repo", task.Repo, "number", task.Number, "dispatched_base", base, "branch", branch, "reporter", reporter)
	if err := h.commentWrongBaseOnce(ctx, task.Repo, task.Number, wrongBaseRequeueComment(reporter, base, branch)); err != nil {
		h.logger.Warn("[contribute-ws] wrong_base verdict: re-queue comment failed", "repo", task.Repo, "number", task.Number, "error", err)
	}
}

// parkWrongBaseIssue submits the Parked fact through the stage engine's
// sanctioned sink, books the needs-decision hold in the contribute ledger,
// and comments once.
func (h *ContributeWSHub) parkWrongBaseIssue(ctx context.Context, task *WSTaskAssign, base, branch, reporter string) {
	key := task.identityKey()
	reason := wrongBaseParkReason(base, branch)
	label := h.configuredNeedsDecisionLabel()
	if err := h.parkWrongBase(ctx, task.Repo, task.Number, reason); err != nil {
		h.logger.Warn("[contribute-ws] wrong_base verdict: parking the issue failed", "repo", task.Repo, "number", task.Number, "error", err)
	}
	h.markTaskCompletedVerdictKeySignal(key, "", completionVerdictNeedsDecision, reporter, reason, completionSignalVerdict)
	h.logger.Info("[contribute-ws] wrong_base verdict: parked the issue for a maintainer",
		"repo", task.Repo, "number", task.Number, "dispatched_base", base, "branch", branch, "reporter", reporter, "label", label)
	if err := h.commentWrongBaseOnce(ctx, task.Repo, task.Number, wrongBaseParkComment(reporter, base, branch, label)); err != nil {
		h.logger.Warn("[contribute-ws] wrong_base verdict: park comment failed", "repo", task.Repo, "number", task.Number, "error", err)
	}
}

func (h *ContributeWSHub) parkWrongBase(ctx context.Context, repo string, number int, reason string) error {
	if h.wrongBaseParker != nil {
		return h.wrongBaseParker(ctx, repo, number, reason)
	}
	if h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
		return ghpkg.ErrNoGitHubClient
	}
	gh := h.server.deps.GHClient
	return gh.StageFacts().Parked(ctx, gh.IssueStageRef(repo, number), h.configuredNeedsDecisionLabel())
}

func (h *ContributeWSHub) commentWrongBaseOnce(ctx context.Context, repo string, number int, body string) error {
	if h.wrongBaseCommenter != nil {
		return h.wrongBaseCommenter(ctx, repo, number, body)
	}
	if h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
		return ghpkg.ErrNoGitHubClient
	}
	client := h.server.deps.GHClient
	posted, err := client.IssueCommentsContain(ctx, repo, number, wrongBaseMarker)
	if err != nil {
		return err
	}
	if posted {
		return nil
	}
	return client.CreateIssueComment(ctx, repo, number, body)
}
