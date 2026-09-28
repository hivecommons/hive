package escalate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const defaultQueueSize = 32

// defaultRetryBackoff is the wait before each re-attempt of a failed
// delivery: three attempts in all, spread over a few seconds so a provider
// throttling a burst (ntfy.sh, Pushover answer 429) has time to recover.
var defaultRetryBackoff = []time.Duration{time.Second, 4 * time.Second}

// maxRetryAfter bounds how long a provider's Retry-After can hold a sink's
// worker. The worker is serial, so a longer wait would only move the loss to
// the queue's drop_oldest instead of this event.
const maxRetryAfter = 30 * time.Second

type AuditFunc func(action, detail, sink string)

type Dispatcher struct {
	ctx     context.Context
	cancel  context.CancelFunc
	logger  *slog.Logger
	audit   AuditFunc
	backoff []time.Duration
	mu      sync.RWMutex
	closed  bool
	sinks   []*worker
}

type worker struct {
	sink Sink
	min  Severity
	ch   chan Event
}

func NewDispatcher(ctx context.Context, logger *slog.Logger, audit AuditFunc) *Dispatcher {
	if ctx == nil {
		ctx = context.Background()
	}
	if logger == nil {
		logger = slog.Default()
	}
	child, cancel := context.WithCancel(ctx)
	return &Dispatcher{ctx: child, cancel: cancel, logger: logger, audit: audit, backoff: defaultRetryBackoff}
}

func (d *Dispatcher) Register(sink Sink, min Severity, queueSize int) {
	if d == nil || sink == nil || severityRank(min) == 0 {
		return
	}
	if queueSize <= 0 {
		queueSize = defaultQueueSize
	}
	w := &worker{sink: sink, min: min, ch: make(chan Event, queueSize)}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.sinks = append(d.sinks, w)
	d.mu.Unlock()
	go d.run(w)
}

func (d *Dispatcher) Stop() {
	if d == nil {
		return
	}
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		d.cancel()
	}
	d.mu.Unlock()
}

func (d *Dispatcher) Context() context.Context {
	if d == nil {
		return context.Background()
	}
	return d.ctx
}

func (d *Dispatcher) Dispatch(ev Event) {
	if d == nil || !ev.valid() {
		return
	}
	// Scrub once, on the way in, so every registered sink — and every future
	// one — receives text that has already been through the shared scrubber.
	ev = scrubEvent(ev)
	d.mu.RLock()
	if d.closed {
		d.mu.RUnlock()
		return
	}
	workers := append([]*worker(nil), d.sinks...)
	d.mu.RUnlock()
	for _, w := range workers {
		if !SeverityAtLeast(ev.Severity, w.min) {
			continue
		}
		select {
		case w.ch <- ev:
		default:
			select {
			case dropped := <-w.ch:
				d.record("escalation_drop_oldest", fmt.Sprintf("severity=%s title=%q", dropped.Severity, dropped.Title), w.sink.Name())
			default:
			}
			select {
			case w.ch <- ev:
			default:
				d.record("escalation_drop_newest", fmt.Sprintf("severity=%s title=%q", ev.Severity, ev.Title), w.sink.Name())
			}
		}
	}
}

func (d *Dispatcher) run(w *worker) {
	for {
		select {
		case <-d.ctx.Done():
			return
		case ev := <-w.ch:
			d.deliver(w, ev)
		}
	}
}

// deliver attempts ev on w's sink, re-attempting a retryable failure after
// each backoff step, and records the event as failed once attempts run out,
// the failure is permanent, or the dispatcher stops.
func (d *Dispatcher) deliver(w *worker, ev Event) {
	var err error
	for attempt := 0; ; attempt++ {
		if err = w.sink.Deliver(d.ctx, ev); err == nil {
			return
		}
		if attempt >= len(d.backoff) || !retryable(err) || d.ctx.Err() != nil {
			break
		}
		t := time.NewTimer(retryWait(d.backoff[attempt], err))
		select {
		case <-d.ctx.Done():
			t.Stop()
		case <-t.C:
		}
		if d.ctx.Err() != nil {
			break
		}
	}
	d.logger.Warn("escalation sink delivery failed", "sink", w.sink.Name(), "severity", ev.Severity, "error", err)
	d.record("escalation_delivery_failed", fmt.Sprintf("severity=%s title=%q error=%v", ev.Severity, ev.Title, err), w.sink.Name())
}

// retryable reports whether another attempt at the same delivery can
// succeed. Only a provider's explicit rejection of the request (a 4xx other
// than 408/429) is final; transport errors and non-HTTP sinks keep retrying.
func retryable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.retryable()
	}
	return true
}

// retryWait is the backoff step, stretched to the provider's Retry-After
// when it asks for longer, up to maxRetryAfter.
func retryWait(step time.Duration, err error) time.Duration {
	var se *statusError
	if errors.As(err, &se) && se.retryAfter > step {
		return min(se.retryAfter, maxRetryAfter)
	}
	return step
}

func (d *Dispatcher) record(action, detail, sink string) {
	if d.audit != nil {
		d.audit(action, detail, sink)
	}
	if d.logger != nil {
		d.logger.Warn(action, "sink", sink, "detail", detail)
	}
}
