package logscrub

import (
	"context"
	"log/slog"
	"regexp"
)

// TokenPattern matches GitHub token forms (ghs_/ghp_/gho_/ghu_/ghr_/github_pat_)
// and JWT-shaped triples. It is exported so other packages (e.g. pkg/ioscan) can
// reuse the same secret-detection regex instead of duplicating it; keep this
// the single source of truth for these shapes.
var TokenPattern = regexp.MustCompile(
	`(ghs_|ghp_|gho_|ghu_|ghr_|github_pat_)[A-Za-z0-9_]{10,}` +
		`|eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}`,
)

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`HIVE-CANARY-[A-Fa-f0-9]{48}`),
	TokenPattern,
	regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`),
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{16,}\b`),
	regexp.MustCompile(`(?s)-----BEGIN\s+(?:(?:RSA|EC|OPENSSH|DSA)\s+)?PRIVATE\s+KEY-----.*?-----END\s+(?:(?:RSA|EC|OPENSSH|DSA)\s+)?PRIVATE\s+KEY-----`),
	regexp.MustCompile(`(?s)-----BEGIN\s+ENCRYPTED\s+PRIVATE\s+KEY-----.*?-----END\s+ENCRYPTED\s+PRIVATE\s+KEY-----`),
	regexp.MustCompile(`(?s)-----BEGIN\s+PGP\s+PRIVATE\s+KEY\s+BLOCK-----.*?-----END\s+PGP\s+PRIVATE\s+KEY\s+BLOCK-----`),
}

const redacted = "[REDACTED]"

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
	return ScrubString(s)
}

// ScrubString redacts credential-shaped substrings from text before it is
// logged or published. Keep patterns here as the single reusable scrubber for
// GitHub tokens, AWS access keys, bearer tokens, JWTs, and private-key blocks.
func ScrubString(s string) string {
	for _, p := range secretPatterns {
		s = p.ReplaceAllString(s, redacted)
	}
	return s
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
