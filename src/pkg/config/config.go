package config

import (
	"fmt"
	"sync"
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

var (
	saveObserverMu sync.Mutex
	saveObserver   func()
)

// SetSaveObserver registers a callback invoked immediately before a valid
// Config.Save begins writing files. The main process uses this to arm the
// config watcher with SkipNext for every programmatic save, so the fsnotify
// event from the primary config write does not reload a stale snapshot over
// the already-correct in-memory config. Passing nil clears the observer.
func SetSaveObserver(fn func()) {
	saveObserverMu.Lock()
	defer saveObserverMu.Unlock()
	saveObserver = fn
}

func notifySaveObserver() {
	saveObserverMu.Lock()
	fn := saveObserver
	saveObserverMu.Unlock()
	if fn != nil {
		fn()
	}
}

// dashboardAuthTokenFile is the mounted Secret key used by hosted spokes when
// the token is not injected as an env var. Tests redirect it to a hermetic path.
var dashboardAuthTokenFile = "/secrets/dashboard-token"

type Config struct {
	Project  ProjectConfig          `yaml:"project"`
	Policies PoliciesConfig         `yaml:"policies"`
	Agents   map[string]AgentConfig `yaml:"agents"`
	// AgentsGitHubAPIHourlyCap is the fleet default for per-agent GitHub API
	// reads through the proxy. It is encoded under agents.github_api_hourly_cap
	// by Config's YAML hooks so existing agent-name keys remain unchanged.
	AgentsGitHubAPIHourlyCap    int `yaml:"-" json:"-"`
	agentsGitHubAPIHourlyCapSet bool
	Governor                    GovernorConfig      `yaml:"governor"`
	GitHub                      GitHubConfig        `yaml:"github"`
	GitLab                      GitLabConfig        `yaml:"gitlab,omitempty"`
	Gitea                       GiteaConfig         `yaml:"gitea,omitempty"`
	Notifications               NotificationsConfig `yaml:"notifications"`
	Dashboard                   DashboardConfig     `yaml:"dashboard"`
	Data                        DataConfig          `yaml:"data"`
	Deployment                  DeploymentConfig    `yaml:"deployment,omitempty" json:"deployment,omitempty"`
	Knowledge                   KnowledgeConfig     `yaml:"knowledge"`
	Hub                         HubConfig           `yaml:"hub"`
	Fleet                       FleetConfig         `yaml:"fleet,omitempty" json:"fleet,omitempty"`
	// Issues tunes issue lifecycle automation. Zero value keeps the safe
	// defaults: close issues after their fix PR merges, and backfill hourly.
	Issues                     IssuesConfig        `yaml:"issues,omitempty" json:"issues,omitempty"`
	Contribute                  ContributeConfig    `yaml:"contribute,omitempty" json:"contribute,omitempty"`
	HiveID                      string              `yaml:"hive_id"`
	ACMMLevel                   *int                `yaml:"acmm_level,omitempty" json:"acmm_level"`
	// ModelRoles is the named model-roles map (#9722): a small set of names,
	// each naming a backend, a model and a reasoning effort, referenced as
	// "@<role>" wherever hive asks for a model (today: agents' model and the
	// advisor block). Default empty → nothing changes.
	ModelRoles map[string]ModelRole `yaml:"model_roles,omitempty" json:"model_roles,omitempty"`
	// Advisor is the fleet-wide advisor lane block (#9722): a turn-synchronous
	// second model that reviews each finished turn of an advised agent and can
	// object before the agent's next step. Opt-in; per-agent overrides live on
	// AgentConfig.Advisor.
	Advisor   AdvisorConfig   `yaml:"advisor,omitempty" json:"advisor,omitempty"`
	Variables VariablesConfig `yaml:"variables,omitempty"`
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
	Persona      PersonaConfig      `yaml:"persona,omitempty" json:"persona,omitempty"`
	Intent       IntentConfig       `yaml:"intent,omitempty" json:"intent,omitempty"`
	// Sentinel flags PRs that look like security overrides, privilege
	// escalation or codebase damage, from any author. Default on; see
	// SentinelConfig.
	Sentinel   SentinelConfig   `yaml:"sentinel,omitempty" json:"sentinel,omitempty"`
	Escalation EscalationConfig `yaml:"escalation,omitempty" json:"escalation,omitempty"`
	Evidence   EvidenceConfig   `yaml:"evidence,omitempty" json:"evidence,omitempty"`
	// Jev configures the shared Jev (TypeSafe AI typed-decision model) client
	// that agents with jev_mode: assist reach through the hive's local decision
	// endpoint (hivecommons/hive#8939). Zero value: OpenRouter-hosted
	// typesafe/jev-1.13, key from JEV_API_KEY or the connected OpenRouter
	// gateway. No agent has the tool unless its own jev_mode says so.
	Jev JevConfig `yaml:"jev,omitempty" json:"jev,omitempty"`
	// Runs tunes long-running runs and the opt-in Spektacular stage runner
	// (hivecommons/hive#8303). Zero value: runner off, default retry budget.
	Runs       RunsConfig       `yaml:"runs,omitempty" json:"runs,omitempty"`
	Retro      RetroConfig      `yaml:"retro,omitempty" json:"retro,omitempty"`
	Swarm      SwarmConfig      `yaml:"swarm,omitempty" json:"swarm,omitempty"`
	Autonomy   AutonomyConfig   `yaml:"autonomy,omitempty" json:"autonomy,omitempty"`
	Review     ReviewConfig     `yaml:"review,omitempty" json:"review,omitempty"`
	Compliance ComplianceConfig `yaml:"compliance,omitempty" json:"compliance,omitempty"`
	AutoMerge  AutoMergeConfig  `yaml:"auto_merge,omitempty" json:"auto_merge,omitempty"`
	// DuplicateSweep gates the cross-PR duplicate suggestion pass
	// (hivecommons/hive#7469 capability B). Default off → zero behaviour
	// change and no GitHub traffic.
	DuplicateSweep DuplicateSweepConfig `yaml:"duplicate_sweep,omitempty" json:"duplicate_sweep,omitempty"`
	// UpstreamWatch declares per-repo upstreams and filters for the fork
	// upstream watch (hivecommons/hive#9966). Default off.
	UpstreamWatch UpstreamWatchConfig `yaml:"upstream_watch,omitempty" json:"upstream_watch,omitempty"`
	// AgentSandbox configures the phase-1 credential-free sandbox runner. It is
	// disabled by default and agents must opt in individually.
	AgentSandbox AgentSandboxConfig `yaml:"agent_sandbox,omitempty" json:"agent_sandbox,omitempty"`
	// Convergence toggles the convergence-driven admission surfaces
	// (kubestellar/hive#3845 follow-ons). Default off → zero behaviour change.
	Convergence ConvergenceConfig `yaml:"convergence,omitempty" json:"convergence,omitempty"`
	// WriteSurface configures the audited GitHub write surface
	// (hivecommons/hive#9587): per-lane allowlists of the relay operations an
	// agent may ask the hive to perform, and the lanes whose direct GitHub
	// writes the proxy refuses (enforce, #9772). Default empty -> every agent
	// keeps every operation and every direct write path it has today.
	WriteSurface WriteSurfaceConfig `yaml:"write_surface,omitempty" json:"write_surface,omitempty"`
	// Publication is the audit campaign's authorized issue publisher opt-in
	// (hivecommons/hive#8353). Default off → nothing is ever filed.
	Publication PublicationConfig `yaml:"publication,omitempty" json:"publication,omitempty"`
	// ReleaseSentinel is the opt-in bounded repair loop for failed release CI
	// (hivecommons/hive#9585). Default off → nothing is watched or dispatched.
	ReleaseSentinel ReleaseSentinelConfig `yaml:"release_sentinel,omitempty" json:"release_sentinel,omitempty"`
	// Classification mirrors the Go-consumed subset of hive-project.yaml's
	// `classification:` block (currently review_bots, hivecommons/hive#7360).
	// Default empty → the review-thread reconciler is off.
	Classification ClassificationConfig `yaml:"classification,omitempty" json:"classification,omitempty"`
	// Turn gates the re-entrant conversation-as-state rollout (#5799). Default
	// off leaves every agent on the legacy tmux loop until an operator opts in.
	Turn TurnConfig `yaml:"turn,omitempty" json:"turn,omitempty"`
	// TaskMCP configures the task-scoped MCP endpoint. Remote contributor
	// exposure is opt-in; with remote_enabled false the endpoint remains usable
	// only through the existing dashboard/contribute auth path.
	TaskMCP TaskMCPConfig `yaml:"task_mcp,omitempty" json:"task_mcp,omitempty"`

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

type DataConfig struct {
	MetricsDir         string `yaml:"metrics_dir"`
	LogsDir            string `yaml:"logs_dir"`
	ClaudeSessionsDir  string `yaml:"claude_sessions_dir"`
	CopilotSessionsDir string `yaml:"copilot_sessions_dir"`
	BobSessionsDir     string `yaml:"bob_sessions_dir"`
	AgentsDir          string `yaml:"agents_dir"`

	// SessionRetentionDays is how many days of CLI session-state directories to
	// keep. Session directories are created per CLI invocation and were never
	// deleted, so they grow without bound on the shared /data PVC.
	//
	// Pointer so an explicit 0 (disable pruning) is distinguishable from the
	// key being absent (apply the default).
	SessionRetentionDays *int `yaml:"session_retention_days,omitempty"`
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

// MarshalYAML persists only declared agents. Runtime-derived replicas are
// re-created by ExpandAgentReplicas on the next load so they never collide with
// their base agent's replicas setting after a save.
func (c Config) MarshalYAML() (interface{}, error) {
	type plain Config
	out := plain(c)
	if c.Agents != nil {
		out.Agents = make(map[string]AgentConfig, len(c.Agents))
		for name, agent := range c.Agents {
			if agent.ReplicaOf != "" {
				continue
			}
			agent.ReplicaIndex = 0
			agent.ReplicaCount = 0
			out.Agents[name] = agent
		}
	}
	return marshalConfigYAMLWithAgentCap(c, out)
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
