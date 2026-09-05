package agent

import (
	"strings"
	"time"
)

type RestartEvent struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

func cloneRestartEvents(in []RestartEvent) []RestartEvent {
	if len(in) == 0 {
		return nil
	}
	out := make([]RestartEvent, len(in))
	copy(out, in)
	return out
}

func (m *Manager) SeedRestartTelemetry(name string, count int, events []RestartEvent, lastReason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if agent, ok := m.agents[name]; ok {
		agent.RestartCount = count
		agent.RestartEvents = pruneRestartEvents(events, time.Now())
		agent.LastRestartReason = lastReason
	}
}

func (m *Manager) RestartTelemetry(name string) (total, last24h int, lastRestartAt time.Time, lastReason string, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	agent, ok := m.agents[name]
	if !ok {
		return 0, 0, time.Time{}, "", false
	}
	now := time.Now()
	events := pruneRestartEvents(cloneRestartEvents(agent.RestartEvents), now)
	if len(events) > 0 {
		last := events[len(events)-1]
		lastRestartAt = last.At
		lastReason = last.Reason
	}
	if lastReason == "" {
		lastReason = agent.LastRestartReason
	}
	return agent.RestartCount, len(events), lastRestartAt, lastReason, true
}

func (m *Manager) recordRestartLocked(agent *AgentProcess, reason string) {
	if agent == nil {
		return
	}
	now := time.Now()
	agent.RestartCount++
	agent.LastRestartReason = sanitizeRestartReason(reason)
	agent.RestartEvents = append(pruneRestartEvents(agent.RestartEvents, now), RestartEvent{
		At:     now,
		Reason: agent.LastRestartReason,
	})
}

func pruneRestartEvents(events []RestartEvent, now time.Time) []RestartEvent {
	cutoff := now.Add(-24 * time.Hour)
	out := events[:0]
	for _, ev := range events {
		if ev.At.IsZero() || ev.At.Before(cutoff) {
			continue
		}
		out = append(out, ev)
	}
	return out
}

func sanitizeRestartReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "operator"
	}
	if len(reason) > 80 {
		reason = reason[:80]
	}
	return reason
}
