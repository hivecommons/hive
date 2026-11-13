package main

import (
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/scheduler"
)

// PER-AGENT PROVIDER QUOTA HOLD (hivecommons/hive#18112).
//
// The provider spend gate (gateKickMessagesForProviderBudget) only fires for
// agents behind the hive's own inference gateway: its latch is set by the
// gateway proxy, which never sees a backend that calls its provider directly
// (GitHub Copilot, for one). Those agents still raise crux.Process.QuotaExhausted
// from the pane ("You have exceeded your monthly quota"), but nothing withheld
// their kicks, so the governor kept kicking them — and sending them review
// work — every cycle, each turn ending at once on the same quota error.
//
// The hold is PER AGENT, unlike the spend gate, because the limit is on that
// agent's provider account, not on a key every agent shares: one agent out of
// quota must never hold any other agent. A held agent still gets one probe
// kick per governor.provider_budget.probe_interval_s, so the hive notices when
// the quota resets (the flag clears once the pane stops showing the message).

// agentQuotaHoldState remembers, per quota-exhausted agent, when the last
// probe kick was actually delivered. Package-level (agentQuotaHold) for the
// same reason as providerBudgetProbe: runEvalCycle has no state of its own.
type agentQuotaHoldState struct {
	mu        sync.Mutex
	lastProbe map[string]time.Time
}

// MarkProbeDelivered records that a probe kick reached agent at the given
// time. Call it only after a kick is delivered: a withheld, deferred or failed
// kick learned nothing about the quota and must not push the next probe out.
func (s *agentQuotaHoldState) MarkProbeDelivered(agent string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastProbe == nil {
		s.lastProbe = map[string]time.Time{}
	}
	s.lastProbe[agent] = at
}

// Forget drops agent's probe time, so a later quota hit starts fresh.
func (s *agentQuotaHoldState) Forget(agent string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.lastProbe, agent)
}

// LastProbe reports when a probe kick was last delivered to agent; the zero
// time means never, which makes the agent due a probe at once.
func (s *agentQuotaHoldState) LastProbe(agent string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastProbe[agent]
}

// quotaExhaustedAgents reads the per-agent quota flag from one status snapshot
// and forgets the probe time of every agent whose flag is clear, so its kicks
// flow normally and its next quota hit starts a fresh interval. nil entries
// are skipped. The returned set holds only the exhausted agents.
func (s *agentQuotaHoldState) quotaExhaustedAgents(statuses map[string]*crux.Process) map[string]bool {
	exhausted := map[string]bool{}
	for name, proc := range statuses {
		if proc == nil {
			continue
		}
		if proc.QuotaExhausted {
			exhausted[name] = true
			continue
		}
		s.Forget(name)
	}
	return exhausted
}

// gateKickMessagesForAgentQuota withholds kicks for agents whose provider
// quota is exhausted, except for one probe kick per held agent once
// probeInterval has passed since that agent's last delivered probe (a zero
// lastProbe is due at once). Only the FIRST message for a probing agent is
// kept; the rest of that agent's messages are withheld this cycle. Agents
// that are not exhausted pass through untouched. Message order is preserved.
//
// withheld and probes name agents in message order; an agent appears in
// withheld once per withheld message, and in probes at most once.
func gateKickMessagesForAgentQuota(messages []scheduler.KickMessage, quotaExhausted func(agent string) bool, lastProbe func(agent string) time.Time, now time.Time, probeInterval time.Duration) (kept []scheduler.KickMessage, withheld []string, probes []string) {
	decided := map[string]bool{}
	for _, msg := range messages {
		if !quotaExhausted(msg.Agent) {
			kept = append(kept, msg)
			continue
		}
		if !decided[msg.Agent] {
			decided[msg.Agent] = true
			if last := lastProbe(msg.Agent); last.IsZero() || now.Sub(last) >= probeInterval {
				probes = append(probes, msg.Agent)
				kept = append(kept, msg)
				continue
			}
		}
		withheld = append(withheld, msg.Agent)
	}
	return kept, withheld, probes
}

// compactDuration renders an operator-facing interval without the zero tails
// of time.Duration.String: 30m, 2h, 1h30m, 45s.
func compactDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	s := d.Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
