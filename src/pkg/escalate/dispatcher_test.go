package escalate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
		audits = append(audits, action+":"+sink+":"+detail)
	})
	d.backoff = []time.Duration{time.Millisecond}
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
	// page2 is the event evicted from the full queue; the audit must name it,
	// not page3, which was the one that got in.
	want := `escalation_drop_oldest:page:severity=page title="page2"`
	found := false
	for _, a := range audits {
		if a == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected %s audit, got %v", want, audits)
	}
}

// providerServer answers each request with the next status in codes, then
// 200 once they run out, and counts the requests it saw.
func providerServer(t *testing.T, codes ...int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(hits.Add(1))
		if n <= len(codes) {
			w.WriteHeader(codes[n-1])
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

type auditLog struct {
	mu    sync.Mutex
	lines []string
}

func (a *auditLog) record(action, detail, sink string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lines = append(a.lines, action+":"+sink+":"+detail)
}

func (a *auditLog) snapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.lines...)
}

func TestDispatcherRetriesThrottledDelivery(t *testing.T) {
	// ntfy.sh answering 429 to a burst: two throttled answers must not lose
	// the page when the provider recovers within the backoff.
	srv, hits := providerServer(t, http.StatusTooManyRequests, http.StatusTooManyRequests)
	var audits auditLog
	d := NewDispatcher(context.Background(), nil, audits.record)
	defer d.Stop()
	d.backoff = []time.Duration{time.Millisecond, time.Millisecond}
	d.Register(&NtfySink{URL: srv.URL}, SeverityPage, 4)
	d.Dispatch(Event{Severity: SeverityPage, Title: "Governor budget exhausted"})

	waitFor(t, func() bool { return hits.Load() >= 3 })
	time.Sleep(20 * time.Millisecond)
	if got := hits.Load(); got != 3 {
		t.Fatalf("provider requests = %d, want 3 (429, 429, 200)", got)
	}
	if lines := audits.snapshot(); len(lines) != 0 {
		t.Fatalf("audits = %v, want none: the third attempt delivered", lines)
	}
}

func TestDispatcherDoesNotRetryRejectedRequest(t *testing.T) {
	srv, hits := providerServer(t, http.StatusBadRequest, http.StatusBadRequest, http.StatusBadRequest)
	var audits auditLog
	d := NewDispatcher(context.Background(), nil, audits.record)
	defer d.Stop()
	d.backoff = []time.Duration{time.Millisecond, time.Millisecond}
	d.Register(&NtfySink{URL: srv.URL}, SeverityPage, 4)
	d.Dispatch(Event{Severity: SeverityPage, Title: "bad"})

	waitFor(t, func() bool { return len(audits.snapshot()) > 0 })
	if got := hits.Load(); got != 1 {
		t.Fatalf("provider requests = %d, want 1: a 400 rejects the request itself", got)
	}
	want := `escalation_delivery_failed:ntfy:severity=page title="bad" error=status 400`
	if lines := audits.snapshot(); len(lines) != 1 || lines[0] != want {
		t.Fatalf("audits = %v, want [%s]", lines, want)
	}
}

func TestDispatcherGivesUpAfterBoundedAttempts(t *testing.T) {
	srv, hits := providerServer(t, 503, 503, 503, 503, 503)
	var audits auditLog
	d := NewDispatcher(context.Background(), nil, audits.record)
	defer d.Stop()
	d.backoff = []time.Duration{time.Millisecond, time.Millisecond}
	d.Register(&NtfySink{URL: srv.URL}, SeverityPage, 4)
	d.Dispatch(Event{Severity: SeverityPage, Title: "down"})

	waitFor(t, func() bool { return len(audits.snapshot()) > 0 })
	if got := hits.Load(); got != 3 {
		t.Fatalf("provider requests = %d, want 3 (first attempt + len(backoff))", got)
	}
}

func TestDispatcherFailureAuditRedactsTopicURL(t *testing.T) {
	// A closed server gives a transport error, whose *url.Error text would
	// otherwise carry the full topic URL — the topic's only credential.
	srv := httptest.NewServer(http.NotFoundHandler())
	topicURL := srv.URL + "/hive-secret-topic-8f3a"
	srv.Close()
	var audits auditLog
	d := NewDispatcher(context.Background(), nil, audits.record)
	defer d.Stop()
	d.backoff = nil
	d.Register(&NtfySink{URL: topicURL}, SeverityPage, 4)
	d.Dispatch(Event{Severity: SeverityPage, Title: "page"})

	waitFor(t, func() bool { return len(audits.snapshot()) > 0 })
	lines := audits.snapshot()
	if !strings.HasPrefix(lines[0], "escalation_delivery_failed:ntfy:") {
		t.Fatalf("audits = %v, want delivery failure", lines)
	}
	if strings.Contains(lines[0], "hive-secret-topic-8f3a") {
		t.Fatalf("delivery failure audit leaks the topic URL: %s", lines[0])
	}
}

func TestRetryWait(t *testing.T) {
	step := time.Second
	cases := []struct {
		name string
		err  error
		want time.Duration
	}{
		{"non-status error uses the step", errors.New("dial"), step},
		{"shorter Retry-After uses the step", &statusError{code: 429, retryAfter: time.Millisecond}, step},
		{"longer Retry-After is honoured", &statusError{code: 429, retryAfter: 7 * time.Second}, 7 * time.Second},
		{"Retry-After is capped", &statusError{code: 429, retryAfter: time.Hour}, maxRetryAfter},
	}
	for _, tc := range cases {
		if got := retryWait(step, tc.err); got != tc.want {
			t.Errorf("%s: retryWait = %v, want %v", tc.name, got, tc.want)
		}
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
