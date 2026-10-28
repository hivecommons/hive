// Package promptsrc resolves an agent's kick prompt from an external source
// (currently a GitHub repo file) at kick time, with graceful fallback so a
// transient fetch failure never crashes or blanks a kick.
//
// Design (resolve-at-kick, not bake-at-create):
//
// The kick-template pipeline (pkg/scheduler) already loads the template fresh on
// every kick, so a live GitHub fetch fits the existing model — an operator can
// edit the prompt on their fork and the next kick picks it up, no redeploy. The
// trade-off is a failure mode the baked approach doesn't have: the repo can be
// unreachable at kick time. This package handles that by caching the last
// successfully fetched body per source and returning it (then the caller's inline
// template) on any error. The very first kick after a cold start, before any
// successful fetch, falls through to the caller's inline/baked template — which
// is why callers also bake a copy at agent-create time.
//
// Security: fetching is gated by an Allow func the caller supplies from the
// SEED-only config policy (see config.Config.GitHubPromptAllowed). A source whose
// "owner/repo" slug is not allowlisted is never fetched, so a user-writable
// dashboard overlay cannot point an agent at an arbitrary repo the App is
// installed on. This package performs no network I/O of its own beyond the
// injected Fetcher, and never touches tokens directly.
package promptsrc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultMaxPromptBytes caps how large a fetched prompt body may be
// (hivecommons/hive#12130). A kick prompt is a markdown template; the largest
// shipped policy (src/policies/scanner-automerge.md) is ~33 KiB, so 96 KiB
// leaves better than 2x headroom for a real template while refusing a
// mis-pointed prompt_source before it costs a ~100K-token kick. This is a token
// budget, not just a memory bound — the old 512 KiB cap was the latter.
//
// A body over the cap is REJECTED whole, never truncated: a half-policy is a
// policy with its closing rules cut off, and that is worse than no remote
// policy at all. The resolver falls back to its last-known-good cached copy,
// then to the caller's inline template, and logs the rejection with the
// source and the observed size. TestShippedTemplatesFitUnderCap pins the
// headroom against every shipped template.
const DefaultMaxPromptBytes = 96 * 1024 // 96 KiB

// MaxPromptBytesEnv names the environment variable that overrides
// DefaultMaxPromptBytes. It accepts a positive byte count (e.g. "131072") or a
// value with a KiB/MiB suffix ("128KiB", "1MiB"). Unset, blank, or
// unparseable values leave the default in force.
const MaxPromptBytesEnv = "HIVE_PROMPT_SOURCE_MAX_BYTES"

// MaxPromptBytes returns the effective cap: MaxPromptBytesEnv when set to a
// positive size, else DefaultMaxPromptBytes.
func MaxPromptBytes() int {
	if n, ok := parseSizeBytes(os.Getenv(MaxPromptBytesEnv)); ok {
		return n
	}
	return DefaultMaxPromptBytes
}

// parseSizeBytes parses "65536", "64KiB", "64KB", "1MiB" or "1MB" into a byte
// count. Anything else, or a non-positive result, reports ok=false.
func parseSizeBytes(v string) (int, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	mult := 1
	upper := strings.ToUpper(v)
	switch {
	case strings.HasSuffix(upper, "MIB"), strings.HasSuffix(upper, "MB"):
		mult = 1024 * 1024
		v = v[:len(v)-len(upper[strings.LastIndex(upper, "M"):])]
	case strings.HasSuffix(upper, "KIB"), strings.HasSuffix(upper, "KB"):
		mult = 1024
		v = v[:len(v)-len(upper[strings.LastIndex(upper, "K"):])]
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 || n > (1<<31-1)/mult {
		return 0, false
	}
	return n * mult, true
}

// ErrPromptTooLarge is returned (wrapped) by FetchOnce when the fetched body
// exceeds the cap, so the dashboard can tell the operator the file was refused
// whole rather than silently baking a cut-off policy.
var ErrPromptTooLarge = errors.New("prompt source exceeds size cap")

// oversizeError describes a rejected body with the source, the size seen and
// the cap in force — the three things an operator needs to fix it.
func oversizeError(src Source, size, limit int) error {
	return fmt.Errorf("%w: %s:%s is %d bytes, cap is %d bytes (%s to raise); rejected whole, not truncated",
		ErrPromptTooLarge, src.Slug(), src.Path, size, limit, MaxPromptBytesEnv)
}

// defaultFetchTimeout bounds a single live fetch so a slow/hung GitHub API call
// cannot stall a kick. On timeout the resolver falls back to the cache/inline.
const defaultFetchTimeout = 8 * time.Second

// Source is the minimal, config-package-agnostic description of a GitHub prompt
// source. The config.PromptSourceConfig is adapted into this at the call site.
type Source struct {
	Owner string
	Repo  string
	Path  string
	Ref   string // optional branch/tag/SHA; "" = default branch
}

// Slug is the "owner/repo" identifier used for allowlist matching and cache keys.
func (s Source) Slug() string {
	if s.Owner == "" || s.Repo == "" {
		return ""
	}
	return s.Owner + "/" + s.Repo
}

// key is the full cache key including path and ref, so two agents reading
// different files from the same repo don't share a cache entry.
func (s Source) key() string {
	return strings.Join([]string{s.Owner, s.Repo, s.Path, s.Ref}, "\x00")
}

// valid reports whether the source is fully specified.
func (s Source) valid() bool {
	return s.Owner != "" && s.Repo != "" && s.Path != ""
}

// Fetcher reads a file's decoded contents from a repo at an optional ref. It is
// satisfied by *github.Client.GetFileContentRef, and stubbed in tests.
type Fetcher interface {
	GetFileContentRef(ctx context.Context, owner, repo, path, ref string) (string, error)
}

// AllowFunc reports whether a given "owner/repo" slug may be fetched. Callers
// wire this to the seed-only policy (config.Config.GitHubPromptAllowed).
type AllowFunc func(slug string) bool

// Logger is the tiny logging surface this package needs, satisfied by *slog.Logger.
type Logger interface {
	Warn(msg string, args ...any)
	Info(msg string, args ...any)
}

// Resolver fetches prompt bodies live and remembers the last good value per
// source so a later failure can fall back to it. It is safe for concurrent use.
type Resolver struct {
	fetcher Fetcher
	allow   AllowFunc
	logger  Logger
	timeout time.Duration
	// maxBytes is the cap a fetched body must fit under to be used at all.
	maxBytes int

	mu    sync.RWMutex
	cache map[string]string // source key -> last-known-good body
}

// NewResolver builds a Resolver. fetcher may be nil (e.g. token-mode boot without
// an App client), in which case every resolution reports a miss and the caller
// falls back to its inline template. allow must be non-nil; a nil allow denies
// everything (fail closed). logger may be nil.
func NewResolver(fetcher Fetcher, allow AllowFunc, logger Logger) *Resolver {
	if allow == nil {
		allow = func(string) bool { return false }
	}
	return &Resolver{
		fetcher:  fetcher,
		allow:    allow,
		logger:   logger,
		timeout:  defaultFetchTimeout,
		maxBytes: MaxPromptBytes(),
		cache:    map[string]string{},
	}
}

// SetMaxBytes overrides the size cap for this resolver. Non-positive values
// are ignored so a misconfiguration cannot disable the bound.
func (r *Resolver) SetMaxBytes(n int) {
	if n > 0 {
		r.maxBytes = n
	}
}

// MaxBytes reports the cap this resolver enforces.
func (r *Resolver) MaxBytes() int { return r.maxBytes }

// Result carries a resolved prompt body plus provenance for logging/UI. Ok is
// false when nothing could be resolved (not set, denied, no fetcher, and no
// cached value) — the caller should then use its inline template.
type Result struct {
	Body   string
	Ok     bool
	Source string // provenance: "github:owner/repo@ref", "github-cache:...", or "denied"/"unset"/"no-client"/"error"/"oversize"
}

// Resolve attempts a live fetch of the source, falling back to the last-known-good
// cached body on any failure. It never returns an error: a kick must proceed even
// when the source is unreachable, so failure is expressed as Ok=false (or a cached
// Result) and logged, not propagated.
func (r *Resolver) Resolve(ctx context.Context, src Source) Result {
	if !src.valid() {
		return Result{Source: "unset"}
	}
	slug := src.Slug()
	if !r.allow(slug) {
		if r.logger != nil {
			r.logger.Warn("prompt source repo not allowlisted, ignoring (falling back to inline template)", "repo", slug)
		}
		return Result{Source: "denied"}
	}
	if r.fetcher == nil {
		// No GitHub client wired (e.g. token-less boot). Serve cache if we have
		// one, else report a miss so the caller uses its inline template.
		if body, ok := r.cached(src); ok {
			return Result{Body: body, Ok: true, Source: "github-cache:" + slug}
		}
		return Result{Source: "no-client"}
	}

	fetchCtx := ctx
	if ctx == nil {
		fetchCtx = context.Background()
	}
	fetchCtx, cancel := context.WithTimeout(fetchCtx, r.timeout)
	defer cancel()

	body, err := r.fetcher.GetFileContentRef(fetchCtx, src.Owner, src.Repo, src.Path, src.Ref)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("failed to fetch prompt from GitHub, falling back", "repo", slug, "path", src.Path, "error", err)
		}
		if body, ok := r.cached(src); ok {
			return Result{Body: body, Ok: true, Source: "github-cache:" + slug}
		}
		return Result{Source: "error"}
	}
	if len(body) > r.maxBytes {
		// Reject whole. Truncating here handed the agent a policy with its
		// tail — typically the closing rules — cut off, and nothing told the
		// operator. The cached copy is the last body that DID fit.
		if r.logger != nil {
			r.logger.Warn("prompt from GitHub exceeds size cap; REJECTED whole (not truncated), falling back",
				"repo", slug, "path", src.Path, "ref", src.Ref, "size_bytes", len(body), "cap_bytes", r.maxBytes, "override_env", MaxPromptBytesEnv)
		}
		if cached, ok := r.cached(src); ok {
			return Result{Body: cached, Ok: true, Source: "github-cache:" + slug}
		}
		return Result{Source: "oversize"}
	}
	r.store(src, body)
	return Result{Body: body, Ok: true, Source: r.provenance(src)}
}

func (r *Resolver) provenance(src Source) string {
	ref := src.Ref
	if ref == "" {
		ref = "default"
	}
	return fmt.Sprintf("github:%s@%s", src.Slug(), ref)
}

func (r *Resolver) cached(src Source) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	body, ok := r.cache[src.key()]
	return body, ok
}

func (r *Resolver) store(src Source, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[src.key()] = body
}

// FetchOnce does a single gated fetch WITHOUT touching the resolver cache. It is
// used at agent-create/edit time to bake an initial copy and to validate that the
// source is reachable before saving. It returns an error (unlike Resolve) so the
// UI can surface a bad owner/repo/path to the operator.
func FetchOnce(ctx context.Context, fetcher Fetcher, allow AllowFunc, src Source) (string, error) {
	if !src.valid() {
		return "", fmt.Errorf("prompt source is incomplete (owner, repo, and path are required)")
	}
	slug := src.Slug()
	if allow == nil || !allow(slug) {
		return "", fmt.Errorf("repo %q is not on the GitHub prompt allowlist", slug)
	}
	if fetcher == nil {
		return "", fmt.Errorf("no GitHub client available to fetch prompt")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	fctx, cancel := context.WithTimeout(ctx, defaultFetchTimeout)
	defer cancel()
	body, err := fetcher.GetFileContentRef(fctx, src.Owner, src.Repo, src.Path, src.Ref)
	if err != nil {
		return "", err
	}
	if limit := MaxPromptBytes(); len(body) > limit {
		return "", oversizeError(src, len(body), limit)
	}
	return body, nil
}
