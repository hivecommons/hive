package main

import (
	"context"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalate"
)

type recordingEscalationSink struct{ ch chan escalate.Event }

func (s *recordingEscalationSink) Name() string { return "recording" }
func (s *recordingEscalationSink) Deliver(ctx context.Context, ev escalate.Event) error {
	select {
	case s.ch <- ev:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func installTestEscalationDispatcher(t *testing.T) *recordingEscalationSink {
	t.Helper()
	sink := &recordingEscalationSink{ch: make(chan escalate.Event, 8)}
	d := escalate.NewDispatcher(context.Background(), hookTestLogger(), nil)
	d.Register(sink, escalate.SeverityInfo, 8)
	escalationRuntime.Lock()
	old := escalationRuntime.d
	escalationRuntime.d = d
	escalationRuntime.Unlock()
	t.Cleanup(func() {
		d.Stop()
		escalationRuntime.Lock()
		escalationRuntime.d = old
		escalationRuntime.Unlock()
	})
	return sink
}

func TestAgentPauseEmitterPagesThroughRealPausePath(t *testing.T) {
	sink := installTestEscalationDispatcher(t)
	mgr := agent.NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, hookTestLogger(), agent.ProjectContext{})
	installAgentPauseEmitter(mgr)
	if err := mgr.PauseBy("scanner", "dashboard-api", "operator maintenance", "owner"); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-sink.ch:
		if ev.Severity != escalate.SeverityPage || ev.Title != "Agent paused: scanner" {
			t.Fatalf("event = %+v, want page agent pause", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("agent pause did not page escalation dispatcher")
	}
}

func TestBudgetExhaustionAlertPagesEscalation(t *testing.T) {
	sink := installTestEscalationDispatcher(t)
	gov, srv, notifier := budgetAlertsFixture(t)
	gov.SetBudgetLimit(1000)
	gov.UpdateBudgetFromTotals(0, nil, nil)
	trans := gov.UpdateBudgetFromTotals(1000, nil, nil)
	applyBudgetAlerts(gov, trans, srv, notifier)
	select {
	case ev := <-sink.ch:
		if ev.Severity != escalate.SeverityPage || ev.Title != "Governor budget exhausted: kicks stopped" {
			t.Fatalf("event = %+v, want budget page", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("budget exhaustion did not page escalation dispatcher")
	}
}

func TestGovernorModeChangeDoesNotFakePausePage(t *testing.T) {
	sink := installTestEscalationDispatcher(t)
	gov := governorForHookTests(t)
	installGovernorModeChangeEmitter(gov)
	gov.Evaluate(25, 0, 0, 0)
	select {
	case ev := <-sink.ch:
		t.Fatalf("ordinary governor mode transition must not page: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}
