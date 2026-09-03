package config

import (
	"fmt"
	"log"
	"strings"
	"time"
)

const (
	MaxAgentReplicas = 5

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

	// defaultAdvisoryMaxFindings is the digest's default top-N cap. Ten fits
	// in a screenful, which is the point: the digest is a "what should I look
	// at next" list, not an inventory.
	defaultAdvisoryMaxFindings = 10
	// defaultAdvisoryStalenessDays is how long an advisory bead survives
	// without being re-reported. Advisory agents re-scan far more often than
	// weekly, so a finding untouched for a week is one no agent still sees.
	defaultAdvisoryStalenessDays = 7
)

func (c *Config) applyDefaults() {
	// Repo targets are built as org + "/" + repo, so an entry that already
	// carries the org resolves to "org/org/repo" and every agent fails. Strip a
	// matching org prefix off both primary_repo and every repos entry on load.
	// primary_repo has been normalized here for a long time; repos was not,
	// which is why live configs are seen with a correct bare primary_repo next
	// to an org-qualified repos list. A mismatched org is deliberately left
	// alone so ValidateRepoTargets still rejects it rather than silently
	// retargeting the hive at a repository the owner never named.
	if c.Project.PrimaryRepo != "" && c.Project.Org != "" {
		c.Project.PrimaryRepo, _ = NormalizeRepoForOrg(c.Project.Org, c.Project.PrimaryRepo)
	}
	if len(c.Project.Repos) > 0 && c.Project.Org != "" {
		c.Project.Repos, _ = NormalizeProjectRepos(c.Project.Org, c.Project.Repos)
	}
	if c.Dashboard.Port == 0 {
		c.Dashboard.Port = defaultDashboardPort
	}
	if c.Dashboard.AgentPollIntervalS == 0 {
		c.Dashboard.AgentPollIntervalS = defaultAgentPollIntervalS
	}
	if normalized, err := ValidateSnapshotFrameAncestors(c.Dashboard.SnapshotFrameAncestors); err == nil {
		c.Dashboard.SnapshotFrameAncestors = normalized
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
	if c.Data.BobSessionsDir == "" {
		c.Data.BobSessionsDir = "/data/home/.bob"
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
		if agent.Replicas == 0 {
			agent.Replicas = 1
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

	defaultTierLimits := map[string]TierRate{
		"newcomer":    {MaxPerHour: 3, MaxPerDay: 10, MaxConcurrent: 1},
		"contributor": {MaxPerHour: 10, MaxPerDay: 50, MaxConcurrent: 2},
		"trusted":     {MaxPerHour: 30, MaxPerDay: 200, MaxConcurrent: 5},
		"merger":      {MaxPerHour: 30, MaxPerDay: 200, MaxConcurrent: 5},
		"advisor":     {MaxPerHour: 0, MaxPerDay: 0, MaxConcurrent: 0},
	}
	if c.Hub.TierLimits == nil {
		c.Hub.TierLimits = map[string]TierRate{}
	}
	for tier, limits := range defaultTierLimits {
		if _, ok := c.Hub.TierLimits[tier]; !ok {
			c.Hub.TierLimits[tier] = limits
		}
	}

	if len(c.Governor.Labels.Exempt) == 0 {
		c.Governor.Labels.Exempt = []string{
			"nightly-tests", "LFX", "meta-tracker",
			"auto-qa-tuning-report", "adopters",
			"changes-requested", "waiting-on-author",
		}
	}
	if strings.TrimSpace(c.Governor.Labels.AutoMerge) == "" {
		c.Governor.Labels.AutoMerge = DefaultAutoMergeLabel
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
	// #4041: hives that saved their config while the pre-#3959 defaults were
	// in effect have that generic list MATERIALIZED as explicit values in
	// their persisted config (Save() marshals the whole struct, defaults
	// included), and "defaults only apply to an empty list" meant the #3959
	// fix could never reach them — a live hosted hive's quality agent flapped
	// on `(?i)copilot auth` for days with restart_count 83. A list that is
	// byte-identical to the old default set expresses no operator intent, so
	// treat it as "default": drop it and let the corrected defaults apply. A
	// list that differs in ANY way is an operator's, and stays verbatim.
	if IsLegacyDefaultLoginPatterns(c.Governor.Sensing.LoginPatterns) {
		log.Printf("[config] migrating login_patterns: persisted list matches the pre-#3959 defaults verbatim — dropping it so the corrected defaults apply (customized lists are never touched)")
		c.Governor.Sensing.LoginPatterns = nil
	}
	if len(c.Governor.Sensing.LoginPatterns) == 0 {
		// Each pattern must match the CLI's own login CHROME, never ordinary
		// English. The login-detector matches these against the agent's PANE —
		// which contains the issue bodies, PR diffs and CI logs the agent is
		// READING — so generic phrases pause an agent for merely discussing an
		// auth error. The previous defaults ("authentication required",
		// "unauthorized.*401", "session expired", "login required", "please
		// log in", "token expired") did exactly that on a live hive: a
		// ci-maintainer reading CI logs full of 401s and a supervisor
		// summarising issues across 39 repos were paused repeatedly, and the
		// two HIGHEST-cadence agents are hit hardest because the detector runs
		// at kick time, so exposure scales with kick frequency. Reproduced on
		// demand by typing "authentication required" into a healthy agent's
		// input box (unsubmitted — just rendered on the pane) and kicking it.
		c.Governor.Sensing.LoginPatterns = append([]string(nil), defaultLoginPatterns...)
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
	if c.Governor.Advisory.MaxFindings == 0 {
		c.Governor.Advisory.MaxFindings = defaultAdvisoryMaxFindings
	}
	if c.Governor.Advisory.StalenessDays == 0 {
		c.Governor.Advisory.StalenessDays = defaultAdvisoryStalenessDays
	}
	if c.Governor.Advisory.PRAutoClose == nil {
		on := true
		c.Governor.Advisory.PRAutoClose = &on
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
		"telemetry": {
			Emoji: "📡", Color: "#00a8cc", Aliases: []string{"tm"},
			LaneKeywords:   []string{"observability", "opentelemetry", "prometheus", "grafana", "tracing", "metrics", "structured-logging", "servicemonitor", "podmonitor"},
			DetectKeywords: []string{"telemetry", "observability", "opentelemetry", "prometheus"},
			BeadRole:       "worker", SortOrder: 65, IncludeRepos: true,
		},
		"operations": {
			Emoji: "🚨", Color: "#d35400", Aliases: []string{"op"},
			LaneKeywords:   []string{"healthz", "readyz", "readiness", "slo-", "sli-", "service-level-objective", "service-level-indicator", "error-budget", "runbook", "incident-response", "rollback", "alerting"},
			DetectKeywords: []string{"operations", "operability", "healthz", "runbook"},
			BeadRole:       "worker", SortOrder: 66, IncludeRepos: true,
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
