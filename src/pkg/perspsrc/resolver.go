package perspsrc

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/defsrc"
)

// defaultFetchTimeout bounds a single live fetch so a slow/hung GitHub API call
// cannot stall a reload. On timeout the resolver keeps the last-known-good set.
const defaultFetchTimeout = 8 * time.Second

// Resolver fetches review perspective documents live and remembers the last
// good document per source so a later failure keeps that set in effect. It is
// modelled on defsrc.Resolver and shares its Fetcher, AllowFunc and Logger.
// Safe for concurrent use.
type Resolver struct {
	fetcher defsrc.Fetcher
	allow   defsrc.AllowFunc
	logger  defsrc.Logger
	timeout time.Duration

	mu    sync.RWMutex
	cache map[string]string // source key -> last-known-good raw YAML
}

// NewResolver builds a Resolver. fetcher may be nil (token-mode boot without an
// App client), in which case only a cached document can resolve. Callers wire
// allow to the seed-only gate (config.Config.GitHubPromptAllowed /
// GitHubDefinitionAllowed); a nil allow denies everything (fail closed).
// logger may be nil.
func NewResolver(fetcher defsrc.Fetcher, allow defsrc.AllowFunc, logger defsrc.Logger) *Resolver {
	if allow == nil {
		allow = func(string) bool { return false }
	}
	return &Resolver{
		fetcher: fetcher,
		allow:   allow,
		logger:  logger,
		timeout: defaultFetchTimeout,
		cache:   map[string]string{},
	}
}

// Result carries a resolved perspective set plus provenance. Ok is false when
// nothing could be resolved (not set, denied, no fetcher, unreachable or
// malformed with no last-known-good); the caller then falls back to inline
// review.perspectives. Warning is set when the latest fetch or document failed,
// whether or not a last-known-good set is still in effect.
type Result struct {
	Names   []string
	Focus   map[string]string
	Ok      bool
	Source  string // "github:owner/repo@ref", "github-cache:owner/repo", "denied", "unset", "no-client", "error"
	Warning string
}

// Resolve fetches the perspective document for src. It never returns an error:
// a reload must proceed even when the source is unreachable or malformed. A
// document that fails validation keeps the last-known-good set (with a
// Warning) rather than silently falling back to the built-in set.
func (r *Resolver) Resolve(ctx context.Context, src config.DefinitionSourceConfig) Result {
	if !src.IsSet() {
		return Result{Source: "unset"}
	}
	slug := src.Slug()
	if !r.allow(slug) {
		if r.logger != nil {
			r.logger.Warn("review perspectives source repo not allowlisted, ignoring (keeping inline review.perspectives)", "repo", slug)
		}
		return Result{Source: "denied"}
	}
	if r.fetcher == nil {
		if res, ok := r.fromCache(src, ""); ok {
			return res
		}
		return Result{Source: "no-client"}
	}

	fetchCtx := ctx
	if ctx == nil {
		fetchCtx = context.Background()
	}
	fetchCtx, cancel := context.WithTimeout(fetchCtx, r.timeout)
	defer cancel()

	raw, err := r.fetcher.GetFileContentRef(fetchCtx, src.Owner, src.Repo, src.Path, src.Ref)
	if err != nil {
		warning := fmt.Sprintf("fetching review perspectives from %s/%s failed: %v", slug, src.Path, err)
		if r.logger != nil {
			r.logger.Warn("failed to fetch review perspectives from GitHub, keeping last-known-good", "repo", slug, "path", src.Path, "error", err)
		}
		if res, ok := r.fromCache(src, warning); ok {
			return res
		}
		return Result{Source: "error", Warning: warning}
	}
	names, focus, err := ParseDocument(raw)
	if err != nil {
		warning := fmt.Sprintf("review perspectives document %s/%s is invalid: %v", slug, src.Path, err)
		if r.logger != nil {
			r.logger.Warn("review perspectives from GitHub are malformed, keeping last-known-good", "repo", slug, "path", src.Path, "error", err)
		}
		if res, ok := r.fromCache(src, warning); ok {
			return res
		}
		return Result{Source: "error", Warning: warning}
	}
	// Only a document that parsed cleanly is cached, so a later failure keeps
	// a known-good set, never a corrupt one.
	r.store(src, raw)
	return Result{Names: names, Focus: focus, Ok: true, Source: provenance(src)}
}

// fromCache resolves the last-known-good document for src, if any. It was
// validated when cached, so a parse failure here is not expected; it is still
// treated as a miss rather than trusted.
func (r *Resolver) fromCache(src config.DefinitionSourceConfig, warning string) (Result, bool) {
	raw, ok := r.cached(src)
	if !ok {
		return Result{}, false
	}
	names, focus, err := ParseDocument(raw)
	if err != nil {
		return Result{}, false
	}
	return Result{Names: names, Focus: focus, Ok: true, Source: "github-cache:" + src.Slug(), Warning: warning}, true
}

func provenance(src config.DefinitionSourceConfig) string {
	ref := src.Ref
	if ref == "" {
		ref = "default"
	}
	return fmt.Sprintf("github:%s@%s", src.Slug(), ref)
}

func sourceKey(src config.DefinitionSourceConfig) string {
	return src.Owner + "\x00" + src.Repo + "\x00" + src.Path + "\x00" + src.Ref
}

func (r *Resolver) cached(src config.DefinitionSourceConfig) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	raw, ok := r.cache[sourceKey(src)]
	return raw, ok
}

func (r *Resolver) store(src config.DefinitionSourceConfig, raw string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[sourceKey(src)] = raw
}

// ApplyToConfig resolves cfg.Review.PerspectivesSource and records the result
// on cfg.Review.LinkedPerspectives, with its provenance and any warning. When
// nothing resolves, LinkedPerspectives is left nil so EffectivePerspectives
// falls back to inline review.perspectives. A nil cfg or resolver is a no-op.
func ApplyToConfig(ctx context.Context, cfg *config.Config, resolver *Resolver, logger defsrc.Logger) {
	if cfg == nil || resolver == nil {
		return
	}
	cfg.Review.LinkedPerspectives = nil
	if !cfg.Review.PerspectivesSource.IsSet() {
		return
	}
	res := resolver.Resolve(ctx, *cfg.Review.PerspectivesSource)
	if !res.Ok {
		return
	}
	cfg.Review.LinkedPerspectives = &config.LinkedPerspectives{
		Names:   res.Names,
		Focus:   res.Focus,
		Source:  res.Source,
		Warning: res.Warning,
	}
	if logger != nil {
		logger.Info("applied live review perspectives", "source", res.Source, "perspectives", len(res.Names))
	}
}
