package watchdog

import (
	"fmt"
	"strings"
	"time"
)

// productionThreshold is the no-production window for one agent: the
// configured NoProductionFor, widened to the agent's cadence period plus
// one kick's worth of slack when the governor kicks it less often than the
// window (#13817). A 24h lane cannot be expected to show evidence every 6h;
// it CAN be expected to show evidence within one cadence of its last kick,
// and the slack covers a kick that lands late or runs long. The note names
// the cadence so the condition explains why the threshold moved.
func (r *Reconciler) productionThreshold(name string, settings Settings) (time.Duration, string) {
	threshold := settings.NoProductionFor
	kp, ok := r.fleet.(KickPeriodReporter)
	if !ok {
		return threshold, ""
	}
	period, ok := kp.KickPeriod(name)
	if !ok || period <= 0 {
		return threshold, ""
	}
	widened := period + period/cadenceSlackDivisor
	if widened <= threshold {
		return threshold, ""
	}
	return widened, fmt.Sprintf("cadence %v widens the %v window to %v", period, settings.NoProductionFor, widened)
}

// reconcileReadiness updates the Producing condition from production
// evidence. A readiness failure alone NEVER pauses or restarts (design
// decision on RFC open question 3): it publishes Producing=False and a
// warning alert, degrading the agent's standing rather than its life.
func (r *Reconciler) reconcileReadiness(name string, obs Observation, now time.Time) {
	settings := r.snapshotSettings()
	last, ok := r.fleet.LastProduction(name)
	status := ConditionUnknown
	reason := "NoEvidence"
	message := "no production evidence source is readable for this agent"
	if obs.ProviderErrorClass != "" {
		status = ConditionFalse
		reason = "ProviderInferenceError"
		message = fmt.Sprintf("blocked: inference (%s)", obs.ProviderErrorClass)
		if line := strings.TrimSpace(obs.ProviderErrorLine); line != "" {
			message += ": " + line
		}
	} else if ok {
		age := now.Sub(last)
		threshold, thresholdNote := r.productionThreshold(name, settings)
		switch {
		case age < threshold:
			status = ConditionTrue
			reason = "RecentProduction"
			message = fmt.Sprintf("last production evidence %v ago", age.Round(time.Minute))
			if thresholdNote != "" && age >= settings.NoProductionFor {
				reason = "WithinCadence"
				message += " — " + thresholdNote
			}
		default:
			// Silence is only a fault when there is work to be silent about.
			// An agent with an empty queue producing nothing is behaving
			// correctly, and reporting it would light this condition on every
			// healthy quiet hive — the failure mode the hub's inactivity rule
			// exists to prevent (agent_inactivity.go: a facet which lights up
			// constantly gets ignored). Same threshold as the hub's
			// inactiveAgentMinQueued: one waiting item is enough to make "and
			// nothing is happening" wrong.
			//
			// Nor is silence a fault when the governor is deliberately not
			// kicking the agent: the hive-wide backlog is not this agent's
			// work while its cadence is paused or its lane is idle (#11910).
			suppressed := ""
			if ks, ok := r.fleet.(KickSuppressionReporter); ok {
				suppressed = ks.KickSuppressedReason(name)
			}
			queued, queueKnown := 0, false
			if suppressed == "" {
				queued, queueKnown = r.fleet.QueuedWork(name)
			}
			switch {
			case suppressed != "":
				status = ConditionTrue
				reason = "IdleNotScheduled"
				message = fmt.Sprintf("no production for %v, but the governor is not kicking this agent: %s", age.Round(time.Minute), suppressed)
			case !queueKnown:
				status = ConditionUnknown
				reason = "QueueUnknown"
				message = fmt.Sprintf("no production for %v, but the work queue could not be read — not called a fault without knowing there was work to do", age.Round(time.Minute))
			case queued < minQueuedForNotProducing:
				status = ConditionTrue
				reason = "IdleNoWorkQueued"
				message = fmt.Sprintf("no production for %v, and nothing is queued — an agent with no work to do is correct, not unhealthy", age.Round(time.Minute))
			default:
				status = ConditionFalse
				reason = "NoRecentProduction"
				message = fmt.Sprintf("no production evidence for %v (threshold %v) while %d item(s) are queued", age.Round(time.Minute), threshold, queued)
				if thresholdNote != "" {
					message += "; " + thresholdNote
				}
				if lister, ok := r.fleet.(QueuedWorkLister); ok {
					refs, hiveWide := lister.QueuedWorkRefs(name)
					message += queuedRefsSuffix(queued, refs, hiveWide)
				}
			}
		}
	}

	r.mu.Lock()
	rec := r.recordLocked(name)
	prev, _ := FindCondition(rec.Conditions, ConditionProducing)
	rec.Conditions = setCondition(rec.Conditions, Condition{
		Type: ConditionProducing, Status: status, Reason: reason, Message: message,
	}, now)
	r.mu.Unlock()

	if status == ConditionFalse && prev.Status != ConditionFalse {
		r.logger.Warn("watchdog: agent not producing", "agent", name, "detail", message)
		if r.alerter != nil {
			r.alerter.AddSystemAlert(producingAlertID(name), severityWarning,
				fmt.Sprintf("Agent %q is alive but not producing: %s", name, message))
		}
	}
	if status == ConditionTrue && prev.Status == ConditionFalse && r.alerter != nil {
		r.alerter.ClearSystemAlert(producingAlertID(name))
	}
}
