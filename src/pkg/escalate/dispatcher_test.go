package escalate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeSink struct {
	name string
	mu   sync.Mutex
	evs  []Event
	fail int
}

func (s *fakeSink) Name() string { return s.name }
func (s *fakeSink) Deliver(ctx context.Context, ev Event) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail > 0 {
		s.fail--
		return errors.New("boom")
	}
	s.evs = append(s.evs, ev)
	return nil
}
func (s *fakeSink) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.evs) }

func TestDispatcherSeverityRetryAndDrop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var audits []string
	d := NewDispatcher(ctx, nil, func(action, detail, sink string) { audits = append(audits, action+":"+sink) })
	decision := &fakeSink{name: "decision", fail: 1}
	page := &fakeSink{name: "page"}
	d.Register(decision, SeverityDecision, 4)
	d.Register(page, SeverityPage, 1)
	d.Dispatch(Event{Severity: SeverityInfo, Title: "info"})
	d.Dispatch(Event{Severity: SeverityDecision, Title: "decision"})
	d.Dispatch(Event{Severity: SeverityPage, Title: "page1"})
	d.Dispatch(Event{Severity: SeverityPage, Title: "page2"})
	waitFor(t, func() bool { return decision.count() >= 2 && page.count() >= 1 })
	if decision.count() != 3 {
		t.Fatalf("decision deliveries = %d, want 3", decision.count())
	}
	if page.count() != 1 {
		t.Fatalf("page deliveries = %d, want 1", page.count())
	}
	if len(audits) == 0 {
		t.Fatal("expected drop audit")
	}
}

func TestDispatcherInvalidAndStoppedAreNoops(t *testing.T) {
	sink := &fakeSink{name: "s"}
	d := NewDispatcher(context.Background(), nil, nil)
	d.Register(sink, SeverityInfo, 1)
	d.Dispatch(Event{Severity: "bad", Title: "x"})
	d.Dispatch(Event{Severity: SeverityInfo})
	d.Stop()
	d.Dispatch(Event{Severity: SeverityInfo, Title: "x"})

	time.Sleep(20 * time.Millisecond)
	if sink.count() != 0 {
		t.Fatalf("deliveries after noops = %d", sink.count())
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met")
}
