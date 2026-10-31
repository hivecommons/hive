package logscrub

import (
	"context"
	"encoding"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
)

const (
	githubTokenPattern       = `(ghs_|ghp_|gho_|ghu_|ghr_|github_pat_)[A-Za-z0-9_]{10,}`
	jwtPattern               = `eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}`
	bearerTokenPattern       = `(?i)\bBearer\s+(?:[A-Za-z0-9._~+/=-]{16,}\b|%[A-Za-z]|\$[A-Za-z_][A-Za-z0-9_]*|\$\{[A-Za-z_][A-Za-z0-9_]*\}|\{\{[^\n]*\}\})`
	bearerPlaceholderPattern = `(?i)^\bBearer\s+(?:%[A-Za-z]|\$[A-Za-z_][A-Za-z0-9_]*|\$\{[A-Za-z_][A-Za-z0-9_]*\}|\{\{[^\n]*\}\})$`
)

// TokenPattern matches GitHub token forms (ghs_/ghp_/gho_/ghu_/ghr_/github_pat_)
// and JWT-shaped triples. It is exported so other packages (e.g. pkg/ioscan) can
// reuse the same secret-detection regex instead of duplicating it; keep this
// the single source of truth for these shapes.
var TokenPattern = regexp.MustCompile(githubTokenPattern + `|` + jwtPattern)

// secretPattern gives every scrubbed credential shape a stable category name.
// bin/contributor-relay.js declares the same closed category set, and the parity
// test in scrub_test.go fails when either implementation grows without the other.
type secretPattern struct {
	category string
	regexp   *regexp.Regexp
	skip     func(string) bool
}

var secretPatterns = []secretPattern{
	{category: "hive-canary", regexp: regexp.MustCompile(`HIVE-CANARY-[A-Fa-f0-9]{48}`)},
	{category: "github-token", regexp: regexp.MustCompile(githubTokenPattern)},
	{category: "jwt", regexp: regexp.MustCompile(jwtPattern)},
	{category: "aws-access-key", regexp: regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{category: "bearer-token", regexp: regexp.MustCompile(bearerTokenPattern), skip: regexp.MustCompile(bearerPlaceholderPattern).MatchString},
	{category: "private-key", regexp: regexp.MustCompile(`(?s)-----BEGIN\s+(?:(?:RSA|EC|OPENSSH|DSA)\s+)?PRIVATE\s+KEY-----.*?-----END\s+(?:(?:RSA|EC|OPENSSH|DSA)\s+)?PRIVATE\s+KEY-----`)},
	{category: "encrypted-private-key", regexp: regexp.MustCompile(`(?s)-----BEGIN\s+ENCRYPTED\s+PRIVATE\s+KEY-----.*?-----END\s+ENCRYPTED\s+PRIVATE\s+KEY-----`)},
	{category: "pgp-private-key", regexp: regexp.MustCompile(`(?s)-----BEGIN\s+PGP\s+PRIVATE\s+KEY\s+BLOCK-----.*?-----END\s+PGP\s+PRIVATE\s+KEY\s+BLOCK-----`)},
}

const redacted = "[REDACTED]"

type scrubConfig struct {
	markers bool
}

// Option changes how credential-shaped substrings are represented after
// scrubbing. The default replacement stays stable for logs and audit records.
type Option func(*scrubConfig)

// WithMarkers replaces each secret-shaped span with <redacted:<kind>>, where
// <kind> is the stable category name for the matched pattern. Use this only
// where the reader must know that source text was masked before they saw it.
func WithMarkers() Option {
	return func(c *scrubConfig) { c.markers = true }
}

// Handler wraps a slog.Handler and redacts credential-shaped substrings from
// messages and textual attribute values before forwarding to the inner handler.
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
func ScrubString(s string, opts ...Option) string {
	cfg := scrubConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	for _, p := range secretPatterns {
		replacement := redacted
		if cfg.markers {
			replacement = "<redacted:" + p.category + ">"
		}
		s = p.regexp.ReplaceAllStringFunc(s, func(match string) string {
			if p.skip != nil && p.skip(match) {
				return match
			}
			return replacement
		})
	}
	return s
}

func scrubAttr(a slog.Attr) slog.Attr {
	// Resolve lazy values here so the inner handler cannot render unsanitized
	// strings or groups after the scrubbing boundary.
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindAny {
		if text, ok := renderAny(a.Value.Any()); ok {
			return slog.String(a.Key, scrub(text))
		}
	}
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

// renderAny returns the text slog.TextHandler would render for a KindAny
// value that implements a text interface, using the precedence of Go 1.27+
// (TextAppender, then TextMarshaler, then fmt for error/Stringer). Go 1.26
// and earlier slog ignores TextAppender and renders MarshalText instead; the
// text differs there but is scrubbed either way. ok is false when the value
// has no text form and should pass through unchanged.
func renderAny(v any) (text string, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			// Mirror slog's appendValue: a nil pointer receiver that does not
			// guard itself renders as <nil>; anything else reports the panic
			// instead of crashing the caller.
			if rv := reflect.ValueOf(v); rv.Kind() == reflect.Pointer && rv.IsNil() {
				text, ok = "<nil>", true
				return
			}
			text, ok = fmt.Sprintf("!PANIC: %v", r), true
		}
	}()
	switch v := v.(type) {
	case encoding.TextAppender:
		b, err := v.AppendText(nil)
		if err != nil {
			// Do not let the inner handler render an unsanitized error.
			return "!ERROR:" + err.Error(), true
		}
		return string(b), true
	case encoding.TextMarshaler:
		b, err := v.MarshalText()
		if err != nil {
			return "!ERROR:" + err.Error(), true
		}
		return string(b), true
	case error, fmt.Stringer:
		return fmt.Sprint(v), true
	case []byte:
		return string(v), true
	case []string:
		return fmt.Sprintf("%v", v), true
	}
	// Composite payloads are rendered by the inner handler after the scrubbing
	// boundary, so render them here conservatively; scalars stay untouched.
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer && !rv.IsNil() {
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map, reflect.Struct, reflect.Slice, reflect.Array, reflect.Interface:
		return fmt.Sprintf("%+v", v), true
	}
	return "", false
}
