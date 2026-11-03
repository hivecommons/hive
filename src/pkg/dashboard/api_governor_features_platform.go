package dashboard

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/writeback"
)

// featuresPlatformBody is the part of the Features PUT that covers the small
// platform blocks data.*, deployment.*, evidence.*, gitea.*, gitlab.*, mint.*,
// supersession_sweep.*, compliance.posture_checks.*, planning.*,
// swarm.* and tool_approval.inbox_path. Every field is a pointer so an absent
// key leaves the config untouched.
//
// Three paths are deliberately read-only (returned by GET, never accepted on
// PUT): deployment.upgrade_helper names an executable the dashboard runs, and
// mint.key_path / evidence.signing_key_file name signing keys provisioned on
// disk. Only the paths are returned, never file contents.
type featuresPlatformBody struct {
	DataAgentsDir            *string `json:"dataAgentsDir"`
	DataBobSessionsDir       *string `json:"dataBobSessionsDir"`
	DataClaudeSessionsDir    *string `json:"dataClaudeSessionsDir"`
	DataCopilotSessionsDir   *string `json:"dataCopilotSessionsDir"`
	DataLogsDir              *string `json:"dataLogsDir"`
	DataMetricsDir           *string `json:"dataMetricsDir"`
	DataSessionRetentionDays *string `json:"dataSessionRetentionDays"`

	DeploymentPodmanMode        *string `json:"deploymentPodmanMode"`
	DeploymentUpgradeRequestDir *string `json:"deploymentUpgradeRequestDir"`

	EvidenceEnabled *bool `json:"evidenceEnabled"`

	GiteaURL       *string `json:"giteaUrl"`
	GiteaTokenEnv  *string `json:"giteaTokenEnv"`
	GitLabURL      *string `json:"gitlabUrl"`
	GitLabTokenEnv *string `json:"gitlabTokenEnv"`

	MintMaxTTLSeconds *int `json:"mintMaxTtlSeconds"`

	SupersessionGracePeriod *string `json:"supersessionGracePeriod"`

	CompliancePostureWindowDays  *int `json:"compliancePostureWindowDays"`
	CompliancePostureHistoryDays *int `json:"compliancePostureHistoryDays"`

	PlanningDesignApprovedLabel  *string `json:"planningDesignApprovedLabel"`
	PlanningMaxConcurrentDesigns *int    `json:"planningMaxConcurrentDesigns"`
	PlanningMaxDesignRevisions   *int    `json:"planningMaxDesignRevisions"`

	SwarmEventName        *string `json:"swarmEventName"`
	SwarmCallToArms       *string `json:"swarmCallToArms"`
	SwarmLeaderboardTitle *string `json:"swarmLeaderboardTitle"`

	ToolApprovalInboxPath *string `json:"toolApprovalInboxPath"`

	// Write-back containment (writeback.Config): the three unsolicited
	// knobs, the per-repo mode map (repo → mode, "" clears) and per-kind
	// overrides (kind → {min_level, require_label}; null clears).
	WritebackMinLevel      *int                                     `json:"writebackMinLevel"`
	WritebackRequireLabels *bool                                    `json:"writebackRequireLabels"`
	WritebackBackfill      *bool                                    `json:"writebackBackfill"`
	WritebackPerRepo       map[string]string                        `json:"writebackPerRepo"`
	WritebackKinds         map[string]*writeback.KindPolicyOverride `json:"writebackKinds"`
}

// maxSwarmTextLen bounds the swarm banner strings shown on the dashboard.
const maxSwarmTextLen = 200

// settingsAbsPath validates an optional absolute path: empty is allowed only
// when allowEmpty is set, otherwise the path must be absolute and clean.
func settingsAbsPath(name, raw string, allowEmpty bool) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		if allowEmpty {
			return "", nil
		}
		return "", fmt.Errorf("%s must be a clean absolute path", name)
	}
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		if allowEmpty {
			return "", fmt.Errorf("%s must be empty (default) or a clean absolute path", name)
		}
		return "", fmt.Errorf("%s must be a clean absolute path", name)
	}
	return p, nil
}

// settingsForgeURL validates an optional forge instance root (http or https
// with a host). Empty means the built-in default.
func settingsForgeURL(name, raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("%s must be empty or an http(s) URL such as https://gitlab.example.com", name)
	}
	return strings.TrimRight(v, "/"), nil
}

// prepare validates every field the request carried and returns the mutation
// to apply. Nothing is changed when an error is returned.
func (b featuresPlatformBody) prepare() (func(*config.Config), error) {
	var steps []func(*config.Config)

	for _, f := range []struct {
		raw  *string
		name string
		dst  func(*config.Config) *string
	}{
		{b.DataAgentsDir, "data agents_dir", func(c *config.Config) *string { return &c.Data.AgentsDir }},
		{b.DataBobSessionsDir, "data bob_sessions_dir", func(c *config.Config) *string { return &c.Data.BobSessionsDir }},
		{b.DataClaudeSessionsDir, "data claude_sessions_dir", func(c *config.Config) *string { return &c.Data.ClaudeSessionsDir }},
		{b.DataCopilotSessionsDir, "data copilot_sessions_dir", func(c *config.Config) *string { return &c.Data.CopilotSessionsDir }},
		{b.DataLogsDir, "data logs_dir", func(c *config.Config) *string { return &c.Data.LogsDir }},
		{b.DataMetricsDir, "data metrics_dir", func(c *config.Config) *string { return &c.Data.MetricsDir }},
	} {
		if f.raw == nil {
			continue
		}
		p, err := settingsAbsPath(f.name, *f.raw, false)
		if err != nil {
			return nil, err
		}
		dst := f.dst
		steps = append(steps, func(c *config.Config) { *dst(c) = p })
	}
	if b.DataSessionRetentionDays != nil {
		raw := strings.TrimSpace(*b.DataSessionRetentionDays)
		if raw == "" {
			steps = append(steps, func(c *config.Config) { c.Data.SessionRetentionDays = nil })
		} else {
			days, err := strconv.Atoi(raw)
			if err != nil || days < 0 {
				return nil, fmt.Errorf("data session_retention_days must be empty (default), 0 (never prune) or a positive number of days")
			}
			steps = append(steps, func(c *config.Config) { c.Data.SessionRetentionDays = &days })
		}
	}

	for _, f := range []struct {
		raw  *string
		name string
		dst  func(*config.Config) *string
	}{
		{b.DeploymentUpgradeRequestDir, "deployment upgrade_request_dir", func(c *config.Config) *string { return &c.Deployment.UpgradeRequestDir }},
		{b.ToolApprovalInboxPath, "tool_approval inbox_path", func(c *config.Config) *string { return &c.ToolApproval.InboxPath }},
	} {
		if f.raw == nil {
			continue
		}
		p, err := settingsAbsPath(f.name, *f.raw, true)
		if err != nil {
			return nil, err
		}
		dst := f.dst
		steps = append(steps, func(c *config.Config) { *dst(c) = p })
	}
	if b.DeploymentPodmanMode != nil {
		mode := strings.ToLower(strings.TrimSpace(*b.DeploymentPodmanMode))
		if mode != "" && mode != podmanModeRootless && mode != podmanModeRootful {
			return nil, fmt.Errorf("deployment podman_mode must be empty, %s or %s", podmanModeRootless, podmanModeRootful)
		}
		steps = append(steps, func(c *config.Config) { c.Deployment.PodmanMode = mode })
	}

	if b.EvidenceEnabled != nil {
		v := *b.EvidenceEnabled
		steps = append(steps, func(c *config.Config) { c.Evidence.Enabled = &v })
	}

	for _, f := range []struct {
		raw  *string
		name string
		dst  func(*config.Config) *string
	}{
		{b.GiteaURL, "gitea gitea_url", func(c *config.Config) *string { return &c.Gitea.URL }},
		{b.GitLabURL, "gitlab gitlab_url", func(c *config.Config) *string { return &c.GitLab.URL }},
	} {
		if f.raw == nil {
			continue
		}
		v, err := settingsForgeURL(f.name, *f.raw)
		if err != nil {
			return nil, err
		}
		dst := f.dst
		steps = append(steps, func(c *config.Config) { *dst(c) = v })
	}
	for _, f := range []struct {
		raw  *string
		name string
		dst  func(*config.Config) *string
	}{
		{b.GiteaTokenEnv, "gitea token_env", func(c *config.Config) *string { return &c.Gitea.TokenEnv }},
		{b.GitLabTokenEnv, "gitlab token_env", func(c *config.Config) *string { return &c.GitLab.TokenEnv }},
	} {
		if f.raw == nil {
			continue
		}
		name := strings.TrimSpace(*f.raw)
		if name != "" && !envVarNamePattern.MatchString(name) {
			return nil, fmt.Errorf("%s must be an environment variable name (letters, digits, underscore), not a token value", f.name)
		}
		dst := f.dst
		steps = append(steps, func(c *config.Config) { *dst(c) = name })
	}

	if b.MintMaxTTLSeconds != nil {
		v := *b.MintMaxTTLSeconds
		if v < 0 || v > config.MintHardCapTTLSeconds {
			return nil, fmt.Errorf("mint max_ttl_seconds must be 0 (default) to %d", config.MintHardCapTTLSeconds)
		}
		steps = append(steps, func(c *config.Config) { c.Mint.MaxTTLSeconds = v })
	}

	if b.WritebackMinLevel != nil {
		v := *b.WritebackMinLevel
		if v < 0 || v > config.MaxACMMLevel {
			return nil, fmt.Errorf("writeback unsolicited.min_level must be 0 to %d", config.MaxACMMLevel)
		}
		steps = append(steps, func(c *config.Config) {
			if v == 0 {
				c.Writeback.Unsolicited.MinLevel = nil
				return
			}
			c.Writeback.Unsolicited.MinLevel = &v
		})
	}
	if b.WritebackRequireLabels != nil {
		v := *b.WritebackRequireLabels
		steps = append(steps, func(c *config.Config) { c.Writeback.Unsolicited.RequireLabels = &v })
	}
	if b.WritebackBackfill != nil {
		v := *b.WritebackBackfill
		steps = append(steps, func(c *config.Config) { c.Writeback.Unsolicited.Backfill = &v })
	}
	if b.WritebackPerRepo != nil {
		per := map[string]writeback.RepoOverride{}
		for repo, mode := range b.WritebackPerRepo {
			repo = strings.TrimSpace(repo)
			m := writeback.RepoMode(strings.ToLower(strings.TrimSpace(mode)))
			if repo == "" || m == writeback.RepoModeDefault {
				continue
			}
			if !m.Valid() {
				return nil, fmt.Errorf("writeback per_repo.%s.mode %q must be one of off, issues-only, labels-only, full", repo, mode)
			}
			per[repo] = writeback.RepoOverride{Mode: m}
		}
		steps = append(steps, func(c *config.Config) {
			if len(per) == 0 {
				c.Writeback.PerRepo = nil
				return
			}
			c.Writeback.PerRepo = per
		})
	}
	if b.WritebackKinds != nil {
		kinds := map[string]writeback.KindPolicyOverride{}
		for kind, ov := range b.WritebackKinds {
			kind = strings.ToLower(strings.TrimSpace(kind))
			if !writeback.IsKind(kind) {
				return nil, fmt.Errorf("writeback kinds.%s: unknown write-back kind", kind)
			}
			if ov == nil || (ov.MinLevel == nil && ov.RequireLabel == nil && ov.UnlabeledMinLevel == nil && ov.OwnOnly == nil) {
				continue
			}
			for _, lv := range []*int{ov.MinLevel, ov.UnlabeledMinLevel} {
				if lv != nil && (*lv < 0 || *lv > config.MaxACMMLevel) {
					return nil, fmt.Errorf("writeback kinds.%s: level must be 0 to %d", kind, config.MaxACMMLevel)
				}
			}
			kinds[kind] = *ov
		}
		steps = append(steps, func(c *config.Config) {
			if len(kinds) == 0 {
				c.Writeback.Kinds = nil
				return
			}
			c.Writeback.Kinds = kinds
		})
	}

	if b.SupersessionGracePeriod != nil {
		d, err := parseSettingsDuration("supersession_sweep grace_period", *b.SupersessionGracePeriod)
		if err != nil {
			return nil, err
		}
		steps = append(steps, func(c *config.Config) { c.SupersessionSweep.GracePeriod = d })
	}

	if v := b.CompliancePostureWindowDays; v != nil {
		if *v < 0 || *v > config.MaxCompliancePostureWindowDays {
			return nil, fmt.Errorf("compliance posture_checks window_days must be 0 (default) to %d", config.MaxCompliancePostureWindowDays)
		}
		days := *v
		steps = append(steps, func(c *config.Config) { c.Compliance.PostureChecks.WindowDays = days })
	}
	if v := b.CompliancePostureHistoryDays; v != nil {
		if *v != 0 && (*v < config.MinCompliancePostureHistoryDays || *v > config.MaxCompliancePostureHistoryDays) {
			return nil, fmt.Errorf("compliance posture_checks history_days must be 0 (default) or %d to %d", config.MinCompliancePostureHistoryDays, config.MaxCompliancePostureHistoryDays)
		}
		days := *v
		steps = append(steps, func(c *config.Config) { c.Compliance.PostureChecks.HistoryDays = days })
	}

	if b.PlanningDesignApprovedLabel != nil {
		v := strings.TrimSpace(*b.PlanningDesignApprovedLabel)
		if err := validateGovernorLabels([]string{v}); err != nil {
			return nil, err
		}
		steps = append(steps, func(c *config.Config) { c.Planning.DesignApprovedLabel = v })
	}
	for _, f := range []struct {
		v    *int
		name string
	}{
		{b.PlanningMaxConcurrentDesigns, "max_concurrent_designs"},
		{b.PlanningMaxDesignRevisions, "max_design_revisions"},
	} {
		if f.v != nil && *f.v < 0 {
			return nil, fmt.Errorf("planning %s must be 0 (default) or greater", f.name)
		}
	}
	if b.PlanningMaxConcurrentDesigns != nil {
		v := *b.PlanningMaxConcurrentDesigns
		steps = append(steps, func(c *config.Config) { c.Planning.MaxConcurrentDesigns = v })
	}
	if b.PlanningMaxDesignRevisions != nil {
		v := *b.PlanningMaxDesignRevisions
		steps = append(steps, func(c *config.Config) { c.Planning.MaxDesignRevisions = v })
	}

	for _, f := range []struct {
		raw  *string
		name string
		dst  func(*config.Config) *string
	}{
		{b.SwarmEventName, "swarm event_name", func(c *config.Config) *string { return &c.Swarm.EventName }},
		{b.SwarmCallToArms, "swarm call_to_arms", func(c *config.Config) *string { return &c.Swarm.CallToArms }},
		{b.SwarmLeaderboardTitle, "swarm leaderboard_title", func(c *config.Config) *string { return &c.Swarm.LeaderboardTitle }},
	} {
		if f.raw == nil {
			continue
		}
		v := sanitizeString(*f.raw)
		if len(v) > maxSwarmTextLen {
			return nil, fmt.Errorf("%s must be at most %d characters", f.name, maxSwarmTextLen)
		}
		dst := f.dst
		steps = append(steps, func(c *config.Config) { *dst(c) = v })
	}

	return func(c *config.Config) {
		for _, step := range steps {
			step(c)
		}
	}, nil
}

// featuresPlatformResponse is the GET side of featuresPlatformBody. The
// *Defaults maps carry what an empty or zero field falls back to so the
// dialog can show it as a placeholder. Token settings return only the
// environment variable NAME; key and helper settings return only the path.
func featuresPlatformResponse(cfg *config.Config) map[string]interface{} {
	retention := ""
	if cfg.Data.SessionRetentionDays != nil {
		retention = strconv.Itoa(*cfg.Data.SessionRetentionDays)
	}
	return map[string]interface{}{
		"dataAgentsDir":            cfg.Data.AgentsDir,
		"dataBobSessionsDir":       cfg.Data.BobSessionsDir,
		"dataClaudeSessionsDir":    cfg.Data.ClaudeSessionsDir,
		"dataCopilotSessionsDir":   cfg.Data.CopilotSessionsDir,
		"dataLogsDir":              cfg.Data.LogsDir,
		"dataMetricsDir":           cfg.Data.MetricsDir,
		"dataSessionRetentionDays": retention,

		"deploymentPodmanMode":        cfg.Deployment.PodmanMode,
		"deploymentUpgradeRequestDir": cfg.Deployment.UpgradeRequestDir,
		"deploymentUpgradeHelper":     cfg.Deployment.UpgradeHelper,

		"evidenceEnabled":        cfg.Evidence.IsEnabled(),
		"evidenceSigningKeyFile": cfg.Evidence.SigningKeyPath(),

		"giteaUrl":       cfg.Gitea.URL,
		"giteaTokenEnv":  cfg.Gitea.TokenEnv,
		"gitlabUrl":      cfg.GitLab.URL,
		"gitlabTokenEnv": cfg.GitLab.TokenEnv,

		"mintKeyPath":       cfg.Mint.KeyPath,
		"mintMaxTtlSeconds": cfg.Mint.MaxTTLSeconds,

		"supersessionGracePeriod": settingsDurationString(cfg.SupersessionSweep.GracePeriod),

		"compliancePostureWindowDays":  cfg.Compliance.PostureChecks.WindowDays,
		"compliancePostureHistoryDays": cfg.Compliance.PostureChecks.HistoryDays,

		"planningDesignApprovedLabel":  cfg.Planning.DesignApprovedLabel,
		"planningMaxConcurrentDesigns": cfg.Planning.MaxConcurrentDesigns,
		"planningMaxDesignRevisions":   cfg.Planning.MaxDesignRevisions,

		"swarmEventName":        cfg.Swarm.EventName,
		"swarmCallToArms":       cfg.Swarm.CallToArms,
		"swarmLeaderboardTitle": cfg.Swarm.LeaderboardTitle,

		"toolApprovalInboxPath": cfg.ToolApproval.InboxPath,

		"writebackMinLevel":      cfg.Writeback.MinLevelOrDefault(),
		"writebackRequireLabels": cfg.Writeback.RequireLabelsEnabled(),
		"writebackBackfill":      cfg.Writeback.BackfillEnabled(),
		"writebackPerRepo":       writebackPerRepoResponse(cfg.Writeback),
		"writebackKinds":         cfg.Writeback.Kinds,
		"writebackKindMatrix":    cfg.Writeback.EffectiveKinds(),
		"writebackRepoModes":     writeback.RepoModes(),
		"writebackOptInLabels":   cfg.WritebackOptInLabels(""),

		"platformDefaults": map[string]interface{}{
			"writebackMinLevel":            writeback.DefaultMinLevel,
			"writebackRequireLabels":       true,
			"writebackBackfill":            false,
			"writebackKindMatrix":          writeback.Config{}.EffectiveKinds(),
			"dataSessionRetentionDays":     config.DefaultSessionRetentionDays,
			"giteaTokenEnv":                config.DefaultGiteaTokenEnv,
			"gitlabUrl":                    config.DefaultGitLabURL,
			"gitlabTokenEnv":               config.DefaultGitLabTokenEnv,
			"mintMaxTtlSeconds":            config.MintDefaultMaxTTLSeconds,
			"mintHardCapTtlSeconds":        config.MintHardCapTTLSeconds,
			"supersessionGracePeriod":      github.DefaultSupersessionGracePeriod.String(),
			"compliancePostureWindowDays":  config.DefaultCompliancePostureWindowDays,
			"compliancePostureHistoryDays": config.DefaultCompliancePostureHistoryDays,
			"planningDesignApprovedLabel":  config.DefaultDesignApprovedLabel,
			"planningMaxConcurrentDesigns": config.DefaultMaxConcurrentDesigns,
			"planningMaxDesignRevisions":   config.DefaultMaxDesignRevisions,
			"swarmEventName":               DefaultSwarmName,
			"swarmCallToArms":              "All hands on deck!",
			"swarmLeaderboardTitle":        "Swarm leaderboard",
		},
	}
}

// writebackPerRepoResponse flattens writeback.per_repo to repo → mode for
// the Features tab editor (always a map, never null).
func writebackPerRepoResponse(c writeback.Config) map[string]string {
	out := map[string]string{}
	for repo, ov := range c.PerRepo {
		out[repo] = string(ov.Mode)
	}
	return out
}
