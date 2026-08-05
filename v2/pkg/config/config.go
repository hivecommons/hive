package config

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/v2/pkg/resolve"
	"gopkg.in/yaml.v3"
)

// saveMu serializes all Config.Save() calls process-wide. Multiple goroutines
// persist config concurrently — the mutex-guarded pause callback, the async
// dashboard PersistFunc, the ACMM-level saver, HTTP mutation handlers. Each
// does yaml.Marshal(c) + write. Without serialization, two Save() calls race:
// the one that finishes writing LAST wins the file, and if it marshaled a
// staler snapshot (e.g. before a later pause committed to c.Agents), that
// pause is silently lost. Pausing 7 agents in quick succession reliably left
// only the last 1-2 on the PVC. Serializing every Save() closes the race.
var saveMu sync.Mutex

type Config struct {
	Project       ProjectConfig          `yaml:"project"`
	Policies      PoliciesConfig         `yaml:"policies"`
	Agents        map[string]AgentConfig `yaml:"agents"`
	Governor      GovernorConfig         `yaml:"governor"`
	GitHub        GitHubConfig           `yaml:"github"`
	Notifications NotificationsConfig    `yaml:"notifications"`
	Dashboard     DashboardConfig        `yaml:"dashboard"`
	Data          DataConfig             `yaml:"data"`
	Knowledge     KnowledgeConfig        `yaml:"knowledge"`
	Hub           HubConfig              `yaml:"hub"`
	HiveID        string                 `yaml:"hive_id"`
	ACMMLevel     *int                   `yaml:"acmm_level,omitempty" json:"acmm_level"`
	Variables     VariablesConfig        `yaml:"variables,omitempty"`

	// RemovedAgents are agent names an operator deliberately deleted. It is a
	// TOMBSTONE list, and it exists because deletion had no durable record
	// anywhere: the delete handlers dropped the agent from the in-memory map
	// and re-saved the overlay, but an agent lives in THREE places — the
	// ConfigMap seed, /data/agent-configs/<name>.yaml, and the dashboard
	// overlay — and Load() UNIONS all three via MergeAgentOverrides, which
	// only ever adds. So the next config reload (fsnotify, observed ~36s after
	// the delete on a live hive) re-materialized the agent, and even after
	// that the next ApplyPack re-created it from the ACMM pack. That is the
	// reported "I deleted brainstorm and guide and they always come back".
	//
	// A tombstone is scoped to the agent NAME and persists indefinitely,
	// including across ACMM level changes. The alternative — clearing
	// tombstones on a level change so a higher pack can reintroduce the agent
	// — was rejected: an operator who deletes `guide` is expressing "I do not
	// want this agent", not "I do not want it at this level", and silently
	// resurrecting it during an unrelated level bump is the same class of
	// silent revert as the bug itself. Re-adding the agent explicitly (the
	// Governor grid's add, the agent CRUD create, or an import) clears the
	// tombstone, which is the one unambiguous signal that the operator changed
	// their mind. A genuinely NEW pack agent — one never deleted here — is
	// unaffected and is still added on a level increase.
	RemovedAgents []string `yaml:"removed_agents,omitempty" json:"removed_agents,omitempty"`

	SourcePath string `yaml:"-" json:"-"`
}

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
	Schedule             string   `yaml:"schedule"`
	ExtractFrom          []string `yaml:"extract_from"`
	AutoPromoteThreshold float64  `yaml:"auto_promote_threshold"`
}

type KnowledgePrimer struct {
	MaxFacts      int      `yaml:"max_facts"`
	Priority      []string `yaml:"priority"`
	MergeStrategy string   `yaml:"merge_strategy"`
}

type ProjectConfig struct {
	Org         string   `yaml:"org"`
	Name        string   `yaml:"name"`
	Repos       []string `yaml:"repos"`
	AIAuthor    string   `yaml:"ai_author"`
	PrimaryRepo string   `yaml:"primary_repo"`
	OpenPRs     *bool    `yaml:"open_prs,omitempty"`
}

// PRsAllowed returns whether agents may open pull requests. Defaults to true.
func (p *ProjectConfig) PRsAllowed() bool {
	if p.OpenPRs != nil {
		return *p.OpenPRs
	}
	return true
}

type PoliciesConfig struct {
	Repo         string        `yaml:"repo"`
	Branch       string        `yaml:"branch"`
	Path         string        `yaml:"path"`
	PollInterval time.Duration `yaml:"poll_interval"`
	LocalDir     string        `yaml:"local_dir"`
}

// StatsDisplayEntry defines a single metric to show in the agent's sidebar/detail view.
type StatsDisplayEntry struct {
	Key        string `yaml:"key" json:"key"`
	Label      string `yaml:"label" json:"label"`
	Source     string `yaml:"source" json:"source"`
	Field      string `yaml:"field" json:"field"`
	Style      string `yaml:"style" json:"style"`
	TrendField string `yaml:"trend_field,omitempty" json:"trendField,omitempty"`
	Target     int    `yaml:"target,omitempty" json:"target,omitempty"`
	// Desc is a one-line explanation of what the stat verifies, rendered
	// as a hover tooltip in the dashboard (health checks especially).
	Desc string `yaml:"desc,omitempty" json:"desc,omitempty"`
}

// ChannelConfig declares a trigger channel for an agent.
type ChannelConfig struct {
	Type     string            `yaml:"type" json:"type"`
	Enabled  *bool             `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Events   []string          `yaml:"events,omitempty" json:"events,omitempty"`
	Patterns []string          `yaml:"patterns,omitempty" json:"patterns,omitempty"`
	Schedule string            `yaml:"schedule,omitempty" json:"schedule,omitempty"`
	Match    map[string]string `yaml:"match,omitempty" json:"match,omitempty"`
	Repos    []string          `yaml:"repos,omitempty" json:"repos,omitempty"`
}

// IsEnabled returns whether this channel is active (defaults to true).
func (c *ChannelConfig) IsEnabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// ToolRule is a single allow/deny rule for a tool pattern.
type ToolRule struct {
	Pattern string `yaml:"pattern" json:"pattern"`
	Action  string `yaml:"action" json:"action"`
	Reason  string `yaml:"reason,omitempty" json:"reason,omitempty"`
}

// ToolsConfig declares what tools an agent can use.
type ToolsConfig struct {
	Preset string     `yaml:"preset,omitempty" json:"preset,omitempty"`
	Rules  []ToolRule `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// ConnectionAuth describes how to authenticate to a connection.
type ConnectionAuth struct {
	Type   string `yaml:"type" json:"type"`
	EnvVar string `yaml:"env_var,omitempty" json:"env_var,omitempty"`
	File   string `yaml:"file,omitempty" json:"file,omitempty"`
}

// ConnectionConfig declares an external service integration for an agent.
type ConnectionConfig struct {
	Name    string            `yaml:"name" json:"name"`
	Type    string            `yaml:"type" json:"type"`
	URI     string            `yaml:"uri,omitempty" json:"uri,omitempty"`
	Auth    *ConnectionAuth   `yaml:"auth,omitempty" json:"auth,omitempty"`
	EnvName string            `yaml:"env_name,omitempty" json:"env_name,omitempty"`
	Options map[string]string `yaml:"options,omitempty" json:"options,omitempty"`
}

type AgentConfig struct {
	ID       string `yaml:"id" json:"id,omitempty"`
	Backend  string `yaml:"backend" json:"backend,omitempty"`
	Model    string `yaml:"model" json:"model,omitempty"`
	BeadsDir string `yaml:"beads_dir" json:"beads_dir,omitempty"`
	Enabled  bool   `yaml:"enabled" json:"enabled,omitempty"`
	// Paused persists an operator pause across restarts/upgrades. Without
	// this, every pod restart rebuilt agents un-paused (Go zero value), so
	// an operator pause was silently undone on the next upgrade.
	Paused      bool `yaml:"paused" json:"paused,omitempty"`
	ClearOnKick bool `yaml:"clear_on_kick" json:"clear_on_kick"`
	CLIPinned   bool `yaml:"cli_pinned" json:"cli_pinned,omitempty"`

	// ModelOwner / BackendOwner record WHO last set Model / Backend. An ACMM
	// pack owns these fields until an operator changes them in the Governor
	// grid; from that point the operator owns them and ApplyPack must not
	// reconcile them back to the pack value.
	//
	// Without this, a pack re-apply — which happens on EVERY hive restart
	// (cmd/hive/main.go "merging pack updates") — rewrote Model back to the
	// pack default, so an operator's model choice silently reverted on the
	// next restart. That is the "I've deleted those 4 times, they always come
	// back" report: the revert was a restart, not a propagation delay.
	//
	// Stored as a string owner rather than a bool so "never set" (empty),
	// "pack-owned" and "operator-owned" stay distinguishable across upgrades
	// of hives whose config predates this field.
	ModelOwner      string `yaml:"model_owner" json:"model_owner,omitempty"`
	BackendOwner    string `yaml:"backend_owner" json:"backend_owner,omitempty"`
	StaleTimeout    int    `yaml:"stale_timeout" json:"stale_timeout,omitempty"`
	RestartStrategy string `yaml:"restart_strategy" json:"restart_strategy,omitempty"`
	LaunchCmd       string `yaml:"launch_cmd" json:"launch_cmd,omitempty"`
	DisplayName     string `yaml:"display_name" json:"display_name,omitempty"`
	Description     string `yaml:"description" json:"description,omitempty"`

	// Phase 2: config-driven agent behavior fields
	Role           string   `yaml:"role" json:"role,omitempty"`
	SortOrder      int      `yaml:"sort_order" json:"sort_order,omitempty"`
	Emoji          string   `yaml:"emoji" json:"emoji,omitempty"`
	Color          string   `yaml:"color" json:"color,omitempty"`
	Aliases        []string `yaml:"aliases" json:"aliases,omitempty"`
	LaneKeywords   []string `yaml:"lane_keywords" json:"lane_keywords,omitempty"`
	DetectKeywords []string `yaml:"detect_keywords" json:"detect_keywords,omitempty"`
	KickTemplate   string   `yaml:"kick_template" json:"kick_template,omitempty"`
	// PromptSource, when set, sources the agent's kick prompt from a GitHub repo
	// instead of (or in addition to) an inline KickTemplate. It is resolved live
	// at kick time via the hive's GitHub App token, with graceful fallback to the
	// baked/inline template when the repo is unreachable. The user-writable
	// dashboard overlay may set this, but the fetch is gated to a seed-only repo
	// allowlist (see VarSecurityConfig.GitHubPromptAllowlist), so a compromised
	// overlay cannot read arbitrary repos the App is installed on.
	PromptSource *PromptSourceConfig `yaml:"prompt_source,omitempty" json:"prompt_source,omitempty"`
	// DefinitionSource, when set, keeps the WHOLE agent linked to a GitHub repo:
	// the portable AgentDefinition YAML at owner/repo/path@ref is re-fetched on
	// reload and its operator-safe fields are merged over the baked agent, so
	// edits on the repo propagate without a redeploy. It never overrides
	// security-sensitive/seed-only fields (see pkg/defsrc for the field boundary)
	// and is gated to the same seed-only repo allowlist as PromptSource, so a
	// user-writable overlay can neither widen the allowlist nor escalate an
	// agent's privileges via a live definition. On fetch failure the last-known-good
	// baked definition is kept — a live agent is never blanked or crashed.
	DefinitionSource *DefinitionSourceConfig `yaml:"definition_source,omitempty" json:"definition_source,omitempty"`
	IncludeRepos     *bool                   `yaml:"include_repos" json:"include_repos,omitempty"`
	MetricsCollector string                  `yaml:"metrics_collector" json:"metrics_collector,omitempty"`
	BeadRole         string                  `yaml:"bead_role" json:"bead_role,omitempty"`
	StatsDisplay     []StatsDisplayEntry     `yaml:"stats_display" json:"stats_display,omitempty"`
	ACMMLevels       []int                   `yaml:"acmm_levels" json:"acmm_levels,omitempty"`
	Mode             string                  `yaml:"mode" json:"mode,omitempty"`
	OnDemand         bool                    `yaml:"on_demand" json:"on_demand,omitempty"`
	CavemanMode      string                  `yaml:"caveman_mode" json:"caveman_mode,omitempty"`

	// Channels declares how this agent gets triggered (kick, webhook, discord, schedule, bead).
	// When nil/empty, the agent uses governor timer kicks by default (implicit kick channel).
	Channels []ChannelConfig `yaml:"channels,omitempty" json:"channels,omitempty"`

	// Tools declares what tools this agent can use. When nil, the existing Mode field governs.
	Tools *ToolsConfig `yaml:"tools,omitempty" json:"tools,omitempty"`

	// Connections declares external service integrations (MCP servers, APIs, knowledge sources).
	Connections []ConnectionConfig `yaml:"connections,omitempty" json:"connections,omitempty"`

	// Managed is true for agents loaded from the overlay directory (not base config).
	Managed bool `yaml:"-" json:"managed"`

	// clearOnKickSet tracks whether YAML explicitly set clear_on_kick to false
	clearOnKickSet bool
	// enabledSet tracks whether YAML explicitly set enabled (distinguishes
	// "not specified" from "enabled: false").
	enabledSet bool
	// name is the YAML map key, set during config load
	name string
}

// Name returns the human-readable YAML key for this agent.
func (a *AgentConfig) Name() string {
	return a.name
}

// HasChannel returns true if the agent has a channel of the given type.
func (a *AgentConfig) HasChannel(t string) bool {
	for _, ch := range a.Channels {
		if ch.Type == t {
			return true
		}
	}
	return false
}

// ChannelsOfType returns all channels matching the given type.
func (a *AgentConfig) ChannelsOfType(t string) []ChannelConfig {
	var result []ChannelConfig
	for _, ch := range a.Channels {
		if ch.Type == t {
			result = append(result, ch)
		}
	}
	return result
}

// UsesGovernorKick returns true when the agent should receive governor timer kicks.
// This is true when no channels are declared (implicit kick) or when an explicit
// kick channel is present.
func (a *AgentConfig) UsesGovernorKick() bool {
	if len(a.Channels) == 0 {
		return true
	}
	return a.HasChannel("kick")
}

// ShouldIncludeRepos returns whether the repos section should be appended to kicks.
// Defaults to true for all agents except those with IncludeRepos explicitly set to false.
func (a *AgentConfig) ShouldIncludeRepos() bool {
	if a.IncludeRepos != nil {
		return *a.IncludeRepos
	}
	return true
}

// GetBeadRole returns the bead role, defaulting to "worker".
func (a *AgentConfig) GetBeadRole() string {
	if a.BeadRole != "" {
		return a.BeadRole
	}
	return "worker"
}

// GetSortOrder returns the sort order. Supervisor-role agents default to 0 (first).
func (a *AgentConfig) GetSortOrder() int {
	if a.SortOrder != 0 {
		return a.SortOrder
	}
	if a.BeadRole == "supervisor" {
		return 0
	}
	return 100
}

func (a *AgentConfig) UnmarshalYAML(value *yaml.Node) error {
	type plain AgentConfig
	if err := value.Decode((*plain)(a)); err != nil {
		return err
	}
	// Check if clear_on_kick / enabled were explicitly present in YAML
	for i := 0; i < len(value.Content)-1; i += 2 {
		switch value.Content[i].Value {
		case "clear_on_kick":
			a.clearOnKickSet = true
		case "enabled":
			a.enabledSet = true
		}
	}
	return nil
}

// Ownership markers for AgentConfig.ModelOwner / BackendOwner.
const (
	// FieldOwnerPack marks a value written by an ACMM pack. Pack-owned values
	// are reconciled to the current pack on every apply.
	FieldOwnerPack = "pack"
	// FieldOwnerOperator marks a value an operator chose in the Governor grid.
	// Operator-owned values are never overwritten by a pack apply.
	FieldOwnerOperator = "operator"
)

// ModelIsOperatorOwned reports whether an operator explicitly chose this
// agent's model, which makes it immune to pack reconciliation.
func (a AgentConfig) ModelIsOperatorOwned() bool {
	return a.ModelOwner == FieldOwnerOperator
}

// BackendIsOperatorOwned reports whether an operator explicitly chose this
// agent's backend (the grid's "method" column).
func (a AgentConfig) BackendIsOperatorOwned() bool {
	return a.BackendOwner == FieldOwnerOperator
}

// EnabledExplicitlySet returns true when the user's YAML explicitly set the
// "enabled" field (allowing us to distinguish "not specified" from "enabled: false").
func (a *AgentConfig) EnabledExplicitlySet() bool {
	return a.enabledSet
}

type GovernorConfig struct {
	Modes         map[string]ModeConfig `yaml:"modes"`
	EvalIntervalS int                   `yaml:"eval_interval_s"`
	Labels        LabelsConfig          `yaml:"labels"`
	Sensing       SensingConfig         `yaml:"sensing"`
	Health        HealthConfig          `yaml:"health"`
	Budget        BudgetConfig          `yaml:"budget"`
	Logging       LoggingConfig         `yaml:"logging"`
	LiteLLM       LiteLLMConfig         `yaml:"litellm"`
	VLLM          InferenceAuthConfig   `yaml:"vllm"`
	LLMD          InferenceAuthConfig   `yaml:"llm-d"`
	// Bob holds the IBM bobshell CLI backend's API-key location. Required for
	// agents with backend "bob": bobshell's browser SSO flow cannot complete in
	// a headless pod.
	Bob        BobConfig        `yaml:"bob"`
	Trajectory TrajectoryConfig `yaml:"trajectory"`
	// Gateways is the list of named model gateways (OpenAI-compatible endpoints
	// like OpenRouter, a LiteLLM proxy, vLLM, or llm-d). An agent routes through
	// a gateway by naming it as its backend. When empty, a single implicit
	// gateway named "litellm" is synthesized from the legacy LiteLLM block above
	// so existing hives keep working with zero config change. See ResolveGateway.
	Gateways []GatewayConfig `yaml:"gateways"`
	// AttributionTrailer controls the VISIBLE invocation-attribution line
	// ("— hive: agent=… backend=… model=…") appended at creation time to the
	// body of PRs the hive opens for agents (the PR-request watcher) and issues
	// the hive itself creates. It is a *bool so absent (nil) is distinct from an
	// explicit false: default is ON (see AttributionTrailerEnabled), matching
	// the github.app_authored_prs convention. It gates ONLY the visible trailer
	// — the audit-log entry for every such creation is written unconditionally,
	// so turning this off never removes the operator's ability to answer "which
	// backend/model produced this PR?".
	AttributionTrailer *bool `yaml:"attribution_trailer,omitempty"`
}

// AttributionTrailerEnabled reports whether the visible attribution trailer on
// hive-created PRs/issues is on for this hive. Default ON: a hive that says
// nothing gets the trailer; set `governor.attribution_trailer: false` to hide
// it. The audit-log entry for each creation is unconditional and is NOT gated
// by this.
func (g *GovernorConfig) AttributionTrailerEnabled() bool {
	return g.AttributionTrailer == nil || *g.AttributionTrailer
}

// GatewayConfig is one named, OpenAI-compatible model gateway. A hive may
// configure several at once (e.g. OpenRouter for public models plus a private
// LiteLLM proxy for internal ones); each agent picks one by naming it as its
// backend. Secrets are referenced by env-var NAME or file PATH only — never
// inlined — matching the LiteLLM/inference key-handling rule elsewhere.
type GatewayConfig struct {
	// Name is the unique gateway id an agent names as its backend (e.g.
	// "openrouter", "corp-litellm"). Must be non-empty and unique per hive.
	Name string `yaml:"name" json:"name"`
	// Kind drives preset defaults + labeling: openrouter | litellm | vllm |
	// llm-d | watsonx | custom. Purely descriptive at runtime (all are
	// OpenAI-compatible); the endpoint is what actually routes. The one
	// exception is "watsonx", which additionally needs an IAM-minted bearer
	// (not the raw key) and a project_id header — see ProjectID below and the
	// watsonx token minter (pkg/watsonx).
	Kind string `yaml:"kind" json:"kind,omitempty"`
	// Endpoint is the OpenAI-compatible base URL, e.g. https://openrouter.ai/api/v1.
	// For a watsonx gateway this is the model-gateway base
	// https://<region>.ml.cloud.ibm.com/ml/gateway — hive appends /v1/models and
	// /v1/chat/completions to reach watsonx's OpenAI-compatible surface
	// (.../ml/gateway/v1/models, .../ml/gateway/v1/chat/completions).
	Endpoint string `yaml:"endpoint" json:"endpoint"`
	// APIKeyEnv / APIKeyFile resolve this gateway's key (env var NAME and/or file
	// PATH — never the value). Empty is allowed for keyless endpoints (some vLLM).
	// For watsonx this resolves the IBM Cloud API key that is exchanged for a
	// short-lived IAM token; the raw key is never sent as the bearer.
	APIKeyEnv  string `yaml:"api_key_env" json:"api_key_env,omitempty"`
	APIKeyFile string `yaml:"api_key_file" json:"api_key_file,omitempty"`
	// DefaultModel is used when an agent routed through this gateway selects none.
	DefaultModel string `yaml:"default_model" json:"default_model,omitempty"`
	// CABundle is an optional PEM path for a private CA (never disables verify).
	CABundle string `yaml:"ca_bundle" json:"ca_bundle,omitempty"`
	// ProjectID is the watsonx project (or space) id an OpenAI client does not
	// send but watsonx requires for billing/limits. It is sent as the
	// X-IBM-Project-ID header on outbound requests to a watsonx gateway. Only
	// meaningful for Kind == watsonx; omitempty keeps existing gateways
	// byte-identical in hive.yaml. Not a secret (an identifier, not a
	// credential), so unlike the key it is stored inline.
	ProjectID string `yaml:"project_id,omitempty" json:"project_id,omitempty"`
	// Region is the watsonx region slug (e.g. us-south, eu-de, jp-tok) the UI
	// preset uses to build the endpoint template. Purely a convenience for the
	// preset; Endpoint is authoritative. Only meaningful for Kind == watsonx.
	Region string `yaml:"region,omitempty" json:"region,omitempty"`
}

// gatewayKind values.
const (
	GatewayKindOpenRouter = "openrouter"
	GatewayKindLiteLLM    = "litellm"
	GatewayKindVLLM       = "vllm"
	GatewayKindLLMD       = "llm-d"
	GatewayKindWatsonx    = "watsonx"
	GatewayKindCustom     = "custom"

	// legacyLiteLLMGatewayName is the name of the implicit gateway synthesized
	// from the legacy Governor.LiteLLM block when no gateways are configured. It
	// matches the historical "litellm" agent backend so existing agents route
	// unchanged.
	legacyLiteLLMGatewayName = "litellm"
)

// ResolvedGateways returns the effective gateway list: the explicitly-configured
// Gateways when present, otherwise a single implicit gateway synthesized from the
// legacy LiteLLM block (only if that block has an endpoint). This is what lets a
// hive with no `gateways:` and a classic `litellm:` block keep working, while a
// hive that lists gateways gets exactly those.
func (g GovernorConfig) ResolvedGateways() []GatewayConfig {
	if len(g.Gateways) > 0 {
		return g.Gateways
	}
	if g.LiteLLM.Endpoint == "" {
		return nil
	}
	return []GatewayConfig{{
		Name:         legacyLiteLLMGatewayName,
		Kind:         GatewayKindLiteLLM,
		Endpoint:     g.LiteLLM.Endpoint,
		APIKeyEnv:    g.LiteLLM.APIKeyEnv,
		APIKeyFile:   g.LiteLLM.APIKeyFile,
		DefaultModel: g.LiteLLM.DefaultModel,
		CABundle:     g.LiteLLM.CABundle,
	}}
}

// ResolveGateway looks up a gateway by name in the resolved list. An empty name
// returns the FIRST resolved gateway (the default), so an inference agent that
// names no specific gateway routes through the default one. Returns nil if no
// gateway matches (or none are configured).
func (g GovernorConfig) ResolveGateway(name string) *GatewayConfig {
	gws := g.ResolvedGateways()
	if len(gws) == 0 {
		return nil
	}
	if name == "" {
		gw := gws[0]
		return &gw
	}
	for i := range gws {
		if strings.EqualFold(gws[i].Name, name) {
			gw := gws[i]
			return &gw
		}
	}
	return nil
}

// ResolveLiteLLMInferenceKey resolves the API key an agent should present when
// its inference routes through the legacy "litellm" backend. It MUST agree with
// the key the entitlement/probe path validates (dashboard gateways.go/cost.go/
// openrouter.go, which use ResolveGateway(name).ResolveAPIKey()) — otherwise a
// key rotation performed via the Model Gateways tab updates only the gateway key
// file, entitlement passes, but inference keeps sending the stale legacy key and
// 401s.
//
// Resolution rule:
//   - When an EXPLICIT `gateways:` block is configured, resolve the key from the
//     gateway matching this backend (its own api_key_file — the file the Model
//     Gateways tab writes), exactly as entitlement does. One source, no drift.
//   - When NO explicit gateways are configured, fall back to the legacy
//     Governor.LiteLLM resolver, which consults the k8s Secret mount and PVC copy
//     in addition to api_key_file. The synthetic gateway from ResolvedGateways
//     lacks that multi-location fallback, so we must not use it here — preserving
//     today's behavior for classic single-`litellm:`-block hives.
//
// backend is the agent's backend name (typically "litellm"); it selects which
// explicit gateway to consult.
func (g GovernorConfig) ResolveLiteLLMInferenceKey(backend string) string {
	if len(g.Gateways) > 0 {
		if gw := g.ResolveGateway(backend); gw != nil {
			return gw.ResolveAPIKey()
		}
	}
	return g.LiteLLM.ResolveAPIKey()
}

// ResolveAPIKey returns this gateway's key value, preferring the env var when
// set and falling back to the file. Returns "" when neither yields a value
// (a keyless endpoint). Mirrors LiteLLMConfig key resolution.
func (gw GatewayConfig) ResolveAPIKey() string {
	if gw.APIKeyEnv != "" {
		if v := os.Getenv(gw.APIKeyEnv); v != "" {
			return v
		}
	}
	if gw.APIKeyFile != "" {
		if b, err := os.ReadFile(gw.APIKeyFile); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

// TrajectoryConfig governs the trajectory-review lane: a periodic,
// second-model check that reads each running agent's recent transcript and
// scores whether the sequence of actions is still working toward the agent's
// assigned intent (its last kick), pausing the agent when it diverges. This
// is defense against trajectory-level goal drift — individually-innocuous
// steps that assemble toward an unauthorized outcome — which action-level
// gating cannot see. On by default; the lane no-ops when no reviewer endpoint
// resolves, so "on" is safe even without inference set up.
//
// The reviewer needs any OpenAI-compatible /v1/chat/completions endpoint — a
// LiteLLM gateway, a vLLM server, or an llm-d front. It does NOT drive an
// interactive CLI: it makes one stateless request per agent per cycle and
// reads back a strict-JSON verdict, which the CLIs (stateful tmux sessions)
// cannot provide.
type TrajectoryConfig struct {
	// Enabled turns the review lane on. Pointer so an omitted key defaults to
	// enabled (applyDefaults sets it), while an explicit "false" disables it.
	Enabled *bool `yaml:"enabled"`
	// Endpoint is the reviewer's OpenAI-compatible base URL (LiteLLM, vLLM, or
	// llm-d — anything serving /v1/chat/completions). Empty → fall back to the
	// governor LiteLLM endpoint. Storing it here lets the reviewer target a
	// cheap local model independent of the agents' inference config.
	Endpoint string `yaml:"endpoint"`
	// APIKeyEnv / APIKeyFile resolve the reviewer endpoint's key (env var NAME
	// or file PATH; never the value). Empty → fall back to the LiteLLM key.
	// A bare vLLM server needs no key; a gateway usually does.
	APIKeyEnv  string `yaml:"api_key_env"`
	APIKeyFile string `yaml:"api_key_file"`
	// IntervalS is how often (seconds) the lane evaluates running agents.
	// It runs off the governor tick, so the effective floor is the governor
	// eval interval; a value below that reviews every tick. 0 → default.
	IntervalS int `yaml:"interval_s"`
	// Model is the reviewer model id. Empty → the governor LiteLLM
	// default_model. A cheap-but-capable model is appropriate; a tiny model
	// will emit weaker verdicts (the lane fails open, so it degrades toward
	// "catches less," not "false-pauses").
	Model string `yaml:"model"`
	// TranscriptLines caps how many trailing transcript lines are sent to the
	// reviewer. 0 → default. Bounds token cost and keeps the review focused
	// on recent behavior.
	TranscriptLines int `yaml:"transcript_lines"`
	// OnDivergence is the action taken when a trajectory is judged divergent:
	// "pause" (stop the agent and alert — default) or "alert" (notify only,
	// leave the agent running). Any other value is treated as "alert".
	OnDivergence string `yaml:"on_divergence"`
	// ExemptAgents are never reviewed (e.g. advisory-only agents that open no
	// PRs and touch no infrastructure).
	ExemptAgents []string `yaml:"exempt_agents"`
}

// IsEnabled reports whether the trajectory-review lane is on. Default is ON:
// a nil Enabled (key omitted) counts as enabled, only an explicit false
// disables it. The lane itself no-ops when no reviewer endpoint resolves, so
// defaulting on is safe even for hives without inference configured.
func (t TrajectoryConfig) IsEnabled() bool {
	return t.Enabled == nil || *t.Enabled
}

// ResolveReviewer returns the reviewer endpoint, API key, and model, falling
// back to the governor LiteLLM block for any field the trajectory block leaves
// empty. This is what lets the reviewer target a LiteLLM gateway, a vLLM
// server, or an llm-d front interchangeably: it only needs an
// OpenAI-compatible /v1/chat/completions URL. The key value is resolved from
// a file or env var and is never stored in hive.yaml.
func (g *GovernorConfig) ResolveReviewer() (endpoint, apiKey, model string) {
	t := g.Trajectory
	endpoint = strings.TrimRight(strings.TrimSpace(t.Endpoint), "/")
	if endpoint == "" {
		endpoint = g.LiteLLM.ResolveEndpoint()
	}
	apiKey = resolveKeyFromFileThenEnv(t.APIKeyFile, t.APIKeyEnv)
	if apiKey == "" {
		apiKey = g.LiteLLM.ResolveAPIKey()
	}
	model = strings.TrimSpace(t.Model)
	if model == "" {
		model = g.LiteLLM.DefaultModel
	}
	return endpoint, apiKey, model
}

// ReviewerReady reports whether the reviewer has both an endpoint and a model
// resolved — i.e. whether enabling the lane will actually run reviews. The UI
// uses this to distinguish "on and running" from "on but not configured", so
// a safety control never silently no-ops while showing as enabled.
func (g *GovernorConfig) ReviewerReady() bool {
	endpoint, _, model := g.ResolveReviewer()
	return endpoint != "" && model != ""
}

// resolveKeyFromFileThenEnv reads a secret from a file path, then an env var
// NAME, returning "" if neither yields a value. Mirrors the LiteLLM/inference
// resolution order without pulling in defaults.
func resolveKeyFromFileThenEnv(file, env string) string {
	if file != "" {
		if data, err := os.ReadFile(file); err == nil {
			if k := strings.TrimSpace(string(data)); k != "" {
				return k
			}
		}
	}
	if env != "" {
		if k := os.Getenv(env); k != "" {
			return k
		}
	}
	return ""
}

// Discovery-auth defaults for the self-hosted inference backends. Like
// LiteLLM, hive.yaml stores only the env var NAME and/or key FILE PATH —
// never the key value itself (Config.Save() writes the expanded config
// back to disk, so a key value in YAML would be persisted in plaintext).
const (
	// DefaultVLLMAPIKeyEnv is the env var consulted for the vLLM model
	// discovery API key when governor.vllm.api_key_env is not set.
	DefaultVLLMAPIKeyEnv = "HIVE_VLLM_API_KEY"
	// DefaultLLMDAPIKeyEnv is the env var consulted for the llm-d model
	// discovery API key when governor.llm-d.api_key_env is not set.
	DefaultLLMDAPIKeyEnv = "HIVE_LLMD_API_KEY"
)

// InferenceAuthConfig holds optional /v1/models discovery auth for a
// self-hosted inference backend (vllm, llm-d). Plain vLLM/llm-d servers
// need no key, but the configured endpoint may actually be a LiteLLM
// gateway, which entitlement-filters /v1/models per API key and hides
// key-gated models from anonymous callers.
type InferenceAuthConfig struct {
	APIKeyEnv  string `yaml:"api_key_env"`  // env var NAME holding the key; never the key value
	APIKeyFile string `yaml:"api_key_file"` // path to a file holding the key
}

// ResolveAPIKey returns the backend's discovery API key using the
// resolution order: key file (api_key_file) → env var named by
// api_key_env → defaultEnv. Returns "" when no key is configured. The key
// value itself is never stored in hive.yaml.
func (c *InferenceAuthConfig) ResolveAPIKey(defaultEnv string) string {
	if c.APIKeyFile != "" {
		if data, err := os.ReadFile(c.APIKeyFile); err == nil {
			if key := strings.TrimSpace(string(data)); key != "" {
				return key
			}
		}
	}
	if c.APIKeyEnv != "" {
		if key := os.Getenv(c.APIKeyEnv); key != "" {
			return key
		}
	}
	if defaultEnv != "" {
		return os.Getenv(defaultEnv)
	}
	return ""
}

type LoggingConfig struct {
	Dir        string `yaml:"dir"`
	MaxSizeMB  int    `yaml:"max_size_mb"`
	MaxAgeDays int    `yaml:"max_age_days"`
	MaxBackups int    `yaml:"max_backups"`
	Compress   bool   `yaml:"compress"`
	Level      string `yaml:"level"`
}

// LiteLLM key/endpoint resolution defaults. hive.yaml stores only the env
// var NAME and/or key FILE PATH — never the key value itself. Config.Save()
// writes the expanded config back to disk, so a key value stored in YAML
// would be baked into the file in plaintext.
const (
	// DefaultLiteLLMAPIKeyEnv is the env var consulted for the LiteLLM API
	// key when api_key_env is not set in hive.yaml.
	DefaultLiteLLMAPIKeyEnv = "HIVE_LITELLM_API_KEY"
	// DefaultLiteLLMAPIKeyFile is the key file consulted when api_key_file
	// is not set. Matches the /secrets volume used for k8s Secret mounts.
	DefaultLiteLLMAPIKeyFile = "/secrets/litellm_api_key"
	// WritableSecretsDir is the PVC-backed directory where the dashboard
	// persists secret VALUES entered in the UI. Unlike /secrets (a
	// read-only Kubernetes Secret mount), /data is the hive's writable
	// persistent volume, so files written here survive pod restarts and
	// hosted users can set keys without cluster access.
	WritableSecretsDir = "/data/secrets"
	// WritableLiteLLMAPIKeyFile is where the dashboard stores an API key
	// value entered in the LiteLLM config UI. hive.yaml references it via
	// api_key_file; the key value itself never enters hive.yaml or logs.
	WritableLiteLLMAPIKeyFile = WritableSecretsDir + "/litellm_api_key"
	// LiteLLMEndpointEnv overrides governor.litellm.endpoint at runtime
	// (mirrors HIVE_VLLM_ENDPOINT / HIVE_LLMD_ENDPOINT).
	LiteLLMEndpointEnv = "HIVE_LITELLM_ENDPOINT"
)

// Bob (IBM bobshell) API-key resolution. bobshell cannot authenticate in a pod
// any other way: its default flow (W3ID SSO) opens a browser and polls a
// localhost callback port, which in a headless container cannot succeed and
// instead burns a 3-minute timeout per launch. IBM's documented remedy for
// non-interactive sessions is API-key auth. As with LiteLLM, hive.yaml stores
// only the env var NAME and/or key FILE PATH — never the key value, because
// Config.Save() round-trips the config back to disk.
const (
	// BobAPIKeyEnvVar is the env var name bobshell itself reads the key from.
	// Verified against the installed bundle (bobshell 1.0.6 bundle/bob.js):
	// `process.env.BOBSHELL_API_KEY`. This is the name injected INTO the
	// agent's environment, distinct from the hive-side variable below that
	// an operator sets on the hive pod.
	BobAPIKeyEnvVar = "BOBSHELL_API_KEY"

	// DefaultBobAPIKeyEnv is the hive-side env var consulted for the bob API
	// key when governor.bob.api_key_env is not set in hive.yaml.
	DefaultBobAPIKeyEnv = "HIVE_BOB_API_KEY"

	// DefaultBobAPIKeyFile is the key file consulted when api_key_file is not
	// set. Matches the read-only /secrets volume used for k8s Secret mounts.
	DefaultBobAPIKeyFile = "/secrets/bob_api_key"

	// WritableBobAPIKeyFile is the PVC-backed location a hosted operator can
	// write the key to without cluster access, mirroring
	// WritableLiteLLMAPIKeyFile. Referenced by path only; never by value.
	WritableBobAPIKeyFile = WritableSecretsDir + "/bob_api_key"

	// BobAuthTypeEnvVar is the env var bobshell reads to pick its auth type.
	// It is only a FALLBACK DEFAULT, not an override. From bundle/bob.js
	// (bobshell 1.0.6), the auth-dialog preselect is:
	//   let l=null,d=process.env.BOBSHELL_DEFAULT_AUTH_TYPE;
	//   d&&Object.values(fr).includes(d)&&(l=d);
	//   let c=a.findIndex(p=>e.merged.security?.auth?.selectedType
	//         ? p.value===e.merged.security.auth.selectedType
	//         : l ? p.value===l : ...)
	// i.e. a persisted security.auth.selectedType WINS over this env var.
	// An invalid value is a hard error ("Invalid value for
	// BOBSHELL_DEFAULT_AUTH_TYPE"), so it must be one of the `fr` enum values.
	BobAuthTypeEnvVar = "BOBSHELL_DEFAULT_AUTH_TYPE"

	// BobAuthTypeAPIKey selects API-key auth. It is bob's own enum constant
	// `USE_BOBSHELL="api-key"` from bundle/bob.js (the sibling value being
	// `W3ID_SSO="sso"`), so it is dictated by the vendor, not by us. Not a
	// secret — it is the literal string "api-key" and carries no credential.
	BobAuthTypeAPIKey = "api-key"

	// BobAuthMethodFlag is bobshell's `--auth-method` CLI flag. It EXISTS in
	// bobshell 1.0.6 — an earlier fix removed it after concluding from
	// `bob --help` that it did not. That conclusion was wrong: bundle/bob.js
	// registers it and then HIDES it from help output:
	//   t.option("auth-method",{type:"string",...,choices:[fr.W3ID_SSO,fr.USE_BOBSHELL]});
	//   let a=[...,"auth-method"]; a.forEach(c=>t.hide(c));
	// so it is functional but invisible to `--help | grep auth-method`.
	//
	// It is the STRONGEST control available, because it is the only input that
	// beats the persisted settings file. bob stores it as
	// globalThis.authMethodByCliArg, and the settings-normalization step reads:
	//   let n=globalThis.authMethodByCliArg||t.merged.security.auth.selectedType||r;
	// — the CLI arg is consulted FIRST, ahead of the persisted selectedType.
	// It also suppresses the write-back that would otherwise persist a
	// different value (`&&!globalThis.authMethodByCliArg`), so passing it
	// makes hive's choice authoritative without bob rewriting the shared file.
	BobAuthMethodFlag = "--auth-method"

	// BobApprovalModeFlag / BobApprovalModeYolo set bob's tool-approval policy.
	// Verified in bobshell 1.0.6 `bob --help`:
	//   --approval-mode  Set the approval mode: default (prompt for approval),
	//                    auto_edit (auto-approve edit tools), yolo
	//                    (auto-approve all tools)
	//                    [string] [choices: "default", "auto_edit", "yolo"]
	//
	// Without it bob defaults to "default" and the TUI shows
	// `Auto-approve: Off`, so an unattended agent blocks forever on the first
	// tool call — no human is attached to answer the prompt. With
	// `--approval-mode yolo` the TUI shows `Auto-approve: Full` and bob
	// executes shell/edit tools unattended (verified live on a spoke: bob
	// wrote /tmp/_tool.txt with no approval prompt).
	//
	// The named flag is preferred over the equivalent `-y`/`--yolo` boolean
	// because it states the mode at the call site.
	BobApprovalModeFlag = "--approval-mode"
	BobApprovalModeYolo = "yolo"

	// BobTrustFlag marks the agent's workspace as trusted. bobshell 1.0.6
	// otherwise renders "This folder is not trusted. Some features may be
	// disabled." and gates tool availability behind that state. Verified in
	// `bob --help`:
	//   --trust  specify trust level for the current workspace
	//
	// Passing the flag is preferred over seeding $HOME/.bob/trustedFolders.json
	// because the flag is per-launch and stateless: it needs no knowledge of
	// bob's on-disk trust schema, cannot drift when that schema changes, and
	// applies to whatever workdir the agent is launched in. The shared
	// /data/home is used by EVERY bob agent on a hive, so a seeded trust file
	// would also be a fleet-wide mutation of the kind that already caused the
	// selectedType incident (see BobAuthTypeEnvVar).
	BobTrustFlag = "--trust"

	// BobSettingsRelPath is the persisted settings file, relative to $HOME.
	// From bundle/bob.js: `as=".bob"` and
	// `getGlobalSettingsPath(){return fu.join(t.getGlobalGeminiDir(),"settings.json")}`
	// where getGlobalGeminiDir() is `path.join(os.homedir(), as)`.
	// On a hive this resolves to /data/home/.bob/settings.json — a SHARED file,
	// so one agent picking SSO at the prompt re-breaks every other bob agent.
	BobSettingsRelPath = ".bob/settings.json"

	// BobSettingsAuthKey / BobSettingsSelectedTypeKey / BobSettingsEnforcedTypeKey
	// are the nested JSON keys hive owns inside that file. Shape per bundle:
	//   e.merged.security?.auth?.selectedType   // which method is chosen
	//   e.merged.security?.auth?.enforcedType   // FILTERS the option list:
	//     e.merged.security?.auth?.enforcedType && (a=a.filter(p=>p.value===...))
	//   and validation: eEr() errors when enforcedType !== the active type.
	// Setting enforcedType to api-key leaves SSO unselectable, so a stray
	// interactive pick cannot re-break the fleet.
	BobSettingsSecurityKey     = "security"
	BobSettingsAuthKey         = "auth"
	BobSettingsSelectedTypeKey = "selectedType"
	BobSettingsEnforcedTypeKey = "enforcedType"

	// BobSettingsFileMode keeps the shared settings file group-writable: it
	// lives in /data/home/.bob (drwxrwx--- dev:node) and bob runs as agent UIDs
	// in group `node`, which must still be able to write sibling state. Making
	// it read-only is deliberately NOT done — bob calls setValue() on this file
	// during normal startup normalization, and an EACCES there is an unhandled
	// write path, so re-assertion on every launch is the safer self-heal.
	BobSettingsFileMode = 0o664

	// BobSettingsDirMode matches the observed /data/home/.bob (drwxrwx---).
	BobSettingsDirMode = 0o770

	// BobStateDirName is the per-workspace state directory bob creates inside
	// the directory it is launched in (in addition to the shared $HOME/.bob).
	// Its .bob-errors/ subdirectory is the logger target behind bob's
	// "Failed to initialize logger:" message, observed in production at
	// /data/agents/<name>/.bob/.bob-errors/errors-YYYY-MM-DD.log.
	BobStateDirName = ".bob"
)

// BobConfig configures the IBM bobshell ("bob") CLI backend. Only the
// key's LOCATION is stored — never the key itself.
type BobConfig struct {
	APIKeyEnv  string `yaml:"api_key_env" json:"api_key_env,omitempty"`   // env var NAME holding the key; default HIVE_BOB_API_KEY
	APIKeyFile string `yaml:"api_key_file" json:"api_key_file,omitempty"` // path to a file holding the key; default /secrets/bob_api_key
}

// ResolveAPIKey returns the bob API key, or "" when none is configured.
// Key FILES are consulted in priority order — the configured api_key_file,
// then the k8s Secret mount (DefaultBobAPIKeyFile), then the PVC file
// (WritableBobAPIKeyFile) — followed by the env var named by api_key_env and
// finally DefaultBobAPIKeyEnv. This mirrors LiteLLMConfig.ResolveAPIKey so a
// key stays working if hive.yaml is re-seeded and the api_key_file pointer is
// lost, or if the PVC copy is wiped but an admin Secret exists.
func (c *BobConfig) ResolveAPIKey() string {
	key, _ := c.resolveAPIKeyWithSource()
	return key
}

// ResolveAPIKeySource reports WHERE the key was found without exposing the
// value: "file:<path>", "env:<NAME>", or "" when unconfigured. Safe to log
// and safe to return from APIs.
func (c *BobConfig) ResolveAPIKeySource() string {
	_, source := c.resolveAPIKeyWithSource()
	return source
}

func (c *BobConfig) resolveAPIKeyWithSource() (string, string) {
	if c == nil {
		return "", ""
	}
	files := []string{c.APIKeyFile, DefaultBobAPIKeyFile, WritableBobAPIKeyFile}
	seen := map[string]bool{"": true}
	for _, f := range files {
		if seen[f] {
			continue
		}
		seen[f] = true
		if data, err := os.ReadFile(f); err == nil {
			if key := strings.TrimSpace(string(data)); key != "" {
				return key, "file:" + f
			}
		}
	}
	// TrimSpace the env-var sources too, matching the file branch above. bob
	// does NO trimming of its own — bundle/bob.js reads
	// `process.env.BOBSHELL_API_KEY` verbatim into the Authorization header —
	// and hive delivers the value through `tmux set-environment`, which passes
	// it as a raw exec argument with no shell word-splitting to strip a stray
	// newline. So any trailing whitespace here reaches IBM's API inside the
	// header and 401s, which bob surfaces as a fallback to the SSO flow rather
	// than as an auth error. Cheap to normalize; impossible to debug from the
	// symptom.
	if c.APIKeyEnv != "" {
		if key := strings.TrimSpace(os.Getenv(c.APIKeyEnv)); key != "" {
			return key, "env:" + c.APIKeyEnv
		}
	}
	if key := strings.TrimSpace(os.Getenv(DefaultBobAPIKeyEnv)); key != "" {
		return key, "env:" + DefaultBobAPIKeyEnv
	}
	return "", ""
}

// LiteLLMConfig configures the litellm inference backend: an OpenAI-compatible
// LiteLLM proxy (remote or local) that agents reach through the hive's
// inference translator.
type LiteLLMConfig struct {
	Endpoint     string `yaml:"endpoint"`      // base URL, e.g. https://litellm.example.com
	APIKeyEnv    string `yaml:"api_key_env"`   // env var NAME holding the key; default HIVE_LITELLM_API_KEY
	APIKeyFile   string `yaml:"api_key_file"`  // path to a file holding the key; default /secrets/litellm_api_key
	DefaultModel string `yaml:"default_model"` // model used when an agent has none selected
	CABundle     string `yaml:"ca_bundle"`     // optional PEM path for a private CA (never disables verification)
	LocalProxy   bool   `yaml:"local_proxy"`   // run the bundled litellm binary as a local translator fallback
}

// ResolveAPIKey returns the LiteLLM API key. Key FILES are consulted in
// priority order — the configured api_key_file, then the k8s Secret mount
// (DefaultLiteLLMAPIKeyFile), then the dashboard-written PVC file
// (WritableLiteLLMAPIKeyFile) — followed by the env var named by
// api_key_env and finally DefaultLiteLLMAPIKeyEnv. Returns "" when no key
// is configured. The key value itself is never stored in hive.yaml.
//
// Consulting all three file locations means a key saved via the dashboard
// keeps working even if hive.yaml is reset (e.g. re-seeded from a
// ConfigMap) and the api_key_file pointer is lost, and an admin-managed
// Secret key keeps working if the PVC copy is wiped.
func (c *LiteLLMConfig) ResolveAPIKey() string {
	key, _ := c.resolveAPIKeyWithSource()
	return key
}

// ResolveAPIKeySource reports where ResolveAPIKey found the key without
// exposing the value: "file:<path>", "env:<NAME>", or "" when no key is
// configured. Safe to return from APIs (the dashboard shows it as the
// "Key detected" store).
func (c *LiteLLMConfig) ResolveAPIKeySource() string {
	_, source := c.resolveAPIKeyWithSource()
	return source
}

func (c *LiteLLMConfig) resolveAPIKeyWithSource() (string, string) {
	files := []string{c.APIKeyFile, DefaultLiteLLMAPIKeyFile, WritableLiteLLMAPIKeyFile}
	seen := map[string]bool{"": true}
	for _, f := range files {
		if seen[f] {
			continue
		}
		seen[f] = true
		if data, err := os.ReadFile(f); err == nil {
			if key := strings.TrimSpace(string(data)); key != "" {
				return key, "file:" + f
			}
		}
	}
	if c.APIKeyEnv != "" {
		if key := os.Getenv(c.APIKeyEnv); key != "" {
			return key, "env:" + c.APIKeyEnv
		}
	}
	if key := os.Getenv(DefaultLiteLLMAPIKeyEnv); key != "" {
		return key, "env:" + DefaultLiteLLMAPIKeyEnv
	}
	return "", ""
}

// ResolveEndpoint returns the effective LiteLLM base URL: the
// HIVE_LITELLM_ENDPOINT env var when set, otherwise the YAML endpoint.
func (c *LiteLLMConfig) ResolveEndpoint() string {
	if ep := os.Getenv(LiteLLMEndpointEnv); ep != "" {
		return ep
	}
	return c.Endpoint
}

// Validate checks that the configured endpoint (when set) parses as an
// absolute http(s) URL.
func (c *LiteLLMConfig) Validate() error {
	if c.Endpoint == "" {
		return nil
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil {
		return fmt.Errorf("governor.litellm.endpoint %q is not a valid URL: %w", c.Endpoint, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("governor.litellm.endpoint %q must be an absolute http(s) URL", c.Endpoint)
	}
	return nil
}

type LabelsConfig struct {
	Exempt []string `yaml:"exempt"`
}

type SensingConfig struct {
	GHRatePatterns     []string `yaml:"gh_rate_patterns"`
	CLIExcludePatterns []string `yaml:"cli_exclude_patterns"`
	LoginPatterns      []string `yaml:"login_patterns"`
	TTLSeconds         int      `yaml:"ttl_seconds"`
	PullbackSeconds    int      `yaml:"pullback_seconds"`
}

type HealthConfig struct {
	HealthcheckInterval int  `yaml:"healthcheck_interval"`
	RestartCooldown     int  `yaml:"restart_cooldown"`
	ModelLock           bool `yaml:"model_lock"`
}

type BudgetConfig struct {
	TotalTokens int64 `yaml:"total_tokens"`
	PeriodDays  int   `yaml:"period_days"`
	CriticalPct int   `yaml:"critical_pct"`
}

type ModeConfig struct {
	Threshold int               `yaml:"threshold"`
	Cadences  map[string]string `yaml:"cadences"`
}

// UnmarshalYAML implements custom unmarshaling for ModeConfig.
// The YAML format has threshold and agent cadences as sibling keys:
//
//	idle:
//	  threshold: 0
//	  scanner: 15m
//	  ci-maintainer: 15m
//
// This method separates "threshold" into the Threshold field and collects
// all other keys into the Cadences map.
func (m *ModeConfig) UnmarshalYAML(value *yaml.Node) error {
	var raw map[string]string
	if err := value.Decode(&raw); err != nil {
		return err
	}

	m.Cadences = make(map[string]string)

	const thresholdKey = "threshold"
	if v, ok := raw[thresholdKey]; ok {
		var t int
		if _, err := fmt.Sscanf(v, "%d", &t); err != nil {
			return fmt.Errorf("invalid threshold value %q: %w", v, err)
		}
		m.Threshold = t
	}

	for k, v := range raw {
		if k == thresholdKey {
			continue
		}
		m.Cadences[k] = v
	}

	return nil
}

// MarshalYAML produces the flat format expected by UnmarshalYAML:
// threshold as a sibling key alongside agent cadences.
func (m ModeConfig) MarshalYAML() (interface{}, error) {
	out := make(map[string]interface{})
	out["threshold"] = m.Threshold
	for k, v := range m.Cadences {
		out[k] = v
	}
	return out, nil
}

type GitHubConfig struct {
	AppID              int64  `yaml:"app_id"`
	InstallationID     int64  `yaml:"installation_id"`
	DocsInstallationID int64  `yaml:"docs_installation_id"`
	KeyFile            string `yaml:"key_file"`
	Token              string `yaml:"token"`
	OAuthClientID      string `yaml:"oauth_client_id"`
	// Forge_ names the GitHub instance this hive's App and repos live on, as a
	// bare host: "github.com" or "github.ibm.com". It is the SINGLE
	// AUTHORITATIVE identity field — app_id, app_slug, api_url and base_url are
	// all derivable from it (see Forge, ResolvedAppID, forgeIdentities).
	//
	// Read it through the Forge() method, never directly: Forge() applies the
	// dual-read fallback that keeps a not-yet-backfilled spoke resolving exactly
	// as it does today. The trailing underscore exists solely because the method
	// owns the good name; the YAML key is a clean `forge`.
	//
	// Empty is valid during the migration window and means "infer from the URL
	// fields". Once the fleet is backfilled it becomes mandatory — per the
	// owner: explicitly defined on every spoke on every cluster, no empties, no
	// inheritance. The implicit empty is what disguised the 2026-07-31 damage.
	Forge_ string `yaml:"forge,omitempty"`
	// AppSlug is the GitHub App URL slug for the install link.
	// For public GitHub: "kubestellar-hive". For GHE: your app's slug.
	AppSlug string `yaml:"app_slug"`
	// APIURL is the GitHub API base URL. Defaults to DefaultGitHubAPIURL.
	// For GitHub Enterprise, set to e.g. "https://github.ibm.com/api/v3".
	APIURL string `yaml:"api_url"`
	// BaseURL is the GitHub web base URL. Defaults to DefaultGitHubBaseURL.
	// For GitHub Enterprise, set to e.g. "https://github.ibm.com".
	BaseURL string `yaml:"base_url"`
	// AppAuthoredPRs controls App-bot authorship: when enabled AND ai_author is
	// empty, agents author PRs/commits as the GitHub App bot ("<slug>[bot]") via
	// the App installation token, instead of the Copilot-login user.
	//
	// It is a *bool so absent (nil) is distinct from an explicit false. Default is
	// ON (see AppAuthoredPRsEnabled): App-bot authorship is now the fleet norm, so
	// a hive that says nothing gets it. Set `app_authored_prs: false` to opt a
	// specific hive OUT. This only affects write-capable hives — the token is
	// injected solely for agents whose tier CanPush() and only when a real App is
	// installed (m.appAuth != nil), so an App-less or advisory hive is unaffected.
	AppAuthoredPRs *bool `yaml:"app_authored_prs,omitempty"`
	// OAuthBaseURLOverride and OAuthAPIURLOverride redirect the DEVICE-FLOW LOGIN
	// endpoints away from public github.com. They are a TEST SEAM ONLY — see the
	// comment on OAuthBaseURL(). They carry the `-` yaml tag so they can never be
	// set from a hive.yaml, on the hub, or over the config API: only Go code in a
	// test can assign them.
	OAuthBaseURLOverride string `yaml:"-"`
	OAuthAPIURLOverride  string `yaml:"-"`
}

// PlaceholderAppID is the sentinel `github.app_id` written into a hive's config
// when the hive is provisioned BEFORE its real GitHub App exists — a pooled
// "available-*" placeholder, or a hive whose owner has not installed the App
// yet. Config validation requires either github.token or github.app_id, so the
// seed cannot leave app_id empty; this value fills the slot without ever naming
// a real App.
//
// It is NOT a valid GitHub App ID and must never be handed to
// github.NewAppAuth. Every test for "is a GitHub App configured?" must use
// GitHubConfig.HasApp() rather than a bare `AppID != 0` — the sentinel is
// non-zero, so a bare zero-test reports a placeholder as a real App. That is
// exactly the bug that put hosted hives into permanent CrashLoopBackOff: the
// moment installation_id became non-zero (the owner installed the App), the
// spoke committed to App auth, failed to read a PEM that was never provisioned,
// and exited before the HTTP listener bound — invisible from the dashboard.
const PlaceholderAppID int64 = 999999999

const (
	// DefaultGitHubAPIURL is the default GitHub API endpoint (public github.com).
	DefaultGitHubAPIURL = "https://api.github.com"
	// DefaultGitHubBaseURL is the default GitHub web URL (public github.com).
	DefaultGitHubBaseURL = "https://github.com"
	// DefaultGitHubAppSlug is the public Hive GitHub App slug.
	DefaultGitHubAppSlug = "kubestellar-hive"
	// DefaultOAuthClientID is the PUBLIC github.com Hive App client ID used for
	// device-flow login. Login is always github.com (see OAuthBaseURL), so this
	// is the correct client for every hive — including GHE hives, whose users
	// still sign in with a github.com identity. No secret; safe to hardcode.
	DefaultOAuthClientID = "Ov23ligE2p0gjXg6xAUf"
)

// ─────────────────────────────────────────────────────────────────────────
// FORGE IDENTITY CONSTANTS
//
// A "forge" is the GitHub instance a hive's App and repos live on. Every
// identity value below is derived from the forge — never written independently
// — so the mixed states that broke seven hives on 2026-07-31 (a GHE app_id
// beside an empty, therefore-public api_url) cannot be constructed.
//
// These are the SPOKE-side well-known defaults. The hub carries the
// authoritative per-cluster registration in clusters.json, which may name a
// different App on either forge; these values are what a spoke falls back to
// when the hub has pushed nothing.
// ─────────────────────────────────────────────────────────────────────────
const (
	// ForgePublic is the canonical host label for public github.com. It is a
	// HOSTNAME, not a URL and not a sentinel word — unlike the hub's legacy
	// "public" request sentinel, it is directly comparable to HostLabel().
	ForgePublic = "github.com"

	// ForgeGHEIBM is the IBM GitHub Enterprise host. It is the only GHE forge
	// the fleet currently runs on; naming it as a constant keeps the derivation
	// table literal-free and makes a second GHE forge a one-line addition.
	ForgeGHEIBM = "github.ibm.com"
)

// forgeIdentity is the complete, indivisible identity set for one forge. The
// four fields move together or not at all — that atomicity is the entire point
// of deriving them from a single `forge` value.
type forgeIdentity struct {
	AppID   int64
	AppSlug string
	APIURL  string
	BaseURL string
}

// Forge identity values.
//
// PublicGitHubAppID/PublicGitHubAppSlug and the Enterprise* set are declared
// once, in provenance.go, alongside the rules that validate them — a second
// declaration of the same literal is exactly the drift those rules exist to
// catch. The GHEIBM* names below are aliases so the derivation table reads in
// forge terms without duplicating any value.
const (
	// GHEIBMAppID is the github.ibm.com Hive App's numeric ID.
	GHEIBMAppID = EnterpriseGitHubAppID
	// GHEIBMAppSlug is the github.ibm.com Hive App's URL slug. A GHE instance
	// hosts its own App registration and it is NOT named "kubestellar-hive";
	// falling back to the public slug builds an install link that 404s.
	GHEIBMAppSlug = EnterpriseGitHubAppSlug
	// GHEIBMBaseURL is the github.ibm.com web base URL.
	GHEIBMBaseURL = EnterpriseGitHubBaseURL
	// GHEIBMAPIURL is the github.ibm.com REST API root. GHE serves its API
	// under /api/v3 rather than on a separate api.* hostname.
	GHEIBMAPIURL = EnterpriseGitHubAPIURL
)

// forgeIdentities is the derivation table: forge host -> its complete identity
// set. This is the single source of truth that replaces four independently
// writable fields. Adding a forge means adding a row here; there is no way to
// express a half-identity.
var forgeIdentities = map[string]forgeIdentity{
	ForgePublic: {
		AppID:   PublicGitHubAppID,
		AppSlug: PublicGitHubAppSlug,
		// Empty, not DefaultGitHubAPIURL/DefaultGitHubBaseURL: the public forge's
		// canonical on-disk representation has ALWAYS been the empty string, and
		// ResolvedAPIURL/ResolvedBaseURL already map empty -> the public default.
		// Storing the URLs literally here would make the migration a visible
		// rewrite of every healthy public spoke's config rather than a no-op.
		APIURL:  "",
		BaseURL: "",
	},
	ForgeGHEIBM: {
		AppID:   GHEIBMAppID,
		AppSlug: GHEIBMAppSlug,
		APIURL:  GHEIBMAPIURL,
		BaseURL: GHEIBMBaseURL,
	},
}

// KnownForge reports whether host names a forge with a known identity set.
// An unknown forge is not an error — a hive may legitimately run against a
// third GHE instance the table does not name — but its identity cannot be
// DERIVED, so explicit fields remain load-bearing there.
func KnownForge(host string) bool {
	_, ok := forgeIdentities[normalizeForgeHost(host)]
	return ok
}

// normalizeForgeHost reduces any spelling of a forge — bare host, web URL, API
// URL, mixed case, trailing slash — to its canonical bare-host label.
func normalizeForgeHost(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimSuffix(strings.Trim(s, "/"), "/api/v3")
	host := strings.SplitN(s, "/", 2)[0]
	if host == "" || host == "api.github.com" {
		return ForgePublic
	}
	return host
}

// Forge returns the forge this hive's App and repos live on, as a bare host
// label ("github.com", "github.ibm.com").
//
// PRECEDENCE — and this is the whole design in three lines:
//
//  1. An explicit `forge:` wins. It is the single authoritative field.
//  2. Otherwise infer from the legacy URL fields via HostLabel()/IsGHE().
//  3. Empty everything means public github.com, as it always has.
//
// Step 2 is the DUAL-READ WINDOW. Until every spoke carries `forge:`, a spoke
// that has not been backfilled must resolve exactly as it does today — so this
// never returns anything a pre-migration hive would not already have resolved.
func (g GitHubConfig) Forge() string {
	if f := strings.TrimSpace(g.Forge_); f != "" {
		return normalizeForgeHost(f)
	}
	return normalizeForgeHost(g.HostLabel())
}

// ResolvedAppID returns the GitHub App ID for this hive, completing the
// resolver set alongside ResolvedAPIURL/ResolvedBaseURL/ResolvedAppSlug.
//
// An explicitly configured app_id wins — including the placeholder sentinel,
// which callers detect with IsPlaceholderApp() and must keep seeing. Only when
// app_id is unset is it derived from the forge, which is what lets a hive be
// described by `forge:` alone.
func (g GitHubConfig) ResolvedAppID() int64 {
	if g.AppID != 0 {
		return g.AppID
	}
	if id, ok := forgeIdentities[g.Forge()]; ok {
		return id.AppID
	}
	return 0
}

// ForgeIdentityMismatches reports every identity field that CONTRADICTS this
// hive's forge. Empty means the identity set is coherent.
//
// This is the check that would have caught the 2026-07-31 incident at the
// write boundary. It differs from IdentitySetIssues in the way that matters:
// IdentitySetIssues compares the four fields against EACH OTHER, so it is blind
// whenever the pushed value is the only GHE-looking field present (a GHE app_id
// beside a public slug and empty URLs produces no disagreement at all — verified).
// This compares each field against the DECLARED forge, which is an external
// anchor the pusher does not control.
//
// A field that is empty is not a mismatch: empty means "derive me".
func (g GitHubConfig) ForgeIdentityMismatches() []string {
	forge := g.Forge()
	want, known := forgeIdentities[forge]
	if !known {
		// A forge outside the table has no derivable identity to compare
		// against. Reporting mismatches here would flag every legitimate
		// third-forge hive; saying nothing keeps explicit fields authoritative
		// exactly where they still need to be.
		return nil
	}
	var out []string
	// app_id is compared only when a REAL App is configured. The placeholder
	// sentinel means "no App yet" and legitimately appears on both forges.
	if g.AppID != 0 && !g.IsPlaceholderApp() && g.AppID != want.AppID {
		out = append(out, fmt.Sprintf(
			"app_id %d does not belong to forge %s (expected %d) — an App ID from another forge returns 404 Integration not found on token creation",
			g.AppID, forge, want.AppID))
	}
	if s := strings.TrimSpace(g.AppSlug); s != "" && !strings.EqualFold(s, want.AppSlug) {
		out = append(out, fmt.Sprintf(
			"app_slug %q does not belong to forge %s (expected %q) — the App install link would 404",
			s, forge, want.AppSlug))
	}
	if u := strings.TrimSpace(g.APIURL); u != "" && normalizeForgeHost(u) != forge {
		out = append(out, fmt.Sprintf(
			"api_url %q names forge %s, not %s", u, normalizeForgeHost(u), forge))
	}
	if u := strings.TrimSpace(g.BaseURL); u != "" && normalizeForgeHost(u) != forge {
		out = append(out, fmt.Sprintf(
			"base_url %q names forge %s, not %s", u, normalizeForgeHost(u), forge))
	}
	return out
}

// IsPlaceholderApp reports whether app_id is the "no real App yet" sentinel.
func (g GitHubConfig) IsPlaceholderApp() bool {
	return g.AppID == PlaceholderAppID
}

// HasApp reports whether a REAL GitHub App is configured — a non-zero app_id
// that is not the placeholder sentinel. This is the only correct presence test
// for App auth; `AppID != 0` treats a placeholder as real.
//
// It deliberately says nothing about installation_id or the key file: those are
// separate readiness conditions (see HasUsableApp) that a hive can acquire
// later, over the heartbeat or the config API.
func (g GitHubConfig) HasApp() bool {
	return g.AppID != 0 && !g.IsPlaceholderApp()
}

// HasUsableApp reports whether App auth can actually be attempted: a real
// app_id AND an installation to mint tokens against. The key file is resolved
// separately (config value, then $GH_APP_KEY_FILE, then the on-disk fallbacks),
// so it is not part of this test.
func (g GitHubConfig) HasUsableApp() bool {
	return g.HasApp() && g.InstallationID != 0
}

// ConfiguredButUninstalled reports the half-configured state that must NEVER
// read as healthy: a real App is named but there is no installation to mint
// tokens against. Every token this hive will ever hold is App-minted, so this
// state means auth is a countdown — any client still working is riding a
// cached token that cannot be renewed. Callers use this to raise the install
// banner and fail github_auth from CONFIG truth, not from whether a residual
// token still breathes.
func (g GitHubConfig) ConfiguredButUninstalled() bool {
	return g.HasApp() && g.InstallationID == 0
}

// ResolvedAPIURL returns the configured API URL or the default for github.com.
func (g GitHubConfig) ResolvedAPIURL() string {
	if g.APIURL != "" {
		return g.APIURL
	}
	return DefaultGitHubAPIURL
}

// ResolvedBaseURL returns the base URL of the GitHub the hive's App/repos live
// on, as a full "https://host" URL. It prefers an explicit base_url, but falls
// back to the host of api_url when base_url is blank — pooled/placeholder GHE
// hives legitimately carry base_url: "" with api_url: https://github.ibm.com/api/v3,
// and resolving those to github.com breaks App-host-derived URLs (notably the
// App install link, which would point at github.com/apps/... instead of the GHE
// github.ibm.com/github-apps/... path). This mirrors HostLabel()'s base-or-api
// derivation. Login/device-flow paths deliberately do NOT use this — see
// OAuthBaseURL, which is always public github.com.
func (g GitHubConfig) ResolvedBaseURL() string {
	if g.BaseURL != "" {
		return g.BaseURL
	}
	// base_url blank: derive from api_url's host if it points at a GHE instance.
	if host := g.HostLabel(); host != DefaultGitHubBaseURL[len("https://"):] {
		return "https://" + host
	}
	return DefaultGitHubBaseURL
}

// IsGHE returns true if the hive's App/repos live on a GitHub Enterprise instance
// rather than public github.com. It looks at BOTH base_url and api_url (via
// HostLabel), so a hive carrying only a GHE api_url (base_url: "") is still
// correctly recognized as GHE — matching ResolvedBaseURL and HostLabel.
func (g GitHubConfig) IsGHE() bool {
	return g.HostLabel() != DefaultGitHubBaseURL[len("https://"):]
}

// HostLabel returns the bare GitHub hostname this hive's App/repos live on:
// "github.com" for public GitHub, or the GHE hostname (e.g. "github.ibm.com").
//
// It is the DEFINITIVE host for display and reporting, and deliberately looks at
// BOTH base_url and api_url. Many hives (notably pooled placeholders) carry an
// empty base_url but a GHE api_url (api_url: https://github.ibm.com/api/v3,
// base_url: "") — reporting the host from base_url alone renders those as
// github.com and under-counts GHE hives in the spokes table. Preferring base_url
// when set, then falling back to the api_url's host, makes the reported host
// match the GitHub the hive actually talks to.
func (g GitHubConfig) HostLabel() string {
	pick := g.BaseURL
	if pick == "" {
		pick = g.APIURL
	}
	pick = strings.TrimSpace(pick)
	pick = strings.TrimPrefix(pick, "https://")
	pick = strings.TrimPrefix(pick, "http://")
	pick = strings.TrimSuffix(strings.Trim(pick, "/"), "/api/v3")
	host := strings.SplitN(pick, "/", 2)[0]
	if host == "" || strings.EqualFold(host, "api.github.com") {
		return DefaultGitHubBaseURL[len("https://"):] // "github.com"
	}
	return host
}

// OAuthBaseURL and OAuthAPIURL are the hosts used for DEVICE-FLOW LOGIN and
// user-identity token validation. They are ALWAYS public github.com, decoupled
// from the App/repo host (ResolvedBaseURL/ResolvedAPIURL, which may be GHE).
//
// Why the split: the Hive login OAuth app (oauth_client_id, default the public
// "Ov23ligE2p0gjXg6xAUf") is a github.com app, and every user signs in with a
// github.com identity — even on a hive whose REPOS and GitHub App live on GHE.
// Pointing the device-flow endpoints at a GHE host (as ResolvedBaseURL does for
// a GHE hive) makes GitHub return "Not Found" and the dashboard shows "could not
// verify GitHub identity". Login must therefore use these fixed public hosts;
// only the App-token / repo-API paths follow the per-hive (possibly GHE) host.
// The oauth_*_url_override fields exist ONLY as a test seam. They are
// deliberately absent from every production config: a hive that sets them would
// send its users' device-flow login to a non-github.com host, which is exactly
// the misconfiguration the split above prevents. Tests point them at an
// httptest server so the login path can be exercised without touching the real
// github.com endpoints — see TestMain in pkg/dashboard, which additionally
// hard-fails any test that leaves them unset and would otherwise dial out.
func (g GitHubConfig) OAuthBaseURL() string {
	if g.OAuthBaseURLOverride != "" {
		return g.OAuthBaseURLOverride
	}
	return DefaultGitHubBaseURL
}

func (g GitHubConfig) OAuthAPIURL() string {
	if g.OAuthAPIURLOverride != "" {
		return g.OAuthAPIURLOverride
	}
	return DefaultGitHubAPIURL
}

// OAuthClientIDResolved returns the client ID for device-flow login: the
// configured oauth_client_id, or the public github.com default. Because login
// is always github.com, the default is correct for every hive — a GHE hive must
// not fall back to a GHE client ID here, or device flow against github.com fails
// with an unknown-client error. A hive that has explicitly set oauth_client_id
// to a github.com app keeps that; the default only fills a blank.
func (g GitHubConfig) OAuthClientIDResolved() string {
	if g.OAuthClientID != "" {
		return g.OAuthClientID
	}
	return DefaultOAuthClientID
}

// ResolvedAppSlug returns the app slug for this hive: the configured value,
// else the slug DERIVED from the forge, else the public default.
//
// The forge step is what stops a GHE hive that names only `forge:` from falling
// back to "kubestellar-hive" — a slug that names no App on GHE and builds an
// install link that 404s. That 404 is the failure this whole design exists to
// make unrepresentable, so the derivation belongs here rather than at call sites.
func (g GitHubConfig) ResolvedAppSlug() string {
	if g.AppSlug != "" {
		return g.AppSlug
	}
	if id, ok := forgeIdentities[g.Forge()]; ok && id.AppSlug != "" {
		return id.AppSlug
	}
	return DefaultGitHubAppSlug
}

// BotLogin returns the GitHub App bot login ("<app-slug>[bot]") when a GitHub
// App is configured (AppID set), or "" otherwise. This is the account that
// actually authors PRs and commits when the hive authenticates as an
// installation rather than a personal token.
func (g GitHubConfig) BotLogin() string {
	// Only a REAL, installed App has a bot that can author anything. A bare
	// `AppID != 0` also passes for the placeholder sentinel (PlaceholderAppID)
	// and for a real app_id that was never installed (installation_id: 0) —
	// neither can mint a token or author a PR, so neither has a bot login. Using
	// HasUsableApp() keeps EffectiveAIAuthor() empty for those, so the UI shows
	// "-" (no author) rather than a phantom "<slug>[bot]" for an App-less hive.
	if !g.HasUsableApp() {
		return ""
	}
	return g.ResolvedAppSlug() + "[bot]"
}

// EffectiveAIAuthor returns the GitHub identity agents should author PRs and
// commits as. An explicitly configured project.ai_author always wins. When
// ai_author is empty, the result depends on the opt-in github.app_authored_prs
// flag: if set, agents author as the GitHub App bot ("<slug>[bot]") — deriving
// the bot login here rather than persisting it into ai_author keeps App-bot mode
// durable across restarts (clearing ai_author is enough; nothing writes the bot
// name back into config). If the flag is NOT set, this returns "" exactly as
// before, so a hive that has not opted in behaves identically on upgrade.
func (c *Config) EffectiveAIAuthor() string {
	if c.Project.AIAuthor != "" {
		return c.Project.AIAuthor
	}
	if c.GitHub.AppAuthoredPRsEnabled() {
		return c.GitHub.BotLogin()
	}
	return ""
}

// AppAuthoredPRsEnabled reports whether App-bot authorship is on for this hive.
// The default is ON (nil == true): App-bot authorship is the fleet norm, so a
// hive that never set app_authored_prs gets it. Only an explicit
// `app_authored_prs: false` disables it. Note this is only the INTENT — the
// token is still injected solely for CanPush() tiers with a real App installed,
// so an App-less or advisory hive never actually writes regardless of this flag.
func (g GitHubConfig) AppAuthoredPRsEnabled() bool {
	if g.AppAuthoredPRs == nil {
		return true
	}
	return *g.AppAuthoredPRs
}

// AppInstallURL returns the full URL to install the GitHub App.
// For GHE: {base_url}/github-apps/{slug}/installations/new
// For github.com: https://github.com/apps/{slug}/installations/new
//
// It returns "" for a GitHub Enterprise host whose App slug is not configured.
//
// # WHY EMPTY RATHER THAN A BEST-EFFORT URL
//
// DefaultGitHubAppSlug ("kubestellar-hive") names the App registered on PUBLIC
// github.com. GitHub Enterprise hosts a SEPARATE App registry, and an enterprise
// registration is rarely given the same slug — so falling back to the default on
// a GHE host emits a link to an App that provably does not exist there, and the
// user lands on a GitHub 404. That is strictly worse than no link: a dead link
// reads as "this product is broken", and it sent a real user to
// github.ibm.com/github-apps/kubestellar-hive/installations/new.
//
// The empty string is the honest answer — "this host's App slug was never
// recorded" — and callers render it as a legible message naming the missing
// config instead of a button that 404s. Public github.com keeps the default,
// where it is correct by construction.
func (g GitHubConfig) AppInstallURL() string {
	base := strings.TrimRight(g.ResolvedBaseURL(), "/")
	if g.IsGHE() {
		// No default is admissible here: only an explicitly configured slug can
		// name an App on this enterprise host.
		slug := strings.TrimSpace(g.AppSlug)
		if slug == "" {
			return ""
		}
		return base + "/github-apps/" + slug + "/installations/new"
	}
	return base + "/apps/" + g.ResolvedAppSlug() + "/installations/new"
}

type NotificationsConfig struct {
	Ntfy    *NtfyConfig    `yaml:"ntfy,omitempty"`
	Slack   *SlackConfig   `yaml:"slack,omitempty"`
	Discord *DiscordConfig `yaml:"discord,omitempty"`
}

type NtfyConfig struct {
	Server string `yaml:"server"`
	Topic  string `yaml:"topic"`
}

type SlackConfig struct {
	Webhook string `yaml:"webhook"`
}

type DiscordConfig struct {
	Webhook   string `yaml:"webhook"`
	BotToken  string `yaml:"bot_token"`
	ChannelID string `yaml:"channel_id"`
}

type HubConfig struct {
	Enabled             bool   `yaml:"enabled"`
	URL                 string `yaml:"url"`
	IsPublic            bool   `yaml:"is_public"`
	SnapshotURL         string `yaml:"snapshot_url"`
	DashboardURL        string `yaml:"dashboard_url"`
	HiveType            string `yaml:"hive_type"`
	ClusterID           string `yaml:"cluster_id"`
	AutoSnapshot        bool   `yaml:"auto_snapshot"`
	AutoUpgrade         bool   `yaml:"auto_upgrade"`
	ContributeSuspended bool   `yaml:"contribute_suspended"`
	// Contribute title/author/label filters use a single list plus a mode:
	//   - FilterModeAllow ("allow"): allowlist — an item passes ONLY if it
	//     matches the list (a non-empty list is required for the filter to gate;
	//     an empty allow list means "no items pass" is intentionally avoided —
	//     see passesContributeFilter, where an empty allow list is treated as
	//     "filter off" so a half-configured filter never silently blocks all).
	//   - FilterModeDeny ("deny", default): denylist — an item is skipped if it
	//     matches the list; everything else passes.
	// The *DenyTitles/*DenyAuthors/*DenyLabels fields hold the LIST for each
	// filter regardless of mode (names kept for backward compatibility with
	// existing on-disk config; the mode decides allow vs deny). ContributeAllowLabels
	// is retained only for one-time migration into DenyLabels+LabelsMode.
	ContributeTitlesMode          string   `yaml:"contribute_titles_mode,omitempty"`
	ContributeAuthorsMode         string   `yaml:"contribute_authors_mode,omitempty"`
	ContributeLabelsMode          string   `yaml:"contribute_labels_mode,omitempty"`
	ContributeAllowLabels         []string `yaml:"contribute_allow_labels"`
	ContributeDenyLabels          []string `yaml:"contribute_deny_labels"`
	ContributeDenyTitles          []string `yaml:"contribute_deny_titles"`
	ContributeDenyAuthors         []string `yaml:"contribute_deny_authors"`
	ContributeAllowModels         []string `yaml:"contribute_allow_models"`
	ContributeRejectUnknownModels bool     `yaml:"contribute_reject_unknown_models"`
	// ContributeSkipAssignedToOthers, when true, makes the /contribute queue
	// skip any issue that is already assigned to someone OTHER than the
	// contributor requesting work. An issue assigned to the contributor
	// themselves (or unassigned) is still eligible. Default false preserves the
	// prior behavior of handing out issues regardless of assignment (#2357).
	ContributeSkipAssignedToOthers bool `yaml:"contribute_skip_assigned_to_others"`
	// ContributeCooldownEnabled toggles the POST-COMPLETION cooldown that keeps a
	// just-worked issue out of the /contribute queue for a while (see
	// contribute_ws.go markTaskCompleted / isTaskInCooldown). It is a POINTER so
	// that an absent value (older on-disk config that predates this toggle) means
	// "unset" and defaults to ENABLED — the prior, backward-compatible behavior.
	// A non-nil false explicitly DISABLES cooldown gating (no completed issue is
	// ever excluded from the queue for cooldown; failure quarantine is unaffected
	// and stays on). Use IsContributeCooldownEnabled() to resolve the effective
	// value rather than reading the pointer directly.
	ContributeCooldownEnabled *bool `yaml:"contribute_cooldown_enabled,omitempty"`
	// ContributeCooldownHours is the WITH-PR completion cooldown period in hours —
	// the operator-tunable replacement for the hardcoded default of
	// contributeCooldownDefaultHours (168h / one week). 0 or unset means "use the
	// default"; any positive value is clamped to
	// [contributeCooldownMinHours, contributeCooldownMaxHours] in Normalize.
	// Resolve with ContributeCooldownHoursOrDefault(), never by reading the raw
	// field (which may legitimately be 0 == default). The short NO-PR cooldown is
	// left as its own const and is not tuned here (the operator specifically asked
	// for the week-long period to be adjustable).
	ContributeCooldownHours int `yaml:"contribute_cooldown_hours,omitempty"`
	// ContributeQueueOrder is the OPERATOR PRIORITY OVERRIDE for the ready-work
	// queue: an ordered list of "owner/repo#number" keys the operator dragged to
	// the front on the Operations tab. When set, these issues are OFFERED FIRST —
	// both in the queue display (ReadyQueue) and in selectTask's candidate ordering
	// — in exactly this order; everything else follows in the established default
	// order. It only reorders OFFER PRIORITY: a key here that is filtered out by
	// admission / cooldown / disabled-repo / in-flight rules is still excluded, and
	// a stale key (no longer actionable) is simply skipped. Persisted through the
	// same Config.Hub.* mechanism as the other admission settings so it survives
	// restart, and edited only through the authenticated PUT /api/contribute/queue/order
	// endpoint (owner/read-write only).
	ContributeQueueOrder []string `yaml:"contribute_queue_order,omitempty"`
	// ContributeQueueHold is the OPERATOR HOLD set for the ready-work queue: an
	// unordered list of "owner/repo#number" keys the operator parked from the
	// Operations tab. A held issue is NEVER offered — it is excluded from BOTH the
	// queue display's offer-eligible set (ReadyQueue) and selectTask's candidate
	// selection — and it stays parked INDEFINITELY until the operator Resumes it.
	// This is DISTINCT from cooldown (time-based, self-clearing): a hold is a
	// manual, persistent operator decision. Held rows remain VISIBLE on the
	// Operations tab (rendered greyed with an "on hold" badge) so the operator can
	// always see and Resume them. Persisted through the same Config.Hub.* mechanism
	// as ContributeQueueOrder so it survives restart, and edited only through the
	// authenticated POST /api/contribute/queue/hold endpoint (owner/read-write only).
	ContributeQueueHold []string `yaml:"contribute_queue_hold,omitempty"`
	// ContributeQueueHoldReasons is an OPTIONAL parallel map (canonical
	// "owner/repo#number" key -> short operator note) annotating why an issue in
	// ContributeQueueHold was parked. It is a companion to — not a replacement for —
	// ContributeQueueHold: the []string set above remains the authoritative source of
	// truth for WHICH issues are held (every admission check reads it); this map only
	// carries the human-facing REASON, surfaced in the on-hold badge tooltip. A hold
	// with no reason simply has no entry here (the badge falls back to its generic
	// text), so holding without a note works exactly as before. Kept as a parallel
	// map, rather than folding the reason into ContributeQueueHold, so the many
	// read sites of the []string set stay untouched. Written only by the same
	// authenticated POST /api/contribute/queue/hold endpoint that maintains the set,
	// and pruned to the held keys on every write so it never leaks stale reasons.
	ContributeQueueHoldReasons map[string]string `yaml:"contribute_queue_hold_reasons,omitempty"`
	// ContributeRequireExplicitAccept gates HOW a contributor's scoped GitHub
	// credential is delivered relative to task acceptance (kubestellar/hive#2537).
	// The credential is ALWAYS delivered only AFTER an acceptance decision — it no
	// longer travels bundled in the task_assign message. This toggle only chooses
	// WHO makes that decision:
	//   - nil / false (DEFAULT): trusted-source AUTO-ACCEPT. A task that already
	//     passed admission (the title/author/label filters, disabled-repo/tier
	//     gates, cooldown, and the per-tier trust gate in selectTask) is
	//     auto-accepted the instant it is assigned, and the scoped credential is
	//     delivered immediately after — no human in the loop. This keeps an
	//     unattended fleet running exactly as before: the only observable change is
	//     that the credential arrives in a distinct message right after task_assign
	//     rather than inside it.
	//   - true: EXPLICIT (manual/human) acceptance. The hub withholds the credential
	//     until the client sends a task_accepted for the assigned task; a task that
	//     is never accepted (declined, timed out, or reconnected away) never
	//     receives a credential. This is the opt-in "mandatory acceptance" mode for
	//     operators who want a wait state.
	// A POINTER so an absent value (older on-disk config) resolves to the
	// backward-compatible auto-accept default via IsContributeRequireExplicitAccept().
	ContributeRequireExplicitAccept *bool               `yaml:"contribute_require_explicit_accept,omitempty"`
	DisabledRepos                   []string            `yaml:"disabled_repos"`
	DisabledTiers                   []string            `yaml:"disabled_tiers"`
	TierLimits                      map[string]TierRate `yaml:"tier_limits"`
	SnapshotIntervalMin             int                 `yaml:"snapshot_interval_min"`
}

// Contribute completion-cooldown defaults and clamp bounds. These live in the
// config package because both the resolver methods below and the Normalize path
// reference them; the dashboard keeps its own equal DEFAULT const
// (completedTaskCooldownHours) as the runtime fallback for hubs built without a
// Config (e.g. direct-in-test construction).
const (
	// contributeCooldownDefaultHours is the with-PR completion cooldown used when
	// ContributeCooldownHours is unset/0 — one week, matching the historical
	// hardcoded default.
	contributeCooldownDefaultHours = 168
	// contributeCooldownMinHours / contributeCooldownMaxHours clamp an
	// operator-supplied period to a sane range (one hour .. one year) so a stray
	// value cannot park an issue effectively forever or disable the cooldown by
	// rounding to zero.
	contributeCooldownMinHours = 1
	contributeCooldownMaxHours = 8760
)

// IsContributeCooldownEnabled resolves the effective on/off state of the
// post-completion cooldown. A nil pointer (unset, older config) defaults to
// ENABLED for backward compatibility; an explicit false disables it.
func (h HubConfig) IsContributeCooldownEnabled() bool {
	return h.ContributeCooldownEnabled == nil || *h.ContributeCooldownEnabled
}

// IsContributeRequireExplicitAccept resolves the effective acceptance mode for
// contributor credential delivery (kubestellar/hive#2537). A nil pointer (unset,
// older config) resolves to FALSE — trusted-source auto-accept — so an existing
// deployment keeps handing credentials to admitted tasks without a wait state; an
// explicit true opts into mandatory (human/manual) acceptance where the credential
// is withheld until the client accepts the assigned task.
func (h HubConfig) IsContributeRequireExplicitAccept() bool {
	return h.ContributeRequireExplicitAccept != nil && *h.ContributeRequireExplicitAccept
}

// ContributeCooldownHoursOrDefault resolves the with-PR cooldown PERIOD in
// hours. A value <= 0 (unset) yields the default (contributeCooldownDefaultHours);
// a positive value is returned as-is (Normalize has already clamped any stored
// value to the valid range, and this method re-clamps defensively for callers
// that build a Hub without running Normalize, e.g. tests).
func (h HubConfig) ContributeCooldownHoursOrDefault() int {
	if h.ContributeCooldownHours <= 0 {
		return contributeCooldownDefaultHours
	}
	if h.ContributeCooldownHours < contributeCooldownMinHours {
		return contributeCooldownMinHours
	}
	if h.ContributeCooldownHours > contributeCooldownMaxHours {
		return contributeCooldownMaxHours
	}
	return h.ContributeCooldownHours
}

type TierRate struct {
	MaxPerHour    int `yaml:"max_per_hour" json:"max_per_hour"`
	MaxPerDay     int `yaml:"max_per_day" json:"max_per_day"`
	MaxConcurrent int `yaml:"max_concurrent" json:"max_concurrent"`
}

type DashboardConfig struct {
	Port               int    `yaml:"port"`
	SnapshotDir        string `yaml:"snapshot_dir"`
	AuthToken          string `yaml:"auth_token"`
	AgentPollIntervalS int    `yaml:"agent_poll_interval_s"`
	// AuthorizedUsers is the allowlist of GitHub usernames permitted to log in
	// to a direct-route (non-hub-proxied) spoke via the device flow. The first
	// entry is treated as the owner (read-write); the rest are granted viewers
	// (read-only) unless an explicit "username:role" suffix is given. On the
	// hub-proxied path, nginx injects X-Hive-User/X-Hive-Role and this list is
	// consulted only by the read-only Access tab, NOT for gating logins.
	// Populated on both hub-proxied hosted hives (for the Access view + hub
	// Manage Access) and standalone direct-route spokes (for device-flow authz).
	AuthorizedUsers []string `yaml:"authorized_users"`
	// HubProxied is true when this hive sits behind the hub's nginx auth-proxy,
	// which authenticates every request and injects trusted X-Hive-User/X-Hive-Role
	// headers (hive-oke hosted hives). When true the hive TRUSTS those headers and
	// keeps the shared-token path enabled even if AuthorizedUsers is non-empty —
	// nginx is the gate, and the allowlist is informational (Access tab) only.
	//
	// When false (the default) a non-empty AuthorizedUsers list means this is a
	// STANDALONE direct-route spoke with no nginx in front (vllm-d), so it must
	// strip client-supplied identity headers and enforce per-user device-flow
	// authz itself. Decoupling these two meanings fixes hub-proxied hives being
	// wrongly forced into direct-route mode (which broke their dashboard link and
	// snapshot preview) the moment they were granted an authorized_users list.
	HubProxied bool `yaml:"hub_proxied"`
}

// Role strings used for direct-route spoke authorization. These mirror the
// roles the hub injects via X-Hive-Role on the proxied path so the read-only
// gating in the dashboard behaves identically on both paths.
const (
	RoleOwner = "owner"
	RoleRead  = "read"
	// RoleReadWrite is granted by the hub's Manage Access screen (see the role
	// validation in hub.handleAccessAdd, which accepts read / read-write /
	// owner). It was missing here, so splitAuthorizedEntry treated
	// "user:read-write" as an unknown suffix and folded the whole string into
	// the username — the allowlist then contained a user literally named
	// "cbrooker27:read-write", no lookup could ever match, and every login was
	// rejected as unauthorized despite the grant being present and correct.
	RoleReadWrite = "read-write"
)

// AuthorizedRole resolves a GitHub username against the spoke's authorized-users
// allowlist and returns the user's role and whether they are authorized.
//
// Each entry is either "username" or "username:role" (role = "owner" or "read").
// An entry without an explicit role defaults to "owner" for the first entry
// (the hive owner) and "read" for the rest (granted viewers). Matching is
// case-insensitive because GitHub usernames are case-insensitive.
//
// A spoke with an empty AuthorizedUsers list is NOT a direct-route spoke that
// enforces device-flow authz (it is either hub-proxied or misconfigured); the
// caller decides how to treat that case — see IsDirectRouteAuthzEnabled.
func (d DashboardConfig) AuthorizedRole(username string) (string, bool) {
	if username == "" {
		return "", false
	}
	want := strings.ToLower(username)
	for i, entry := range d.AuthorizedUsers {
		name, role := splitAuthorizedEntry(entry)
		if name == "" {
			continue
		}
		if strings.ToLower(name) != want {
			continue
		}
		if role == "" {
			if i == 0 {
				role = RoleOwner
			} else {
				role = RoleRead
			}
		}
		return role, true
	}
	return "", false
}

// IsDirectRouteAuthzEnabled reports whether this hive must enforce per-user
// authorization on device-flow logins ITSELF because there is no hub nginx in
// front of it. True only for a STANDALONE spoke: it has an authorized-users
// allowlist AND is not hub-proxied.
//
// A hub-proxied hive (HubProxied=true) can also carry an allowlist — for the
// read-only Access tab and the hub's Manage Access screen — but nginx is the
// gate there, so it must NOT flip into direct-route mode (which would strip the
// trusted X-Hive-User/X-Hive-Role headers nginx injects and disable the shared
// token, breaking the dashboard link and the snapshot preview).
func (d DashboardConfig) IsDirectRouteAuthzEnabled() bool {
	return !d.HubProxied && len(d.AuthorizedUsers) > 0
}

// splitAuthorizedEntry parses a "username" or "username:role" allowlist entry.
func splitAuthorizedEntry(entry string) (name, role string) {
	entry = strings.TrimSpace(entry)
	if idx := strings.LastIndex(entry, ":"); idx >= 0 {
		name = strings.TrimSpace(entry[:idx])
		role = strings.ToLower(strings.TrimSpace(entry[idx+1:]))
		if role != RoleOwner && role != RoleRead && role != RoleReadWrite {
			// Unknown role suffix — treat the whole thing as a bare username so
			// a stray colon can never silently downgrade or escalate access.
			return strings.TrimSpace(entry), ""
		}
		return name, role
	}
	return entry, ""
}

type DataConfig struct {
	MetricsDir         string `yaml:"metrics_dir"`
	LogsDir            string `yaml:"logs_dir"`
	ClaudeSessionsDir  string `yaml:"claude_sessions_dir"`
	CopilotSessionsDir string `yaml:"copilot_sessions_dir"`
	AgentsDir          string `yaml:"agents_dir"`
}

// Load reads hive.yaml, then applies config.env overrides if present.
// Precedence: hive.yaml < config.env < explicit env vars (via ${} interpolation).
func Load(path string) (*Config, error) {
	return LoadWithOverrides(path, "")
}

// LoadWithOverrides reads hive.yaml and applies a config.env override file.
// If envPath is empty, it looks for config.env next to hive.yaml, then at
// /etc/hive/config.env. Pass "-" to skip config.env entirely.
func LoadWithOverrides(path, envPath string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	expanded := expandEnvVars(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	if envPath != "-" {
		if envPath == "" {
			envPath = findConfigEnv(path)
		}
		if envPath != "" {
			if err := cfg.applyConfigEnv(envPath); err != nil {
				return nil, fmt.Errorf("applying config.env %s: %w", envPath, err)
			}
		}
	}

	cfg.SourcePath = path
	cfg.applyBootstrapEnv()
	cfg.applyDefaults()

	// Merge per-agent overlay files from the agents directory.
	if cfg.Data.AgentsDir != "" {
		overlays, err := LoadAgentOverrides(cfg.Data.AgentsDir)
		if err != nil {
			return nil, fmt.Errorf("loading agent overlays: %w", err)
		}
		cfg.MergeAgentOverrides(overlays)
		// Re-apply defaults for overlay agents.
		for name := range overlays {
			cfg.ApplyAgentDefaults(name)
		}
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return &cfg, nil
}

// LoadWithDashboardOverlay loads the config from path, then — in Kubernetes
// mode — re-applies the dashboard overlay's agent configs on top, mirroring
// the entrypoint's boot-time seed+overlay merge.
//
// Why this exists: the ConfigMap seed at path carries only provision-time
// agent fields. Runtime reconciliation (ApplyPack raising a hive's ACMM level
// updates kick_template/mode/model) is persisted to the dashboard overlay, NOT
// the seed. The entrypoint merges the overlay over the seed once at boot, but a
// live ConfigMap remount rewrites the seed back to its stale values and fires
// the config watcher. If the watcher reloaded the raw seed it would silently
// revert every reconciled agent field (observed: a hive raised to L5 dropped
// its scanner back to the L2/L3 advisory template at runtime). Applying the
// overlay here keeps the reload consistent with boot.
func LoadWithDashboardOverlay(path string) (*Config, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	if !IsKubernetesPod() {
		return cfg, nil
	}
	data, err := os.ReadFile(DashboardOverlayFile)
	if err != nil {
		// No overlay (or unreadable) — the seed is authoritative, as at boot.
		return cfg, nil
	}
	var overlay Config
	if err := yaml.Unmarshal([]byte(expandEnvVars(string(data))), &overlay); err != nil {
		return cfg, nil // malformed overlay: fall back to seed, don't fail the reload
	}
	// Tombstones live in the dashboard overlay because that is the only agent
	// source the dashboard can write. Adopt them BEFORE the fullness guard below
	// so a short/empty overlay (one that has no agents yet, or only carries the
	// removed_agents list) still yields the tombstone. Previously this ran AFTER
	// the guard, so on a reload the guard's early return dropped RemovedAgents to
	// empty; the ~2-min saver then rewrote every layer tombstone-free and the
	// deleted agents reappeared on an interval (#2439). Merge already skips and
	// prunes tombstoned agents, so adopting them early is safe even when we bail.
	if len(overlay.RemovedAgents) > 0 {
		cfg.RemovedAgents = overlay.RemovedAgents
		cfg.PruneRemovedAgents()
		// Observability (#2439): this runs on boot AND on every ~2-min config reload,
		// so keep it at DEBUG. It confirms the reload adopted the overlay's tombstones
		// BEFORE the fullness guard below — the exact ordering whose absence let the
		// deleted agents reappear on an interval.
		slog.Default().Debug("reload: adopted removed-agents from overlay",
			"hive_id", cfg.HiveID,
			"count", len(cfg.RemovedAgents),
			"agents", cfg.RemovedAgents,
		)
	}
	// Guard: the overlay must look like a full hive config (same check the
	// entrypoint and validateSaveGuard apply) before we trust its agents.
	if overlay.Project.Org == "" || len(overlay.Agents) == 0 {
		return cfg, nil
	}
	// Overlay agents win — they carry the reconciled pack-behavior fields.
	cfg.MergeAgentOverrides(overlay.Agents)
	for name := range overlay.Agents {
		cfg.ApplyAgentDefaults(name)
	}
	// Security invariant: cfg.Variables (resolver defs + the exec/http trust
	// policy) comes ONLY from the seed loaded above — the overlay's Variables
	// block is intentionally NOT merged. The dashboard overlay is user-writable,
	// so honoring its resolver policy would let a compromised overlay enable
	// script/http execution. Keep this true if overlay merging is ever expanded.
	return cfg, nil
}

// findConfigEnv returns the path to a config.env file, or "" if none found.
func findConfigEnv(yamlPath string) string {
	candidates := []string{
		strings.TrimSuffix(yamlPath, "hive.yaml") + "config.env",
		"/etc/hive/config.env",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// ParseEnvFile reads a flat KEY=VALUE file (# comments, blank lines skipped).
func ParseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	result := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		result[key] = val
	}
	return result, scanner.Err()
}

// applyConfigEnv merges flat KEY=VALUE overrides into the loaded config.
func (c *Config) applyConfigEnv(path string) error {
	env, err := ParseEnvFile(path)
	if err != nil {
		return err
	}

	if v, ok := env["PROJECT_ORG"]; ok {
		c.Project.Org = v
	}
	if v, ok := env["PROJECT_REPOS"]; ok {
		c.Project.Repos = strings.Fields(v)
	}
	if v, ok := env["PROJECT_AI_AUTHOR"]; ok {
		c.Project.AIAuthor = v
	}
	if v, ok := env["PROJECT_PRIMARY_REPO"]; ok {
		c.Project.PrimaryRepo = v
	}
	if v, ok := env["PROJECT_OPEN_PRS"]; ok {
		b := v == "true" || v == "1" || v == "yes"
		c.Project.OpenPRs = &b
	}
	if v, ok := env["AGENTS_ENABLED"]; ok {
		for _, name := range strings.Fields(v) {
			if agent, exists := c.Agents[name]; exists {
				agent.Enabled = true
				c.Agents[name] = agent
			}
		}
	}
	if v, ok := env["DASHBOARD_PORT"]; ok {
		var port int
		if _, err := fmt.Sscanf(v, "%d", &port); err == nil && port > 0 {
			c.Dashboard.Port = port
		}
	}
	if v, ok := env["DASHBOARD_AUTH_TOKEN"]; ok {
		c.Dashboard.AuthToken = v
	}
	if c.Dashboard.AuthToken == "" {
		if v, ok := env["HIVE_DASHBOARD_TOKEN"]; ok {
			c.Dashboard.AuthToken = v
		}
	}

	return nil
}

func (c *Config) applyBootstrapEnv() {
	if repo := os.Getenv("HIVE_REPO"); repo != "" {
		parts := strings.SplitN(repo, "/", 2)
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			if c.Project.Org == "" {
				c.Project.Org = parts[0]
			}
			if len(c.Project.Repos) == 0 {
				c.Project.Repos = []string{parts[1]}
			}
			if c.Project.PrimaryRepo == "" {
				c.Project.PrimaryRepo = parts[1]
			}
		}
	}
	// K8s deployments pass the auth token as an OS env var from a Secret.
	// applyConfigEnv only reads file-based config.env, so without this
	// the token is silently ignored and the dashboard is unauthenticated.
	if c.Dashboard.AuthToken == "" {
		if v := os.Getenv("DASHBOARD_AUTH_TOKEN"); v != "" {
			c.Dashboard.AuthToken = v
		}
	}
	if c.Dashboard.AuthToken == "" {
		if v := os.Getenv("HIVE_DASHBOARD_TOKEN"); v != "" {
			c.Dashboard.AuthToken = v
		}
	}
	// K8s-provisioned spokes receive their per-hive authorized GitHub users as a
	// comma-separated env var (owner first). This is what lets a direct-route
	// spoke reject unauthorized device-flow logins without the hub proxy.
	if len(c.Dashboard.AuthorizedUsers) == 0 {
		if v := os.Getenv("HIVE_AUTHORIZED_USERS"); v != "" {
			c.Dashboard.AuthorizedUsers = parseAuthorizedUsers(v)
		}
	}
}

// parseAuthorizedUsers splits a comma-separated authorized-users list, trimming
// whitespace and dropping empty entries. Order is preserved so the first entry
// remains the owner.
func parseAuthorizedUsers(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if u := strings.TrimSpace(p); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// expandEnvVars substitutes ${VAR} references in the raw config text. It runs
// BEFORE the YAML is parsed, so to honor an operator `variables:` block it
// first bootstrap-parses just that block from the same text, builds a
// config-scoped resolve.Registry, and delegates. With no `variables:` block the
// registry is env-only and the result is byte-identical to the legacy behavior
// (${NAME} -> os.LookupEnv(NAME), unset left literal).
func expandEnvVars(s string) string {
	reg := configRegistryFromText(s)
	return reg.Expand(context.Background(), s, resolve.ScopeConfig, nil)
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

const (
	defaultDashboardPort          = 3002
	defaultAgentPollIntervalS     = 10
	defaultEvalIntervalS          = 300
	defaultPollIntervalMins       = 5
	defaultKnowledgeMaxFacts      = 25
	defaultKnowledgeEngine        = "llm-wiki"
	defaultCuratorSchedule        = "daily"
	defaultPromoteThreshold       = 0.9
	defaultSensingTTLSeconds      = 900
	defaultSensingPullbackSeconds = 900
	defaultHealthcheckIntervalS   = 300
	defaultRestartCooldownS       = 60
	defaultBudgetPeriodDays       = 7
	defaultBudgetCriticalPct      = 90
	defaultLogMaxSizeMB           = 50
	defaultLogMaxAgeDays          = 7
	defaultLogMaxBackups          = 10
	defaultLogLevel               = "info"
)

func (c *Config) applyDefaults() {
	if c.Project.PrimaryRepo != "" && c.Project.Org != "" {
		prefix := c.Project.Org + "/"
		if strings.HasPrefix(c.Project.PrimaryRepo, prefix) {
			c.Project.PrimaryRepo = strings.TrimPrefix(c.Project.PrimaryRepo, prefix)
		}
	}
	if c.Dashboard.Port == 0 {
		c.Dashboard.Port = defaultDashboardPort
	}
	if c.Dashboard.AgentPollIntervalS == 0 {
		c.Dashboard.AgentPollIntervalS = defaultAgentPollIntervalS
	}
	if c.Governor.EvalIntervalS == 0 {
		c.Governor.EvalIntervalS = defaultEvalIntervalS
	}
	if c.Governor.Trajectory.IsEnabled() {
		if c.Governor.Trajectory.OnDivergence == "" {
			c.Governor.Trajectory.OnDivergence = "pause"
		}
	}
	if c.Policies.PollInterval == 0 {
		c.Policies.PollInterval = time.Duration(defaultPollIntervalMins) * time.Minute
	}
	if c.Data.MetricsDir == "" {
		c.Data.MetricsDir = "/data/metrics"
	}
	if c.Data.LogsDir == "" {
		c.Data.LogsDir = "/data/logs"
	}
	if c.Data.ClaudeSessionsDir == "" {
		c.Data.ClaudeSessionsDir = "/data/home/.claude/projects"
	}
	if c.Data.CopilotSessionsDir == "" {
		c.Data.CopilotSessionsDir = "/data/home/.copilot/session-state"
	}
	if c.Data.AgentsDir == "" {
		c.Data.AgentsDir = "/data/agent-configs"
	}
	if c.Hub.URL == "" {
		c.Hub.URL = "https://hive.kubestellar.io"
		c.Hub.IsPublic = true
	}
	for name, agent := range c.Agents {
		agent.name = name
		if agent.ID == "" {
			agent.ID = name
		}
		if agent.BeadsDir == "" {
			agent.BeadsDir = fmt.Sprintf("/data/beads/%s", name)
		}
		// Default to enabled unless the user explicitly set enabled: false.
		if !agent.Enabled && !agent.enabledSet {
			agent.Enabled = true
		}
		if !agent.clearOnKickSet {
			agent.ClearOnKick = true
		}
		if agent.Role == "" {
			agent.Role = name
		}
		applyKnownAgentDefaults(name, &agent)
		c.Agents[name] = agent
	}

	if len(c.Hub.ContributeDenyTitles) == 0 {
		c.Hub.ContributeDenyTitles = []string{
			"*dependency dashboard*",
			"*renovate dashboard*",
			"epic:*",
			"epic(*",
		}
	}
	if len(c.Hub.ContributeDenyAuthors) == 0 {
		c.Hub.ContributeDenyAuthors = []string{
			"renovate[bot]",
			"dependabot[bot]",
			"mergeraptor[bot]",
		}
	}

	// Contribute filter modes: default to deny (the pre-mode behavior — the
	// *Deny* lists were always deny lists). Normalize any stored value.
	c.Hub.ContributeTitlesMode = NormalizeFilterMode(c.Hub.ContributeTitlesMode)
	c.Hub.ContributeAuthorsMode = NormalizeFilterMode(c.Hub.ContributeAuthorsMode)
	c.Hub.ContributeLabelsMode = NormalizeFilterMode(c.Hub.ContributeLabelsMode)

	// Contribute completion-cooldown period: leave 0 (== "use default") alone, but
	// clamp any explicitly-set value to [min,max] so a stray input cannot park an
	// issue effectively forever or round down to disable the cooldown.
	if c.Hub.ContributeCooldownHours != 0 {
		if c.Hub.ContributeCooldownHours < contributeCooldownMinHours {
			c.Hub.ContributeCooldownHours = contributeCooldownMinHours
		} else if c.Hub.ContributeCooldownHours > contributeCooldownMaxHours {
			c.Hub.ContributeCooldownHours = contributeCooldownMaxHours
		}
	}

	// One-time migration of the old dual label lists into the single list+mode.
	// If a legacy allow list was set (and no new label list/mode has been chosen
	// yet), adopt it as an allow filter. Otherwise the deny list (if any) stands.
	// After migration the allow list is cleared so it isn't re-applied.
	if len(c.Hub.ContributeAllowLabels) > 0 && len(c.Hub.ContributeDenyLabels) == 0 &&
		c.Hub.ContributeLabelsMode == FilterModeDeny {
		c.Hub.ContributeDenyLabels = c.Hub.ContributeAllowLabels
		c.Hub.ContributeLabelsMode = FilterModeAllow
		c.Hub.ContributeAllowLabels = nil
	}

	if len(c.Hub.TierLimits) == 0 {
		c.Hub.TierLimits = map[string]TierRate{
			"newcomer":    {MaxPerHour: 3, MaxPerDay: 10, MaxConcurrent: 1},
			"contributor": {MaxPerHour: 10, MaxPerDay: 50, MaxConcurrent: 2},
			"trusted":     {MaxPerHour: 30, MaxPerDay: 200, MaxConcurrent: 5},
			"advisor":     {MaxPerHour: 0, MaxPerDay: 0, MaxConcurrent: 0},
		}
	}

	if len(c.Governor.Labels.Exempt) == 0 {
		c.Governor.Labels.Exempt = []string{
			"nightly-tests", "LFX", "meta-tracker",
			"auto-qa-tuning-report", "adopters",
			"changes-requested", "waiting-on-author",
		}
	}
	if len(c.Governor.Sensing.GHRatePatterns) == 0 {
		c.Governor.Sensing.GHRatePatterns = []string{
			"API rate limit exceeded",
			"secondary rate limit",
			"403.*rate limit",
			"You have exceeded a secondary rate",
			"retry-after:[[:space:]]*[0-9]",
			"gh: Resource not accessible",
			"abuse detection mechanism",
		}
	}
	if len(c.Governor.Sensing.CLIExcludePatterns) == 0 {
		c.Governor.Sensing.CLIExcludePatterns = []string{
			"You.re out of extra usage",
			"out of extra usage",
			"extra usage.*resets",
			"resets [0-9]+(:[0-9]+)?[aApP][mM]",
		}
	}
	if len(c.Governor.Sensing.LoginPatterns) == 0 {
		c.Governor.Sensing.LoginPatterns = []string{
			"please log in",
			"authentication required",
			"not logged in",
			"login required",
			"session expired",
			"token expired",
			"unauthorized.*401",
			"gh auth login",
			"claude login",
			"copilot auth",
		}
	}
	if c.Governor.Sensing.TTLSeconds == 0 {
		c.Governor.Sensing.TTLSeconds = defaultSensingTTLSeconds
	}
	if c.Governor.Sensing.PullbackSeconds == 0 {
		c.Governor.Sensing.PullbackSeconds = defaultSensingPullbackSeconds
	}
	if c.Governor.Health.HealthcheckInterval == 0 {
		c.Governor.Health.HealthcheckInterval = defaultHealthcheckIntervalS
	}
	if c.Governor.Health.RestartCooldown == 0 {
		c.Governor.Health.RestartCooldown = defaultRestartCooldownS
	}
	if c.Governor.Budget.PeriodDays == 0 {
		c.Governor.Budget.PeriodDays = defaultBudgetPeriodDays
	}
	if c.Governor.Budget.CriticalPct == 0 {
		c.Governor.Budget.CriticalPct = defaultBudgetCriticalPct
	}
	if c.Governor.Logging.Dir == "" {
		c.Governor.Logging.Dir = c.Data.LogsDir
	}
	if c.Governor.Logging.MaxSizeMB == 0 {
		c.Governor.Logging.MaxSizeMB = defaultLogMaxSizeMB
	}
	if c.Governor.Logging.MaxAgeDays == 0 {
		c.Governor.Logging.MaxAgeDays = defaultLogMaxAgeDays
	}
	if c.Governor.Logging.MaxBackups == 0 {
		c.Governor.Logging.MaxBackups = defaultLogMaxBackups
	}
	if !c.Governor.Logging.Compress {
		c.Governor.Logging.Compress = true
	}
	if c.Governor.Logging.Level == "" {
		c.Governor.Logging.Level = defaultLogLevel
	}

	if c.Knowledge.Enabled {
		if c.Knowledge.Engine == "" {
			c.Knowledge.Engine = defaultKnowledgeEngine
		}
		if c.Knowledge.Primer.MaxFacts == 0 {
			c.Knowledge.Primer.MaxFacts = defaultKnowledgeMaxFacts
		}
		if c.Knowledge.Primer.MergeStrategy == "" {
			c.Knowledge.Primer.MergeStrategy = "precedence"
		}
		if len(c.Knowledge.Primer.Priority) == 0 {
			c.Knowledge.Primer.Priority = []string{"regression", "gotcha", "test_scaffold", "pattern", "decision"}
		}
		if c.Knowledge.Curator.Schedule == "" {
			c.Knowledge.Curator.Schedule = defaultCuratorSchedule
		}
		if c.Knowledge.Curator.AutoPromoteThreshold == 0 {
			c.Knowledge.Curator.AutoPromoteThreshold = defaultPromoteThreshold
		}
		if c.Knowledge.BeadSynthesizer.Schedule == "" {
			c.Knowledge.BeadSynthesizer.Schedule = "hourly"
		}
		if c.Knowledge.BeadSynthesizer.MinConfidence == 0 {
			c.Knowledge.BeadSynthesizer.MinConfidence = 0.5
		}
		if c.Knowledge.BeadSynthesizer.TargetLayer == "" {
			c.Knowledge.BeadSynthesizer.TargetLayer = "project"
		}
		if c.Knowledge.BeadSynthesizer.MaxFactsPerCycle == 0 {
			c.Knowledge.BeadSynthesizer.MaxFactsPerCycle = 20
		}
		if c.Knowledge.BeadSynthesizer.VaultPath == "" {
			c.Knowledge.BeadSynthesizer.VaultPath = "/data/vaults/bead-synth-wiki"
		}
	}
}

// applyKnownAgentDefaults populates metadata fields for well-known agent names
// when those fields are not explicitly set in YAML. This bridges existing configs.
func applyKnownAgentDefaults(name string, agent *AgentConfig) {
	type knownAgent struct {
		Emoji          string
		Color          string
		Aliases        []string
		LaneKeywords   []string
		DetectKeywords []string
		BeadRole       string
		SortOrder      int
		IncludeRepos   bool
	}

	known := map[string]knownAgent{
		"scanner": {
			Emoji: "🔍", Color: "#3498db", Aliases: []string{"sc"},
			LaneKeywords:   []string{"bug", "triage", "typo", "fix"},
			DetectKeywords: []string{"scanner", "triage", "issue", "bug"},
			BeadRole:       "worker", SortOrder: 20, IncludeRepos: true,
		},
		"ci-maintainer": {
			Emoji: "🔧", Color: "#2ecc71", Aliases: []string{"ci"},
			LaneKeywords:   []string{"workflow-failure", "ci-failure", "nightly", "coverage", "regression", "ga4", "analytics"},
			DetectKeywords: []string{"ci-maintainer", "review", "ci", "coverage", "ga4"},
			BeadRole:       "worker", SortOrder: 30, IncludeRepos: true,
		},
		"architect": {
			Emoji: "🏗", Color: "#9b59b6", Aliases: []string{"ar"},
			LaneKeywords:   []string{"rfc", "architecture", "refactor", "redesign", "migration", "breaking change", "protocol", "api design"},
			DetectKeywords: []string{"architect", "rfc", "refactor"},
			BeadRole:       "worker", SortOrder: 40, IncludeRepos: true,
		},
		"outreach": {
			Emoji: "🌐", Color: "#e67e22", Aliases: []string{"ou"},
			LaneKeywords:   []string{"adopters", "outreach", "community", "engagement"},
			DetectKeywords: []string{"outreach", "adopters", "community"},
			BeadRole:       "worker", SortOrder: 50, IncludeRepos: false,
		},
		"supervisor": {
			Emoji: "👑", Color: "#e74c3c", Aliases: []string{"su"},
			DetectKeywords: []string{"supervisor", "sweep", "monitor"},
			BeadRole:       "supervisor", SortOrder: 10, IncludeRepos: true,
		},
		"sec-check": {
			Emoji: "🛡", Color: "#1abc9c", Aliases: []string{"se"},
			DetectKeywords: []string{"security", "sec-check", "vulnerability"},
			BeadRole:       "worker", SortOrder: 60, IncludeRepos: true,
		},
		"quality": {
			Emoji: "🧪", Color: "#3498db", Aliases: []string{"te", "qa"},
			LaneKeywords:   []string{"test-gap", "test-strategy", "test-coverage", "test-scaffold", "untested", "missing-tests"},
			DetectKeywords: []string{"quality", "test", "coverage"},
			BeadRole:       "worker", SortOrder: 35, IncludeRepos: true,
		},
		"strategist": {
			Emoji: "🧠", Color: "#f39c12", Aliases: []string{"sg"},
			DetectKeywords: []string{"strategist", "strategy"},
			BeadRole:       "worker", SortOrder: 70, IncludeRepos: true,
		},
		"guide": {
			Emoji: "📖", Color: "#8e44ad", Aliases: []string{"gu"},
			LaneKeywords:   []string{"docs", "documentation", "readme", "guide", "tutorial", "onboarding"},
			DetectKeywords: []string{"guide", "docs", "documentation"},
			BeadRole:       "worker", SortOrder: 45, IncludeRepos: true,
		},
	}

	k, ok := known[name]
	if !ok {
		return
	}

	if agent.Emoji == "" {
		agent.Emoji = k.Emoji
	}
	if agent.Color == "" {
		agent.Color = k.Color
	}
	if len(agent.Aliases) == 0 && len(k.Aliases) > 0 {
		agent.Aliases = k.Aliases
	}
	if len(agent.LaneKeywords) == 0 && len(k.LaneKeywords) > 0 {
		agent.LaneKeywords = k.LaneKeywords
	}
	if len(agent.DetectKeywords) == 0 && len(k.DetectKeywords) > 0 {
		agent.DetectKeywords = k.DetectKeywords
	}
	if agent.BeadRole == "" {
		agent.BeadRole = k.BeadRole
	}
	if agent.SortOrder == 0 {
		agent.SortOrder = k.SortOrder
	}
	if agent.IncludeRepos == nil {
		v := k.IncludeRepos
		agent.IncludeRepos = &v
	}
}

// InferenceBackends is the canonical list of self-hosted inference backend
// IDs. It lives in the config package (a leaf in the import graph) so the
// agent and proxy packages can share it without an import cycle
// (proxy → agent → config).
var InferenceBackends = []string{"vllm", "llm-d", "litellm"}

// IsInferenceBackend returns true if the backend name is a self-hosted
// inference backend rather than a CLI tool.
func IsInferenceBackend(backend string) bool {
	for _, b := range InferenceBackends {
		if b == backend {
			return true
		}
	}
	return false
}

func (c *Config) validate() error {
	if c.Project.Org == "" {
		return fmt.Errorf("project.org is required")
	}
	// Repos can be empty — L1 inception starts with just an idea, no repo.
	if len(c.Agents) == 0 {
		return fmt.Errorf("at least one agent must be configured")
	}
	// Deliberately a bare zero-test, NOT HasApp(): PlaceholderAppID exists
	// precisely so a hive awaiting its real App can satisfy this check and boot
	// into dashboard-only mode. Everywhere else, use HasApp().
	// A hive described by `forge:` alone satisfies this too: ResolvedAppID()
	// derives a real App ID from a known forge, so the identity is present even
	// though app_id is not written down. Without this, the end state of this
	// design — one field naming the forge, the rest derived — fails validation
	// and the spoke will not boot.
	// An EXPLICIT `forge:` satisfies this too: ResolvedAppID() derives a real
	// App ID from a known forge, so the identity is present even though app_id
	// is not written down. Deliberately keyed on Forge_ (the raw field) and not
	// Forge() — Forge() INFERS public for a blank config, which would make an
	// empty github block validate and silently boot a hive with no credentials
	// at all. Only a forge the operator actually wrote counts.
	if c.GitHub.Token == "" && c.GitHub.AppID == 0 &&
		!(strings.TrimSpace(c.GitHub.Forge_) != "" && c.GitHub.ResolvedAppID() != 0) {
		return fmt.Errorf("github.token, github.app_id or github.forge is required")
	}
	if err := c.Governor.LiteLLM.Validate(); err != nil {
		return err
	}
	// A configured gateway name is a valid agent backend: naming a gateway as
	// the backend routes that agent through it. Names are matched
	// case-insensitively to mirror ResolveGateway.
	gatewayNames := map[string]bool{}
	for _, gw := range c.Governor.ResolvedGateways() {
		if gw.Name != "" {
			gatewayNames[strings.ToLower(gw.Name)] = true
		}
	}
	for name, agent := range c.Agents {
		validBackends := map[string]bool{"claude": true, "copilot": true, "goose": true, "codex": true, "pi": true, "bob": true, "aider": true}
		// Inference backends (vllm, llm-d, litellm) are valid persisted
		// agent backends too — they launch the claude CLI routed through
		// the inference translator.
		for _, b := range InferenceBackends {
			validBackends[b] = true
		}
		if agent.Backend != "" && !validBackends[agent.Backend] && !gatewayNames[strings.ToLower(agent.Backend)] {
			return fmt.Errorf("agent %s: invalid backend %q (must be claude, copilot, goose, codex, pi, bob, aider, an inference backend: %s, or a configured gateway name)", name, agent.Backend, strings.Join(InferenceBackends, ", "))
		}
		validCavemanModes := map[string]bool{"": true, "lite": true, "full": true, "ultra": true, "wenyan": true}
		if !validCavemanModes[agent.CavemanMode] {
			return fmt.Errorf("agent %s: invalid caveman_mode %q (must be lite, full, ultra, or wenyan)", name, agent.CavemanMode)
		}
		if err := validateChannels(name, agent.Channels); err != nil {
			return err
		}
		if err := validateTools(name, agent.Tools); err != nil {
			return err
		}
		if err := validateConnections(name, agent.Connections); err != nil {
			return err
		}
	}
	return nil
}

func validateChannels(agentName string, channels []ChannelConfig) error {
	validTypes := map[string]bool{"kick": true, "webhook": true, "discord": true, "schedule": true, "bead": true}
	for i, ch := range channels {
		if !validTypes[ch.Type] {
			return fmt.Errorf("agent %s: channel[%d]: invalid type %q", agentName, i, ch.Type)
		}
		switch ch.Type {
		case "webhook":
			if len(ch.Events) == 0 {
				return fmt.Errorf("agent %s: channel[%d]: webhook requires at least one event", agentName, i)
			}
		case "discord":
			if len(ch.Patterns) == 0 {
				return fmt.Errorf("agent %s: channel[%d]: discord requires at least one pattern", agentName, i)
			}
		case "schedule":
			if ch.Schedule == "" {
				return fmt.Errorf("agent %s: channel[%d]: schedule requires a cron expression", agentName, i)
			}
		case "bead":
			if len(ch.Match) == 0 {
				return fmt.Errorf("agent %s: channel[%d]: bead requires at least one match criterion", agentName, i)
			}
		}
	}
	return nil
}

func validateTools(agentName string, tools *ToolsConfig) error {
	if tools == nil {
		return nil
	}
	validPresets := map[string]bool{"": true, "advisory": true, "issues-only": true, "issues-prs": true, "full": true}
	if !validPresets[tools.Preset] {
		return fmt.Errorf("agent %s: tools.preset %q is invalid (must be advisory, issues-only, issues-prs, or full)", agentName, tools.Preset)
	}
	validActions := map[string]bool{"allow": true, "deny": true}
	for i, rule := range tools.Rules {
		if rule.Pattern == "" {
			return fmt.Errorf("agent %s: tools.rules[%d]: pattern is required", agentName, i)
		}
		if !validActions[rule.Action] {
			return fmt.Errorf("agent %s: tools.rules[%d]: action must be allow or deny, got %q", agentName, i, rule.Action)
		}
	}
	return nil
}

func validateConnections(agentName string, conns []ConnectionConfig) error {
	validTypes := map[string]bool{"mcp": true, "api": true, "knowledge": true}
	seen := map[string]bool{}
	for i, conn := range conns {
		if conn.Name == "" {
			return fmt.Errorf("agent %s: connections[%d]: name is required", agentName, i)
		}
		if seen[conn.Name] {
			return fmt.Errorf("agent %s: connections[%d]: duplicate name %q", agentName, i, conn.Name)
		}
		seen[conn.Name] = true
		if !validTypes[conn.Type] {
			return fmt.Errorf("agent %s: connections[%d]: invalid type %q (must be mcp, api, or knowledge)", agentName, i, conn.Type)
		}
		if (conn.Type == "mcp" || conn.Type == "api") && conn.URI == "" {
			return fmt.Errorf("agent %s: connections[%d]: %s requires a uri", agentName, i, conn.Type)
		}
		if conn.Auth != nil {
			validAuthTypes := map[string]bool{"env": true, "file": true}
			if !validAuthTypes[conn.Auth.Type] {
				return fmt.Errorf("agent %s: connections[%d]: auth.type must be env or file, got %q", agentName, i, conn.Auth.Type)
			}
			if conn.Auth.Type == "env" && conn.Auth.EnvVar == "" {
				return fmt.Errorf("agent %s: connections[%d]: auth.env_var is required when auth.type is env", agentName, i)
			}
			if conn.Auth.Type == "file" && conn.Auth.File == "" {
				return fmt.Errorf("agent %s: connections[%d]: auth.file is required when auth.type is file", agentName, i)
			}
		}
	}
	return nil
}

func (c *Config) EnabledAgents() map[string]AgentConfig {
	result := make(map[string]AgentConfig)
	for name, agent := range c.Agents {
		if agent.Enabled {
			result[name] = agent
		}
	}
	return result
}

// ResolveAgent finds an agent by name or ID and returns its YAML key (name).
// Returns the key and true if found, empty string and false otherwise.
func (c *Config) ResolveAgent(nameOrID string) (string, bool) {
	if _, ok := c.Agents[nameOrID]; ok {
		return nameOrID, true
	}
	for name, agent := range c.Agents {
		if agent.ID == nameOrID {
			return name, true
		}
	}
	return "", false
}

// AgentByID returns the agent config with the given ID.
func (c *Config) AgentByID(id string) (AgentConfig, bool) {
	for _, agent := range c.Agents {
		if agent.ID == id {
			return agent, true
		}
	}
	return AgentConfig{}, false
}

// validateSaveGuard checks that essential fields are present before allowing
// a config write. This prevents docker compose down -v (or similar) from
// causing Save() to overwrite hive.yaml with an empty/minimal config that
// would crash-loop on next startup.
func (c *Config) validateSaveGuard() error {
	if c.Project.Org == "" {
		log.Printf("WARNING: config.Save() blocked — project.org is empty, would corrupt hive.yaml")
		return fmt.Errorf("project.org is empty")
	}
	// Zero agents is a legitimate state when the operator deliberately deleted
	// them all: #2361's tombstones (RemovedAgents) are the durable record of
	// that intent. Blocking the save here would make the last deletion
	// unpersistable — the in-memory roster empties, the write is refused, and
	// the next reload restores the agents from the seed, silently undoing the
	// operator's action. That is precisely the "they always come back" bug
	// #2361 fixed, reintroduced through the save path.
	//
	// An empty roster with NO tombstones is still refused: that is the
	// truncated/uninitialised case this guard exists to catch. The two states
	// are distinguishable, so distinguish them rather than rejecting both.
	if len(c.Agents) == 0 && len(c.RemovedAgents) == 0 {
		log.Printf("WARNING: config.Save() blocked — no agents configured and no tombstones, would corrupt hive.yaml")
		return fmt.Errorf("no agents configured")
	}
	return nil
}

// Save marshals the current config back to its source YAML file using an
// inode-preserving write (open → truncate → write → sync). This is critical
// for Docker bind-mounted files: an atomic rename (temp + rename) replaces
// the inode, which silently breaks the bind mount — the host file is never
// updated, so changes are lost on container restart.
//
// As a safety measure, Save refuses to write if essential fields are missing
// (project.org, at least one agent). This prevents an empty or minimal config
// from overwriting the bind-mounted hive.yaml — a scenario that causes
// crash-loops on the next startup ("project.org is required").
func (c *Config) Save() error {
	saveMu.Lock()
	defer saveMu.Unlock()
	return c.saveLocked()
}

// SetAgentPausedAndSave atomically updates one agent's Paused field and
// persists the config, all under saveMu. This is the pause-callback path
// (AgentMgr.Pause/Resume). Doing the c.Agents read-modify-write and the Save
// under the SAME lock as every other saver eliminates both the map-mutation
// race (two goroutines writing c.Agents) and the file-level lost-write race.
// Returns whether a change was made (false when already at the target state).
func (c *Config) SetAgentPausedAndSave(name string, paused bool) (bool, error) {
	saveMu.Lock()
	defer saveMu.Unlock()
	ac, ok := c.Agents[name]
	if !ok || ac.Paused == paused {
		return false, nil
	}
	ac.Paused = paused
	c.Agents[name] = ac
	return true, c.saveLocked()
}

// ReconcilePausedAndSave sets each named agent's Paused field to the given
// live value and persists, all under saveMu. This is the async PersistFunc
// path (persistState): it carries the authoritative live paused set from the
// agent manager, so its write is a correcting one rather than a stale snapshot
// that could clobber a concurrent pause. Serializing it with SetAgentPausedAndSave
// under saveMu is what closes the race that dropped pauses when many agents
// were paused in quick succession.
func (c *Config) ReconcilePausedAndSave(livePaused map[string]bool) error {
	saveMu.Lock()
	defer saveMu.Unlock()
	for name, paused := range livePaused {
		if ac, ok := c.Agents[name]; ok && ac.Paused != paused {
			ac.Paused = paused
			c.Agents[name] = ac
		}
	}
	return c.saveLocked()
}

// saveLocked performs the actual marshal-and-write. Callers MUST hold saveMu.
func (c *Config) saveLocked() error {
	if c.SourcePath == "" {
		return fmt.Errorf("config has no source path")
	}
	if err := c.validateSaveGuard(); err != nil {
		return fmt.Errorf("refusing to save invalid config: %w", err)
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	// Open the existing file (preserving its inode) rather than creating a
	// temp file and renaming. Rename breaks Docker bind mounts because it
	// replaces the inode — the host file is never updated, so acmm_level
	// and other runtime changes are lost on container restart.
	f, err := os.OpenFile(c.SourcePath, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		// File may not exist yet — fall back to create. Continue below so
		// the PVC backup and dashboard overlay are still written.
		if writeErr := os.WriteFile(c.SourcePath, data, 0o644); writeErr != nil {
			return fmt.Errorf("writing config (create fallback): %w", writeErr)
		}
	} else {
		if _, err := f.Write(data); err != nil {
			f.Close()
			return fmt.Errorf("writing config: %w", err)
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return fmt.Errorf("syncing config: %w", err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("closing config: %w", err)
		}
	}

	// Persist the runtime config to the PVC. In K8s this is a recovery copy
	// (the ConfigMap seed plus the overlay is authoritative); in Docker/LXC
	// it IS the boot-time source of truth, since there is no ConfigMap and
	// no overlay there. The entrypoint decides which applies.
	//
	// Always written under the new name. The legacy file is never written,
	// renamed or removed here — see RuntimeConfigFileLegacy.
	runtimePath := RuntimeConfigFile
	if err := os.WriteFile(runtimePath, data, 0o644); err != nil {
		// Common cause: init container created the file as root, runtime user
		// can't overwrite. Remove and retry so runtime state is not silently lost.
		os.Remove(runtimePath)
		if retryErr := os.WriteFile(runtimePath, data, 0o644); retryErr != nil {
			log.Printf("[config] warning: failed to write PVC runtime config to %s (even after remove): %v", runtimePath, retryErr)
		} else {
			log.Printf("[config] PVC runtime config written to %s (recovered from permission error)", runtimePath)
		}
	} else {
		log.Printf("[config] PVC runtime config written to %s", runtimePath)
	}

	c.saveDashboardOverlay()
	return nil
}

// RuntimeConfigFile is where Save() persists the full runtime config on the
// PVC. Its role differs by environment, which is exactly why the old
// hive.yaml.bak name was misleading enough to cost debugging time:
//
//   - Kubernetes: a post-merge SNAPSHOT. The entrypoint writes it after
//     merging the dashboard overlay over the ConfigMap seed, and reads it
//     back only in the disaster fallback (ConfigMap missing or empty).
//   - Docker/LXC: a live boot INPUT and the source of truth. There is no
//     ConfigMap and no overlay, so the entrypoint restores this file over
//     the config path on every boot. It is the only reason a dashboard save
//     survives a container recreation — see saveDashboardOverlay, which
//     early-returns outside Kubernetes for that reason.
//
// ".runtime" is accurate for both; ".bak" implied "the restorable backup",
// which is true only of the Kubernetes half.
const RuntimeConfigFile = "/data/hive.yaml.runtime"

// RuntimeConfigFileLegacy is the pre-rename name of RuntimeConfigFile.
//
// It is READ as a fallback and never written, renamed or removed: ~51 live
// hives carry only this file, and on Docker/LXC it is the single copy of
// their live configuration. Mutating it at boot could lose owner
// customisations with no warning, so the migration is copy-forward only —
// readers prefer RuntimeConfigFile and fall back to this one.
//
// Removable one release after every live hive has written the new name.
const RuntimeConfigFileLegacy = "/data/hive.yaml.bak"

// DashboardOverlayFile is where Save() persists a secret-free copy of the
// dashboard-edited config on the PVC in Kubernetes mode. The copy-config
// init container re-seeds /etc/hive/hive.yaml FROM THE CONFIGMAP on every
// pod boot, so without this overlay every dashboard save (LiteLLM
// endpoint, notifications, agent tweaks, ...) silently vanished on the
// next restart or upgrade. The entrypoint merges this file over the
// ConfigMap seed at boot; the ConfigMap stays authoritative for the
// hub/admin-managed keys (acmm_level, hub.is_public).
//
// A package var (not const) only so tests can point it at a temp dir; it
// never changes at runtime in production.
var DashboardOverlayFile = "/data/hive.yaml.dashboard"

// IsKubernetesPod reports whether the process is running inside a
// Kubernetes pod (mirrors the entrypoint's IS_KUBERNETES detection).
func IsKubernetesPod() bool {
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return true
	}
	_, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token")
	return err == nil
}

// saveDashboardOverlay writes the secret-free PVC overlay in Kubernetes
// mode. Failures are logged, never fatal: the primary save already
// succeeded, the overlay only affects persistence across pod restarts.
//
// The write MUST be atomic (temp file + rename), unlike saveLocked()'s
// inode-preserving write to the bind-mounted primary config. DashboardOverlayFile
// lives on the PVC (not a bind mount), so rename is safe here, and it is the
// only way to avoid a truncated/partial overlay if the pod is killed mid-write
// (a redeploy sends SIGTERM/SIGKILL at an arbitrary instant). A truncate-in-place
// write (os.WriteFile) can leave the file cut off partway through — GitHubConfig
// marshals AFTER Agents/Project in the Config struct field order (see the
// struct tags above), so a truncated overlay can silently keep valid
// project/agents blocks while losing app_id/installation_id/key_file entirely.
// The entrypoint's merge script only sanity-checks project.org and agents
// before trusting the overlay wholesale, so that truncated-but-plausible file
// would pass the guard and revert a dashboard-installed GitHub App to the
// placeholder ConfigMap seed on the next restart — exactly the durability bug
// this atomic write prevents.
func (c *Config) saveDashboardOverlay() {
	if !IsKubernetesPod() {
		// Docker/LXC mode: RuntimeConfigFile is already the boot-time
		// source of truth there, so dashboard saves persist without an
		// overlay.
		return
	}
	data, err := c.dashboardOverlayBytes()
	if err != nil {
		log.Printf("[config] warning: failed to marshal dashboard overlay: %v", err)
		return
	}
	tmpPath := DashboardOverlayFile + ".tmp"
	const overlayFileMode = 0o644
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, overlayFileMode)
	if err != nil {
		log.Printf("[config] warning: failed to open dashboard overlay temp file %s (dashboard saves will not survive pod restarts): %v", tmpPath, err)
		return
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		log.Printf("[config] warning: failed to write dashboard overlay temp file %s (dashboard saves will not survive pod restarts): %v", tmpPath, err)
		return
	}
	if err := f.Sync(); err != nil {
		f.Close()
		log.Printf("[config] warning: failed to fsync dashboard overlay temp file %s (dashboard saves will not survive pod restarts): %v", tmpPath, err)
		return
	}
	if err := f.Close(); err != nil {
		log.Printf("[config] warning: failed to close dashboard overlay temp file %s (dashboard saves will not survive pod restarts): %v", tmpPath, err)
		return
	}
	if err := os.Rename(tmpPath, DashboardOverlayFile); err != nil {
		log.Printf("[config] warning: failed to rename dashboard overlay into place %s (dashboard saves will not survive pod restarts): %v", DashboardOverlayFile, err)
		return
	}
	log.Printf("[config] dashboard overlay written to %s (merged over the ConfigMap seed at next boot)", DashboardOverlayFile)
}

// dashboardOverlayBytes marshals the config with env-derived secret VALUES
// collapsed back to their env-var forms, so the PVC overlay stays
// secret-free. Load() re-expands ${VAR} references and applyBootstrapEnv
// re-fills the dashboard auth token from the pod env, so nothing is lost.
func (c *Config) dashboardOverlayBytes() ([]byte, error) {
	// Shallow copy: top-level fields are struct values, so mutating the
	// copy's GitHub/Dashboard sections leaves the live config untouched
	// (the shared Agents map is not modified).
	cp := *c
	if tok := os.Getenv("HIVE_GITHUB_TOKEN"); tok != "" && cp.GitHub.Token == tok {
		cp.GitHub.Token = "${HIVE_GITHUB_TOKEN}"
	}
	for _, env := range []string{"DASHBOARD_AUTH_TOKEN", "HIVE_DASHBOARD_TOKEN"} {
		if v := os.Getenv(env); v != "" && cp.Dashboard.AuthToken == v {
			cp.Dashboard.AuthToken = ""
			break
		}
	}
	return yaml.Marshal(&cp)
}

// WildcardMatch checks if text matches a pattern supporting:
// - * wildcards (match any substring)
// - /regex/ syntax for full regex
// - plain substring match (case-insensitive)
func WildcardMatch(text, pattern string) bool {
	text = strings.ToLower(text)
	pattern = strings.TrimSpace(pattern)

	if strings.HasPrefix(pattern, "/") && strings.HasSuffix(pattern, "/") {
		re, err := regexp.Compile("(?i)" + pattern[1:len(pattern)-1])
		if err != nil {
			return false
		}
		return re.MatchString(text)
	}

	pattern = strings.ToLower(pattern)
	if strings.Contains(pattern, "*") {
		parts := strings.Split(pattern, "*")
		idx := 0
		for _, part := range parts {
			if part == "" {
				continue
			}
			found := strings.Index(text[idx:], part)
			if found < 0 {
				return false
			}
			idx += found + len(part)
		}
		if !strings.HasPrefix(pattern, "*") && !strings.HasPrefix(text, parts[0]) {
			return false
		}
		if !strings.HasSuffix(pattern, "*") && !strings.HasSuffix(text, parts[len(parts)-1]) {
			return false
		}
		return true
	}

	return strings.Contains(text, pattern)
}

// MatchesAny returns true if text matches any pattern in the list.
func MatchesAny(text string, patterns []string) bool {
	for _, p := range patterns {
		if WildcardMatch(text, p) {
			return true
		}
	}
	return false
}

// Contribute filter modes for title/author/label gating.
const (
	// FilterModeDeny (the default) skips items that MATCH the list; everything
	// else passes.
	FilterModeDeny = "deny"
	// FilterModeAllow passes ONLY items that MATCH the list; everything else is
	// skipped. An EMPTY allow list is treated as "filter off" (see
	// FilterPasses) so a half-configured allow filter never silently blocks
	// every item.
	FilterModeAllow = "allow"
)

// NormalizeFilterMode returns a valid mode, defaulting to deny for empty/unknown
// values (backward compatible: existing config has no mode field, and its lists
// were always deny lists).
func NormalizeFilterMode(mode string) string {
	if mode == FilterModeAllow {
		return FilterModeAllow
	}
	return FilterModeDeny
}

// FilterPasses reports whether a single value (an issue/PR title, author, or one
// of its labels) passes a mode+list filter.
//
//   - deny  mode: pass unless value matches the list.
//   - allow mode: pass only if value matches the list; BUT an empty allow list
//     means "not configured" → pass (never block everything on an empty list).
//
// Patterns use the same wildcard/regex syntax as MatchesAny.
func FilterPasses(value string, list []string, mode string) bool {
	switch NormalizeFilterMode(mode) {
	case FilterModeAllow:
		if len(list) == 0 {
			return true // allow filter not configured → don't gate
		}
		return MatchesAny(value, list)
	default: // deny
		return !MatchesAny(value, list)
	}
}

// LabelsFilterPasses applies a label filter across ALL of an item's labels.
//
//   - deny  mode: pass unless ANY label matches the deny list.
//   - allow mode: pass only if AT LEAST ONE label matches the allow list; an
//     empty allow list means "not configured" → pass.
//
// This is the label-specific counterpart to FilterPasses (labels are a set, not
// a single scalar, so the allow/deny quantifiers differ).
func LabelsFilterPasses(labels []string, list []string, mode string) bool {
	switch NormalizeFilterMode(mode) {
	case FilterModeAllow:
		if len(list) == 0 {
			return true // allow filter not configured → don't gate
		}
		for _, l := range labels {
			if MatchesAny(l, list) {
				return true
			}
		}
		return false
	default: // deny
		for _, l := range labels {
			if MatchesAny(l, list) {
				return false
			}
		}
		return true
	}
}
