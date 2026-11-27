package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRefreshInferenceActivityPreservesLatestAndIgnoresFailures(t *testing.T) {
	old := time.Now().Add(-time.Minute)
	fresh := time.Now()
	m := &Manager{agents: map[string]*AgentProcess{"alice": {Name: "alice", LastInferenceActivity: old}, "bob": {Name: "bob"}}}
	m.refreshInferenceActivity(context.Background(), func(context.Context) (map[string]time.Time, error) {
		// The reader may perform I/O and must not run with the manager locked.
		m.mu.Lock()
		m.mu.Unlock()
		return map[string]time.Time{"alice": fresh, "unknown": fresh}, nil
	})
	if !m.agents["alice"].LastInferenceActivity.Equal(fresh) || !m.agents["bob"].LastInferenceActivity.IsZero() {
		t.Fatal("activity was not attributed to the matching agent")
	}
	m.refreshInferenceActivity(context.Background(), func(context.Context) (map[string]time.Time, error) {
		return nil, errors.New("offline")
	})
	m.refreshInferenceActivity(context.Background(), func(context.Context) (map[string]time.Time, error) {
		return map[string]time.Time{"alice": old}, nil
	})
	if !m.agents["alice"].LastInferenceActivity.Equal(fresh) {
		t.Fatal("failed or stale poll erased activity")
	}
}
