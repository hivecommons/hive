package config

import (
	"sort"
	"strings"
	"time"

	"fmt"

	"gopkg.in/yaml.v3"
)

// Sandbox runtimes. SandboxRuntimePodman is the default when nothing is set.
const (
	SandboxRuntimePodman = "podman"
	SandboxRuntimeJob    = "job"
)

// ValidSandboxRuntimes is the closed set a config may name.
var ValidSandboxRuntimes = map[string]bool{SandboxRuntimePodman: true, SandboxRuntimeJob: true}

// SandboxJobConfig is everything the Job runtime needs beyond the image
// (#6311). The pod-template fields pass through to Kubernetes verbatim.
//
// The workspace is shared between hive and the Job by mounting the SAME
// PersistentVolumeClaim in both: WorkspaceClaim names it, and
// WorkspaceClaimMount is where hive sees it (default /data, where the
// standalone and hub-provisioned deployments mount hive-data). The sandbox
// workspace root must live under that mount. When the Job lands on a
// different node than hive, the claim has to be ReadWriteMany.
//
// EnvFromSecrets is the ONLY credential path into the Job. Hive's brokered
// GitHub token never enters it; hive pushes and opens the PR from the
// workspace after the Job exits, exactly as it does for the Podman runtime.
type SandboxJobConfig struct {
	WorkspaceClaim      string                 `yaml:"workspace_claim,omitempty" json:"workspace_claim,omitempty"`
	WorkspaceClaimMount string                 `yaml:"workspace_claim_mount,omitempty" json:"workspace_claim_mount,omitempty"`
	NodeSelector        map[string]string      `yaml:"node_selector,omitempty" json:"node_selector,omitempty"`
	Tolerations         []SandboxJobToleration `yaml:"tolerations,omitempty" json:"tolerations,omitempty"`
	Resources           SandboxJobResources    `yaml:"resources,omitempty" json:"resources,omitempty"`
	ServiceAccount      string                 `yaml:"service_account,omitempty" json:"service_account,omitempty"`
	EnvFromSecrets      []string               `yaml:"env_from_secrets,omitempty" json:"env_from_secrets,omitempty"`
	Volumes             []SandboxJobVolume     `yaml:"volumes,omitempty" json:"volumes,omitempty"`
	TTLSeconds          int                    `yaml:"ttl_seconds,omitempty" json:"ttl_seconds,omitempty"`
}

// SandboxJobToleration mirrors the subset of a Kubernetes toleration an
// operator writes by hand.
type SandboxJobToleration struct {
	Key      string `yaml:"key,omitempty" json:"key,omitempty"`
	Operator string `yaml:"operator,omitempty" json:"operator,omitempty"`
	Value    string `yaml:"value,omitempty" json:"value,omitempty"`
	Effect   string `yaml:"effect,omitempty" json:"effect,omitempty"`
}

// SandboxJobResources mirrors Kubernetes resource requirements with string
// quantities so a device limit (`<vendor>.com/<device>: "2"`) passes through.
type SandboxJobResources struct {
	Limits   map[string]string `yaml:"limits,omitempty" json:"limits,omitempty"`
	Requests map[string]string `yaml:"requests,omitempty" json:"requests,omitempty"`
}

// SandboxJobVolume is an additional PVC mounted into the Job (a compile
// cache, a model store).
type SandboxJobVolume struct {
	Name      string `yaml:"name,omitempty" json:"name,omitempty"`
	Claim     string `yaml:"claim" json:"claim"`
	MountPath string `yaml:"mount_path" json:"mount_path"`
	ReadOnly  bool   `yaml:"read_only,omitempty" json:"read_only,omitempty"`
}

func sortedRuntimeNames() []string {
	names := make([]string, 0, len(ValidSandboxRuntimes))
	for n := range ValidSandboxRuntimes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// SandboxRuntime returns the per-agent runtime override, then the global
// runtime, then SandboxRuntimePodman. The value is returned as written; the
// manager rejects one outside ValidSandboxRuntimes at kick time.
func (a *AgentConfig) SandboxRuntime(global AgentSandboxConfig) string {
	if a != nil && a.Sandbox != nil && strings.TrimSpace(a.Sandbox.Runtime) != "" {
		return strings.TrimSpace(a.Sandbox.Runtime)
	}
	if strings.TrimSpace(global.Runtime) != "" {
		return strings.TrimSpace(global.Runtime)
	}
	return SandboxRuntimePodman
}

// SandboxJob returns the effective Job runtime settings. A per-agent job
// block replaces the global one wholesale for the pod-template fields (node
// selector, tolerations, resources, secrets, volumes, service account) — an
// agent that needs an accelerator names its own scheduling. The claim
// fields and the TTL are cluster facts rather than per-agent choices, so
// they fall back to the global block when the per-agent one leaves them
// empty.
func (a *AgentConfig) SandboxJob(global AgentSandboxConfig) SandboxJobConfig {
	var g SandboxJobConfig
	if global.Job != nil {
		g = *global.Job
	}
	if a == nil || a.Sandbox == nil || a.Sandbox.Job == nil {
		return g
	}
	out := *a.Sandbox.Job
	if strings.TrimSpace(out.WorkspaceClaim) == "" {
		out.WorkspaceClaim = g.WorkspaceClaim
	}
	if strings.TrimSpace(out.WorkspaceClaimMount) == "" {
		out.WorkspaceClaimMount = g.WorkspaceClaimMount
	}
	if out.TTLSeconds == 0 {
		out.TTLSeconds = g.TTLSeconds
	}
	return out
}

// ValidateKickTemplateName gates the SHAPE of a kick_template value: it is a
// bare file name that the scheduler looks up under the policy directories and
// the embedded defaults, never a path. Empty is fine (convention lookup). A
// value carrying a path separator or ".." is refused at load/save time — the
// scheduler joins it under /data/policies and the cloned examples dir, so a
// path would read outside them. Whether the NAME resolves to a file is a
// separate, softer question (a template can legitimately be created later by
// the prompt editor), answered with a warning by
// scheduler.WarnDanglingKickTemplates and shown in the prompt editor
// (hivecommons/hive#7390).
func ValidateKickTemplateName(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	if strings.ContainsAny(v, `/\`) || strings.Contains(v, "..") {
		return fmt.Errorf("invalid kick_template %q: must be a bare file name such as scanner-holdgated.md, not a path", v)
	}
	return nil
}

type AgentConfig struct {
	ID           string             `yaml:"id" json:"id,omitempty"`
	Backend      string             `yaml:"backend" json:"backend,omitempty"`
	Model        string             `yaml:"model" json:"model,omitempty"`
	ReviewModels ReviewModelsConfig `yaml:"review_models,omitempty" json:"review_models,omitempty"`
	// ReasoningEffort pins the reasoning effort the agent's CLI is launched
	// with, for backends that expose one (see ReasoningEffortsByBackend):
	// codex is passed `-c model_reasoning_effort="<v>"`, agy `--effort <v>`.
	// Empty means the backend's own default. Backends with no effort control
	// ignore it. The scripted launch paths (bin/agent-launch.sh, the
	// contributor relay) already honor the same choice via
	// AGENT_REASONING_EFFORT; this field is the in-config analogue for
	// manager-launched agents.
	ReasoningEffort string `yaml:"reasoning_effort,omitempty" json:"reasoning_effort,omitempty"`
	BeadsDir        string `yaml:"beads_dir" json:"beads_dir,omitempty"`
	Enabled         bool   `yaml:"enabled" json:"enabled,omitempty"`
	Replicas        int    `yaml:"replicas,omitempty" json:"replicas,omitempty"`
	ReplicaOf       string `yaml:"-" json:"replicaOf,omitempty"`
	ReplicaIndex    int    `yaml:"-" json:"replicaIndex,omitempty"`
	ReplicaCount    int    `yaml:"-" json:"replicaCount,omitempty"`
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
	PauseOwner string `yaml:"pause_owner" json:"pause_owner,omitempty"`

	// Repos scopes this agent to the repositories it serves (#6204). Empty —
	// the zero value, and every agent in every config written before this
	// field — means the whole hive, so an existing roster is unchanged.
	//
	// It is the missing half of Hive's per-repo story: AGENTS.md and
	// repo-local skills already vary what an agent KNOWS per repo, but the
	// roster itself was a function of the hive, so a specialist added for one
	// repository woke on cadence and hunted for its concern in all the others.
	//
	// Entries are spelled as project.repos spells them — a bare name
	// ("console") or an explicit cross-org reference ("laredo/cuga-agent") —
	// and matched org-qualified and case-folded. See agent_repos.go; the
	// predicate every enforcement point asks is Config.AgentServesRepo.
	Repos []string `yaml:"repos,omitempty" json:"repos,omitempty"`
	// ReposOwner records WHO set Repos, with the same FieldOwner* vocabulary
	// as ModelOwner/BackendOwner/PauseOwner. No pack ships a repo scope today,
	// so nothing reconciles it away today; the marker exists so that one which
	// someday does cannot widen an operator's specialist back to the whole
	// hive on the next restart — the #5632/#5706 clobber family this field
	// would otherwise join.
	ReposOwner string `yaml:"repos_owner,omitempty" json:"repos_owner,omitempty"`

	StaleTimeout    int    `yaml:"stale_timeout" json:"stale_timeout,omitempty"`
	RestartStrategy string `yaml:"restart_strategy" json:"restart_strategy,omitempty"`
	LaunchCmd       string `yaml:"launch_cmd" json:"launch_cmd,omitempty"`
	AgentSpec       string `yaml:"agent_spec" json:"agent_spec,omitempty"`
	// BusyNoActivityThreshold is how long a Working pane may go without any
	// Copilot transcript activity before repeated undeliverable kicks surface
	// an operator-visible condition. Unset defaults to 30m.
	BusyNoActivityThreshold time.Duration `yaml:"busy_no_activity_threshold,omitempty" json:"busy_no_activity_threshold,omitempty"`
	// MaxTurnDuration is an optional visibility-only ceiling for a Working
	// turn. 0 disables it; exceeding it raises busy-over-ceiling but never
	// interrupts the agent.
	MaxTurnDuration time.Duration `yaml:"max_turn_duration,omitempty" json:"max_turn_duration,omitempty"`
	DisplayName     string        `yaml:"display_name" json:"display_name,omitempty"`
	Description     string        `yaml:"description" json:"description,omitempty"`

	// Phase 2: config-driven agent behavior fields
	Role           string              `yaml:"role" json:"role,omitempty"`
	SortOrder      int                 `yaml:"sort_order" json:"sort_order,omitempty"`
	Emoji          string              `yaml:"emoji" json:"emoji,omitempty"`
	Color          string              `yaml:"color" json:"color,omitempty"`
	Aliases        []string            `yaml:"aliases" json:"aliases,omitempty"`
	LaneKeywords   []string            `yaml:"lane_keywords" json:"lane_keywords,omitempty"`
	DetectKeywords []string            `yaml:"detect_keywords" json:"detect_keywords,omitempty"`
	KickTemplate   string              `yaml:"kick_template" json:"kick_template,omitempty"`
	TaskMCP        *AgentTaskMCPConfig `yaml:"task_mcp,omitempty" json:"task_mcp,omitempty"`
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
	// CadenceScope opts this agent into repo-scoped governor cadence scheduling.
	// Empty/aggregate preserves one hive-wide cadence entry; per_repo is opt-in.
	CadenceScope     string              `yaml:"cadence_scope,omitempty" json:"cadence_scope,omitempty"`
	IncludeRepos     *bool               `yaml:"include_repos" json:"include_repos,omitempty"`
	MetricsCollector string              `yaml:"metrics_collector" json:"metrics_collector,omitempty"`
	BeadRole         string              `yaml:"bead_role" json:"bead_role,omitempty"`
	StatsDisplay     []StatsDisplayEntry `yaml:"stats_display" json:"stats_display,omitempty"`
	ACMMLevels       []int               `yaml:"acmm_levels" json:"acmm_levels,omitempty"`
	Mode             string              `yaml:"mode" json:"mode,omitempty"`
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
	Converse *bool `yaml:"converse,omitempty" json:"converse,omitempty"`
	OnDemand bool  `yaml:"on_demand" json:"on_demand,omitempty"`
	// OnDemandOwner records WHO last set OnDemand, with the same FieldOwner*
	// vocabulary as ModelOwner. on_demand is a pack-behavior field — it says
	// whether the agent wakes on the governor's cadence or only when something
	// asks for it — so ApplyPack reconciles it to the current pack like
	// kick_template and mode. Before it did, the flag written by a previous
	// pack definition stuck forever: a hive whose L5/L6 pack once shipped an
	// on-demand reviewer kept it on-demand after the pack moved to the cadenced
	// design, so the new template was applied but never kicked. The operator's
	// own toggle (#7446) stamps FieldOwnerOperator and is left alone.
	OnDemandOwner string `yaml:"on_demand_owner,omitempty" json:"on_demand_owner,omitempty"`
	CavemanMode   string `yaml:"caveman_mode" json:"caveman_mode,omitempty"`
	// JevMode opts this agent into the Jev typed-decision tool
	// (hivecommons/hive#8939): "" | off | assist. See ValidJevModes.
	JevMode string `yaml:"jev_mode,omitempty" json:"jev_mode,omitempty"`
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

	// Standby is the per-lane standby-contributor block from RFC #7629: when
	// this lane is paused for budget, offer its queue to approved standby
	// contributors whose model configuration clears the lane's floor.
	//
	// Nil — the zero value, and every config written before this field —
	// means the lane has no standby block at all, which is what keeps an
	// existing hive byte-for-byte unchanged through a load/save cycle. As of
	// S2 the block is parsed, defaulted and validated; NOTHING reads it to
	// make a decision. See pkg/config/standby.go and
	// src/docs/design/standby-contributors.md.
	Standby *StandbyConfig `yaml:"standby,omitempty" json:"standby,omitempty"`

	// Channels declares how this agent gets triggered. "kick" is the governor
	// runtime; "mention" is valid only alongside "kick". When nil/empty, the
	// agent uses governor timer kicks by default (implicit kick channel).
	// See ChannelConfig for why former webhook/discord/schedule/bead types are rejected.
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

func (a *AgentConfig) HasEnabledChannel(t string) bool {
	for _, ch := range a.Channels {
		if ch.Type == t && ch.IsEnabled() {
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

const cadenceTargetSeparator = "|"

// CadenceTargetKey returns the persisted governor cadence/last-kick key for an
// agent, optionally scoped to a repo. Empty repo preserves the legacy agent key.
func CadenceTargetKey(agent, repo string) string {
	if repo == "" {
		return agent
	}
	return agent + cadenceTargetSeparator + repo
}

// SplitCadenceTargetKey separates a governor cadence/last-kick key into agent
// and repo. Keys without a repo are the legacy aggregate shape.
func SplitCadenceTargetKey(key string) (agent, repo string) {
	agent, repo, ok := strings.Cut(key, cadenceTargetSeparator)
	if !ok {
		return key, ""
	}
	return agent, repo
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

	var optedIn, noImage, noClaim, badRuntime []string
	for name, a := range cfg.Agents {
		if !a.SandboxEnabled(cfg.AgentSandbox) {
			continue
		}
		optedIn = append(optedIn, name)
		if strings.TrimSpace(a.SandboxImage(cfg.AgentSandbox)) == "" {
			noImage = append(noImage, name)
		}
		switch rt := a.SandboxRuntime(cfg.AgentSandbox); {
		case !ValidSandboxRuntimes[rt]:
			badRuntime = append(badRuntime, name+"="+rt)
		case rt == SandboxRuntimeJob && strings.TrimSpace(a.SandboxJob(cfg.AgentSandbox).WorkspaceClaim) == "":
			noClaim = append(noClaim, name)
		}
	}
	sort.Strings(optedIn)
	sort.Strings(noImage)
	sort.Strings(noClaim)
	sort.Strings(badRuntime)

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
	if len(badRuntime) > 0 {
		out = append(out, fmt.Sprintf(
			"agent(s) %s name a sandbox runtime hive does not have; sandboxed kicks will fail outright — "+
				"sandbox.runtime must be one of %q (#6311)",
			strings.Join(badRuntime, ", "), sortedRuntimeNames()))
	}
	if len(noClaim) > 0 {
		out = append(out, fmt.Sprintf(
			"agent(s) %s use the job sandbox runtime but resolve no job.workspace_claim; the Job has no way to see the workspace and every kick will fail — "+
				"set agent_sandbox.job.workspace_claim (or the per-agent sandbox.job.workspace_claim) to the PVC hive's data lives on (#6311)",
			strings.Join(noClaim, ", ")))
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

const DefaultBusyNoActivityThreshold = 30 * time.Minute

// EffectiveBusyNoActivityThreshold returns the silence threshold for surfacing
// a Working-but-quiet agent. Non-positive values use the operator-safe default.
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

// PauseIsOperatorOwned reports whether an operator explicitly owns this
// agent's pause/run state — stamped at dashboard-API create and on an explicit
// operator resume — which makes the agent immune to the ACMM pack visibility
// sweep's "agent not in pack level N" pause on apply/restart (#5706), the same
// contract ModelIsOperatorOwned provides for models.
func (a AgentConfig) PauseIsOperatorOwned() bool {
	return a.PauseOwner == FieldOwnerOperator
}

// CadenceIsOperatorOwned reports whether an operator explicitly set the
// cadence for agent in the given governor mode, which makes it immune to pack
// reconciliation — the same contract ModelIsOperatorOwned provides for models.
func (g *GovernorConfig) CadenceIsOperatorOwned(mode, agent string) bool {
	return g.CadenceOwners[mode][agent] == FieldOwnerOperator
}

// ClaimCadenceOwnership marks the (mode, agent) cadence as operator-owned so
// no subsequent pack apply reconciles it back to the pack default.
func (g *GovernorConfig) ClaimCadenceOwnership(mode, agent string) {
	if g.CadenceOwners == nil {
		g.CadenceOwners = make(map[string]map[string]string)
	}
	if g.CadenceOwners[mode] == nil {
		g.CadenceOwners[mode] = make(map[string]string)
	}
	g.CadenceOwners[mode][agent] = FieldOwnerOperator
}

// ReleaseCadenceOwnership drops the ownership marker for (mode, agent). Called
// when the cadence entry itself is removed (e.g. the agent left the roster) so
// a stale claim cannot outlive the value it protected.
func (g *GovernorConfig) ReleaseCadenceOwnership(mode, agent string) {
	owners, ok := g.CadenceOwners[mode]
	if !ok {
		return
	}
	delete(owners, agent)
	if len(owners) == 0 {
		delete(g.CadenceOwners, mode)
	}
}

// EnabledExplicitlySet returns true when the user's YAML explicitly set the
// "enabled" field (allowing us to distinguish "not specified" from "enabled: false").
func (a *AgentConfig) EnabledExplicitlySet() bool {
	return a.enabledSet
}
