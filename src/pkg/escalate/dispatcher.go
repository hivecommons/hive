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

// defaultDrainTimeout bounds how long a stopped dispatcher keeps delivering
// the events already queued before it cancels in-flight deliveries and
// audits whatever is left as escalation_dropped_on_stop.
const defaultDrainTimeout = 30 * time.Second

// defaultRetryBackoff is the wait before each re-attempt of a failed
// delivery: three attempts in all, spread over a few seconds so a provider
// throttling a burst (ntfy.sh, Pushover answer 429) has time to recover.
// Once Stop has begun draining, no wait runs past the drain deadline.
var defaultRetryBackoff = []time.Duration{time.Second, 4 * time.Second}

// maxRetryAfter bounds how long a provider's Retry-After can hold a sink's
// worker. The worker is serial, so a longer wait would only move the loss to
// the queue's drop_oldest instead of this event.
const maxRetryAfter = 30 * time.Second

type AuditFunc func(action, detail, sink string)

type Dispatcher struct {
	ctx          context.Context
	cancel       context.CancelFunc
	logger       *slog.Logger
	audit        AuditFunc
	backoff      []time.Duration
	drainTimeout time.Duration
	// drainDeadline is set by Stop before it closes stopping; read it only
	// after receiving from stopping.
	drainDeadline time.Time
	stopping      chan struct{}
	workers       sync.WaitGroup
	mu            sync.RWMutex
	closed        bool
	sinks         []*worker
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
	return &Dispatcher{ctx: child, cancel: cancel, logger: logger, audit: audit, backoff: defaultRetryBackoff, drainTimeout: defaultDrainTimeout, stopping: make(chan struct{})}
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
	d.workers.Add(1)
	d.mu.Unlock()
	go d.run(w)
}

// Stop closes intake and returns without blocking. Workers keep delivering
// the events already queued; once every queue is empty, or after the drain
// timeout, the dispatcher context is cancelled. Anything still queued at
// that point is audited as escalation_dropped_on_stop, never silently lost.
// A retry wait in progress or begun during the drain never runs past the
// drain deadline: the event gets its final attempt at once instead.
func (d *Dispatcher) Stop() {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	d.drainDeadline = time.Now().Add(d.drainTimeout)
	close(d.stopping)
	d.mu.Unlock()
	drained := make(chan struct{})
	go func() {
		d.workers.Wait()
		close(drained)
	}()
	go func() {
		t := time.NewTimer(time.Until(d.drainDeadline))
		defer t.Stop()
		select {
		case <-drained:
		case <-t.C:
		case <-d.ctx.Done():
		}
		d.cancel()
	}()
}

func (d *Dispatcher) Context() context.Context {
	if d == nil {
		return context.Background()
	}
	return d.ctx
}

// Admits reports whether Dispatch would hand an event of severity sev to at
// least one registered sink. Dispatch delivers asynchronously and returns
// nothing, so a producer that latches "already escalated" checks this first
// rather than spending its latch on an event every sink's floor would drop.
func (d *Dispatcher) Admits(sev Severity) bool {
	if d == nil {
		return false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return false
	}
	for _, w := range d.sinks {
		if SeverityAtLeast(sev, w.min) {
			return true
		}
	}
	return false
}

func (d *Dispatcher) Dispatch(ev Event) {
	if d == nil || !ev.valid() {
		return
	}
	// Scrub once, on the way in, so every registered sink — and every future
	// one — receives text that has already been through the shared scrubber.
	ev = scrubEvent(ev)
	// Hold the read lock through the enqueue: Stop takes the write lock to
	// close intake, so every event a worker's drain can miss is one Dispatch
	// saw as closed and audited here.
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		d.record("escalation_dropped_on_stop", fmt.Sprintf("severity=%s title=%q", ev.Severity, ev.Title), "dispatcher")
		return
	}
	for _, w := range d.sinks {
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
	defer d.workers.Done()
	for {
		select {
		case <-d.ctx.Done():
			d.dropQueued(w)
			return
		case <-d.stopping:
			d.drain(w)
			return
		case ev := <-w.ch:
			if d.ctx.Err() != nil {
				d.recordDropped(w, ev)
				continue
			}
			d.deliver(w, ev)
		}
	}
}

// drain delivers what is left in w's queue after Stop. No new events can
// arrive (intake is closed), so an empty queue means the worker is done.
func (d *Dispatcher) drain(w *worker) {
	for {
		if d.ctx.Err() != nil {
			d.dropQueued(w)
			return
		}
		select {
		case ev := <-w.ch:
			d.deliver(w, ev)
		default:
			return
		}
	}
}

// dropQueued audits every event left in w's queue once the dispatcher can no
// longer deliver it.
func (d *Dispatcher) dropQueued(w *worker) {
	for {
		select {
		case ev := <-w.ch:
			d.recordDropped(w, ev)
		default:
			return
		}
	}
}

func (d *Dispatcher) recordDropped(w *worker, ev Event) {
	d.record("escalation_dropped_on_stop", fmt.Sprintf("severity=%s title=%q", ev.Severity, ev.Title), w.sink.Name())
}

// deliver attempts ev on w's sink, re-attempting a retryable failure after
// each backoff step, and records the event as failed once attempts run out,
// the failure is permanent, or the dispatcher stops. A wait cut short by the
// drain deadline makes the next attempt the last.
func (d *Dispatcher) deliver(w *worker, ev Event) {
	var err error
	last := false
	for attempt := 0; ; attempt++ {
		if err = w.sink.Deliver(d.ctx, ev); err == nil {
			return
		}
		if last || attempt >= len(d.backoff) || !retryable(err) || d.ctx.Err() != nil {
			break
		}
		last = !d.retryPause(retryWait(d.backoff[attempt], err))
		if d.ctx.Err() != nil {
			break
		}
	}
	d.logger.Warn("escalation sink delivery failed", "sink", w.sink.Name(), "severity", ev.Severity, "error", err)
	d.record("escalation_delivery_failed", fmt.Sprintf("severity=%s title=%q error=%v", ev.Severity, ev.Title, err), w.sink.Name())
}

// retryPause waits out one retry backoff and reports whether it ran in full.
// It ends early when the dispatcher context is cancelled, or when Stop's
// drain has begun (before or during the wait) and the wait would end past
// the drain deadline; the caller then makes its final attempt immediately.
func (d *Dispatcher) retryPause(wait time.Duration) bool {
	end := time.Now().Add(wait)
	t := time.NewTimer(wait)
	defer t.Stop()
	stopping := d.stopping
	for {
		select {
		case <-d.ctx.Done():
			return false
		case <-t.C:
			return true
		case <-stopping:
			if end.After(d.drainDeadline) {
				return false
			}
			stopping = nil
		}
	}
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
