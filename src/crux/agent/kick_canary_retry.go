package agent

// A kick that opens with the ioscan exfiltration canary can be refused as a
// whole by a model that reads the canary as a prompt-injection marker
// (Claude Sonnet 5 via the Copilot CLI: "this request includes a 'SECURITY
// CANARY' marker and asks me to act as a scanner agent …"). The refusal used
// to cost the entire cadence slot — a 4h scanner kick, or ~150 five-minute
// supervisor kicks a day on one hosted hive. The refused kick is instead
// re-delivered once with the canary preamble stripped; the retry is a plain
// kick, so a second refusal is the model's verdict on the work itself and
// stands.

// kickTriggerCanaryRetry is the delivery trigger of the one canary-free
// re-delivery a refused canary kick gets. recordDeliveredKickLocked uses it
// to mark the retry so it is never itself retried.
const kickTriggerCanaryRetry = "canary-refusal-retry"

// planCanaryRetry decides from the refused kick alone whether it earns a
// canary-free re-delivery: the kick as handed to delivery must have carried
// a canary preamble with a body after it, and must not already be the retry.
func planCanaryRetry(lastKickInput string, alreadyRetried bool) (plain string, ok bool) {
	if alreadyRetried {
		return "", false
	}
	plain, had := stripCanaryPreamble(lastKickInput)
	if !had {
		return "", false
	}
	return plain, true
}

// retryRefusedCanaryKick is the checkKickRefusal hook: when the kick just
// refused led with a canary, re-deliver it once without one. Delivery goes
// through the full SendKick readiness chain on its own goroutine (the caller
// is the pane poller, and SendKick blocks on the input prompt), so the retry
// lands once the refusing turn has returned to the prompt. Must NOT be
// called with m.mu held.
func (m *Manager) retryRefusedCanaryKick(agent *AgentProcess) {
	m.mu.RLock()
	plain, ok := planCanaryRetry(agent.lastKickInput, agent.canaryRetryDone)
	m.mu.RUnlock()
	if !ok {
		return
	}
	m.logger.Warn("kick refused with the exfiltration canary in front; re-delivering once without it",
		"agent", agent.Name,
		"reason", agent.KickRefusalReason,
	)
	deliver := m.canaryRetryDeliver
	if deliver == nil {
		deliver = func(name, message string) {
			go func() {
				if err := m.sendKick(name, message, kickTriggerCanaryRetry); err != nil {
					m.logger.Warn("canary-free re-delivery of refused kick failed",
						"agent", name, "error", err)
				}
			}()
		}
	}
	deliver(agent.Name, plain)
}
