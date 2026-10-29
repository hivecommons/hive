package agent

import (
	"regexp"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
)

// Kick outcome kinds and KickOutcome are defined in pkg/crux as part of
// the Crux runtime surface (hivecommons/hive#13536); aliased here. pkg/governor
// mirrors the strings (governor.KickOutcome*) so it needs no import edge.
const (
	KickOutcomeQuestion  = crux.KickOutcomeQuestion
	KickOutcomeStandDown = crux.KickOutcomeStandDown
	KickOutcomeNoOp      = crux.KickOutcomeNoOp
	KickOutcomeEnded     = crux.KickOutcomeEnded
)

// KickOutcome is how an agent's most recent kicked turn ended; see
// crux.KickOutcome.
type KickOutcome = crux.KickOutcome

// kickOutcomeGrace is how long after delivery a pane at the input prompt is
// still assumed to be the CLI accepting the kick rather than a finished turn.
// A kick is only delivered to a pane at its prompt, so the prompt is visible
// for a poll or two before the model starts streaming.
const kickOutcomeGrace = 20 * time.Second

// kickOutcomeReasonMaxRunes bounds the pane line kept as the reason.
const kickOutcomeReasonMaxRunes = 200

// kickQuestionPatterns are the agent's OWN voice asking the operator what to
// do — the shape observed live ("What should I focus on?", "Awaiting your kick
// or specific task assignment."). They are matched lower-cased against the
// pane the agent left at its prompt, so they must be phrasings a policy
// template would not carry verbatim in its instructions.
var kickQuestionPatterns = []string{
	"what should i focus on",
	"what would you like me to focus on",
	"what would you like me to work on",
	"which would you like me to",
	"awaiting your kick",
	"awaiting your instruction",
	"awaiting your task",
	"awaiting a specific task",
	"awaiting further instruction",
	"specific task assignment",
	"let me know which",
	"let me know how you'd like to proceed",
	"let me know how you would like to proceed",
	"how would you like to proceed",
	"please specify which",
}

// kickStandDownRe matches a line that STATES a stand-down — "STAND DOWN." or
// "● STAND DOWN — …" at the start of the line, after any bullet or marker —
// and not a line that merely mentions one mid-sentence ("if the snapshot is
// truncated, STAND DOWN and report"), which is what a policy template says.
var kickStandDownRe = regexp.MustCompile(`(?i)^[^a-z0-9]*stand(?:ing)?[ -]down\b`)

// kickNoOpPatterns are explicit "nothing produced" reports. Each entry is a set
// of fragments that must ALL appear on one line.
var kickNoOpPatterns = [][]string{
	{"no issue opened", "no pr opened"},
	{"no issue opened", "no bead created"},
	{"no pr opened", "no bead created"},
}

// kickEchoPrefixes mark lines that are the CLI's rendering of the operator's
// (i.e. the kick's) own text, not the agent's answer. Claude Code renders the
// submitted prompt as "> …" lines. Those never decide an outcome.
var kickEchoPrefixes = []string{"> ", ">"}

func isKickEchoLine(line string) bool {
	t := strings.TrimSpace(line)
	if t == ">" {
		return true
	}
	for _, p := range kickEchoPrefixes {
		if strings.HasPrefix(t, p+" ") || t == p {
			return true
		}
	}
	return false
}

// classifyTurnTail classifies the pane an agent left behind at its input
// prompt. lines is the VISIBLE pane (the turn's final screen: the model's last
// message and the prompt chrome), top to bottom. It returns the outcome kind
// and the deciding line. Precedence: a stated stand-down wins over a question
// (an agent that stands down and then asks is still blocked), and both win
// over the bare no-op report.
func classifyTurnTail(lines []string) (kind, reason string) {
	var question, noop string
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || isKickEchoLine(line) {
			continue
		}
		lower := strings.ToLower(line)
		if kickStandDownRe.MatchString(line) {
			return KickOutcomeStandDown, truncateStr(line, kickOutcomeReasonMaxRunes)
		}
		if question == "" {
			for _, p := range kickQuestionPatterns {
				if strings.Contains(lower, p) {
					question = truncateStr(line, kickOutcomeReasonMaxRunes)
					break
				}
			}
		}
		if noop == "" {
			for _, set := range kickNoOpPatterns {
				all := true
				for _, frag := range set {
					if !strings.Contains(lower, frag) {
						all = false
						break
					}
				}
				if all {
					noop = truncateStr(line, kickOutcomeReasonMaxRunes)
					break
				}
			}
		}
	}
	if question != "" {
		return KickOutcomeQuestion, question
	}
	if noop != "" {
		return KickOutcomeNoOp, noop
	}
	return KickOutcomeEnded, ""
}

// paneShowsTurnEnded reports whether a VISIBLE pane shows a CLI back at its
// idle input prompt with no response in flight — the moment a kicked turn is
// over. It is the same pair of predicates kick delivery waits on before it
// may type (paneShowsInputPrompt / paneShowsAgentWorking, #7085) plus the
// inference-side active-work markers.
func paneShowsTurnEnded(visible string) bool {
	if visible == "" {
		return false
	}
	return paneShowsInputPrompt(visible) && !paneShowsAgentWorking(visible) && !paneShowsActiveWork(visible)
}

// kickOutcomeDue is the cheap gate the pane poller runs every tick before it
// pays for a visible-pane capture: the agent has a delivered kick, that kick's
// turn is not yet classified, delivery is finished, the grace has elapsed and
// the pane changed since the kick (an unchanged pane is the stall watchdog's
// business, not a finished turn). Callers must hold m.mu (read) and must NOT
// hold paneMu.
func kickOutcomeDue(agent *AgentProcess, now time.Time) bool {
	if agent.LastKick == nil || agent.kickDelivering.Load() {
		return false
	}
	if agent.KickOutcome.Settled(agent.LastKick) {
		return false
	}
	if now.Sub(*agent.LastKick) < kickOutcomeGrace {
		return false
	}
	agent.paneMu.RLock()
	changed := agent.LastPaneChange
	agent.paneMu.RUnlock()
	return changed.After(*agent.LastKick)
}

// settleKickOutcomeLocked classifies the kicked turn if visible shows it has
// ended, records the verdict on the agent and notifies the outcome observer.
// Returns the outcome and true when a verdict was recorded on this call.
// Callers must hold m.mu (write).
func (m *Manager) settleKickOutcomeLocked(agent *AgentProcess, visible string, now time.Time) (KickOutcome, bool) {
	if agent.LastKick == nil || agent.KickOutcome.Settled(agent.LastKick) || !paneShowsTurnEnded(visible) {
		return KickOutcome{}, false
	}
	var lines []string
	for _, l := range strings.Split(visible, "\n") {
		if t := strings.TrimRight(l, " \t"); t != "" {
			lines = append(lines, t)
		}
	}
	kind, reason := classifyTurnTail(lines)
	outcome := KickOutcome{Kind: kind, Reason: reason, At: now, KickAt: *agent.LastKick}
	agent.KickOutcome = outcome

	attrs := []any{"agent", agent.Name, "outcome", kind, "seconds_since_kick", int(now.Sub(*agent.LastKick).Seconds())}
	if reason != "" {
		attrs = append(attrs, "reason", reason)
	}
	switch kind {
	case KickOutcomeQuestion:
		m.logger.Warn("kick ended on a clarifying question: the agent asked the operator what to do instead of working (#7421)", attrs...)
	case KickOutcomeStandDown:
		m.logger.Warn("kick ended in a policy stand-down; recorded as blocked, not as a completed kick (#7421)", attrs...)
	case KickOutcomeNoOp:
		m.logger.Warn("kick ended with an explicit nothing-produced report (#7421)", attrs...)
	default:
		m.logger.Info("kick turn ended", attrs...)
	}
	// The turn is over: make its output durable now rather than waiting for
	// the next kick, a restart or a graceful shutdown — a daily-cadence agent
	// whose container is OOM-killed hours later would otherwise lose the run.
	m.archiveKickLogOnTurnEndLocked(agent)
	m.notifyKickOutcome(agent.Name, outcome)
	return outcome, true
}

// maybeSettleKickOutcome is the CLI-backend poller hook: runs the cheap gate
// under a read lock, and only when it passes captures the visible pane and
// classifies. capture is injected so the decision is testable without tmux.
// Must NOT be called with m.mu held.
func (m *Manager) maybeSettleKickOutcome(agent *AgentProcess, capture func() string) {
	now := time.Now()
	m.mu.RLock()
	due := kickOutcomeDue(agent, now)
	m.mu.RUnlock()
	if !due {
		return
	}
	visible := capture()
	if !paneShowsTurnEnded(visible) {
		return
	}
	m.mu.Lock()
	m.settleKickOutcomeLocked(agent, visible, now)
	m.mu.Unlock()
}

// SetKickOutcomeObserver installs (or with nil, removes) the consumer of kick
// outcome verdicts. The governor is wired here in main so it can stop
// counting a no-op as a completed kick. Same atomic.Pointer discipline as the
// kick lifecycle observer: the notification site runs under m.mu, and the
// observer always runs on its own goroutine.
func (m *Manager) SetKickOutcomeObserver(fn func(agentName string, outcome KickOutcome)) {
	if fn == nil {
		m.kickOutcomeObserver.Store(nil)
		return
	}
	m.kickOutcomeObserver.Store(&fn)
}

func (m *Manager) notifyKickOutcome(agentName string, outcome KickOutcome) {
	if fn := m.kickOutcomeObserver.Load(); fn != nil {
		go (*fn)(agentName, outcome)
	}
	if m.sharedPool != nil {
		go func() {
			m.mu.RLock()
			agent := m.agents[agentName]
			leaseID := ""
			if agent != nil {
				leaseID = agent.poolLeaseID
			}
			m.mu.RUnlock()
			if leaseID != "" {
				m.evictSharedPoolSessionAfterTurn(agentName, leaseID, outcome)
			}
		}()
	}
}
