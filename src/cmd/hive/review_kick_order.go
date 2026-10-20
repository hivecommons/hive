package main

import (
	"fmt"
	"log/slog"

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
func orderReviewFixKicksFirst(messages []scheduler.KickMessage, reviewKickByMessage map[string]review.DispatchKick) []scheduler.KickMessage {
	if len(messages) == 0 {
		return messages
	}
	var fixes, rest []scheduler.KickMessage
	fixAgents := map[string]bool{}
	for _, msg := range messages {
		if k, ok := reviewKickByMessage[reviewKickKey(msg)]; ok && k.Kind == "fix" {
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
