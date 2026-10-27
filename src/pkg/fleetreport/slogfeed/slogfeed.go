// Package slogfeed tees WARN and ERROR log records into the fleet-report
// runtime tracker as fleetreport.ClassUnclassifiedRepeated observations, so a
// log line that keeps repeating becomes a self-report even when no hook site
// was ever written for it. The incident that motivated it: a relay watcher
// retried eleven malformed request files for six hours, logging the same
// warning 175 times, and the self-report system — an allow-list fed only by
// hand-placed Observe calls — never noticed.
//
// The handler wraps the process's root slog.Handler. It never blocks or
// fails logging: the wrapped handler runs first and its error is returned
// unchanged; the observation is a bounded, lock-free-on-the-hot-path extra
// that only runs for WARN and above.
package slogfeed

import (
	"context"
	"log/slog"
	"strings"

	"github.com/hivecommons/hive/pkg/fleetreport"
)

const (
	// maxKeyBytes bounds the normalised message used as the condition key so
	// a pathological log line cannot bloat the tracker.
	maxKeyBytes = 200
	// maxDiagnosticsBytes bounds the attribute excerpt attached to the
	// observation; the tracker scrubs and bounds again on storage.
	maxDiagnosticsBytes = 512
	// minLevel is the lowest level the feed observes.
	minLevel = slog.LevelWarn
)

// Observer is the tracker entry point the feed reports into; it matches
// fleetreport.Observe.
type Observer func(class, key, message, diagnostics string)

// Handler is a slog.Handler that forwards every record to the wrapped handler
// and additionally observes WARN+ records on the fleet-report tracker.
type Handler struct {
	next    slog.Handler
	observe Observer
	// attrs are the attributes bound via WithAttrs, so a logger built as
	// logger.With("agent", name) still contributes them to diagnostics.
	attrs []slog.Attr
}

// New wraps next so WARN+ records are observed on the default tracker.
func New(next slog.Handler) *Handler {
	return NewWithObserver(next, fleetreport.Observe)
}

// NewWithObserver is New with an explicit observer (tests).
func NewWithObserver(next slog.Handler, observe Observer) *Handler {
	if observe == nil {
		observe = fleetreport.Observe
	}
	return &Handler{next: next, observe: observe}
}

// Enabled defers to the wrapped handler.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle forwards the record, then observes it when it is WARN or above.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	err := h.next.Handle(ctx, r)
	if r.Level >= minLevel {
		h.feed(r)
	}
	return err
}

// WithAttrs returns a handler that carries attrs on every record.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &Handler{next: h.next.WithAttrs(attrs), observe: h.observe, attrs: merged}
}

// WithGroup returns a handler that groups subsequent attributes.
func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{next: h.next.WithGroup(name), observe: h.observe, attrs: h.attrs}
}

// Key is the condition key the feed derives from a log message: the
// normalised message (numbers, hashes, paths and quoted strings collapsed),
// bounded. Empty when the message should not be observed at all.
func Key(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" || fleetreport.IsBenignMessage(msg) || fleetreport.MatchesExplicitClass(msg) {
		return ""
	}
	key := fleetreport.NormalizeMessage(msg)
	if len(key) > maxKeyBytes {
		key = key[:maxKeyBytes]
	}
	return key
}

func (h *Handler) feed(r slog.Record) {
	if expectedKickHold(r) {
		return
	}
	class := fleetreport.ClassUnclassifiedRepeated
	key := Key(r.Message)
	if key == "" {
		msg := strings.TrimSpace(r.Message)
		if fleetreport.IsBenignMessage(msg) {
			return
		}
		if class = fleetreport.LogFedClass(msg); class == "" {
			return
		}
		key = fleetreport.NormalizeMessage(msg)
		if len(key) > maxKeyBytes {
			key = key[:maxKeyBytes]
		}
	}
	h.observe(class, key, r.Message, diagnostics(r, h.attrs))
}

// expectedKickHold reports whether a "failed to send kick" record carries the
// deliberate post-restart kick cooldown refusal (#7363). The hold is the
// breaker working as designed, not a repeated runtime defect (#14422); other
// kick send failures stay reportable.
func expectedKickHold(r slog.Record) bool {
	if strings.TrimSpace(r.Message) != "failed to send kick" {
		return false
	}
	held := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "error" && strings.Contains(a.Value.String(), "kick held after restart") {
			held = true
			return false
		}
		return true
	})
	return held
}

// diagnostics renders "level attr=value …" from the bound and record
// attributes, bounded. It runs only for WARN+ records that passed the
// filters, so the allocation is paid on the rare path.
func diagnostics(r slog.Record, bound []slog.Attr) string {
	var b strings.Builder
	b.WriteString(r.Level.String())
	write := func(a slog.Attr) bool {
		if b.Len() >= maxDiagnosticsBytes {
			return false
		}
		b.WriteByte(' ')
		b.WriteString(a.Key)
		b.WriteByte('=')
		b.WriteString(a.Value.String())
		return true
	}
	for _, a := range bound {
		if !write(a) {
			break
		}
	}
	r.Attrs(write)
	out := b.String()
	if len(out) > maxDiagnosticsBytes {
		out = out[:maxDiagnosticsBytes]
	}
	return out
}
