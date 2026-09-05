// Agent definition: AgentConfig and its methods, the per-agent and global
// sandbox settings, channels, tools and connections, field-ownership markers,
// and replica naming/expansion plus agent lookup helpers on Config.
package config

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// AgentSandboxConfig controls the podman-rootless sandbox launcher. The top-
// level block is a global gate; per-agent config can opt specific agents in.
type AgentSandboxConfig struct {
	Enabled      bool     `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Image        string   `yaml:"image,omitempty" json:"image,omitempty"`
	EnvAllowlist []string `yaml:"env_allowlist,omitempty" json:"env_allowlist,omitempty"`
	NetworkMode  string   `yaml:"network_mode,omitempty" json:"network_mode,omitempty"`
	TimeoutS     int      `yaml:"timeout_s,omitempty" json:"timeout_s,omitempty"`
	WorkspaceDir string   `yaml:"workspace_dir,omitempty" json:"workspace_dir,omitempty"`
}

// AgentSandboxOverride is the per-agent sandbox opt-in block.
type AgentSandboxOverride struct {
	Enabled      *bool    `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Image        string   `yaml:"image,omitempty" json:"image,omitempty"`
	EnvAllowlist []string `yaml:"env_allowlist,omitempty" json:"env_allowlist,omitempty"`
	NetworkMode  string   `yaml:"network_mode,omitempty" json:"network_mode,omitempty"`
	TimeoutS     int      `yaml:"timeout_s,omitempty" json:"timeout_s,omitempty"`
}

// ChannelConfig declares a trigger channel for an agent.
//
// Only ChannelTypeKick (governor timer kicks) has a runtime. The former
// webhook/discord/schedule/bead trigger types were declarative-only: the
// pkg/channels runtime meant to serve them was never wired into the binary
// and was removed (#5591). Declaring one of those types used to validate
// cleanly while suppressing governor kicks, leaving the agent permanently
// dormant with no diagnostics; ValidateChannels now rejects them instead.
type ChannelConfig struct {
	Type    string `yaml:"type" json:"type"`
	Enabled *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// ChannelTypeKick is the only channel type with a live trigger runtime:
// ordinary governor timer kicks.
const ChannelTypeKick = "kick"

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
	ID           string `yaml:"id" json:"id,omitempty"`
	Backend      string `yaml:"backend" json:"backend,omitempty"`
	Model        string `yaml:"model" json:"model,omitempty"`
	BeadsDir     string `yaml:"beads_dir" json:"beads_dir,omitempty"`
	Enabled      bool   `yaml:"enabled" json:"enabled,omitempty"`
	Replicas     int    `yaml:"replicas,omitempty" json:"replicas,omitempty"`
	ReplicaOf    string `yaml:"-" json:"replicaOf,omitempty"`
	ReplicaIndex int    `yaml:"-" json:"replicaIndex,omitempty"`
	ReplicaCount int    `yaml:"-" json:"replicaCount,omitempty"`
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
	ModelOwner   string `yaml:"model_owner" json:"model_owner,omitempty"`
	BackendOwner string `yaml:"backend_owner" json:"backend_owner,omitempty"`
	// PauseOwner records WHO owns this agent's pause/run state, with the same
	// FieldOwner* vocabulary as ModelOwner/BackendOwner. It is stamped
	// FieldOwnerOperator when the operator creates the agent by hand via the
	// dashboard API (such an agent is never a member of any pack's roster) and
	// when the operator explicitly resumes the agent.
	//
	// Without this, the ACMM pack visibility sweep — which runs on EVERY
	// restart ("ACMM pack applied on startup") — paused every non-pack agent
	// with reason "agent not in pack level N", including reviewer-role agents
	// the operator had explicitly created and resumed from the dashboard. The
	// operator's run-state silently reverted on the next pod roll, every pod
	// roll (#5706 — same clobber family as #5632; cadences grew the equivalent
	// marker in #5668). Empty means "no operator claim": pack-created agents
	// keep the sweep's pause/resume reconciliation unchanged.
	PauseOwner      string `yaml:"pause_owner" json:"pause_owner,omitempty"`
	StaleTimeout    int    `yaml:"stale_timeout" json:"stale_timeout,omitempty"`
	RestartStrategy string `yaml:"restart_strategy" json:"restart_strategy,omitempty"`
	LaunchCmd       string `yaml:"launch_cmd" json:"launch_cmd,omitempty"`
	AgentSpec       string `yaml:"agent_spec" json:"agent_spec,omitempty"`
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
	// Converse opts this agent into the orthogonal `converse` capability
	// (#4492): posting comments on issues and PRs, and leaving PR reviews,
	// independently of Mode. It does not grant issue creation, editing,
	// relabelling, pushing or merging — those stay on the Mode ladder.
	//
	// A POINTER so "unset" stays distinguishable from "explicitly false" across
	// pack re-apply and the dashboard overlay, the same reason ModelOwner
	// exists. Unset means off: `converse` is opt-in at every ACMM level, so an
	// existing hive that says nothing behaves exactly as it did.
	//
	// The shape this exists for is `mode: ADVISORY` + `converse: true` — an
	// agent that can reply on a thread it was mentioned in but cannot file,
	// edit or relabel anything.
	Converse    *bool  `yaml:"converse,omitempty" json:"converse,omitempty"`
	OnDemand    bool   `yaml:"on_demand" json:"on_demand,omitempty"`
	CavemanMode string `yaml:"caveman_mode" json:"caveman_mode,omitempty"`
	// ExplainMode opts this agent into emitting EXPLAIN-prefixed reasoning
	// lines alongside its tool calls, so an operator debugging "why did it do
	// that" has something to read (#3887). Off by default because the
	// explanation costs tokens on every kick.
	//
	// TRI-STATE, and the distinction matters: "" means "inherit the hive-wide
	// default" (HIVE_EXPLAIN_MODE), while "off" is an explicit per-agent
	// opt-OUT that survives an operator turning explanation on fleet-wide.
	// Valid values: "" | off | brief | full — see ExplainMode* constants.
	ExplainMode string `yaml:"explain_mode,omitempty" json:"explain_mode,omitempty"`
	// Sandbox opts this agent into phase-1 sandbox execution when the global
	// agent_sandbox.enabled gate is also true.
	Sandbox *AgentSandboxOverride `yaml:"sandbox,omitempty" json:"sandbox,omitempty"`
	// ReentrantTurn opts this agent into the RFC #4002 envelope runner when the
	// global turn.reentrant.enabled rollout gate is also true.
	ReentrantTurn *bool `yaml:"reentrant_turn,omitempty" json:"reentrant_turn,omitempty"`

	// Channels declares how this agent gets triggered. Only "kick" (governor
	// timer kicks) is a valid type; when nil/empty, the agent uses governor
	// timer kicks by default (implicit kick channel). See ChannelConfig for
	// why the former webhook/discord/schedule/bead types are rejected.
	Channels []ChannelConfig `yaml:"channels,omitempty" json:"channels,omitempty"`

	// Tools declares what tools this agent can use. When nil, the existing Mode field governs.
	Tools *ToolsConfig `yaml:"tools,omitempty" json:"tools,omitempty"`

	// Connections declares external service integrations (MCP servers, APIs, knowledge sources).
	Connections []ConnectionConfig `yaml:"connections,omitempty" json:"connections,omitempty"`

	// sourceFile is the per-agent overlay file this entry was read from.
	sourceFile string

	// Skills names reusable "how to do X" skills to resolve out of the hive's
	// skill registry (pkg/skillreg, loaded from the host-local skills directory)
	// and inject into this agent's kick context. Names are resolved at kick
	// time, so editing a skill file takes effect on the next kick without a
	// restart. An unknown name is skipped, not fatal: a typo degrades the kick
	// rather than blocking the agent.
	//
	// This is deliberately host-local rather than per-repo. Hive agents work
	// over the GitHub API and have no guaranteed per-repo checkout, so a
	// repo-declared skills directory would resolve to nothing on most kicks;
	// the registry directory is the same kind of operator-managed volume as
	// /data/policies and is present on every hive host.
	Skills []string `yaml:"skills,omitempty" json:"skills,omitempty"`

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

// SourceFile returns the per-agent overlay file this entry was loaded from, or
// "" when it came from the main config.
func (a *AgentConfig) SourceFile() string {
	if a == nil {
		return ""
	}
	return a.sourceFile
}

// Name returns the human-readable YAML key for this agent.
func (a *AgentConfig) Name() string {
	return a.name
}

// IsReplica reports whether this agent was materialized from another agent's
// replicas setting rather than declared directly in YAML.
func (a AgentConfig) IsReplica() bool { return a.ReplicaOf != "" }

// BaseName returns the declared agent name whose config/prompt this agent uses.
func (a AgentConfig) BaseName() string {
	if a.ReplicaOf != "" {
		return a.ReplicaOf
	}
	if a.name != "" {
		return a.name
	}
	return a.ID
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
	return a.HasChannel(ChannelTypeKick)
}

// ShouldIncludeRepos returns whether the repos section should be appended to kicks.
// Defaults to true for all agents except those with IncludeRepos explicitly set to false.
func (a *AgentConfig) ShouldIncludeRepos() bool {
	if a.IncludeRepos != nil {
		return *a.IncludeRepos
	}
	return true
}

// SandboxEnabled reports whether an agent's per-agent sandbox block opts it in
// under the global phase-1 gate.
func (a *AgentConfig) SandboxEnabled(global AgentSandboxConfig) bool {
	if !global.Enabled || a == nil || a.Sandbox == nil || a.Sandbox.Enabled == nil {
		return false
	}
	return *a.Sandbox.Enabled
}

// AgentSandboxGateWarnings reports the sandbox misconfigurations that the
// two-gate opt-in otherwise makes SILENT. Empty means nothing to say.
//
// SandboxEnabled (above) requires BOTH agent_sandbox.enabled AND a per-agent
// sandbox.enabled: true. That second gate is deliberate — a sandboxed agent
// runs a completely different execution model (no tmux CLI at all; every kick
// is a podman run against the primary repo), and startSandboxKickLocked in
// pkg/agent has NO fallback to tmux: an agent flipped on without a resolvable
// image fails every kick outright. So the gate is not something to quietly
// collapse.
//
// What it must not do is stay invisible. The dashboard's Security tab writes
// only the GLOBAL flag (handleGovernorSecurity), and it is the only sandbox
// control the UI offers — so an owner can turn "agent sandbox" on, be told the
// setting was updated, and have every agent keep running unconfined. The
// response's own sandboxedAgents field stays 0 and nothing explains why.
//
// #4918 is what that silence costs. An agent doing correct work on an assigned
// third-party repo ran that repo's test suite, a hook escaped its stubs, and
// `rpm-ostree kargs` reached the operator's real deployment. An operator who
// had flipped the sandbox toggle on would reasonably believe they were covered.
//
// These are WARNINGS, not validation errors: every state described here is a
// legal config, and refusing to boot on it would turn a diagnostic into an
// outage.
func AgentSandboxGateWarnings(cfg *Config) []string {
	if cfg == nil || !cfg.AgentSandbox.Enabled {
		// The sandbox is off globally. That is the documented default and the
		// posture the docs describe; it is not a misconfiguration.
		return nil
	}

	var optedIn, noImage []string
	for name, a := range cfg.Agents {
		if !a.SandboxEnabled(cfg.AgentSandbox) {
			continue
		}
		optedIn = append(optedIn, name)
		if strings.TrimSpace(a.SandboxImage(cfg.AgentSandbox)) == "" {
			noImage = append(noImage, name)
		}
	}
	sort.Strings(optedIn)
	sort.Strings(noImage)

	var out []string
	if len(optedIn) == 0 {
		out = append(out, fmt.Sprintf(
			"agent_sandbox.enabled is true but NO agent is opted in, so the sandbox is inert and all %d agent(s) still run unconfined on the tmux path — "+
				"the per-agent gate is separate: set `sandbox: {enabled: true}` on each agent that should be sandboxed (#4918)",
			len(cfg.Agents)))
		return out
	}
	if len(noImage) > 0 {
		out = append(out, fmt.Sprintf(
			"agent(s) %s are sandbox-opted-in but resolve no sandbox image; sandboxed kicks have no tmux fallback and will fail outright — "+
				"set agent_sandbox.image (or the per-agent sandbox.image)",
			strings.Join(noImage, ", ")))
	}
	if len(optedIn) < len(cfg.Agents) {
		out = append(out, fmt.Sprintf(
			"agent_sandbox.enabled is true but only %d of %d agent(s) are opted in (%s); the rest still run unconfined on the tmux path (#4918)",
			len(optedIn), len(cfg.Agents), strings.Join(optedIn, ", ")))
	}
	return out
}

// SandboxImage returns the per-agent image override, then the global default.
func (a *AgentConfig) SandboxImage(global AgentSandboxConfig) string {
	if a != nil && a.Sandbox != nil && a.Sandbox.Image != "" {
		return a.Sandbox.Image
	}
	return global.Image
}

// SandboxEnvAllowlist returns the per-agent env allowlist when set; otherwise
// the global allowlist. Credentials are still filtered by pkg/sandbox.
func (a *AgentConfig) SandboxEnvAllowlist(global AgentSandboxConfig) []string {
	if a != nil && a.Sandbox != nil && len(a.Sandbox.EnvAllowlist) > 0 {
		return append([]string(nil), a.Sandbox.EnvAllowlist...)
	}
	return append([]string(nil), global.EnvAllowlist...)
}

// SandboxNetworkMode returns the per-agent network override, then the global
// sandbox network mode. Empty lets the executor choose its safe default.
func (a *AgentConfig) SandboxNetworkMode(global AgentSandboxConfig) string {
	if a != nil && a.Sandbox != nil && a.Sandbox.NetworkMode != "" {
		return a.Sandbox.NetworkMode
	}
	return global.NetworkMode
}

// SandboxTimeoutS returns the per-agent timeout override, then the global
// timeout. Non-positive values let the executor use its default.
func (a *AgentConfig) SandboxTimeoutS(global AgentSandboxConfig) int {
	if a != nil && a.Sandbox != nil && a.Sandbox.TimeoutS > 0 {
		return a.Sandbox.TimeoutS
	}
	return global.TimeoutS
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

// PauseIsOperatorOwned reports whether an operator explicitly owns this
// agent's pause/run state — stamped at dashboard-API create and on an explicit
// operator resume — which makes the agent immune to the ACMM pack visibility
// sweep's "agent not in pack level N" pause on apply/restart (#5706), the same
// contract ModelIsOperatorOwned provides for models.
func (a AgentConfig) PauseIsOperatorOwned() bool {
	return a.PauseOwner == FieldOwnerOperator
}

// EnabledExplicitlySet returns true when the user's YAML explicitly set the
// "enabled" field (allowing us to distinguish "not specified" from "enabled: false").
func (a *AgentConfig) EnabledExplicitlySet() bool {
	return a.enabledSet
}

// BaseAgentName returns the declared/base agent name for name. For ordinary
// agents it returns name; for materialized replicas (scanner-2) it returns the
// source agent (scanner).
func (c *Config) BaseAgentName(name string) string {
	if c == nil || c.Agents == nil {
		return name
	}
	if ac, ok := c.Agents[name]; ok && ac.ReplicaOf != "" {
		return ac.ReplicaOf
	}
	return name
}

func replicaAgentName(base string, index int) string {
	if index <= 1 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, index)
}

func normalizeReplicaCount(n int) int {
	if n == 0 {
		return 1
	}
	return n
}

// ExpandAgentReplicas materializes each declared agent's replicas setting into
// the existing name-keyed machinery: base, base-2, ..., base-N. Derived agents
// are marked with ReplicaOf and are stripped again when the config is saved.
func (c *Config) ExpandAgentReplicas() error {
	if c == nil || c.Agents == nil {
		return nil
	}
	baseAgents := make(map[string]AgentConfig, len(c.Agents))
	for name, agent := range c.Agents {
		if agent.ReplicaOf != "" {
			continue
		}
		baseAgents[name] = agent
	}
	for name, agent := range c.Agents {
		if agent.ReplicaOf != "" {
			delete(c.Agents, name)
		}
	}
	for name, agent := range baseAgents {
		replicas := normalizeReplicaCount(agent.Replicas)
		if replicas < 1 || replicas > MaxAgentReplicas {
			return fmt.Errorf("agent %s: replicas must be between 1 and %d", name, MaxAgentReplicas)
		}
		for i := 2; i <= replicas; i++ {
			derived := replicaAgentName(name, i)
			if existing, ok := baseAgents[derived]; ok && existing.ReplicaOf == "" {
				return fmt.Errorf("agent %s: replicas:%d would create %s, but an agent with that name already exists", name, replicas, derived)
			}
		}
	}
	for name, agent := range baseAgents {
		replicas := normalizeReplicaCount(agent.Replicas)
		agent.Replicas = replicas
		agent.ReplicaOf = ""
		agent.ReplicaIndex = 1
		agent.ReplicaCount = replicas
		agent.name = name
		c.Agents[name] = agent
		for i := 2; i <= replicas; i++ {
			derivedName := replicaAgentName(name, i)
			derived := agent
			derived.name = derivedName
			derived.ID = derivedName
			derived.BeadsDir = fmt.Sprintf("/data/beads/%s", derivedName)
			derived.Role = agent.Role
			derived.ReplicaOf = name
			derived.ReplicaIndex = i
			derived.ReplicaCount = replicas
			c.Agents[derivedName] = derived
		}
	}
	for name, agent := range c.Agents {
		if agent.ReplicaOf == "" {
			continue
		}
		if _, ok := baseAgents[agent.ReplicaOf]; !ok {
			delete(c.Agents, name)
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
