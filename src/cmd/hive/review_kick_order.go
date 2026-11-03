package main

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/scheduler"
)

// reviewKickKey is the lookup key the eval cycle uses to map an assembled
// kick message back to the review-plan kick it came from.
func reviewKickKey(msg scheduler.KickMessage) string {
	return msg.Agent + "\x00" + msg.Message
}

// orderReviewFixKicksFirst moves each review-plan fix kick ahead of the
// first other message addressed to the same agent, keeping every other
// message in its original order.
//
// Kicks are delivered one at a time and a send waits for the agent's input
// prompt, so when the cycle holds both a governor kick and a fix kick for one
// agent, whichever goes first owns the agent and the other times out. With
// the governor kick first, a fix kick that carries a maintainer's
// changes_requested review or `/hive decision` was planned every cycle and
// never delivered (hivecommons/hive#13100): the governor kick always came
// first, the author agent was mid-turn by the time the fix kick's turn came,
// and the undelivered fix was dropped from the dispatch state to be
// re-planned — and starved again — next cycle. The maintainer's instruction
// outranks routine queue work for the agent it addresses; the governor kick
// is cadence-driven and simply lands on a later cycle.
//
// The inverse starvation is just as real (hivecommons/hive#15166): a fix
// kick DELIVERED leaves the agent's lane busy, so the governor kick behind it
// is deferred — not failed, so nothing records that the agent's backlog went
// unserved — and an adopted contributor PR under changes_requested is
// re-planned as a fix kick EVERY cycle (TestAdoptedPendingFixIsRekickedEveryCycle),
// so the governor kick was deferred cycle after cycle while actionable
// issues sat unclaimed for hours. governorFirst reports the agents whose
// governor kick has been displaced governorKickDisplacementLimit cycles in
// a row; for them the fix kicks keep their original place (after the
// governor kick) for this one cycle and are re-planned next cycle, exactly
// as an undelivered fix kick is today. nil means "never".
func orderReviewFixKicksFirst(messages []scheduler.KickMessage, reviewKickByMessage map[string]review.DispatchKick, governorFirst func(agent string) bool) []scheduler.KickMessage {
	if len(messages) == 0 {
		return messages
	}
	var fixes, rest []scheduler.KickMessage
	fixAgents := map[string]bool{}
	for _, msg := range messages {
		if k, ok := reviewKickByMessage[reviewKickKey(msg)]; ok && k.Kind == "fix" && !(governorFirst != nil && governorFirst(msg.Agent)) {
			fixes = append(fixes, msg)
			fixAgents[msg.Agent] = true
			continue
		}
		rest = append(rest, msg)
	}
	if len(fixes) == 0 {
		return messages
	}
	out := make([]scheduler.KickMessage, 0, len(messages))
	placed := map[string]bool{}
	place := func(agent string) {
		if placed[agent] {
			return
		}
		placed[agent] = true
		for _, f := range fixes {
			if f.Agent == agent {
				out = append(out, f)
			}
		}
	}
	for _, msg := range rest {
		if fixAgents[msg.Agent] {
			place(msg.Agent)
		}
		out = append(out, msg)
	}
	for agent := range fixAgents {
		if !placed[agent] {
			// No other message for this agent: the fix kick stays where it
			// was relative to the rest, i.e. after everything else.
			for _, f := range fixes {
				if f.Agent == agent {
					out = append(out, f)
				}
			}
			placed[agent] = true
		}
	}
	return out
}

// logUndeliveredReviewKick makes a review-plan kick that could not be sent
// visible by PR: the generic "failed to send kick" line names only the
// agent, which is not enough to see that a maintainer's instruction on a
// particular PR is being starved. The planner drops the undelivered kick
// from the dispatch state (review.ConfirmDelivered) and re-plans it on the
// next cycle, so the line says so.
func logUndeliveredReviewKick(logger *slog.Logger, auditor reviewFixAuditor, k review.DispatchKick, err error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("review swarm kick not delivered; re-planned next cycle",
		"kind", k.Kind, "agent", k.Agent, "pr", k.PRRef, "head", k.HeadSHA, "error", err)
	if auditor == nil {
		return
	}
	auditor.AuditLog("governor", "review_kick_undelivered",
		fmt.Sprintf("kind=%s pr=%s head=%s agent=%s error=%v", k.Kind, k.PRRef, k.HeadSHA, k.Agent, err), k.Agent)
}

// governorKickDisplacementLimit is how many consecutive cycles an agent's
// governor kick may be deferred behind a delivered fix kick before the
// governor kick goes first for one cycle. Two: a single displacement is the
// hivecommons/hive#13100 contract working as intended; a second in a row is
// the lane being monopolised, and the backlog kick already carries every
// red/conflicted PR of the agent's own (FIX-BEFORE-NEW) so the fix work is
// not lost, only re-ordered.
const governorKickDisplacementLimit = 2

// kickLaneFairness remembers, per agent, how many cycles in a row the
// governor kick was assembled but deferred because a review-plan fix kick
// for the same agent was delivered first. It is the eval cycle's only memory
// of that displacement: the governor kick is re-rendered from scratch every
// cycle and carries no age of its own.
type kickLaneFairness struct {
	mu        sync.Mutex
	displaced map[string]int
}

func newKickLaneFairness() *kickLaneFairness {
	return &kickLaneFairness{displaced: map[string]int{}}
}

// GovernorFirst reports whether this agent's governor kick has been
// displaced by fix kicks for governorKickDisplacementLimit consecutive
// cycles and must go ahead of them this cycle.
func (f *kickLaneFairness) GovernorFirst(agent string) bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.displaced[agent] >= governorKickDisplacementLimit
}

// Displaced returns the current consecutive-displacement count for logging.
func (f *kickLaneFairness) Displaced(agent string) int {
	if f == nil {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.displaced[agent]
}

// ObserveCycle updates the per-agent counters from one dispatch pass:
// assembled is the ordered message list handed to dispatchAgentKicks,
// delivered the subset that actually went out. For each agent with a
// governor (non-review) kick assembled: delivered resets the counter; not
// delivered while a fix kick for the same agent was delivered counts one more
// displacement. Agents with no governor kick this cycle are left alone — a
// cycle in which the governor had nothing for them says nothing about
// starvation. A governor kick that failed outright (send error) is not a
// displacement either: it is already recorded as a kick failure.
func (f *kickLaneFairness) ObserveCycle(assembled, delivered []scheduler.KickMessage, reviewKickByMessage map[string]review.DispatchKick) {
	if f == nil {
		return
	}
	type laneOutcome struct{ hadGovernor, governorDelivered, fixDelivered bool }
	lanes := map[string]*laneOutcome{}
	lane := func(agent string) *laneOutcome {
		l, ok := lanes[agent]
		if !ok {
			l = &laneOutcome{}
			lanes[agent] = l
		}
		return l
	}
	for _, msg := range assembled {
		if _, isReview := reviewKickByMessage[reviewKickKey(msg)]; !isReview {
			lane(msg.Agent).hadGovernor = true
		}
	}
	for _, msg := range delivered {
		k, isReview := reviewKickByMessage[reviewKickKey(msg)]
		switch {
		case !isReview:
			lane(msg.Agent).governorDelivered = true
		case k.Kind == "fix":
			lane(msg.Agent).fixDelivered = true
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for agent, l := range lanes {
		switch {
		case !l.hadGovernor:
		case l.governorDelivered:
			delete(f.displaced, agent)
		case l.fixDelivered:
			f.displaced[agent]++
		}
	}
}
