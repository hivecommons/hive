package escalate

import (
	"context"
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

type AuditFunc func(action, detail, sink string)

type Dispatcher struct {
	ctx          context.Context
	cancel       context.CancelFunc
	logger       *slog.Logger
	audit        AuditFunc
	drainTimeout time.Duration
	stopping     chan struct{}
	workers      sync.WaitGroup
	mu           sync.RWMutex
	closed       bool
	sinks        []*worker
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
	return &Dispatcher{ctx: child, cancel: cancel, logger: logger, audit: audit, drainTimeout: defaultDrainTimeout, stopping: make(chan struct{})}
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
	close(d.stopping)
	d.mu.Unlock()
	drained := make(chan struct{})
	go func() {
		d.workers.Wait()
		close(drained)
	}()
	go func() {
		t := time.NewTimer(d.drainTimeout)
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
			case <-w.ch:
				d.record("escalation_drop_oldest", fmt.Sprintf("severity=%s title=%q", ev.Severity, ev.Title), w.sink.Name())
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

func (d *Dispatcher) deliver(w *worker, ev Event) {
	if err := w.sink.Deliver(d.ctx, ev); err != nil {
		if retryErr := w.sink.Deliver(d.ctx, ev); retryErr != nil {
			d.logger.Warn("escalation sink delivery failed", "sink", w.sink.Name(), "severity", ev.Severity, "error", retryErr)
			d.record("escalation_delivery_failed", fmt.Sprintf("severity=%s title=%q error=%v", ev.Severity, ev.Title, retryErr), w.sink.Name())
		}
	}
}

func (d *Dispatcher) record(action, detail, sink string) {
	if d.audit != nil {
		d.audit(action, detail, sink)
	}
	if d.logger != nil {
		d.logger.Warn(action, "sink", sink, "detail", detail)
	}
}
