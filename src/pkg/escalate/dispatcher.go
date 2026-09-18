package escalate

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

const defaultQueueSize = 32

type AuditFunc func(action, detail, sink string)

type Dispatcher struct {
	ctx    context.Context
	cancel context.CancelFunc
	logger *slog.Logger
	audit  AuditFunc
	mu     sync.RWMutex
	closed bool
	sinks  []*worker
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
	return &Dispatcher{ctx: child, cancel: cancel, logger: logger, audit: audit}
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

func (d *Dispatcher) Dispatch(ev Event) {
	if d == nil || !ev.valid() {
		return
	}
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
	for {
		select {
		case <-d.ctx.Done():
			return
		case ev := <-w.ch:
			if err := w.sink.Deliver(d.ctx, ev); err != nil {
				if retryErr := w.sink.Deliver(d.ctx, ev); retryErr != nil {
					d.logger.Warn("escalation sink delivery failed", "sink", w.sink.Name(), "severity", ev.Severity, "error", retryErr)
					d.record("escalation_delivery_failed", fmt.Sprintf("severity=%s title=%q error=%v", ev.Severity, ev.Title, retryErr), w.sink.Name())
				}
			}
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
