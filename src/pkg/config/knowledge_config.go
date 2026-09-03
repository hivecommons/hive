// Knowledge and bootstrap-source configuration: knowledge engine/curator/
// primer, bead synthesizer, vault, git/doc sources, prompt and definition
// sources, operator variables, and the resolve-registry wiring on Config.
package config

import (
	"log/slog"
	"strings"

	"github.com/hivecommons/hive/pkg/resolve"
	"gopkg.in/yaml.v3"
)

// VariablesConfig declares operator-defined ${VAR} substitutions and the trust
// policy for resolvers that execute code or do network I/O. It drives the
// pluggable resolve engine (pkg/resolve) used by both config-load and per-kick
// template substitution. Absent (the default) means env-only substitution,
// byte-identical to hive's legacy behavior.
type VariablesConfig struct {
	// Security gates script/http resolvers. Honored ONLY from the trusted config
	// seed — the dashboard overlay's Security block is discarded on load so a
	// user-editable overlay can never enable code execution or network access.
	Security VarSecurityConfig `yaml:"security,omitempty"`
	// Defs maps a variable name (used as ${name}) to its definition.
	Defs map[string]VarDef `yaml:"defs,omitempty"`
}

// VarSecurityConfig is the resolver trust model. Defaults are deny.
type VarSecurityConfig struct {
	AllowExec     bool     `yaml:"allow_exec,omitempty"`
	AllowHTTP     bool     `yaml:"allow_http,omitempty"`
	HTTPAllowlist []string `yaml:"http_allowlist,omitempty"`
	ExecTimeoutS  int      `yaml:"exec_timeout_s,omitempty"`
	HTTPTimeoutS  int      `yaml:"http_timeout_s,omitempty"`

	// AllowGitHubPrompt gates the GitHub-repo prompt-source feature (agents may
	// source their kick prompt from a repo). Like AllowExec/AllowHTTP it is honored
	// ONLY from the trusted config seed — the dashboard overlay's Security block is
	// discarded on load, so a user-editable overlay can never enable this or widen
	// the allowlist. Default false (deny).
	AllowGitHubPrompt bool `yaml:"allow_github_prompt,omitempty"`
	// GitHubPromptAllowlist is the set of "owner/repo" slugs an agent's
	// prompt_source may read from. Required (non-empty) for the feature to work:
	// an empty allowlist denies all repos even when AllowGitHubPrompt is true. This
	// bounds the blast radius to repos the operator explicitly trusts, so the
	// feature cannot be used to read every repo the App happens to be installed on.
	GitHubPromptAllowlist []string `yaml:"github_prompt_allowlist,omitempty"`
}

// PromptSourceConfig describes a GitHub repo location to pull an agent's kick
// prompt from. Owner/Repo/Path are required; Ref (branch/tag/SHA) is optional
// and defaults to the repo's default branch.
type PromptSourceConfig struct {
	// Type selects the source kind. Only "github" is currently supported; an
	// empty type defaults to "github" when a repo is set.
	Type  string `yaml:"type,omitempty" json:"type,omitempty"`
	Owner string `yaml:"owner,omitempty" json:"owner,omitempty"`
	Repo  string `yaml:"repo,omitempty" json:"repo,omitempty"`
	Path  string `yaml:"path,omitempty" json:"path,omitempty"`
	Ref   string `yaml:"ref,omitempty" json:"ref,omitempty"`
}

// Slug returns the "owner/repo" identifier used for allowlist matching, or ""
// when owner or repo is unset.
func (p *PromptSourceConfig) Slug() string {
	if p == nil || p.Owner == "" || p.Repo == "" {
		return ""
	}
	return p.Owner + "/" + p.Repo
}

// IsSet reports whether this prompt source is fully specified (owner, repo, and
// path all present). A partially-filled source is treated as unset so a kick
// falls back to the inline template rather than erroring.
func (p *PromptSourceConfig) IsSet() bool {
	return p != nil && p.Owner != "" && p.Repo != "" && p.Path != ""
}

// DefinitionSourceConfig describes a GitHub repo location to pull a WHOLE agent
// definition (the portable AgentDefinition YAML) from, keeping the agent "live"
// so edits on the repo propagate on the next reload/kick. It is the whole-agent
// analogue of PromptSourceConfig. Owner/Repo/Path are required; Ref is optional
// and defaults to the repo's default branch.
//
// Security: re-applying a live definition NEVER changes security-sensitive or
// seed-only fields (the resolver trust policy, token scopes, etc.). Only the
// operator-safe presentation/behavior fields are merged — see pkg/defsrc for the
// exact allowed-field boundary. The fetch is gated to the same seed-only repo
// allowlist as prompt_source (VarSecurityConfig.GitHubPromptAllowlist), so a
// user-writable dashboard overlay can neither widen the allowlist nor point an
// agent at an arbitrary repo the App happens to be installed on.
type DefinitionSourceConfig struct {
	// Type selects the source kind. Only "github" is currently supported; an
	// empty type defaults to "github" when a repo is set.
	Type  string `yaml:"type,omitempty" json:"type,omitempty"`
	Owner string `yaml:"owner,omitempty" json:"owner,omitempty"`
	Repo  string `yaml:"repo,omitempty" json:"repo,omitempty"`
	Path  string `yaml:"path,omitempty" json:"path,omitempty"`
	Ref   string `yaml:"ref,omitempty" json:"ref,omitempty"`
	// URL is the human-facing source URL the operator pasted in the import UI
	// (e.g. the github.com blob URL). Informational only — Owner/Repo/Path/Ref
	// are authoritative for fetching. Kept so the UI can round-trip it.
	URL string `yaml:"url,omitempty" json:"url,omitempty"`
}

// Slug returns the "owner/repo" identifier used for allowlist matching, or ""
// when owner or repo is unset.
func (d *DefinitionSourceConfig) Slug() string {
	if d == nil || d.Owner == "" || d.Repo == "" {
		return ""
	}
	return d.Owner + "/" + d.Repo
}

// IsSet reports whether this definition source is fully specified (owner, repo,
// and path all present). A partially-filled source is treated as unset so an
// agent keeps its baked definition rather than erroring.
func (d *DefinitionSourceConfig) IsSet() bool {
	return d != nil && d.Owner != "" && d.Repo != "" && d.Path != ""
}

// VarDef is one operator-declared variable. `default` uses a pointer so an
// explicit empty-string default is distinguishable from "no default".
type VarDef struct {
	Type    string            `yaml:"type,omitempty"`  // env|static|script|http
	Scope   string            `yaml:"scope,omitempty"` // config|template|both (default template)
	Default *string           `yaml:"default,omitempty"`
	Value   string            `yaml:"value,omitempty"`   // static
	Env     string            `yaml:"env,omitempty"`     // env source var name
	Command []string          `yaml:"command,omitempty"` // script argv
	URL     string            `yaml:"url,omitempty"`     // http
	Headers map[string]string `yaml:"headers,omitempty"` // http
}

// DocSourceConfigYAML describes an external document to import as knowledge.
type DocSourceConfigYAML struct {
	Name     string `yaml:"name"`
	URL      string `yaml:"url,omitempty"`
	FilePath string `yaml:"file_path,omitempty"`
	Layer    string `yaml:"layer"`
}

type KnowledgeConfig struct {
	Enabled         bool                  `yaml:"enabled"`
	Engine          string                `yaml:"engine"`
	Layers          []KnowledgeLayer      `yaml:"layers"`
	Vaults          []VaultConfig         `yaml:"vaults"`
	GitSources      []GitSourceConfigYAML `yaml:"git_sources"`
	Documents       []DocSourceConfigYAML `yaml:"documents"`
	Curator         KnowledgeCurator      `yaml:"curator"`
	Primer          KnowledgePrimer       `yaml:"primer"`
	BeadSynthesizer BeadSynthesizerConfig `yaml:"bead_synthesizer"`
}

// BeadSynthesizerConfig controls automatic synthesis of completed beads into wiki facts.
// Enabled defaults to true when knowledge is enabled; set to false to opt out.
type BeadSynthesizerConfig struct {
	Enabled          *bool            `yaml:"enabled,omitempty"`
	Schedule         string           `yaml:"schedule"`
	MinConfidence    float64          `yaml:"min_confidence"`
	TargetLayer      string           `yaml:"target_layer"`
	MaxFactsPerCycle int              `yaml:"max_facts_per_cycle"`
	VaultPath        string           `yaml:"vault_path"`
	RetentionPolicy  *RetentionPolicy `yaml:"retention_policy"`
}

// RetentionPolicy controls intelligent bead lifecycle management.
type RetentionPolicy struct {
	MaxBeads               int  `yaml:"max_beads"`
	ArchiveAfterSynthDays  int  `yaml:"archive_after_synth_days"`
	HighPriorityRetainDays int  `yaml:"high_priority_retain_days"`
	PreserveWithDeps       bool `yaml:"preserve_with_deps"`
}

// IsEnabled returns whether bead synthesis is enabled (defaults to true).
func (b BeadSynthesizerConfig) IsEnabled() bool {
	if b.Enabled == nil {
		return true
	}
	return *b.Enabled
}

// GitSourceConfigYAML describes a remote git repo (or subdirectory) to index
// as a knowledge source. Any layer level can have git sources.
type GitSourceConfigYAML struct {
	Name    string `yaml:"name"`
	URL     string `yaml:"url"`
	Branch  string `yaml:"branch,omitempty"`
	Subpath string `yaml:"subpath,omitempty"`
	Layer   string `yaml:"layer"`
}

// VaultConfig describes a file-based Obsidian vault to auto-connect on startup.
type VaultConfig struct {
	Name      string `yaml:"name"`
	Path      string `yaml:"path"`
	AutoIndex bool   `yaml:"auto_index"`
	GitSync   bool   `yaml:"git_sync"`
}

type KnowledgeLayer struct {
	Type   string `yaml:"type"`
	Path   string `yaml:"path,omitempty"`
	URL    string `yaml:"url,omitempty"`
	Shared bool   `yaml:"shared"`
}

type KnowledgeCurator struct {
	AutoPromoteThreshold float64 `yaml:"auto_promote_threshold"`
}

type KnowledgePrimer struct {
	MaxFacts      int      `yaml:"max_facts"`
	Priority      []string `yaml:"priority"`
	MergeStrategy string   `yaml:"merge_strategy"`
}

// bootstrapVariables is a minimal view of hive.yaml used to read the
// `variables:` block before the whole document is expanded/parsed. Unknown keys
// are ignored by yaml.Unmarshal, so this is cheap and tolerant.
type bootstrapVariables struct {
	Variables VariablesConfig `yaml:"variables"`
}

// configRegistryFromText bootstrap-parses the variables block from raw config
// text and returns a config-scoped resolve.Registry. On any parse failure it
// falls back to an env-only registry (legacy behavior), never an error — config
// expansion must not fail the load.
func configRegistryFromText(raw string) *resolve.Registry {
	var bv bootstrapVariables
	if err := yaml.Unmarshal([]byte(raw), &bv); err != nil {
		return resolve.EnvOnly()
	}
	if len(bv.Variables.Defs) == 0 {
		// No custom variables — env-only, byte-identical to legacy.
		return resolve.EnvOnly()
	}
	specs, pol := bv.Variables.toResolveSpecs()
	return resolve.Build(specs, pol, nil)
}

// toResolveSpecs translates the config-level variables block into the
// resolve package's VarSpec list and Policy.
func (v VariablesConfig) toResolveSpecs() ([]resolve.VarSpec, resolve.Policy) {
	specs := make([]resolve.VarSpec, 0, len(v.Defs))
	for name, def := range v.Defs {
		spec := resolve.VarSpec{
			Name:    name,
			Type:    def.Type,
			Scope:   resolve.Scope(def.Scope),
			Value:   def.Value,
			Env:     def.Env,
			Command: def.Command,
			URL:     def.URL,
			Headers: def.Headers,
		}
		if def.Default != nil {
			spec.HasDefault = true
			spec.Default = *def.Default
		}
		specs = append(specs, spec)
	}
	pol := resolve.Policy{
		AllowExec:     v.Security.AllowExec,
		AllowHTTP:     v.Security.AllowHTTP,
		HTTPAllowlist: v.Security.HTTPAllowlist,
		ExecTimeoutS:  v.Security.ExecTimeoutS,
		HTTPTimeoutS:  v.Security.HTTPTimeoutS,
	}
	return specs, pol
}

// ResolveRegistry builds a resolve.Registry from this config's `variables:`
// block, for use at per-kick template substitution sites (scheduler, dashboard
// preview). With no variables configured it returns an env-only registry whose
// Expand — in template scope, where the runtime built-ins win and there is no
// env fallback — reproduces the previous strings.NewReplacer output exactly.
// Pass a logger to surface disabled/invalid resolver diagnostics; nil is fine.
func (c *Config) ResolveRegistry(logger *slog.Logger) *resolve.Registry {
	if len(c.Variables.Defs) == 0 {
		return resolve.EnvOnly()
	}
	specs, pol := c.Variables.toResolveSpecs()
	return resolve.Build(specs, pol, logger)
}

// GitHubPromptAllowed reports whether an agent's prompt_source pointing at the
// given "owner/repo" slug is permitted to be fetched. This mirrors the seed-only
// gating used for exec/http resolvers: it consults c.Variables.Security, which
// LoadWithDashboardOverlay guarantees comes ONLY from the trusted config seed
// (the dashboard overlay's Variables block is never merged). It returns false
// unless the feature is explicitly enabled AND the slug is on the seed-declared
// allowlist, so a user-writable overlay can never widen the set of readable repos.
func (c *Config) GitHubPromptAllowed(slug string) bool {
	if slug == "" || !c.Variables.Security.AllowGitHubPrompt {
		return false
	}
	for _, allowed := range c.Variables.Security.GitHubPromptAllowlist {
		if strings.EqualFold(strings.TrimSpace(allowed), slug) {
			return true
		}
	}
	return false
}

// GitHubDefinitionAllowed reports whether an agent's definition_source pointing
// at the given "owner/repo" slug is permitted to be fetched. A live whole-agent
// definition is at least as trusted as a live prompt (it re-applies more fields),
// so it reuses the SAME seed-only gate as GitHubPromptAllowed: the feature flag
// and repo allowlist come only from the trusted config seed, never the
// user-writable dashboard overlay (LoadWithDashboardOverlay never merges the
// overlay's Variables block). A compromised overlay therefore can neither widen
// the set of readable repos nor point a live definition at an arbitrary repo.
func (c *Config) GitHubDefinitionAllowed(slug string) bool {
	return c.GitHubPromptAllowed(slug)
}
