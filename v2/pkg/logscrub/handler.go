package logscrub

import (
	"context"
	"log/slog"
	"regexp"
)

var tokenPattern = regexp.MustCompile(
	`(ghs_|ghp_|gho_|github_pat_)[A-Za-z0-9_]{10,}` +
		`|eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}`,
)

const redacted = "[REDACTED]"

// Scrub removes credential-shaped values before a diagnostic crosses a
// structured API boundary such as MCP. Callers should still keep the result
// bounded and avoid treating arbitrary subprocess output as trusted data.
func Scrub(value string) string {
	return scrub(value)
}

// Handler wraps a slog.Handler and redacts GitHub token patterns from string
// attribute values before forwarding to the inner handler.
type Handler struct {
	inner slog.Handler
}

func NewHandler(inner slog.Handler) *Handler {
	return &Handler{inner: inner}
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	scrubbed := slog.NewRecord(r.Time, r.Level, scrub(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		scrubbed.AddAttrs(scrubAttr(a))
		return true
	})
	return h.inner.Handle(ctx, scrubbed)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = scrubAttr(a)
	}
	return &Handler{inner: h.inner.WithAttrs(out)}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{inner: h.inner.WithGroup(name)}
}

func scrub(s string) string {
	return tokenPattern.ReplaceAllString(s, redacted)
}

func scrubAttr(a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindString {
		return slog.String(a.Key, scrub(a.Value.String()))
	}
	if a.Value.Kind() == slog.KindGroup {
		attrs := a.Value.Group()
		out := make([]slog.Attr, len(attrs))
		for i, ga := range attrs {
			out[i] = scrubAttr(ga)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	}
	return a
}
