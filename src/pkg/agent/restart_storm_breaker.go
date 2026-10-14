package agent

import (
	"context"
	"fmt"
	"strings"
	"time"
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
	allowed     bool
	backoff     time.Duration
	pauseReason string
}

func (m *Manager) restartWithStormBreaker(ctx context.Context, name, reason, detail string) (bool, error) {
	plan := m.planAutomaticRestart(name, reason, detail, time.Now())
	switch {
	case plan.pauseReason != "":
		if m.logger != nil {
			m.logger.Warn("automatic restart suppressed by restart-storm breaker",
				"agent", name, "reason", reason, "pause_reason", plan.pauseReason)
		}
		return false, m.Pause(name, RestartStormTrigger, plan.pauseReason)
	case plan.backoff > 0:
		if m.logger != nil {
			m.logger.Warn("automatic restart backed off by restart-storm breaker",
				"agent", name, "reason", reason, "backoff", plan.backoff.Round(time.Second))
		}
		return false, nil
	case !plan.allowed:
		return false, nil
	default:
		return true, m.RestartWithReason(ctx, name, reason)
	}
}

func (m *Manager) planAutomaticRestart(name, reason, detail string, now time.Time) restartStormPlan {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok || agent == nil || agent.Paused {
		return restartStormPlan{}
	}
	recent := restartEventsSince(agent.RestartEvents, now.Add(-restartStormWindow))
	if recent >= restartStormPauseAfter {
		msg := restartStormPauseReason(reason, detail, agent)
		agent.LastError = msg
		agent.restartStormBackoffUntil = time.Time{}
		return restartStormPlan{pauseReason: msg}
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
