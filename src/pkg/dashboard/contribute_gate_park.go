package dashboard

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/decisionpolicy"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// A needs-decision verdict (`HIVE_VERDICT: no_work_needed — decision: …`)
// parks an issue on a maintainer: the relay applies the configured decision
// label (stage/045-needs-decision) and the hub books the full cooldown. Five
// of the nine issues at 045 on one hive on 2026-10-19 were not decisions at
// all (evidence in hivecommons/hive#13059): the agents had read
// the target repository's AGENTS.md "offer a spec and wait for acceptance"
// rule, or its "stop and report a missing registered checkout" rule, and
// parked asking the maintainer to accept work the hive had already accepted
// and dispatched (hivecommons/hive#13059).
//
// The hub is the authority on what a completion means, so — exactly as it
// does for a blocked verdict that describes the contributor's own tooling
// (contribute_blocked_tooling.go) — it reclassifies a needs-decision verdict
// whose reason is a repo-local confirmation gate as an ordinary idle
// completion, lifts the decision label the relay just applied, and leaves one
// comment saying the gate is satisfied. That comment is also the budget:
// one re-kick per issue. A second gate park on the same issue keeps its
// label, so a maintainer sees it — the agent was told twice and still
// stopped, which is worth a look.

// gateParkReleaseTimeout bounds the detached GitHub work the release does:
// one comment search, one label removal, at most one comment.
const gateParkReleaseTimeout = 30 * time.Second

// gateParkReleaseMarker tags the release comment so it is posted once per
// issue; its presence is what turns the second gate park into a real one.
const gateParkReleaseMarker = "<!-- hive:needs-decision-released kind=repo-local-gate -->"

// errGateParkAlreadyReleased reports that the issue already had its one
// re-kick, so the label was left in place.
var errGateParkAlreadyReleased = fmt.Errorf("repo-local gate park already released once; leaving the decision label on")

// isNeedsDecisionRepoLocalGate reports whether a needs-decision reason names
// only a target repository's own confirmation/spec gate or a missing
// registered sibling checkout — no risk category a person must own.
func isNeedsDecisionRepoLocalGate(reason string) bool {
	return decisionpolicy.IsRepoLocalGate(strings.TrimSpace(reason))
}

// reclassifyGateParkVerdict demotes a needs-decision verdict whose reason is
// a repo-local gate to idle, unless this hub has already re-kicked the issue
// for one (key is the task identity). The second return reports a demotion;
// every other verdict passes through unchanged.
func (h *ContributeWSHub) reclassifyGateParkVerdict(key, verdict, reason string) (string, bool) {
	if verdict != completionVerdictNeedsDecision || !isNeedsDecisionRepoLocalGate(reason) {
		return verdict, false
	}
	if h != nil && h.gateParkRekicked(key) {
		return verdict, false
	}
	return completionVerdictIdle, true
}

// gateParkRekicks remembers, per task identity, that this hub already
// released one gate park, so the in-loop decision needs no GitHub call; the
// marker comment is the durable copy that survives a hub restart.
type gateParkRekicks struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func (g *gateParkRekicks) has(key string) bool {
	if g == nil || key == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.seen[key]
	return ok
}

func (g *gateParkRekicks) add(key string) {
	if g == nil || key == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.seen == nil {
		g.seen = map[string]time.Time{}
	}
	g.seen[key] = time.Now()
}

func (h *ContributeWSHub) gateParkRekicked(key string) bool {
	return h != nil && h.gateParkRekicks.has(key)
}

// gateParkReleaseComment is the one comment the hub leaves when it lifts a
// gate-park decision label. It tells the next agent (and the maintainer)
// that the dispatch is the acceptance, and records the reason.
func gateParkReleaseComment(reporter, reason, label string) string {
	reporter = strings.TrimSpace(reporter)
	if len(reason) > noWorkReasonMaxLen {
		reason = reason[:noWorkReasonMaxLen]
	}
	reason = strings.TrimSpace(reason)
	who := "a contributor's agent"
	if reporter != "" {
		who = "@" + reporter + "'s agent"
	}
	body := fmt.Sprintf("%s\nHive: %s parked this for a maintainer decision, but the reason is the repository's own confirmation gate (an instruction-file spec/approval step, or a registered sibling checkout that was not cloned yet), not a risk a person must own. The hive dispatching an accepted issue is that acceptance, so the `%s` label it applied has been removed and the issue is re-dispatched; the next agent proceeds without offering a spec or asking to confirm scope, and clones any missing companion checkout under its workspace root. If the next attempt parks on a gate again, the label stays for a maintainer to look at. Reported reason: %s",
		gateParkReleaseMarker, who, label, reason)
	if reason == "" {
		body = strings.TrimSuffix(body, " Reported reason: ")
	}
	return body
}

// releaseGateParkedIssue lifts the decision label a relay applied for a
// repo-local gate verdict and leaves the release comment once. Runs detached
// from the read loop; callers `go` it. If the marker is already on the issue
// (a prior release this hub forgot across a restart), the label is left on —
// that is the escalation — and the in-memory record is backfilled.
func (h *ContributeWSHub) releaseGateParkedIssue(task *WSTaskAssign, reporter, reason string) {
	if h == nil || task == nil || task.Kind != "issue" || task.Repo == "" || task.Number <= 0 || task.ExternalID != "" {
		return
	}
	key := task.identityKey()
	h.gateParkRekicks.add(key)
	ctx, cancel := context.WithTimeout(context.Background(), gateParkReleaseTimeout)
	defer cancel()
	err := h.releaseGateParkLabel(ctx, task.Repo, task.Number, reporter, reason)
	switch {
	case err == errGateParkAlreadyReleased:
		h.logger.Warn("[contribute-ws] repo-local gate park: issue was already re-kicked once; decision label left for a maintainer",
			"repo", task.Repo, "number", task.Number, "reporter", reporter)
	case err != nil:
		h.logger.Warn("[contribute-ws] repo-local gate park: could not release the decision label",
			"repo", task.Repo, "number", task.Number, "reporter", reporter, "error", err)
	default:
		h.logger.Info("[contribute-ws] repo-local gate park: released the decision label for one re-dispatch",
			"repo", task.Repo, "number", task.Number, "reporter", reporter)
	}
}

func (h *ContributeWSHub) releaseGateParkLabel(ctx context.Context, repo string, number int, reporter, reason string) error {
	if h.gateParkReleaser != nil {
		return h.gateParkReleaser(ctx, repo, number, reporter, reason)
	}
	if h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
		return ghpkg.ErrNoGitHubClient
	}
	client := h.server.deps.GHClient
	posted, err := client.IssueCommentsContain(ctx, repo, number, gateParkReleaseMarker)
	if err != nil {
		return err
	}
	if posted {
		return errGateParkAlreadyReleased
	}
	label := h.configuredNeedsDecisionLabel()
	if err := client.RemoveLabel(ctx, repo, number, label); err != nil {
		return err
	}
	return client.CreateIssueComment(ctx, repo, number, gateParkReleaseComment(reporter, reason, label))
}
