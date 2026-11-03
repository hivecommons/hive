package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
)

const (
	RestartStormTrigger = "restart-storm"

	restartStormWindow         = time.Hour
	restartStormPauseAfter     = 10
	restartStormFastRestarts   = 3
	restartStormInitialBackoff = time.Minute
	restartStormMaxBackoff     = 30 * time.Minute
)

type restartStormPlan struct {
	allowed      bool
	backoff      time.Duration
	pauseTrigger string
	pauseReason  string
	// pauseDiagnostics is the self-report excerpt for a restart-storm pause:
	// the pause reason plus the window's restart count and recent causes.
	pauseDiagnostics string
}

func (m *Manager) restartWithStormBreaker(ctx context.Context, name, reason, detail string) (bool, error) {
	plan := m.planAutomaticRestart(name, reason, detail, time.Now())
	if handled, didRestart, err := m.applyRestartStormPlan(name, reason, plan, func() error {
		return m.RestartWithReason(ctx, name, reason)
	}); handled {
		return didRestart, err
	}
	return true, nil
}

func (m *Manager) recoverMissingSessionWithStormBreaker(ctx context.Context, name, reason, detail string) (bool, error) {
	plan := m.planAutomaticRestart(name, reason, detail, time.Now())
	if handled, didRestart, err := m.applyRestartStormPlan(name, reason, plan, func() error {
		restarted, err := m.RestartMissingSessionWithReason(ctx, name, reason)
		if err != nil {
			return err
		}
		if !restarted {
			return errMissingSessionAlreadyAlive
		}
		return err
	}); handled {
		if err == errMissingSessionAlreadyAlive {
			return false, nil
		}
		return didRestart, err
	}
	return true, nil
}

func (m *Manager) applyRestartStormPlan(name, reason string, plan restartStormPlan, restart func() error) (bool, bool, error) {
	switch {
	case plan.pauseReason != "":
		if m.logger != nil {
			m.logger.Warn("automatic restart suppressed by restart-storm breaker",
				"agent", name, "reason", reason, "trigger", plan.pauseTrigger, "pause_reason", plan.pauseReason)
		}
		if plan.pauseTrigger == DiskFullTrigger {
			m.observeCondition(crux.ClassDataDiskPressure, DataDiskPressureKey, "data disk pressure: "+plan.pauseReason, "")
		} else {
			diagnostics := plan.pauseDiagnostics
			if diagnostics == "" {
				diagnostics = plan.pauseReason
			}
			m.observeCondition(crux.ClassRestartStorm, name, "restart storm: "+reason, diagnostics)
		}
		return true, false, m.Pause(name, plan.pauseTrigger, plan.pauseReason)
	case plan.backoff > 0:
		if m.logger != nil {
			m.logger.Warn("automatic restart backed off by restart-storm breaker",
				"agent", name, "reason", reason, "backoff", plan.backoff.Round(time.Second))
		}
		return true, false, nil
	case !plan.allowed:
		return true, false, nil
	default:
		return true, true, restart()
	}
}

func (m *Manager) planAutomaticRestart(name, reason, detail string, now time.Time) restartStormPlan {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok || agent == nil || agent.Paused {
		return restartStormPlan{}
	}
	if agent.effectiveBackend() == "copilot" {
		if msg, full := copilotDiskFullCondition(); full {
			agent.LastError = msg
			agent.restartStormBackoffUntil = time.Time{}
			return restartStormPlan{pauseTrigger: DiskFullTrigger, pauseReason: msg}
		}
	}
	if isCopilotDiskFullError(detail) || isCopilotDiskFullError(reason) {
		msg := diskFullPauseReason(reason, detail)
		agent.LastError = msg
		agent.restartStormBackoffUntil = time.Time{}
		return restartStormPlan{pauseTrigger: DiskFullTrigger, pauseReason: msg}
	}
	recent := restartEventsSince(agent.RestartEvents, now.Add(-restartStormWindow))
	if recent >= restartStormPauseAfter {
		msg := restartStormPauseReason(reason, detail, agent)
		diagnostics := restartStormDiagnostics(msg, agent, now) // before LastError is overwritten
		agent.LastError = msg
		agent.restartStormBackoffUntil = time.Time{}
		return restartStormPlan{pauseTrigger: RestartStormTrigger, pauseReason: msg, pauseDiagnostics: diagnostics}
	}
	if now.Before(agent.restartStormBackoffUntil) {
		return restartStormPlan{backoff: agent.restartStormBackoffUntil.Sub(now)}
	}

	attempt := recent + 1
	if attempt > restartStormFastRestarts {
		agent.restartStormBackoffUntil = now.Add(restartStormBackoffFor(attempt - restartStormFastRestarts))
	}
	return restartStormPlan{allowed: true}
}

func restartEventsSince(events []RestartEvent, cutoff time.Time) int {
	n := 0
	for _, ev := range events {
		if ev.At.IsZero() || ev.At.Before(cutoff) {
			continue
		}
		n++
	}
	return n
}

func restartStormBackoffFor(step int) time.Duration {
	if step <= 0 {
		return 0
	}
	d := restartStormInitialBackoff
	for i := 1; i < step; i++ {
		if d >= restartStormMaxBackoff/2 {
			return restartStormMaxBackoff
		}
		d *= 2
	}
	if d > restartStormMaxBackoff {
		return restartStormMaxBackoff
	}
	return d
}

func restartStormPauseReason(reason, detail string, agent *AgentProcess) string {
	cause := strings.TrimSpace(detail)
	if cause == "" && agent != nil {
		cause = strings.TrimSpace(agent.LastError)
	}
	if cause == "" {
		cause = strings.TrimSpace(reason)
	}
	if cause == "" {
		cause = "automatic restart loop"
	}
	return fmt.Sprintf("restart-storm: %s", sanitizeRestartReason(cause))
}

// restartStormDiagnostics expands a restart-storm pause reason into the
// self-report excerpt. The reason alone ("agent CLI crashed or tmux session
// disappeared") says nothing about what the loop looked like, so the report
// also carries the restart count in the window, the recent restart reasons
// with their counts, and the agent's last error. Everything is bounded and the
// reporter scrubs it before storage.
func restartStormDiagnostics(pauseReason string, agent *AgentProcess, now time.Time) string {
	if agent == nil {
		return pauseReason
	}
	cutoff := now.Add(-restartStormWindow)
	counts := map[string]int{}
	var order []string
	total := 0
	for _, ev := range agent.RestartEvents {
		if ev.At.IsZero() || ev.At.Before(cutoff) {
			continue
		}
		total++
		r := sanitizeRestartReason(ev.Reason)
		if counts[r] == 0 {
			order = append(order, r)
		}
		counts[r]++
	}
	var b strings.Builder
	b.WriteString(pauseReason)
	fmt.Fprintf(&b, "\n%d restarts in the last %s (pause at %d)", total, restartStormWindow, restartStormPauseAfter)
	const maxReasons = 5
	for i, r := range order {
		if i == maxReasons {
			fmt.Fprintf(&b, "\n... and %d more reasons", len(order)-maxReasons)
			break
		}
		fmt.Fprintf(&b, "\n- %s x%d", r, counts[r])
	}
	// The pause reason already carries the sanitized cause, which callers often
	// derive from LastError itself; compare against that cause so the same text
	// is not repeated as "last error:".
	cause := strings.TrimPrefix(pauseReason, "restart-storm: ")
	if last := strings.TrimSpace(agent.LastError); last != "" && sanitizeRestartReason(last) != cause {
		fmt.Fprintf(&b, "\nlast error: %s", last)
	}
	return b.String()
}

func diskFullPauseReason(reason, detail string) string {
	cause := strings.TrimSpace(detail)
	if cause == "" {
		cause = strings.TrimSpace(reason)
	}
	if cause == "" {
		cause = "Copilot launch failed because /data is full or out of quota"
	}
	return fmt.Sprintf("%s: %s", copilotDiskFullReasonHead, sanitizeRestartReason(cause))
}
