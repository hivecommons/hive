package config

import (
	"strings"
	"time"
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
	// SchedulerName sets the pod's schedulerName. Accelerator clusters often
	// gate device-requesting pods behind a custom scheduler via an admission
	// webhook (e.g. `A pod must use "schedulerName: spyre-scheduler"`);
	// without this the Job create is denied before any pod exists.
	SchedulerName  string             `yaml:"scheduler_name,omitempty" json:"scheduler_name,omitempty"`
	EnvFromSecrets []string           `yaml:"env_from_secrets,omitempty" json:"env_from_secrets,omitempty"`
	Volumes        []SandboxJobVolume `yaml:"volumes,omitempty" json:"volumes,omitempty"`
	TTLSeconds     int                `yaml:"ttl_seconds,omitempty" json:"ttl_seconds,omitempty"`
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

// AgentRuntimeConfig is the top-level hive.yaml surface owned by the agent
// runtime (Crux, hivecommons/hive#13536, stage 1): the agent definitions,
// the backend allow/deny list, provider status, the tool-approval desk, the
// credential-free sandbox and the re-entrant turn model. Config embeds it
// with `yaml:",inline"`, so every key keeps its historical top-level
// position (`agents:`, `backends:`, `providers:`, `tool_approval:`,
// `agent_sandbox:`, `turn:`) and existing hive.yaml files parse
// identically; the struct exists so the runtime's configuration can be
// named, documented and eventually handed to Crux as one value. Governor-
// nested runtime keys live in AgentRuntimeGovernorConfig.
type AgentRuntimeConfig struct {
	Agents map[string]AgentConfig `yaml:"agents"`
	// AgentsGitHubAPIHourlyCap is the fleet default for per-agent GitHub API
	// reads through the proxy. It is encoded under agents.github_api_hourly_cap
	// by Config's YAML hooks so existing agent-name keys remain unchanged.
	AgentsGitHubAPIHourlyCap int `yaml:"-" json:"-"`
	// AgentsGitHubAPIHourlyCapSet records whether the key was written at all,
	// so an explicit 0 ("no cap") is distinguishable from an absent key
	// (the default cap). Exported because the Config YAML hooks in
	// pkg/config set and read it; `yaml:"-"` keeps it off the wire.
	AgentsGitHubAPIHourlyCapSet bool `yaml:"-" json:"-"`
	// AgentsSessionRecycle is the fleet default for the CLI session RSS/age
	// recycle policy (#14842). It is encoded under agents.session_recycle by
	// Config's YAML hooks, like AgentsGitHubAPIHourlyCap.
	AgentsSessionRecycle SessionRecycleConfig `yaml:"-" json:"-"`
	// Backends is the spoke-wide backend allow/deny list (#11310), enforced
	// at pack apply and by PUT /api/config/agent/{name}/models.
	Backends BackendsConfig `yaml:"backends,omitempty" json:"backends,omitempty"`
	// Providers configures the read-only per-provider auth/availability
	// status (ADR-0024). See ProvidersConfig.
	Providers ProvidersConfig `yaml:"providers,omitempty" json:"providers,omitempty"`
	// ToolApproval configures the approval desk (RFC #4000): the single
	// decision point every approval-shaped request resolves through, plus the
	// operator rules that steer it. Additive and DEFAULT-OFF — an absent block
	// leaves every existing gate in charge and behavior byte-identical.
	ToolApproval ToolApprovalConfig `yaml:"tool_approval,omitempty" json:"tool_approval,omitempty"`
	// AgentSandbox configures the phase-1 credential-free sandbox runner. It is
	// disabled by default and agents must opt in individually.
	AgentSandbox AgentSandboxConfig `yaml:"agent_sandbox,omitempty" json:"agent_sandbox,omitempty"`
	// Turn gates the re-entrant conversation-as-state rollout (#5799). Default
	// off leaves every agent on the legacy tmux loop until an operator opts in.
	Turn TurnConfig `yaml:"turn,omitempty" json:"turn,omitempty"`
	// RepeatFailure switches on the monitor-only repeated-failing-command
	// check (#14550). Nil or enabled false leaves it off.
	RepeatFailure *RepeatFailureConfig `yaml:"repeat_failure,omitempty" json:"repeat_failure,omitempty"`
}

// AgentRuntimeGovernorConfig is the slice of `governor:` the agent runtime
// owns: the per-agent watchdog, the engine and inference-backend credential
// blocks (LiteLLM, vLLM, llm-d, bob), the named model gateways, trajectory
// review, kick-prompt limits and provider rotation / spending-limit
// handling. GovernorConfig embeds it with `yaml:",inline"`, so the keys stay
// under `governor:` exactly where operators have always written them
// (hivecommons/hive#13536).
type AgentRuntimeGovernorConfig struct {
	// Watchdog configures the per-agent self-healing reconciler (RFC #4665).
	// Zero value = enabled with the RFC defaults; see pkg/config/watchdog.go
	// for why defaults resolve lazily instead of via applyDefaults.
	Watchdog WatchdogConfig      `yaml:"watchdog,omitempty"`
	LiteLLM  LiteLLMConfig       `yaml:"litellm"`
	VLLM     InferenceAuthConfig `yaml:"vllm"`
	LLMD     InferenceAuthConfig `yaml:"llm-d"`
	// Bob holds the IBM bobshell CLI backend's API-key location. Required for
	// agents with backend "bob": bobshell's browser SSO flow cannot complete in
	// a headless pod.
	Bob        BobConfig        `yaml:"bob"`
	Trajectory TrajectoryConfig `yaml:"trajectory"`
	// KickLimits caps the issue and PR lists in every kick prompt
	// (hivecommons/hive#7368). Absent = defaults (100 issues, 50 PRs).
	KickLimits KickLimitsConfig `yaml:"kick_limits,omitempty" json:"kick_limits,omitempty"`
	// Gateways is the list of named model gateways (OpenAI-compatible endpoints
	// like OpenRouter, a LiteLLM proxy, vLLM, or llm-d). An agent routes through
	// a gateway by naming it as its backend. When empty, a single implicit
	// gateway named "litellm" is synthesized from the legacy LiteLLM block above
	// so existing hives keep working with zero config change. See ResolveGateway.
	Gateways []GatewayConfig `yaml:"gateways"`
	// Rotation configures automatic provider failover when a provider's
	// subscription or credit is exhausted. See RFC #3958.
	Rotation RotationConfig `yaml:"rotation,omitempty" json:"rotation,omitempty"`
	// ProviderBudget tunes the response to a PROVIDER spending-limit refusal
	// (#4294) — the gateway declining to spend more money, as distinct from the
	// hive's own token Budget above. See ProviderBudgetConfig.
	ProviderBudget ProviderBudgetConfig `yaml:"provider_budget,omitempty" json:"provider_budget,omitempty"`
}

type AgentConfig struct {
	// GitHubAPIHourlyCap overrides agents.github_api_hourly_cap for this agent.
	// Nil inherits the global value; 0 disables the cap for this agent.
	GitHubAPIHourlyCap *int `yaml:"github_api_hourly_cap,omitempty" json:"github_api_hourly_cap,omitempty"`

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
	// ReviewHeads derives one reviewer agent per entry and replica from this
	// (base) agent, each with its own backend/model: the swarm's model
	// "heads" (hivecommons/hive#13542). With split perspectives every
	// perspective is reviewed once per head, and the base agent is kept out
	// of head dispatch. See review_heads.go.
	ReviewHeads []ReviewHead `yaml:"review_heads,omitempty" json:"review_heads,omitempty"`
	// ReviewHead is set on an agent derived from a base's review_heads: the
	// head key (<base>-<alias>) shared by that head's replicas. Runtime only,
	// like ReplicaOf.
	ReviewHead string `yaml:"-" json:"reviewHead,omitempty"`
	// Paused persists an operator pause across restarts/upgrades. Without
	// this, every pod restart rebuilt agents un-paused (Go zero value), so
	// an operator pause was silently undone on the next upgrade.
	Paused      bool `yaml:"paused" json:"paused,omitempty"`
	ClearOnKick bool `yaml:"clear_on_kick" json:"clear_on_kick"`
	CLIPinned   bool `yaml:"cli_pinned" json:"cli_pinned,omitempty"`
	// Continuous makes this agent re-enter work promptly after a kicked turn has
	// genuinely ended. It is intentionally per-agent and opt-in: cadence still
	// starts/guards the agent, but while continuous is on the next governor kick
	// is driven by the manager's "back at input prompt" turn-end signal plus
	// ContinuousCooldown, never by interrupting a running session.
	Continuous          bool          `yaml:"continuous,omitempty" json:"continuous,omitempty"`
	ContinuousCooldown  time.Duration `yaml:"continuous_cooldown,omitempty" json:"continuous_cooldown,omitempty"`
	ContinuousBudgetPct int           `yaml:"continuous_budget_pct,omitempty" json:"continuous_budget_pct,omitempty"`
	// ContinuousTurnCeiling bounds how long continuous mode waits for the
	// manager's turn-end signal after a kick before it presumes the turn
	// over and re-arms the next kick. The signal is a pane heuristic; when
	// it is missed the agent otherwise sits idle at its prompt forever with
	// no kick attempts, so none of the undeliverable-kick recovery paths
	// ever engage. A re-arm only schedules an ATTEMPT — delivery still
	// refuses to type into a working pane. Unset uses
	// DefaultContinuousTurnCeiling; a negative value disables the ceiling.
	ContinuousTurnCeiling time.Duration `yaml:"continuous_turn_ceiling,omitempty" json:"continuous_turn_ceiling,omitempty"`

	// ModelOwner / BackendOwner record WHO last set Model / Backend. An ACMM
	// pack owns these fields until an operator changes them in the Governor
	// grid; from that point the operator owns them and ApplyPack must not
	// reconcile them back to the pack value. Pack apply also stamps operator
	// on an allowed hand-set backend: that differs from the pack (#13221).
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
	// ModeOwner records WHO last set Mode, with the same FieldOwner*
	// vocabulary as ModelOwner. Without it, a pack re-apply — which happens on
	// EVERY hive restart — reconciled Mode back to the pack value, so an
	// operator's deliberate mode change in the dashboard (e.g. giving architect
	// ISSUES_PRS_MERGE) silently reverted on the next restart. An explicit
	// level change resets it to the pack so the new level's mode applies.
	ModeOwner string `yaml:"mode_owner" json:"mode_owner,omitempty"`
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

	StaleTimeout int `yaml:"stale_timeout" json:"stale_timeout,omitempty"`
	// BusyNoActivityThreshold is how long a Working pane may go without any visible activity
	// before dashboards surface it as possibly stuck. Zero uses DefaultBusyNoActivityThreshold.
	// A pane that stays in that condition for twice the threshold is restarted.
	BusyNoActivityThreshold time.Duration `yaml:"busy_no_activity_threshold,omitempty" json:"busy_no_activity_threshold,omitempty"`
	// MaxTurnDuration is an optional visibility-only ceiling for a Working turn.
	MaxTurnDuration time.Duration `yaml:"max_turn_duration,omitempty" json:"max_turn_duration,omitempty"`
	RestartStrategy string        `yaml:"restart_strategy" json:"restart_strategy,omitempty"`
	LaunchCmd       string        `yaml:"launch_cmd" json:"launch_cmd,omitempty"`
	AgentSpec       string        `yaml:"agent_spec" json:"agent_spec,omitempty"`
	// SessionRecycle overrides the fleet agents.session_recycle policy for
	// this agent (#14842). Nil inherits the fleet value; EnabledAgents()
	// stamps the fleet block into nil pointers so the agent manager sees one
	// resolved policy per agent.
	SessionRecycle *SessionRecycleConfig `yaml:"session_recycle,omitempty" json:"session_recycle,omitempty"`
	DisplayName    string                `yaml:"display_name" json:"display_name,omitempty"`
	Description    string                `yaml:"description" json:"description,omitempty"`
	// Bob holds bob-backend-only per-agent options. Empty is default-off.
	Bob AgentBobConfig `yaml:"bob,omitempty" json:"bob,omitempty"`
	// BobDisplayName is the legacy flat spelling for Bob.SessionLabel. It is
	// kept for backwards compatibility; new configs should use bob.session_label.
	BobDisplayName string `yaml:"bob_display_name,omitempty" json:"bob_display_name,omitempty"`

	// Phase 2: config-driven agent behavior fields
	Role string `yaml:"role" json:"role,omitempty"`
	// Engine names the experimentation engine an agent with role
	// experimenter runs (RFC #11585 part 5). Empty on an experimenter means
	// DefaultExperimentEngine (nous); it is rejected on any other role. See
	// ValidExperimentEngines.
	Engine         string              `yaml:"engine,omitempty" json:"engine,omitempty"`
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
	// Advisor is this agent's advisor-lane override (#9722): any field of the
	// fleet-wide advisor block may be changed here, or the agent opted out
	// entirely with enabled: false. Nil → inherit the fleet default.
	Advisor *AdvisorOverride `yaml:"advisor,omitempty" json:"advisor,omitempty"`
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

	// Variables declares per-agent ${NAME} substitutions for this agent's kick
	// prompt. They merge over hive-level variables.defs for this agent only,
	// and only safe static/env resolvers are honored from this user-writable
	// overlay surface. json:"-" keeps values out of generic agent-config JSON.
	Variables map[string]VarDef `yaml:"variables,omitempty" json:"-"`

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

// AgentBobConfig holds per-agent bob backend options.
type AgentBobConfig struct {
	// SessionLabel overrides governor.bob.session_prefix + agent name for Bob
	// --instance-id. It never changes the Hive agent identity.
	SessionLabel string `yaml:"session_label,omitempty" json:"session_label,omitempty"`
}

// AgentSandboxConfig controls the sandbox kick launcher. The top-level block
// is a global gate; per-agent config can opt specific agents in.
//
// Runtime selects WHAT runs a sandboxed kick: "podman" (the default, a
// rootless container on the hive pod's own node) or "job" (a Kubernetes Job
// in the hive's namespace, #6311). The Job runtime is how an agent's kicks
// reach a different image, a node with accelerator hardware, and operator
// secrets by reference; Job holds its settings.
type AgentSandboxConfig struct {
	Enabled      bool              `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Image        string            `yaml:"image,omitempty" json:"image,omitempty"`
	EnvAllowlist []string          `yaml:"env_allowlist,omitempty" json:"env_allowlist,omitempty"`
	NetworkMode  string            `yaml:"network_mode,omitempty" json:"network_mode,omitempty"`
	TimeoutS     int               `yaml:"timeout_s,omitempty" json:"timeout_s,omitempty"`
	WorkspaceDir string            `yaml:"workspace_dir,omitempty" json:"workspace_dir,omitempty"`
	Runtime      string            `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	Job          *SandboxJobConfig `yaml:"job,omitempty" json:"job,omitempty"`
	// Publish gates what a sandboxed run may publish; see SandboxPublishConfig.
	Publish *SandboxPublishConfig `yaml:"publish,omitempty" json:"publish,omitempty"`
}

// SandboxPublishConfig is the deterministic preflight hive runs before it
// pushes a sandboxed run's commits and opens a PR. The agent's own report is
// necessary but trusts the agent; these commands do not. Each entry is run
// with `sh -lc` in a fresh container/Job of the same image, workspace and
// env allowlist as the agent's run, with HIVE_CHANGED_FILES and
// HIVE_CHANGED_TESTS exported (newline-separated paths from the run's
// commits). A non-zero exit from any command withholds the push and the PR
// and puts the command's output tail on the agent card.
//
// This config is the only source of verify commands. A
// `<!-- hive:publish.verify … -->` block in a kick message is recognised and
// logged but never run: the rendered message carries issue and PR titles and
// other text the hive did not write.
type SandboxPublishConfig struct {
	Verify []string `yaml:"verify,omitempty" json:"verify,omitempty"`
	// VerifyTimeoutS bounds each verify command. Non-positive uses the
	// executor default (DefaultSandboxVerifyTimeoutS).
	VerifyTimeoutS int `yaml:"verify_timeout_s,omitempty" json:"verify_timeout_s,omitempty"`
}

// DefaultSandboxVerifyTimeoutS bounds one publish.verify command when no
// timeout is configured: long enough for a dependency-heavy collect-only or a
// single test file, short enough that a hung verify cannot pin the agent.
const DefaultSandboxVerifyTimeoutS = 900

// AgentSandboxOverride is the per-agent sandbox opt-in block.
type AgentSandboxOverride struct {
	Enabled      *bool             `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Image        string            `yaml:"image,omitempty" json:"image,omitempty"`
	EnvAllowlist []string          `yaml:"env_allowlist,omitempty" json:"env_allowlist,omitempty"`
	NetworkMode  string            `yaml:"network_mode,omitempty" json:"network_mode,omitempty"`
	TimeoutS     int               `yaml:"timeout_s,omitempty" json:"timeout_s,omitempty"`
	Runtime      string            `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	Job          *SandboxJobConfig `yaml:"job,omitempty" json:"job,omitempty"`
	// Publish, when set, replaces the global publish preflight for this
	// agent; an empty verify list here turns the preflight off for it.
	Publish *SandboxPublishConfig `yaml:"publish,omitempty" json:"publish,omitempty"`
}

// BobConfig configures the IBM bobshell ("bob") CLI backend. Only the
// key's LOCATION is stored — never the key itself.
type BobConfig struct {
	APIKeyEnv  string `yaml:"api_key_env" json:"api_key_env,omitempty"`   // env var NAME holding the key; default HIVE_BOB_API_KEY
	APIKeyFile string `yaml:"api_key_file" json:"api_key_file,omitempty"` // path to a file holding the key; default /secrets/bob_api_key
	// KeyName is an optional, human-chosen LABEL for the configured key
	// ("Team inference key", "andy personal", …). It is safe-to-show metadata,
	// NOT a secret — it records WHICH key a hive is set to use so managers can
	// tell keys apart without ever seeing the value (#3596/#3598). omitempty
	// keeps hive.yaml byte-identical on round-trip when unset; an absent name
	// is a normal, backwards-compatible state that the dashboard renders as
	// "(unnamed)" rather than an error.
	KeyName string `yaml:"key_name,omitempty" json:"key_name,omitempty"`
	// SessionPrefix is an optional global prefix for bob --instance-id, used
	// to make Bob/Bobalytics sessions distinguishable without renaming Hive
	// agents. Empty is default-off and preserves the exact launch command.
	SessionPrefix string `yaml:"session_prefix,omitempty" json:"session_prefix,omitempty"`
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

// InferenceAuthConfig holds optional /v1/models discovery auth for a
// self-hosted inference backend (vllm, llm-d). Plain vLLM/llm-d servers
// need no key, but the configured endpoint may actually be a LiteLLM
// gateway, which entitlement-filters /v1/models per API key and hides
// key-gated models from anonymous callers.
type InferenceAuthConfig struct {
	APIKeyHeader string `yaml:"api_key_header,omitempty" json:"api_key_header,omitempty"` // header NAME the key is sent in (default "Authorization")
	APIKeyEnv    string `yaml:"api_key_env" json:"api_key_env"`                           // env var NAME holding the key; never the key value
	APIKeyFile   string `yaml:"api_key_file" json:"api_key_file"`                         // path to a file holding the key
	Endpoint     string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`             // optional endpoint override for this backend
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
	// KeyName is an optional, human-chosen LABEL for this gateway's configured
	// key ("Team inference key", "andy personal", …). It is safe-to-show
	// metadata, NOT a secret — it records WHICH key a gateway is set to use so
	// managers can tell keys apart without ever seeing the value. Mirrors the
	// bob key's KeyName (#3596/#3598), now generalized to every gateway kind
	// (litellm / openrouter / watsonx / vllm). omitempty keeps hive.yaml on
	// existing gateways (which never recorded a name) byte-identical on
	// round-trip; an absent name is a normal, backwards-compatible state the
	// dashboard renders as "(unnamed)" rather than an error.
	KeyName string `yaml:"key_name,omitempty" json:"key_name,omitempty"`
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

// RotationConfig configures automatic provider failover (RFC #3958). When a
// provider's subscription or credit is exhausted, agents are rotated onto a
// different provider at the same capability tier.
type RotationConfig struct {
	// Enabled turns the rotation loop on. Default false — opt-in.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// ThresholdPct is the usage percentage at which a subscription provider
	// is considered exhausted (0–100). Default 85.
	ThresholdPct int `yaml:"threshold_pct,omitempty" json:"threshold_pct,omitempty"`
	// Providers maps provider name → its class config.
	Providers map[string]ProviderRotationConfig `yaml:"providers,omitempty" json:"providers,omitempty"`
	// HeadroomSource selects the provider headroom implementation.
	// "builtin" (default) keeps the in-tree probers; "ccleft" opts in to
	// github.com/tuna-os/ccleft-backed probers where available.
	HeadroomSource string `yaml:"headroom_source,omitempty" json:"headroom_source,omitempty"`
	// HighVolumeCadenceS: agents with a cadence at or below this value (seconds)
	// are high-volume and must NEVER rotate onto subscription providers.
	// Default 1800 (30 min). Protects weekly subscription budgets.
	HighVolumeCadenceS int `yaml:"high_volume_cadence_s,omitempty" json:"high_volume_cadence_s,omitempty"`
	// AgentTiers maps agent name → capability tier ("T1","T2","T3").
	// Rotation stays within tiers; drops only when nothing has headroom.
	AgentTiers map[string]string `yaml:"agents,omitempty" json:"agents,omitempty"`
}

// ProviderRotationConfig describes one provider in the rotation set.
type ProviderRotationConfig struct {
	// Class is "subscription" or "metered".
	Class string `yaml:"class" json:"class"`
	// Backends lists which hive backend names front this provider
	// (e.g. ["claude","pi"] for anthropic; ["litellm"] when litellm fronts deepseek).
	Backends []string `yaml:"backends,omitempty" json:"backends,omitempty"`
	// MonthlyAllowance states the plan's included request count per calendar
	// month for providers metered that way (github/Copilot premium requests,
	// #6980). Their documented usage endpoints report only consumption, never
	// the entitlement, so a remaining percentage exists only when the operator
	// states the allowance here. 0 (unset) leaves the reading an explicit
	// unknown rather than a guessed one.
	MonthlyAllowance int `yaml:"monthly_allowance,omitempty" json:"monthly_allowance,omitempty"`
}

// ProviderBudgetConfig tunes how long the hive keeps agent kicks suspended
// after the inference provider refuses on a spending limit (#4294).
//
// There is exactly one knob because there is exactly one judgement call: how
// much wasted spend to accept in exchange for noticing sooner that the
// provider's window has reset. The hive cannot observe the reset passively —
// the suppression that saves the money also withholds the inference calls that
// would reveal the money is available again — so it periodically lets one
// cycle's kicks through as a probe.
type ProviderBudgetConfig struct {
	// ProbeIntervalS is how long a spend rebuff suppresses kicks before one
	// cycle is allowed through to test whether the provider is serving again.
	// Default 1800 (30 min).
	//
	// Shorter probes recover faster after a reset but burn a run each time on a
	// provider that is still clipped; longer probes waste less and notice later.
	// 30 minutes costs at most ~48 rebuffed runs across a day-long clip — set
	// against the field report's entire cadence firing all day. The probe is
	// how hive stays agnostic to WHEN the provider resets: reset schedules are
	// not knowable (the field report's window rolled at the key's creation time
	// of day, not at midnight — #4294), so hive never predicts one; it just
	// retries cheaply until the provider serves again.
	ProbeIntervalS int `yaml:"probe_interval_s,omitempty" json:"probe_interval_s,omitempty"`
}
