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

// secretPattern is one named redaction category. The Category name is not
// cosmetic: it is the join key for the cross-language parity guard in
// redaction_parity_test.go, which asserts that every category Go redacts is
// also redacted by redactTokens() in bin/contributor-relay.sh. Before #5478
// the two implementations were hand-maintained lists in different languages
// with nothing failing when they drifted, and the relay had silently fallen
// four categories behind — JWTs, PEM private-key blocks, Bearer values and
// AWS access keys all reached the hub unredacted.
//
// Adding a pattern here without a matching HIVE_REDACTION_CATEGORY entry in
// the relay (or a declared exception in the parity test) fails that test.
type secretPattern struct {
	// Category is the stable identifier shared with the relay. Renaming it is
	// a breaking change to the parity guard on both sides.
	Category string
	Re       *regexp.Regexp
}

// Category identifiers. Declared as constants so a typo is a compile error on
// the Go side rather than a silently-unmatched string in the parity test.
const (
	CategoryHiveCanary   = "hive_canary"
	CategoryGitHubToken  = "github_token"
	CategoryJWT          = "jwt"
	CategoryAWSAccessKey = "aws_access_key"
	CategoryBearer       = "bearer"
	CategoryPEMPrivate   = "pem_private_key"
	CategoryPEMEncrypted = "pem_encrypted_private_key"
	CategoryPGPPrivate   = "pgp_private_key"
)

// jwtPattern is the JWT third of TokenPattern, broken out so the parity test
// can name it as its own category. TokenPattern keeps both alternatives fused
// because pkg/ioscan and pkg/pushbroker consume it as a single "is this string
// credential-shaped?" probe.
var jwtPattern = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}`)

// githubTokenPattern is the GitHub-prefix half of TokenPattern, likewise broken
// out for category naming.
var githubTokenPattern = regexp.MustCompile(`(ghs_|ghp_|gho_|ghu_|ghr_|github_pat_)[A-Za-z0-9_]{10,}`)

var secretPatterns = []secretPattern{
	{CategoryHiveCanary, regexp.MustCompile(`HIVE-CANARY-[A-Fa-f0-9]{48}`)},
	{CategoryGitHubToken, githubTokenPattern},
	{CategoryJWT, jwtPattern},
	{CategoryAWSAccessKey, regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{CategoryBearer, regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{16,}\b`)},
	{CategoryPEMPrivate, regexp.MustCompile(`(?s)-----BEGIN\s+(?:(?:RSA|EC|OPENSSH|DSA)\s+)?PRIVATE\s+KEY-----.*?-----END\s+(?:(?:RSA|EC|OPENSSH|DSA)\s+)?PRIVATE\s+KEY-----`)},
	{CategoryPEMEncrypted, regexp.MustCompile(`(?s)-----BEGIN\s+ENCRYPTED\s+PRIVATE\s+KEY-----.*?-----END\s+ENCRYPTED\s+PRIVATE\s+KEY-----`)},
	{CategoryPGPPrivate, regexp.MustCompile(`(?s)-----BEGIN\s+PGP\s+PRIVATE\s+KEY\s+BLOCK-----.*?-----END\s+PGP\s+PRIVATE\s+KEY\s+BLOCK-----`)},
}

// Categories returns the redaction category names Go covers, in declaration
// order. The parity test compares this against the relay's declared set.
func Categories() []string {
	out := make([]string, 0, len(secretPatterns))
	for _, p := range secretPatterns {
		out = append(out, p.Category)
	}
	return out
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
		s = p.Re.ReplaceAllString(s, redacted)
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
