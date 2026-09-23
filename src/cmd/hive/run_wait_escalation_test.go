package main

import (
	"context"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/escalate"
	"github.com/hivecommons/hive/pkg/escalation"
)

type runWaitObserverFunc func() []dashboard.RunWaitSnapshot

func (f runWaitObserverFunc) RunWaitSnapshot() []dashboard.RunWaitSnapshot { return f() }

type channelEscalationSink struct {
	ch chan escalate.Event
}

func (s channelEscalationSink) Name() string { return "test" }
func (s channelEscalationSink) Deliver(_ context.Context, ev escalate.Event) error {
	s.ch <- ev
	return nil
}

func TestRunWaitEscalationDispatchesOneEventPerGeneration(t *testing.T) {
	oldStoreFunc := runWaitEscalationStore
	oldDispatcher := currentEscalationDispatcher()
	t.Cleanup(func() {
		runWaitEscalationStore = oldStoreFunc
		escalationRuntime.Lock()
		if escalationRuntime.d != nil {
			escalationRuntime.d.Stop()
		}
		escalationRuntime.d = oldDispatcher
		escalationRuntime.Unlock()
	})
	store := escalation.Load("")
	runWaitEscalationStore = func() *escalation.Store { return store }
	now := time.Date(2026, 9, 22, 20, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return now })

	ch := make(chan escalate.Event, 2)
	d := escalate.NewDispatcher(context.Background(), nil, nil)
	d.Register(channelEscalationSink{ch: ch}, escalate.SeverityDecision, 4)
	escalationRuntime.Lock()
	escalationRuntime.d = d
	escalationRuntime.Unlock()

	cfg := &config.Config{}
	observer := runWaitObserverFunc(func() []dashboard.RunWaitSnapshot {
		return []dashboard.RunWaitSnapshot{{
			Key:          "hivecommons/hive#8312",
			Stage:        "plan",
			Gen:          3,
			WaitingOn:    "human",
			WaitingSince: now.Add(-2 * time.Hour),
		}}
	})
	runWaitEscalationSweep(cfg, observer, nil)
	assertOneEvent(t, ch, "first generation")
	runWaitEscalationSweep(cfg, observer, nil)
	assertNoEvent(t, ch, "duplicate generation")

	observer = runWaitObserverFunc(func() []dashboard.RunWaitSnapshot {
		return []dashboard.RunWaitSnapshot{{
			Key:          "hivecommons/hive#8312",
			Stage:        "plan",
			Gen:          4,
			WaitingOn:    "human",
			WaitingSince: now.Add(-2 * time.Hour),
		}}
	})
	runWaitEscalationSweep(cfg, observer, nil)
	assertOneEvent(t, ch, "next generation")
}

func assertOneEvent(t *testing.T, ch <-chan escalate.Event, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatalf("%s: timed out waiting for escalation event", label)
	}
}

func assertNoEvent(t *testing.T, ch <-chan escalate.Event, label string) {
	t.Helper()
	select {
	case ev := <-ch:
		t.Fatalf("%s: unexpected event %+v", label, ev)
	default:
	}
}
