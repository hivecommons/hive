package agent

import (
	"context"
	"time"
)

// StartInferenceActivityRefresh polls the translator without holding the
// manager mutex. An unavailable reader leaves the last known activity intact.
func (m *Manager) StartInferenceActivityRefresh(ctx context.Context, read func(context.Context) (map[string]time.Time, error)) {
	m.refreshInferenceActivity(ctx, read)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.refreshInferenceActivity(ctx, read)
		}
	}
}

func (m *Manager) refreshInferenceActivity(ctx context.Context, read func(context.Context) (map[string]time.Time, error)) {
	activity, err := read(ctx)
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, at := range activity {
		if agent := m.agents[name]; agent != nil && at.After(agent.LastInferenceActivity) {
			agent.LastInferenceActivity = at
		}
	}
}
