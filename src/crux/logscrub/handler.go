package logscrub

import (
	"context"
	"log/slog"
	"regexp"
)

const (
	githubTokenPattern       = `(ghs_|ghp_|gho_|ghu_|ghr_|github_pat_)[A-Za-z0-9_]{10,}`
	jwtPattern               = `eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}`
	apiKeyPattern            = `(^|[^A-Za-z0-9._-])(sk-[A-Za-z0-9._\-]{6,})`
	slackWebhookURLPattern   = `https://hooks\.slack\.com/services/[A-Za-z0-9][A-Za-z0-9/_-]+`
	discordWebhookURLPattern = `https://discord(?:app)?\.com/api/webhooks/[0-9]+/[A-Za-z0-9_-]+`
	slackTokenPattern        = `xox[abpors]-[A-Za-z0-9-]{10,}`
	slackAppTokenPattern     = `xapp-[A-Za-z0-9-]{10,}`
	bearerTokenPattern       = `(?i)\bBearer\s+(?:[A-Za-z0-9._~+/=-]{16,}\b|%[A-Za-z]|\$[A-Za-z_][A-Za-z0-9_]*|\$\{[A-Za-z_][A-Za-z0-9_]*\}|\{\{[^\n]*\}\})`
	bearerPlaceholderPattern = `(?i)^\bBearer\s+(?:%[A-Za-z]|\$[A-Za-z_][A-Za-z0-9_]*|\$\{[A-Za-z_][A-Za-z0-9_]*\}|\{\{[^\n]*\}\})$`
	// basicAuthPattern matches an `Authorization: Basic <base64>` header value,
	// including the JSON and `key=value` spellings a log line carries it in.
	// The header name is required: a bare "Basic" followed by a word is prose.
	basicAuthPattern = `(?i)\bauthorization["']?\s*[:=]\s*["']?basic\s+[A-Za-z0-9+/_-]{4,}={0,2}`
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
	{category: "api-key", regexp: regexp.MustCompile(apiKeyPattern)},
	{category: "slack-webhook-url", regexp: regexp.MustCompile(slackWebhookURLPattern)},
	{category: "discord-webhook-url", regexp: regexp.MustCompile(discordWebhookURLPattern)},
	{category: "slack-token", regexp: regexp.MustCompile(slackTokenPattern), skip: lacksASCIIDigit},
	{category: "slack-app-token", regexp: regexp.MustCompile(slackAppTokenPattern), skip: lacksASCIIDigit},
	{category: "aws-access-key", regexp: regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{category: "basic-auth", regexp: regexp.MustCompile(basicAuthPattern)},
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
// GitHub tokens, AWS access keys, bearer tokens, Basic-auth headers, JWTs, and
// private-key blocks.
func ScrubString(s string, opts ...Option) string {
	return scrubString(s, nil, opts)
}

// ScrubStringCounted is ScrubString that also reports how many spans of each
// category it replaced, keyed by the category name used in markers. A read
// that must say what it redacted (the admin MCP log read) uses it; the map is
// nil when nothing was replaced.
func ScrubStringCounted(s string, opts ...Option) (string, map[string]int) {
	counts := map[string]int{}
	out := scrubString(s, counts, opts)
	if len(counts) == 0 {
		return out, nil
	}
	return out, counts
}

func scrubString(s string, counts map[string]int, opts []Option) string {
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
		if p.category == "api-key" {
			if counts != nil {
				if n := len(p.regexp.FindAllStringIndex(s, -1)); n > 0 {
					counts[p.category] += n
				}
			}
			s = p.regexp.ReplaceAllString(s, "${1}"+replacement)
			continue
		}
		s = p.regexp.ReplaceAllStringFunc(s, func(match string) string {
			if p.skip != nil && p.skip(match) {
				return match
			}
			if counts != nil {
				counts[p.category]++
			}
			return replacement
		})
	}
	return s
}

func lacksASCIIDigit(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return false
		}
	}
	return true
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
