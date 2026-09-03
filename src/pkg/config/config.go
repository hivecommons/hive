package config

import "sync"

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
	GitLab        GitLabConfig           `yaml:"gitlab,omitempty"`
	Gitea         GiteaConfig            `yaml:"gitea,omitempty"`
	Notifications NotificationsConfig    `yaml:"notifications"`
	Dashboard     DashboardConfig        `yaml:"dashboard"`
	Data          DataConfig             `yaml:"data"`
	Knowledge     KnowledgeConfig        `yaml:"knowledge"`
	Hub           HubConfig              `yaml:"hub"`
	HiveID        string                 `yaml:"hive_id"`
	ACMMLevel     *int                   `yaml:"acmm_level,omitempty" json:"acmm_level"`
	Variables     VariablesConfig        `yaml:"variables,omitempty"`
	// OTel configures standards-based OTLP trace export. It is the preferred
	// operator-facing block; Tracing is retained as a legacy alias.
	OTel    OTelConfig `yaml:"otel,omitempty" json:"otel,omitempty"`
	Tracing OTelConfig `yaml:"tracing,omitempty"`
	// Triggers is an additive list of CEL-based declarative agent triggers.
	// Default empty → existing label/governor triggering is unchanged.
	Triggers []TriggerRule `yaml:"triggers,omitempty" json:"triggers,omitempty"`
	// Hooks is an additive list of operator-declared state-triggered hooks
	// (RFC #4001): `on: <transition>` → `action: <vetted action>`. Default
	// empty → no hooks fire and behavior is byte-identical to before.
	//
	// This list is OPERATOR-ONLY by construction: it is config, so writing it
	// requires the same authz and carries the same layer provenance as any
	// other config write, and there is deliberately no runtime registration
	// API. Nothing agent-writable can reach it — an agent able to register
	// hooks on its own transitions would have an escalation path.
	Hooks []HookRule `yaml:"hooks,omitempty" json:"hooks,omitempty"`
	// ToolApproval configures the approval desk (RFC #4000): the single
	// decision point every approval-shaped request resolves through, plus the
	// operator rules that steer it. Additive and DEFAULT-OFF — an absent block
	// leaves every existing gate in charge and behavior byte-identical.
	ToolApproval ToolApprovalConfig `yaml:"tool_approval,omitempty" json:"tool_approval,omitempty"`
	Mint         MintConfig         `yaml:"mint,omitempty"`
	Ioscan       IoscanConfig       `yaml:"ioscan,omitempty" json:"ioscan,omitempty"`
	Classifier   ClassifierConfig   `yaml:"classifier,omitempty" json:"classifier,omitempty"`
	Planning     PlanningConfig     `yaml:"planning,omitempty" json:"planning,omitempty"`
	Quality      QualityConfig      `yaml:"quality,omitempty" json:"quality,omitempty"`
	Intent       IntentConfig       `yaml:"intent,omitempty" json:"intent,omitempty"`
	Escalation   EscalationConfig   `yaml:"escalation,omitempty" json:"escalation,omitempty"`
	Retro        RetroConfig        `yaml:"retro,omitempty" json:"retro,omitempty"`
	Review       ReviewConfig       `yaml:"review,omitempty" json:"review,omitempty"`
	AutoMerge    AutoMergeConfig    `yaml:"auto_merge,omitempty" json:"auto_merge,omitempty"`
	// AgentSandbox configures the phase-1 credential-free sandbox runner. It is
	// disabled by default and agents must opt in individually.
	AgentSandbox AgentSandboxConfig `yaml:"agent_sandbox,omitempty" json:"agent_sandbox,omitempty"`
	// Convergence toggles the convergence-driven admission surfaces
	// (kubestellar/hive#3845 follow-ons). Default off → zero behaviour change.
	Convergence ConvergenceConfig `yaml:"convergence,omitempty" json:"convergence,omitempty"`
	// Turn gates the re-entrant conversation-as-state rollout (#5799). Default
	// off leaves every agent on the legacy tmux loop until an operator opts in.
	Turn TurnConfig `yaml:"turn,omitempty" json:"turn,omitempty"`

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

// DefaultOTelServiceName is the OTLP resource service.name used when the
// operator does not set otel.service_name.
// Load reads hive.yaml, then applies config.env overrides if present.
// Precedence: hive.yaml < config.env < explicit env vars (via ${} interpolation).
// findConfigEnv returns the path to a config.env file, or "" if none found.

// their base agent's replicas setting after a save.
// production always uses the fixed in-cluster path.
// this atomic write prevents.
