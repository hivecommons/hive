package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/crux/types"
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
			m.observeCondition(types.ClassDataDiskPressure, DataDiskPressureKey, "data disk pressure: "+plan.pauseReason, "")
		} else {
			m.observeCondition(types.ClassRestartStorm, name, "restart storm: "+reason, plan.pauseReason)
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
		agent.LastError = msg
		agent.restartStormBackoffUntil = time.Time{}
		return restartStormPlan{pauseTrigger: RestartStormTrigger, pauseReason: msg}
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
