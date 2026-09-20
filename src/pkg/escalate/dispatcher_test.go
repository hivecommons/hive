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
	// gate, when non-nil, blocks the first Deliver until closed and signals
	// entered once the worker is parked inside it. That lets a test hold the
	// consumer busy so a subsequent Dispatch deterministically hits a full
	// queue instead of racing the consumer for it.
	gate    chan struct{}
	entered chan struct{}
	gated   bool
}

func (s *fakeSink) Name() string { return s.name }
func (s *fakeSink) Deliver(ctx context.Context, ev Event) error {
	_ = ctx
	s.mu.Lock()
	if s.gate != nil && !s.gated {
		s.gated = true
		s.mu.Unlock()
		close(s.entered)
		<-s.gate
		s.mu.Lock()
	}
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
	var auditMu sync.Mutex
	var audits []string
	d := NewDispatcher(ctx, nil, func(action, detail, sink string) {
		auditMu.Lock()
		defer auditMu.Unlock()
		audits = append(audits, action+":"+sink)
	})
	decision := &fakeSink{name: "decision", fail: 1}
	page := &fakeSink{name: "page", gate: make(chan struct{}), entered: make(chan struct{})}
	d.Register(decision, SeverityDecision, 4)
	d.Register(page, SeverityPage, 1)
	d.Dispatch(Event{Severity: SeverityInfo, Title: "info"})
	d.Dispatch(Event{Severity: SeverityDecision, Title: "decision"})

	// page1 parks the page worker inside Deliver; page2 then fills the depth-1
	// queue and page3 must evict it — no timing assumption about when the
	// consumer wakes.
	d.Dispatch(Event{Severity: SeverityPage, Title: "page1"})
	select {
	case <-page.entered:
	case <-time.After(time.Second):
		t.Fatal("page worker never entered Deliver")
	}
	d.Dispatch(Event{Severity: SeverityPage, Title: "page2"})
	d.Dispatch(Event{Severity: SeverityPage, Title: "page3"})
	close(page.gate)

	waitFor(t, func() bool { return decision.count() >= 4 && page.count() >= 2 })
	if decision.count() != 4 {
		t.Fatalf("decision deliveries = %d, want 4 (decision + 3 pages; first attempt retried)", decision.count())
	}
	if page.count() != 2 {
		t.Fatalf("page deliveries = %d, want 2 (page1, page3)", page.count())
	}
	page.mu.Lock()
	titles := []string{page.evs[0].Title, page.evs[1].Title}
	page.mu.Unlock()
	if titles[0] != "page1" || titles[1] != "page3" {
		t.Fatalf("page titles = %v, want [page1 page3]", titles)
	}
	auditMu.Lock()
	defer auditMu.Unlock()
	found := false
	for _, a := range audits {
		if a == "escalation_drop_oldest:page" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected escalation_drop_oldest:page audit, got %v", audits)
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
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-timer.C:
			t.Fatal("condition not met")
		case <-ticker.C:
			if ok() {
				return
			}
		}
	}
}
