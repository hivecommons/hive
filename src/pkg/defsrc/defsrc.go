// Package defsrc resolves a WHOLE agent definition from an external source
// (currently a GitHub repo file holding the portable AgentDefinition YAML) and
// merges its operator-safe fields over an agent's baked config, so an operator
// who "keeps an agent linked to a repo" sees repo edits propagate on the next
// hive reload — without a redeploy.
//
// It is the whole-agent analogue of pkg/promptsrc (which live-sources only the
// kick prompt). Both share the same design principles:
//
//   - Graceful fallback: a fetch/parse failure NEVER blanks or crashes an agent.
//     The baked (last-known-good) definition is kept and the failure is logged.
//   - Seed-only gating: fetching is gated by an Allow func the caller wires to
//     the trusted config seed (config.Config.GitHubDefinitionAllowed). A repo
//     whose "owner/repo" slug is not allowlisted is never fetched, so a
//     user-writable dashboard overlay cannot point an agent at an arbitrary repo.
//
// TRUST BOUNDARY (the reason this is a separate merge rather than a wholesale
// replace): a repo-sourced definition is user-controlled content. It may set an
// agent's presentation and behavior — but it must NOT be able to escalate the
// agent's privileges or flip any security/seed-only field. mergeAllowedFields
// applies ONLY the explicitly-listed operator-safe fields (name/display/emoji/
// description/tier/role/mode/cadences/channels/tools/prompt template/colors/
// keywords/aliases/connections). Every other field on the baked AgentConfig is
// preserved untouched. If a new privilege-granting field is ever added to
// AgentConfig, it is excluded by default (allow-list, not deny-list) — so the
// safe failure mode is "the live definition can't change it", never "it can".
package defsrc

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"gopkg.in/yaml.v3"
)

// maxDefinitionBytes caps how large a fetched definition YAML may be. An agent
// definition is a small YAML document (kilobytes); this bounds memory/log blast
// radius if a source path points at something huge, while staying well above any
// real definition.
const maxDefinitionBytes = 512 * 1024 // 512 KiB

// defaultFetchTimeout bounds a single live fetch so a slow/hung GitHub API call
// cannot stall a reload. On timeout the resolver keeps the baked definition.
const defaultFetchTimeout = 8 * time.Second

// DefinitionKind is the required `kind` value in the portable AgentDefinition
// YAML. It mirrors the dashboard's exportKind so an exported agent round-trips.
const DefinitionKind = "AgentDefinition"

// Source is the config-package-agnostic description of a GitHub definition
// source. The config.DefinitionSourceConfig is adapted into this at the call site.
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

func (s Source) key() string {
	return s.Owner + "\x00" + s.Repo + "\x00" + s.Path + "\x00" + s.Ref
}

func (s Source) valid() bool {
	return s.Owner != "" && s.Repo != "" && s.Path != ""
}

// Fetcher reads a file's decoded contents from a repo at an optional ref. It is
// satisfied by *github.Client.GetFileContentRef, and stubbed in tests.
type Fetcher interface {
	GetFileContentRef(ctx context.Context, owner, repo, path, ref string) (string, error)
}

// AllowFunc reports whether a given "owner/repo" slug may be fetched. Callers
// wire this to the seed-only policy (config.Config.GitHubDefinitionAllowed).
type AllowFunc func(slug string) bool

// Logger is the tiny logging surface this package needs, satisfied by *slog.Logger.
type Logger interface {
	Warn(msg string, args ...any)
	Info(msg string, args ...any)
}

// AgentDefinition is the portable YAML format for a whole agent. It matches the
// dashboard's import/export schema. Only the fields listed here (and applied in
// mergeAllowedFields) can be sourced live — anything absent from this struct or
// from the merge is, by construction, not settable by a live definition.
type AgentDefinition struct {
	APIVersion string              `yaml:"apiVersion" json:"apiVersion"`
	Kind       string              `yaml:"kind" json:"kind"`
	Metadata   AgentDefinitionMeta `yaml:"metadata" json:"metadata"`
	Spec       AgentDefinitionSpec `yaml:"spec" json:"spec"`
}

type AgentDefinitionMeta struct {
	Name        string `yaml:"name" json:"name"`
	DisplayName string `yaml:"displayName,omitempty" json:"displayName,omitempty"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Emoji       string `yaml:"emoji,omitempty" json:"emoji,omitempty"`
	Color       string `yaml:"color,omitempty" json:"color,omitempty"`
	SpecVersion int    `yaml:"specVersion,omitempty" json:"specVersion,omitempty"`
}

type AgentDefinitionSpec struct {
	Backend         string                    `yaml:"backend,omitempty" json:"backend,omitempty"`
	Model           string                    `yaml:"model,omitempty" json:"model,omitempty"`
	Role            string                    `yaml:"role,omitempty" json:"role,omitempty"`
	Mode            string                    `yaml:"mode,omitempty" json:"mode,omitempty"`
	SortOrder       int                       `yaml:"sortOrder,omitempty" json:"sortOrder,omitempty"`
	BeadRole        string                    `yaml:"beadRole,omitempty" json:"beadRole,omitempty"`
	StaleTimeout    int                       `yaml:"staleTimeout,omitempty" json:"staleTimeout,omitempty"`
	RestartStrategy string                    `yaml:"restartStrategy,omitempty" json:"restartStrategy,omitempty"`
	ClearOnKick     bool                      `yaml:"clearOnKick,omitempty" json:"clearOnKick,omitempty"`
	IncludeRepos    bool                      `yaml:"includeRepos,omitempty" json:"includeRepos,omitempty"`
	LaneKeywords    []string                  `yaml:"laneKeywords,omitempty" json:"laneKeywords,omitempty"`
	DetectKeywords  []string                  `yaml:"detectKeywords,omitempty" json:"detectKeywords,omitempty"`
	Aliases         []string                  `yaml:"aliases,omitempty" json:"aliases,omitempty"`
	Cadences        map[string]config.Cadence `yaml:"cadences,omitempty" json:"cadences,omitempty"`
	PromptTemplate  string                    `yaml:"promptTemplate,omitempty" json:"promptTemplate,omitempty"`
	Channels        []config.ChannelConfig    `yaml:"channels,omitempty" json:"channels,omitempty"`
	Tools           *config.ToolsConfig       `yaml:"tools,omitempty" json:"tools,omitempty"`
	Connections     []config.ConnectionConfig `yaml:"connections,omitempty" json:"connections,omitempty"`
}

// ParseDefinition parses and structurally validates a definition YAML document.
// It returns an error (rather than a zero value) so callers that fetch at
// create/import time can surface a bad document to the operator.
func ParseDefinition(yamlContent string) (*AgentDefinition, error) {
	var def AgentDefinition
	if err := yaml.Unmarshal([]byte(yamlContent), &def); err != nil {
		return nil, fmt.Errorf("invalid definition YAML: %w", err)
	}
	if def.Kind != DefinitionKind {
		return nil, fmt.Errorf("expected kind %q, got %q", DefinitionKind, def.Kind)
	}
	if def.Metadata.Name == "" {
		return nil, fmt.Errorf("metadata.name is required")
	}
	return &def, nil
}

// mergeAllowedFields applies ONLY the operator-safe fields of a live definition
// over a baked agent config, returning the merged copy. This is THE trust
// boundary: every field it touches is presentation/behavior, never a
// privilege-granting or seed-only field. Fields it deliberately never touches
// (so a live definition can't escalate an agent) include:
//
//   - PromptSource / DefinitionSource (the source pointers themselves; a live
//     definition must not be able to re-point the agent at a different repo)
//   - Enabled / Paused / Managed (operator lifecycle state)
//   - ID / BeadsDir / MetricsCollector / ACMMLevels / OnDemand / CavemanMode
//   - anything under the hive-level Variables/Security policy (which does not
//     even live on AgentConfig — it is seed-only by construction)
//
// A field absent from a fetched definition (Go zero value) does NOT clear the
// baked value: empty strings/slices are skipped so a minimal definition can't
// silently wipe presentation. Booleans that carry meaning use the definition's
// value directly (clear_on_kick, include_repos) because their zero value is a
// legitimate setting.
func mergeAllowedFields(baked config.AgentConfig, def *AgentDefinition) config.AgentConfig {
	merged := baked

	// --- metadata (presentation) ---
	if def.Metadata.DisplayName != "" {
		merged.DisplayName = def.Metadata.DisplayName
	}
	if def.Metadata.Description != "" {
		merged.Description = def.Metadata.Description
	}
	if def.Metadata.Emoji != "" {
		merged.Emoji = def.Metadata.Emoji
	}
	if def.Metadata.Color != "" {
		merged.Color = def.Metadata.Color
	}

	// --- spec (behavior / tier / role) ---
	if def.Spec.Backend != "" {
		merged.Backend = def.Spec.Backend
	}
	if def.Spec.Model != "" {
		merged.Model = def.Spec.Model
	}
	if def.Spec.Role != "" {
		merged.Role = def.Spec.Role
	}
	if def.Spec.Mode != "" {
		merged.Mode = def.Spec.Mode
	}
	if def.Spec.SortOrder != 0 {
		merged.SortOrder = def.Spec.SortOrder
	}
	if def.Spec.BeadRole != "" {
		merged.BeadRole = def.Spec.BeadRole
	}
	if def.Spec.StaleTimeout != 0 {
		merged.StaleTimeout = def.Spec.StaleTimeout
	}
	if def.Spec.RestartStrategy != "" {
		merged.RestartStrategy = def.Spec.RestartStrategy
	}
	// clear_on_kick and include_repos carry meaning at their zero value, so the
	// definition's value is authoritative when the source is live.
	merged.ClearOnKick = def.Spec.ClearOnKick
	includeRepos := def.Spec.IncludeRepos
	merged.IncludeRepos = &includeRepos

	if len(def.Spec.LaneKeywords) > 0 {
		merged.LaneKeywords = def.Spec.LaneKeywords
	}
	if len(def.Spec.DetectKeywords) > 0 {
		merged.DetectKeywords = def.Spec.DetectKeywords
	}
	if len(def.Spec.Aliases) > 0 {
		merged.Aliases = def.Spec.Aliases
	}
	if len(def.Spec.Channels) > 0 {
		merged.Channels = def.Spec.Channels
	}
	if def.Spec.Tools != nil {
		merged.Tools = def.Spec.Tools
	}
	if len(def.Spec.Connections) > 0 {
		merged.Connections = def.Spec.Connections
	}

	return merged
}

// Resolver fetches whole-agent definitions live and remembers the last good
// document per source so a later failure can fall back to it. Safe for
// concurrent use.
type Resolver struct {
	fetcher Fetcher
	allow   AllowFunc
	logger  Logger
	timeout time.Duration

	mu    sync.RWMutex
	cache map[string]string // source key -> last-known-good raw YAML
}

// NewResolver builds a Resolver. fetcher may be nil (token-mode boot without an
// App client), in which case every resolution reports a miss and the caller keeps
// the baked definition. allow must be non-nil; a nil allow denies everything
// (fail closed). logger may be nil.
func NewResolver(fetcher Fetcher, allow AllowFunc, logger Logger) *Resolver {
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

// Result carries a merged agent config plus provenance. Ok is false when nothing
// could be resolved (not set, denied, no fetcher, unreachable-with-no-cache, or a
// malformed document); the caller then keeps the baked config unchanged.
type Result struct {
	Merged config.AgentConfig
	Ok     bool
	Source string // "github:owner/repo@ref", "github-cache:...", "denied", "unset", "no-client", "error"
}

// Resolve fetches the live definition for src and merges its allowed fields over
// baked, falling back to the last-known-good cached document on any fetch failure.
// It never returns an error: a reload must proceed even when the source is
// unreachable, so failure is Ok=false (keep baked) and logged, not propagated.
func (r *Resolver) Resolve(ctx context.Context, src Source, baked config.AgentConfig) Result {
	if !src.valid() {
		return Result{Merged: baked, Source: "unset"}
	}
	slug := src.Slug()
	if !r.allow(slug) {
		if r.logger != nil {
			r.logger.Warn("definition source repo not allowlisted, ignoring (keeping baked definition)", "repo", slug)
		}
		return Result{Merged: baked, Source: "denied"}
	}
	if r.fetcher == nil {
		if raw, ok := r.cached(src); ok {
			return r.mergeFromRaw(raw, baked, src, "github-cache:"+slug)
		}
		return Result{Merged: baked, Source: "no-client"}
	}

	fetchCtx := ctx
	if ctx == nil {
		fetchCtx = context.Background()
	}
	fetchCtx, cancel := context.WithTimeout(fetchCtx, r.timeout)
	defer cancel()

	raw, err := r.fetcher.GetFileContentRef(fetchCtx, src.Owner, src.Repo, src.Path, src.Ref)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("failed to fetch agent definition from GitHub, keeping baked", "repo", slug, "path", src.Path, "error", err)
		}
		if cached, ok := r.cached(src); ok {
			return r.mergeFromRaw(cached, baked, src, "github-cache:"+slug)
		}
		return Result{Merged: baked, Source: "error"}
	}
	if len(raw) > maxDefinitionBytes {
		raw = raw[:maxDefinitionBytes]
		if r.logger != nil {
			r.logger.Warn("agent definition from GitHub exceeded size cap, truncated", "repo", slug, "path", src.Path, "cap_bytes", maxDefinitionBytes)
		}
	}
	res := r.mergeFromRaw(raw, baked, src, r.provenance(src))
	if res.Ok {
		// Only cache a document that parsed cleanly, so a later fetch failure
		// falls back to a known-good definition, not a corrupt one.
		r.store(src, raw)
	}
	return res
}

// mergeFromRaw parses a raw definition and merges it; a parse failure keeps the
// baked config (Ok=false) so a corrupt document never blanks a live agent.
func (r *Resolver) mergeFromRaw(raw string, baked config.AgentConfig, src Source, provenance string) Result {
	def, err := ParseDefinition(raw)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("agent definition from GitHub is malformed, keeping baked", "repo", src.Slug(), "path", src.Path, "error", err)
		}
		return Result{Merged: baked, Source: "error"}
	}
	return Result{Merged: mergeAllowedFields(baked, def), Ok: true, Source: provenance}
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
	raw, ok := r.cache[src.key()]
	return raw, ok
}

func (r *Resolver) store(src Source, raw string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[src.key()] = raw
}

// FetchOnce does a single gated fetch WITHOUT touching the resolver cache and
// returns the parsed definition. It is used at import/keep-linked time to
// validate that the source is reachable and well-formed before saving, so the UI
// can surface a bad owner/repo/path/document to the operator. Unlike Resolve it
// returns an error.
func FetchOnce(ctx context.Context, fetcher Fetcher, allow AllowFunc, src Source) (*AgentDefinition, error) {
	if !src.valid() {
		return nil, fmt.Errorf("definition source is incomplete (owner, repo, and path are required)")
	}
	slug := src.Slug()
	if allow == nil || !allow(slug) {
		return nil, fmt.Errorf("repo %q is not on the GitHub definition allowlist", slug)
	}
	if fetcher == nil {
		return nil, fmt.Errorf("no GitHub client available to fetch definition")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	fctx, cancel := context.WithTimeout(ctx, defaultFetchTimeout)
	defer cancel()
	raw, err := fetcher.GetFileContentRef(fctx, src.Owner, src.Repo, src.Path, src.Ref)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxDefinitionBytes {
		raw = raw[:maxDefinitionBytes]
	}
	return ParseDefinition(raw)
}

// MergeAllowedFields exposes the trust-boundary merge for callers that already
// hold a parsed definition (e.g. the import handler applying keep-linked at
// create time). It applies the same operator-safe field allow-list as Resolve.
func MergeAllowedFields(baked config.AgentConfig, def *AgentDefinition) config.AgentConfig {
	return mergeAllowedFields(baked, def)
}

// ApplyToConfig re-resolves every agent that carries a definition_source and
// merges the live definition's operator-safe fields over the agent's baked
// config in place. Agents without a definition_source are left untouched, and
// any agent whose source is unreachable/denied/malformed keeps its baked config
// (Resolve's graceful fallback). It is safe to call on every reload and at
// startup — a nil resolver is a no-op so a token-less boot doesn't panic.
//
// Note: seed-only/privileged fields are preserved because mergeAllowedFields
// only ever touches the operator-safe allow-list; in particular the agent's own
// DefinitionSource/PromptSource pointers are NOT re-pointable by a live
// definition, so a compromised repo cannot redirect the agent elsewhere.
func ApplyToConfig(ctx context.Context, cfg *config.Config, resolver *Resolver, logger Logger) {
	if cfg == nil || resolver == nil {
		return
	}
	for name, agent := range cfg.Agents {
		if !agent.DefinitionSource.IsSet() {
			continue
		}
		src := Source{
			Owner: agent.DefinitionSource.Owner,
			Repo:  agent.DefinitionSource.Repo,
			Path:  agent.DefinitionSource.Path,
			Ref:   agent.DefinitionSource.Ref,
		}
		res := resolver.Resolve(ctx, src, agent)
		if !res.Ok {
			continue
		}
		// Preserve the source pointers and managed flag regardless of what the
		// live definition contained — the merge already excludes them, but assert
		// it here so a future mergeAllowedFields change can't silently leak.
		merged := res.Merged
		merged.DefinitionSource = agent.DefinitionSource
		merged.PromptSource = agent.PromptSource
		merged.Managed = agent.Managed
		cfg.Agents[name] = merged
		if logger != nil {
			logger.Info("re-applied live agent definition", "agent", name, "source", res.Source)
		}
	}
}
