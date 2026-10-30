package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	DefaultAgentsGitHubAPIHourlyCap = 300
	DefaultGitHubAgentReserveFloor  = 400
)

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

// StatsDisplayEntry defines a single metric to show in the agent's sidebar/detail view.
type StatsDisplayEntry struct {
	Key        string `yaml:"key" json:"key"`
	Label      string `yaml:"label" json:"label"`
	Source     string `yaml:"source" json:"source"`
	Field      string `yaml:"field" json:"field"`
	Style      string `yaml:"style" json:"style"`
	Icon       string `yaml:"icon,omitempty" json:"icon,omitempty"`
	TrendField string `yaml:"trend_field,omitempty" json:"trendField,omitempty"`
	Target     int    `yaml:"target,omitempty" json:"target,omitempty"`
	// Desc is a one-line explanation of what the stat verifies, rendered
	// as a hover tooltip in the dashboard (health checks especially).
	Desc string `yaml:"desc,omitempty" json:"desc,omitempty"`
}

// ChannelConfig declares a trigger channel for an agent.
//
// ChannelTypeKick has the governor runtime; ChannelTypeMention has the GitHub
// @-mention runtime, but must be paired with kick so an agent never becomes
// mention-only dormant. The former webhook/discord/schedule/bead trigger types
// were declarative-only: the pkg/channels runtime meant to serve them was never
// wired into the binary and was removed (#5591). ValidateChannels rejects them.
type ChannelConfig struct {
	Type    string `yaml:"type" json:"type"`
	Enabled *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

const (
	// ChannelTypeKick is ordinary governor timer kicks.
	ChannelTypeKick = "kick"
	// ChannelTypeMention is the GitHub @-mention trigger runtime.
	ChannelTypeMention = "mention"
)

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

// Agent explain modes (AgentConfig.ExplainMode / HIVE_EXPLAIN_MODE).
//
// Agents are told to act rather than narrate — see the "Output Rules — Terse
// Mode" block in every agent policy and the EXECUTE, DO NOT NARRATE suffix the
// agent manager appends on inference backends. That instruction exists to stop
// weak models answering a kick with a plan for someone else to run instead of
// running it, which is a real, observed failure and must be preserved. The cost
// of preserving it is that an operator debugging an agent has no visibility
// into WHY it chose what it chose (#3887, split from #3878).
//
// Explain mode buys that visibility back without relaxing the rule: the agent
// still acts first, and the explanation rides ALONGSIDE the tool calls on
// EXPLAIN-prefixed lines rather than replacing them.
const (
	// ExplainModeOff disables explanation. As an explicit per-agent value it
	// also overrides a hive-wide default, which "" (unset) does not.
	ExplainModeOff = "off"
	// ExplainModeBrief asks for one short EXPLAIN line per tool call.
	ExplainModeBrief = "brief"
	// ExplainModeFull adds an end-of-turn EXPLAIN block covering the goal as
	// understood, alternatives rejected, and what would change the decision.
	ExplainModeFull = "full"
)

// ExplainModeEnvVar is the deployment environment variable holding the
// hive-wide default for agents that leave explain_mode unset. It lets an
// operator debugging a misbehaving fleet turn explanation on for every agent at
// once without editing (and later having to unedit) each agent's config — the
// same shape as HIVE_TMUX_HISTORY_LIMIT and HIVE_TMUX_PANE_WIDTH.
//
// It remains supported, but it is no longer the ONLY place the default lives:
// see GovernorConfig.ExplainMode. Hive also injects the RESOLVED mode into
// every agent process under this same name.
const ExplainModeEnvVar = "HIVE_EXPLAIN_MODE"

// ExplainLinePrefix marks a line the agent emitted as debugging explanation
// rather than as ordinary output. It is the whole reason explanation can be a
// SEPARATE stream without new capture infrastructure: agent logs are tmux pane
// scrapes, so there is no second channel to write to, but a stable, greppable
// prefix lets the full-log endpoint (and `grep`) split explanation from work
// after the fact. Chosen to be ASCII, unlikely to collide with tool output, and
// stable — the filter and the prompt must agree on it forever.
const ExplainLinePrefix = "EXPLAIN:"

// Jev per-agent modes (hivecommons/hive#8939). "" and "off" both mean "no Jev
// tool installed, no Jev network calls" — the default. "assist" installs the
// jev_decide skill into the agent's CLI home and lets the agent call the
// hive's Jev decision endpoint for quick typed judgments.
const (
	JevModeOff    = "off"
	JevModeAssist = "assist"
)

// JevEnabled reports whether this agent may call the Jev decision tool.
func (a AgentConfig) JevEnabled() bool { return a.JevMode == JevModeAssist }

// Claude Code's reasoning-effort levels, the closed set `claude --effort`
// accepts (claude --help, v2.1.273+). Named so the launch path, the
// validator and the tests agree on the vocabulary (hivecommons/hive#8377).
const (
	ClaudeEffortLow    = "low"
	ClaudeEffortMedium = "medium"
	ClaudeEffortHigh   = "high"
	ClaudeEffortXHigh  = "xhigh"
	ClaudeEffortMax    = "max"
)

// ClaudeBackend is the backend name of the Claude Code CLI, the only backend
// whose launch command carries `--effort` (hivecommons/hive#8377).
const ClaudeBackend = "claude"

// ReasoningEffortsByBackend lists the reasoning-effort values each CLI
// backend accepts, for the backends that expose an effort control at all:
// codex takes `-c model_reasoning_effort="<v>"`, agy takes `--effort <v>`,
// and claude takes `--effort <v>` (hivecommons/hive#8377). Backends absent
// here have no effort flag; a configured effort is ignored for them rather
// than breaking their launch command.
var ReasoningEffortsByBackend = map[string][]string{
	"codex":       {"minimal", "low", "medium", "high", "xhigh"},
	"agy":         {"low", "medium", "high"},
	ClaudeBackend: {ClaudeEffortLow, ClaudeEffortMedium, ClaudeEffortHigh, ClaudeEffortXHigh, ClaudeEffortMax},
}

// ValidEffort reports whether effort is one of the values backend's effort
// control accepts. An empty effort is never "valid" here — it means unset —
// so callers that want "unset is fine" check for "" first
// (ValidateReasoningEffort does). A backend with no effort control accepts
// nothing.
func ValidEffort(backend, effort string) bool {
	if effort == "" {
		return false
	}
	for _, v := range ReasoningEffortsByBackend[backend] {
		if v == effort {
			return true
		}
	}
	return false
}

// ValidateReasoningEffort reports whether effort is settable for backend:
// empty is always valid (the backend's own default), otherwise the backend
// must have an effort control and the value must be one it accepts. Rejecting
// at set time keeps the failure at the dashboard, not hours later on the kick
// path — the same rationale as ValidateBackend.
func ValidateReasoningEffort(backend, effort string) error {
	if effort == "" {
		return nil
	}
	accepted, ok := ReasoningEffortsByBackend[backend]
	if !ok {
		return fmt.Errorf("backend %s has no reasoning-effort control", backend)
	}
	if ValidEffort(backend, effort) {
		return nil
	}
	return fmt.Errorf("invalid reasoning effort %q for backend %s (accepted: %s)",
		effort, backend, strings.Join(accepted, ", "))
}

const (
	ReviewModelsFallbackPinned        = "pinned"
	ReviewModelsFallbackSkip          = "skip"
	ReviewModelsFallbackRequiresHuman = "requires_human"
)

type ReviewModelPoolEntry struct {
	Backend string `yaml:"backend" json:"backend,omitempty"`
	Model   string `yaml:"model" json:"model,omitempty"`
}

type ReviewModelsConfig struct {
	Pool                []ReviewModelPoolEntry `yaml:"pool,omitempty" json:"pool,omitempty"`
	ExcludeAuthorModel  *bool                  `yaml:"exclude_author_model,omitempty" json:"exclude_author_model,omitempty"`
	ExcludeAuthorFamily bool                   `yaml:"exclude_author_family,omitempty" json:"exclude_author_family,omitempty"`
	Fallback            string                 `yaml:"fallback,omitempty" json:"fallback,omitempty"`
}

func (r ReviewModelsConfig) Configured() bool {
	return len(r.Pool) > 0 || r.ExcludeAuthorModel != nil || r.ExcludeAuthorFamily || strings.TrimSpace(r.Fallback) != ""
}

func (r ReviewModelsConfig) ExcludeAuthorModelEnabled() bool {
	return r.ExcludeAuthorModel == nil || *r.ExcludeAuthorModel
}

func (r ReviewModelsConfig) EffectiveFallback() string {
	switch strings.TrimSpace(r.Fallback) {
	case "":
		return ReviewModelsFallbackPinned
	case ReviewModelsFallbackSkip, ReviewModelsFallbackRequiresHuman:
		return strings.TrimSpace(r.Fallback)
	default:
		return strings.TrimSpace(r.Fallback)
	}
}

// WithPool returns a copy whose pool is replaced, keeping the exclusion and
// fallback settings. The swarm uses it to apply review.perspective_models: a
// per-perspective entry overrides WHICH models are eligible, not the rule
// that decides whether one is.
func (r ReviewModelsConfig) WithPool(pool []ReviewModelPoolEntry) ReviewModelsConfig {
	r.Pool = append([]ReviewModelPoolEntry(nil), pool...)
	return r
}

// SetLoadState records the YAML map key and the overlay file an entry was
// read from. The loader (pkg/config) calls it once per agent after decoding;
// nothing else should. An empty file means the main config.
func (a *AgentConfig) SetLoadState(name, sourceFile string) {
	a.name = name
	a.sourceFile = sourceFile
}

// ClearLoadState drops the load-time bookkeeping (name, source file and
// the explicit-key markers) before an entry is written back to disk, so
// internal-only state never round-trips through an overlay file.
func (a *AgentConfig) ClearLoadState() {
	a.name = ""
	a.sourceFile = ""
	a.clearOnKickSet = false
	a.enabledSet = false
}

// ClearOnKickExplicitlySet reports whether the YAML explicitly wrote
// clear_on_kick, the counterpart of EnabledExplicitlySet.
func (a *AgentConfig) ClearOnKickExplicitlySet() bool {
	return a.clearOnKickSet
}

// MarkExplicit records that enabled and/or clear_on_kick were explicitly
// written, for loaders that decode an agent through a path other than
// UnmarshalYAML (JSON, a test fixture) and need the same distinction.
func (a *AgentConfig) MarkExplicit(enabled, clearOnKick bool) {
	if enabled {
		a.enabledSet = true
	}
	if clearOnKick {
		a.clearOnKickSet = true
	}
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

// CadenceScopeMode returns this agent's cadence scope with the aggregate default.
func (a AgentConfig) CadenceScopeMode() string {
	if a.CadenceScope == CadenceScopePerRepo {
		return CadenceScopePerRepo
	}
	return CadenceScopeAggregate
}

// UsesRepoScopedCadence reports whether this agent opts into per-repo timer scheduling.
func (a AgentConfig) UsesRepoScopedCadence() bool { return a.CadenceScopeMode() == CadenceScopePerRepo }

const DefaultContinuousCooldown = time.Minute

const DefaultContinuousBudgetPct = 80

// DefaultContinuousTurnCeiling is how long continuous mode waits for a kicked
// turn to report an outcome before presuming it ended. It is generous enough
// that a long genuine turn is never cut short (re-arming never interrupts
// the agent anyway), yet bounds the idle-forever failure to well under an
// hour instead of "until the pod restarts".
const DefaultContinuousTurnCeiling = 45 * time.Minute

// EffectiveContinuousTurnCeiling returns the presumed-turn-end bound for
// continuous mode, or 0 when the operator disabled it with a negative value.
func (a AgentConfig) EffectiveContinuousTurnCeiling() time.Duration {
	switch {
	case a.ContinuousTurnCeiling < 0:
		return 0
	case a.ContinuousTurnCeiling > 0:
		return a.ContinuousTurnCeiling
	}
	return DefaultContinuousTurnCeiling
}

// EffectiveContinuousCooldown returns the per-agent minimum delay between a
// detected turn end and the next continuous-mode kick. A non-positive setting
// falls back to a safe one-minute cool-down so an agent that instantly reports
// no work cannot tight-loop.
func (a AgentConfig) EffectiveContinuousCooldown() time.Duration {
	if a.ContinuousCooldown > 0 {
		return a.ContinuousCooldown
	}
	return DefaultContinuousCooldown
}

// EffectiveContinuousBudgetPct returns the token-budget threshold at which
// continuous mode stops scheduling fresh re-kicks. A zero config value means
// the safe default, not "disabled".
func (a AgentConfig) EffectiveContinuousBudgetPct() int {
	if a.ContinuousBudgetPct > 0 {
		return a.ContinuousBudgetPct
	}
	return DefaultContinuousBudgetPct
}

// ContinuousCadence is the one question ContinuousInMode asks of a governor
// cadence: whether its value is the literal "continuous". The governor's
// Cadence type (pkg/config, which also parses cron and interval forms)
// satisfies it; the runtime never sees the rest of that type.
type ContinuousCadence interface {
	IsContinuous() bool
}

// ContinuousInMode reports whether this agent should run in continuous mode
// while the governor is in modeName. The legacy continuous bool remains a
// shorthand for every non-QUIET mode; a per-mode cadence value of "continuous"
// opts in exactly that mode.
func (a AgentConfig) ContinuousInMode(modeName string, cadence ContinuousCadence) bool {
	if cadence.IsContinuous() {
		return true
	}
	return a.Continuous && !strings.EqualFold(strings.TrimSpace(modeName), "quiet")
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

// SandboxPublishVerify returns the publish preflight commands for the agent:
// the per-agent publish block when present (even if its list is empty, which
// disables the preflight for that agent), else the global one.
func (a *AgentConfig) SandboxPublishVerify(global AgentSandboxConfig) []string {
	if a != nil && a.Sandbox != nil && a.Sandbox.Publish != nil {
		return append([]string(nil), a.Sandbox.Publish.Verify...)
	}
	if global.Publish != nil {
		return append([]string(nil), global.Publish.Verify...)
	}
	return nil
}

// SandboxPublishVerifyTimeoutS returns the per-command verify timeout: the
// per-agent value, then the global one, then DefaultSandboxVerifyTimeoutS.
func (a *AgentConfig) SandboxPublishVerifyTimeoutS(global AgentSandboxConfig) int {
	if a != nil && a.Sandbox != nil && a.Sandbox.Publish != nil && a.Sandbox.Publish.VerifyTimeoutS > 0 {
		return a.Sandbox.Publish.VerifyTimeoutS
	}
	if global.Publish != nil && global.Publish.VerifyTimeoutS > 0 {
		return global.Publish.VerifyTimeoutS
	}
	return DefaultSandboxVerifyTimeoutS
}

const DefaultBusyNoActivityThreshold = 30 * time.Minute

// EffectiveBusyNoActivityThreshold returns the silence threshold for surfacing
// busy-but-quiet agents. A zero value keeps the fleet default.
func (a AgentConfig) EffectiveBusyNoActivityThreshold() time.Duration {
	if a.BusyNoActivityThreshold > 0 {
		return a.BusyNoActivityThreshold
	}
	return DefaultBusyNoActivityThreshold
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

// UnmarshalYAML decodes the agent block and records whether `clear_on_kick`
// and `enabled` were written explicitly, so the loader can tell "not
// specified" from "false". It uses the yaml.v3 legacy Unmarshaler form (a
// decode callback rather than a *yaml.Node) so this package stays free of
// the yaml dependency; yaml.v3 honours both.
func (a *AgentConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type plain AgentConfig
	if err := unmarshal((*plain)(a)); err != nil {
		return err
	}
	var keys map[string]interface{}
	if err := unmarshal(&keys); err != nil {
		return err
	}
	if _, ok := keys["clear_on_kick"]; ok {
		a.clearOnKickSet = true
	}
	if _, ok := keys["enabled"]; ok {
		a.enabledSet = true
	}
	return nil
}

// Ownership markers for AgentConfig.ModelOwner / BackendOwner.
const (
	// FieldOwnerPack marks a value written by an ACMM pack. Pack-owned values
	// are reconciled to the current pack on every apply.
	FieldOwnerPack = "pack"
	// FieldOwnerOperator marks a value an operator chose in the Governor grid.
	// Operator-owned values are never overwritten by a pack apply. Pack apply
	// also stamps this on an allowed hand-set backend: that differs from the
	// pack (#13221).
	FieldOwnerOperator = "operator"
	// FieldOwnerSpec marks a value sourced from a BYO agent spec. Spec-owned
	// values are refreshed from the spec on config load/reload unless the
	// operator has explicitly taken ownership.
	FieldOwnerSpec = "spec"
)

// ModelIsOperatorOwned reports whether an operator explicitly chose this
// agent's model, which makes it immune to pack reconciliation.
func (a AgentConfig) ModelIsOperatorOwned() bool {
	return a.ModelOwner == FieldOwnerOperator
}

// OnDemandIsOperatorOwned reports whether an operator explicitly set this
// agent's on_demand flag from the settings dialog (#7446), which makes it
// immune to pack reconciliation.
func (a AgentConfig) OnDemandIsOperatorOwned() bool {
	return a.OnDemandOwner == FieldOwnerOperator
}

// BackendIsOperatorOwned reports whether an operator explicitly chose this
// agent's backend (the grid's "method" column).
func (a AgentConfig) BackendIsOperatorOwned() bool {
	return a.BackendOwner == FieldOwnerOperator
}

// BackendIsPackOwned reports whether an ACMM pack wrote this agent's backend,
// as opposed to an operator's pick or a legacy value with no owner.
func (a AgentConfig) BackendIsPackOwned() bool {
	return a.BackendOwner == FieldOwnerPack
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

// defaultProviderBudgetProbeIntervalS is the spend-rebuff probe interval when
// unset: 30 minutes.
const defaultProviderBudgetProbeIntervalS = DefaultProviderBudgetProbeIntervalS

// DefaultProviderBudgetProbeIntervalS is the exported spend-rebuff probe
// interval default, for editors that show it as a placeholder.
const DefaultProviderBudgetProbeIntervalS = 1800

// EffectiveProbeInterval returns the spend-rebuff probe interval, applying the
// default when unset. A negative or zero value means "unset" rather than
// "never probe": never probing is the deadlock this knob exists to prevent, so
// it is not a reachable configuration.
func (p ProviderBudgetConfig) EffectiveProbeInterval() time.Duration {
	if p.ProbeIntervalS > 0 {
		return time.Duration(p.ProbeIntervalS) * time.Second
	}
	return defaultProviderBudgetProbeIntervalS * time.Second
}

// Rotation headroom source names.
const (
	RotationHeadroomSourceBuiltin = "builtin"
	RotationHeadroomSourceCCLeft  = "ccleft"
)

// defaultRotationThresholdPct is the exhaustion threshold when unset.
const defaultRotationThresholdPct = 85

// defaultHighVolumeCadenceS is the high-volume cadence cutoff when unset.
const defaultHighVolumeCadenceS = 1800

// EffectiveThreshold returns the exhaustion threshold pct, defaulting to 85.
func (r RotationConfig) EffectiveThreshold() int {
	if r.ThresholdPct > 0 {
		return r.ThresholdPct
	}
	return defaultRotationThresholdPct
}

// EffectiveHeadroomSource returns the configured headroom source, defaulting to builtin.
func (r RotationConfig) EffectiveHeadroomSource() string {
	if r.HeadroomSource == RotationHeadroomSourceCCLeft {
		return RotationHeadroomSourceCCLeft
	}
	return RotationHeadroomSourceBuiltin
}

// EffectiveHighVolumeCadenceS returns the high-volume cadence cutoff in
// seconds, defaulting to 1800 (30 minutes).
func (r RotationConfig) EffectiveHighVolumeCadenceS() int {
	if r.HighVolumeCadenceS > 0 {
		return r.HighVolumeCadenceS
	}
	return defaultHighVolumeCadenceS
}

// Threshold scaling curves (GovernorConfig.ThresholdScaling).
const (
	// CadenceScopeAggregate keeps the historical governor behavior: one hive
	// mode is computed from the aggregate actionable issue+PR queue.
	CadenceScopeAggregate = "aggregate"
	// CadenceScopePerRepo computes a separate mode for each successfully
	// scanned repo from that repo's own actionable issue+PR queue.
	CadenceScopePerRepo = "per_repo"
)

const (
	GatewayKindOpenRouter = "openrouter"
	GatewayKindLiteLLM    = "litellm"
	GatewayKindWatsonx    = "watsonx"
	GatewayKindCustom     = "custom"
)

// APIKeySHA256 returns the lowercase SHA-256 hex digest of the exact key
// string Hive will present to an inference gateway. It returns empty for an
// empty key so callers can expose presence/hash without leaking the secret.
func APIKeySHA256(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (gw GatewayConfig) ResolveAPIKey() string {
	if gw.APIKeyEnv != "" {
		if v := os.Getenv(gw.APIKeyEnv); v != "" {
			return v
		}
	}
	if gw.APIKeyFile != "" {
		// SECURITY (audit N8): confine the read to the managed secrets dirs. This
		// gate lives HERE, not only in the HTTP handler, so every caller is
		// covered — including a path that reached hive.yaml some other way (a
		// hand-edited config, an older build, a restored backup).
		if !SecretFilePathAllowed(gw.APIKeyFile) {
			return ""
		}
		if b, err := os.ReadFile(gw.APIKeyFile); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

// IsEnabled reports whether the trajectory-review lane is on. Default is ON:
// a nil Enabled (key omitted) counts as enabled, only an explicit false
// disables it. The lane itself no-ops when no reviewer endpoint resolves, so
// defaulting on is safe even for hives without inference configured.
func (t TrajectoryConfig) IsEnabled() bool {
	return t.Enabled == nil || *t.Enabled
}

// ResolveAPIKey returns the backend's discovery API key using the
// resolution order: key file (api_key_file) → env var named by
// api_key_env → defaultEnv. Returns "" when no key is configured. The key
// value itself is never stored in hive.yaml.
func (c *InferenceAuthConfig) ResolveAPIKey(defaultEnv string) string {
	// SECURITY (audit N8): same confinement as GatewayConfig.ResolveAPIKey — an
	// api_key_file is only ever read from the managed secrets dirs.
	if c.APIKeyFile != "" && SecretFilePathAllowed(c.APIKeyFile) {
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

	// BobV2APIKeyEnvVar is the renamed env var bobshell 2.x reads for the
	// default `provider:"harness"` path. Verified in dist/bob.js 2.0.4:
	// `t.apiKey??process.env.BOB_API_KEY`.
	BobV2APIKeyEnvVar = "BOB_API_KEY"

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
	//
	// bobshell 2.0.4 removed this flag entirely (the only "auth-method" string
	// left in dist/bob.js is ai-gateway-auth-method), so launch code must only
	// pass it to 1.x.
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
	BobAutoApproveFlag  = "--auto-approve"

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

	// BobV2SettingsRelPath is bobshell 2.x's global settings file. From
	// dist/bob.js (2.0.4): getGlobalBobDirectory() is
	// path.join(os.homedir(), ".bob"), getGlobalSettingsDirectory() appends
	// "settings", and the user config joins that directory with "settings.json".
	BobV2SettingsRelPath = ".bob/settings/settings.json"
	BobV2ProviderKey     = "provider"
	BobV2ProviderHarness = "harness"
	BobV2ShellKey        = "bobShell"
	BobV2AutoUpdateKey   = "autoUpdate"

	// BobLegacyGeneralKey and the disable keys cover bob/gemini-lineage 1.x
	// settings shapes. They are harmless for versions that ignore them, and
	// keep auto-update disabled on lagging images while v2 uses
	// bobShell.autoUpdate=false in BobV2SettingsRelPath.
	BobLegacyGeneralKey           = "general"
	BobLegacyDisableAutoUpdateKey = "disableAutoUpdate"
	BobLegacyDisableUpdateNagKey  = "disableUpdateNag"

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
		// SECURITY (audit N8): only the managed secrets dirs. The two defaults
		// appended above already satisfy this; the gate is for c.APIKeyFile,
		// which is operator/API-supplied.
		if !SecretFilePathAllowed(f) {
			continue
		}
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

// InferenceBackends is the canonical list of model-gateway / inference backend
// IDs. It lives in the config package (a leaf in the import graph) so the
// agent and proxy packages can share it without an import cycle
// (proxy → agent → config).
//
// These are NOT agentic CLIs. Every one of them names a model endpoint that
// hive fronts with its own OpenAI-compatible translator and drives with the
// claude CLI; auth is an API key (or, for watsonx, an IAM bearer minted from
// one) supplied by config, never an interactive login.
//
// "watsonx" is here because IBM watsonx.ai is a model gateway in exactly that
// sense. It was previously supported ONLY as a `gateways:` kind and as an
// onboarding UI option, while the agent launcher had no case for it — so
// `backend: watsonx` was accepted by config and then rejected hours later at
// kick time with "unknown backend: watsonx". Anything that enumerates gateway
// backends must read THIS list rather than repeating the members inline.
var InferenceBackends = []string{"vllm", "llm-d", "litellm", "watsonx"}

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

// CLIBackends is the canonical list of agentic-CLI agent backends — backends
// that launch a real coding CLI binary rather than routing to a model gateway.
//
// This exists so the CONFIG VALIDATOR and the LAUNCHER dispatch on one list.
// They used to keep separate inline sets, which is the drift that let
// `backend: watsonx` be accepted at config-set time and then fail at kick time
// with "unknown backend: watsonx". Adding a backend in one place only is now a
// visible omission rather than a silent accept-then-fail.
//
// `agy` is the Antigravity CLI, Google's replacement for the Gemini CLI. Google
// consolidated its CLI tooling under Antigravity at I/O 2026 and Gemini CLI
// STOPPED SERVING personal and Google AI Pro accounts on 2026-06-18; only
// customers holding paid Gemini Code Assist licences can still invoke it. The
// `gemini` entry is therefore retained for those licence holders, but a hive
// whose Google access is a Gemini/AI Pro subscription can only reach Google
// through `agy`. It was already listed in config/backends.conf's
// KNOWN_BACKENDS, so omitting it here reproduced exactly the accept-in-one-
// place drift this list exists to prevent — except inverted: valid in the
// shell config, rejected by the hub.
//
// TestShellAndGoCLIBackendListsAgree (backend_list_parity_test.go) asserts
// this list against config/backends.conf's KNOWN_BACKENDS, with a closed,
// commented set of exceptions (cliBackendExceptions in that file) for the two
// names — litellm, gemini — that are known to belong on only one side. Adding
// a backend here without also updating the shell side (or, if it genuinely
// belongs on only one side, documenting why in cliBackendExceptions) fails
// that test.
var CLIBackends = []string{"claude", "copilot", "goose", "codex", "pi", "bob", "aider", "gemini", "agy", "opencode", "kilo", "muse", "omp", "openhands", "omnigent"}

// IsCLIBackend returns true if the backend launches an agentic CLI binary.
func IsCLIBackend(backend string) bool {
	for _, b := range CLIBackends {
		if b == backend {
			return true
		}
	}
	return false
}

// SupportedBackends returns every backend name valid independent of this
// hive's configuration: the agentic CLIs plus the model-gateway backends. A
// configured gateway NAME is additionally valid as a backend, but that is
// config-dependent and so is checked separately by the validator.
func SupportedBackends() []string {
	out := make([]string, 0, len(CLIBackends)+len(InferenceBackends))
	out = append(out, CLIBackends...)
	out = append(out, InferenceBackends...)
	return out
}

// SelfMergeMinACMMLevel is the lowest ACMM level at which the App is allowed
// to self-merge its own PRs. examples/acmm/l5.md is explicit that L5 does NOT
// grant merge authority ("Merge pull requests — no; L5 PRs remain hold-gated
// for human review"), and examples/acmm/l4.md is even more explicit ("DO NOT
// merge pull requests — L4 is not an auto-merge level"). examples/acmm/l6.md
// is the first level whose Responsibilities include "Merge PRs when CI
// passes". So the gate sits at L6, not L5: an L4 (or L5) hive must never run
// the self-authored auto-merge sweep, regardless of the auto_merge.self_authored
// flag below. This was the root cause of ks/hive (acmm_level: 4) wrongly
// self-merging its own PR — the sweep previously had no ACMM check at all.
const SelfMergeMinACMMLevel = 6

func (a *AgentConfig) HasEnabledChannel(t string) bool {
	for _, ch := range a.Channels {
		if ch.Type == t && ch.IsEnabled() {
			return true
		}
	}
	return false
}

// ModeIsOperatorOwned reports whether an operator explicitly chose this
// agent's mode, which makes it immune to pack reconciliation on restart.
func (a AgentConfig) ModeIsOperatorOwned() bool {
	return a.ModeOwner == FieldOwnerOperator
}
