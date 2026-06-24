package config

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Project       ProjectConfig                `yaml:"project"`
	Policies      PoliciesConfig               `yaml:"policies"`
	Agents        map[string]AgentConfig        `yaml:"agents"`
	Governor      GovernorConfig               `yaml:"governor"`
	GitHub        GitHubConfig                 `yaml:"github"`
	Notifications NotificationsConfig          `yaml:"notifications"`
	Dashboard     DashboardConfig              `yaml:"dashboard"`
	Data          DataConfig                   `yaml:"data"`
	Knowledge     KnowledgeConfig              `yaml:"knowledge"`
	Hub           HubConfig                    `yaml:"hub"`
	HiveID        string                       `yaml:"hive_id"`
	ACMMLevel     *int                         `yaml:"acmm_level,omitempty" json:"acmm_level"`

	SourcePath string `yaml:"-" json:"-"`
}

type KnowledgeConfig struct {
	Enabled    bool                `yaml:"enabled"`
	Engine     string              `yaml:"engine"`
	Layers     []KnowledgeLayer    `yaml:"layers"`
	Vaults     []VaultConfig       `yaml:"vaults"`
	GitSources []GitSourceConfigYAML `yaml:"git_sources"`
	Curator         KnowledgeCurator         `yaml:"curator"`
	Primer          KnowledgePrimer          `yaml:"primer"`
	BeadSynthesizer BeadSynthesizerConfig    `yaml:"bead_synthesizer"`
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
}

type AgentConfig struct {
	ID              string `yaml:"id" json:"id,omitempty"`
	Backend         string `yaml:"backend" json:"backend,omitempty"`
	Model           string `yaml:"model" json:"model,omitempty"`
	BeadsDir        string `yaml:"beads_dir" json:"beads_dir,omitempty"`
	Enabled         bool   `yaml:"enabled" json:"enabled,omitempty"`
	ClearOnKick     bool   `yaml:"clear_on_kick" json:"clear_on_kick"`
	CLIPinned       bool   `yaml:"cli_pinned" json:"cli_pinned,omitempty"`
	StaleTimeout    int    `yaml:"stale_timeout" json:"stale_timeout,omitempty"`
	RestartStrategy string `yaml:"restart_strategy" json:"restart_strategy,omitempty"`
	LaunchCmd       string `yaml:"launch_cmd" json:"launch_cmd,omitempty"`
	DisplayName     string `yaml:"display_name" json:"display_name,omitempty"`
	Description     string `yaml:"description" json:"description,omitempty"`

	// Phase 2: config-driven agent behavior fields
	Role             string            `yaml:"role" json:"role,omitempty"`
	SortOrder        int               `yaml:"sort_order" json:"sort_order,omitempty"`
	Emoji            string            `yaml:"emoji" json:"emoji,omitempty"`
	Color            string            `yaml:"color" json:"color,omitempty"`
	Aliases          []string          `yaml:"aliases" json:"aliases,omitempty"`
	LaneKeywords     []string          `yaml:"lane_keywords" json:"lane_keywords,omitempty"`
	DetectKeywords   []string          `yaml:"detect_keywords" json:"detect_keywords,omitempty"`
	KickTemplate     string            `yaml:"kick_template" json:"kick_template,omitempty"`
	IncludeRepos     *bool             `yaml:"include_repos" json:"include_repos,omitempty"`
	MetricsCollector string            `yaml:"metrics_collector" json:"metrics_collector,omitempty"`
	BeadRole         string            `yaml:"bead_role" json:"bead_role,omitempty"`
	StatsDisplay     []StatsDisplayEntry `yaml:"stats_display" json:"stats_display,omitempty"`
	ACMMLevels       []int             `yaml:"acmm_levels" json:"acmm_levels,omitempty"`
	Mode             string            `yaml:"mode" json:"mode,omitempty"`
	OnDemand         bool              `yaml:"on_demand" json:"on_demand,omitempty"`

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
}

type LoggingConfig struct {
	Dir        string `yaml:"dir"`
	MaxSizeMB  int    `yaml:"max_size_mb"`
	MaxAgeDays int    `yaml:"max_age_days"`
	MaxBackups int    `yaml:"max_backups"`
	Compress   bool   `yaml:"compress"`
	Level      string `yaml:"level"`
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
	AppID                int64  `yaml:"app_id"`
	InstallationID       int64  `yaml:"installation_id"`
	DocsInstallationID   int64  `yaml:"docs_installation_id"`
	KeyFile              string `yaml:"key_file"`
	Token                string `yaml:"token"`
	OAuthClientID        string `yaml:"oauth_client_id"`
	// AppSlug is the GitHub App URL slug for the install link.
	// For public GitHub: "kubestellar-hive". For GHE: your app's slug.
	AppSlug string `yaml:"app_slug"`
	// APIURL is the GitHub API base URL. Defaults to DefaultGitHubAPIURL.
	// For GitHub Enterprise, set to e.g. "https://github.ibm.com/api/v3".
	APIURL string `yaml:"api_url"`
	// BaseURL is the GitHub web base URL. Defaults to DefaultGitHubBaseURL.
	// For GitHub Enterprise, set to e.g. "https://github.ibm.com".
	BaseURL string `yaml:"base_url"`
}

const (
	// DefaultGitHubAPIURL is the default GitHub API endpoint (public github.com).
	DefaultGitHubAPIURL = "https://api.github.com"
	// DefaultGitHubBaseURL is the default GitHub web URL (public github.com).
	DefaultGitHubBaseURL = "https://github.com"
	// DefaultGitHubAppSlug is the public Hive GitHub App slug.
	DefaultGitHubAppSlug = "kubestellar-hive"
)

// ResolvedAPIURL returns the configured API URL or the default for github.com.
func (g GitHubConfig) ResolvedAPIURL() string {
	if g.APIURL != "" {
		return g.APIURL
	}
	return DefaultGitHubAPIURL
}

// ResolvedBaseURL returns the configured base URL or the default for github.com.
func (g GitHubConfig) ResolvedBaseURL() string {
	if g.BaseURL != "" {
		return g.BaseURL
	}
	return DefaultGitHubBaseURL
}

// IsGHE returns true if the configured base URL points to a GitHub Enterprise instance.
func (g GitHubConfig) IsGHE() bool {
	return g.BaseURL != "" && g.BaseURL != DefaultGitHubBaseURL
}

// ResolvedAppSlug returns the configured app slug or the default public Hive app slug.
func (g GitHubConfig) ResolvedAppSlug() string {
	if g.AppSlug != "" {
		return g.AppSlug
	}
	return DefaultGitHubAppSlug
}

// AppInstallURL returns the full URL to install the GitHub App.
// For GHE: {base_url}/github-apps/{slug}/installations/new
// For github.com: https://github.com/apps/{slug}/installations/new
func (g GitHubConfig) AppInstallURL() string {
	base := strings.TrimRight(g.ResolvedBaseURL(), "/")
	slug := g.ResolvedAppSlug()
	if g.IsGHE() {
		return base + "/github-apps/" + slug + "/installations/new"
	}
	return base + "/apps/" + slug + "/installations/new"
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
	Enabled                bool     `yaml:"enabled"`
	URL                    string   `yaml:"url"`
	IsPublic               bool     `yaml:"is_public"`
	SnapshotURL            string   `yaml:"snapshot_url"`
	DashboardURL           string   `yaml:"dashboard_url"`
	HiveType               string   `yaml:"hive_type"`
	ClusterID              string   `yaml:"cluster_id"`
	AutoSnapshot           bool     `yaml:"auto_snapshot"`
	AutoUpgrade            bool     `yaml:"auto_upgrade"`
	ContributeSuspended    bool     `yaml:"contribute_suspended"`
	ContributeAllowLabels  []string `yaml:"contribute_allow_labels"`
	ContributeDenyLabels   []string `yaml:"contribute_deny_labels"`
	ContributeDenyTitles   []string `yaml:"contribute_deny_titles"`
	ContributeDenyAuthors         []string `yaml:"contribute_deny_authors"`
	ContributeAllowModels         []string `yaml:"contribute_allow_models"`
	ContributeRejectUnknownModels bool     `yaml:"contribute_reject_unknown_models"`
	DisabledRepos                 []string            `yaml:"disabled_repos"`
	DisabledTiers          []string            `yaml:"disabled_tiers"`
	TierLimits             map[string]TierRate `yaml:"tier_limits"`
	SnapshotIntervalMin    int                 `yaml:"snapshot_interval_min"`
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
}

type DataConfig struct {
	MetricsDir          string `yaml:"metrics_dir"`
	LogsDir             string `yaml:"logs_dir"`
	ClaudeSessionsDir   string `yaml:"claude_sessions_dir"`
	CopilotSessionsDir  string `yaml:"copilot_sessions_dir"`
	AgentsDir           string `yaml:"agents_dir"`
}

var envVarPattern = regexp.MustCompile(`\$\{([^}]+)\}`)

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
}

func expandEnvVars(s string) string {
	return envVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		varName := strings.TrimSuffix(strings.TrimPrefix(match, "${"), "}")
		if val, ok := os.LookupEnv(varName); ok {
			return val
		}
		return match
	})
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
			BeadRole: "worker", SortOrder: 20, IncludeRepos: true,
		},
		"ci-maintainer": {
			Emoji: "🔧", Color: "#2ecc71", Aliases: []string{"ci"},
			LaneKeywords:   []string{"workflow-failure", "ci-failure", "nightly", "coverage", "regression", "ga4", "analytics"},
			DetectKeywords: []string{"ci-maintainer", "review", "ci", "coverage", "ga4"},
			BeadRole: "worker", SortOrder: 30, IncludeRepos: true,
		},
		"architect": {
			Emoji: "🏗", Color: "#9b59b6", Aliases: []string{"ar"},
			LaneKeywords:   []string{"rfc", "architecture", "refactor", "redesign", "migration", "breaking change", "protocol", "api design"},
			DetectKeywords: []string{"architect", "rfc", "refactor"},
			BeadRole: "worker", SortOrder: 40, IncludeRepos: true,
		},
		"outreach": {
			Emoji: "🌐", Color: "#e67e22", Aliases: []string{"ou"},
			LaneKeywords:   []string{"adopters", "outreach", "community", "engagement"},
			DetectKeywords: []string{"outreach", "adopters", "community"},
			BeadRole: "worker", SortOrder: 50, IncludeRepos: false,
		},
		"supervisor": {
			Emoji: "👑", Color: "#e74c3c", Aliases: []string{"su"},
			DetectKeywords: []string{"supervisor", "sweep", "monitor"},
			BeadRole: "supervisor", SortOrder: 10, IncludeRepos: true,
		},
		"sec-check": {
			Emoji: "🛡", Color: "#1abc9c", Aliases: []string{"se"},
			DetectKeywords: []string{"security", "sec-check", "vulnerability"},
			BeadRole: "worker", SortOrder: 60, IncludeRepos: true,
		},
		"quality": {
			Emoji: "🧪", Color: "#3498db", Aliases: []string{"te", "qa"},
			LaneKeywords:   []string{"test-gap", "test-strategy", "test-coverage", "test-scaffold", "untested", "missing-tests"},
			DetectKeywords: []string{"quality", "test", "coverage"},
			BeadRole: "worker", SortOrder: 35, IncludeRepos: true,
		},
		"strategist": {
			Emoji: "🧠", Color: "#f39c12", Aliases: []string{"sg"},
			DetectKeywords: []string{"strategist", "strategy"},
			BeadRole: "worker", SortOrder: 70, IncludeRepos: true,
		},
		"guide": {
			Emoji: "📖", Color: "#8e44ad", Aliases: []string{"gu"},
			LaneKeywords:   []string{"docs", "documentation", "readme", "guide", "tutorial", "onboarding"},
			DetectKeywords: []string{"guide", "docs", "documentation"},
			BeadRole: "worker", SortOrder: 45, IncludeRepos: true,
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

func (c *Config) validate() error {
	if c.Project.Org == "" {
		return fmt.Errorf("project.org is required")
	}
	// Repos can be empty — L1 inception starts with just an idea, no repo.
	if len(c.Agents) == 0 {
		return fmt.Errorf("at least one agent must be configured")
	}
	if c.GitHub.Token == "" && c.GitHub.AppID == 0 {
		return fmt.Errorf("github.token or github.app_id is required")
	}
	for name, agent := range c.Agents {
		validBackends := map[string]bool{"claude": true, "copilot": true, "goose": true, "codex": true, "pi": true, "bob": true, "aider": true}
		if agent.Backend != "" && !validBackends[agent.Backend] {
			return fmt.Errorf("agent %s: invalid backend %q (must be claude, copilot, goose, codex, pi, bob, or aider)", name, agent.Backend)
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
	if len(c.Agents) == 0 {
		log.Printf("WARNING: config.Save() blocked — no agents configured, would corrupt hive.yaml")
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
		// File may not exist yet — fall back to create.
		if writeErr := os.WriteFile(c.SourcePath, data, 0o644); writeErr != nil {
			return fmt.Errorf("writing config (create fallback): %w", writeErr)
		}
		return nil
	}

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

	// Write a rolling backup to the PVC. This is NOT the primary config —
	// it exists for disaster recovery (e.g. ConfigMap deleted in K8s, or
	// Watchtower wiping a bind mount in Docker). The entrypoint determines
	// which source is authoritative based on the runtime environment.
	backupPath := "/data/hive.yaml.bak"
	if err := os.WriteFile(backupPath, data, 0o644); err != nil {
		log.Printf("[config] warning: failed to write PVC backup to %s: %v", backupPath, err)
	} else {
		log.Printf("[config] PVC backup written to %s (recovery copy, not primary config)", backupPath)
	}
	return nil
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
