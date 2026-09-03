package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

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
	// AutoMerge is the label a merger/owner queue action applies to a PR.
	// Configurable because the label has to live in someone else's
	// repository, where the local convention already exists: Prow-style
	// projects have used `lgtm` for exactly this decision for years, and a
	// hive that hard-codes its own name either collides with that or forces
	// every managed repo to grow a second label meaning the same thing.
	// Defaults to DefaultAutoMergeLabel.
	AutoMerge string `yaml:"automerge"`
}

type SensingConfig struct {
	GHRatePatterns     []string `yaml:"gh_rate_patterns"`
	CLIExcludePatterns []string `yaml:"cli_exclude_patterns"`
	LoginPatterns      []string `yaml:"login_patterns"`
	TTLSeconds         int      `yaml:"ttl_seconds"`
	PullbackSeconds    int      `yaml:"pullback_seconds"`
}

// defaultLoginPatterns is the built-in login-detector pattern set (#3959):
// every entry matches a CLI's own login CHROME, never ordinary English, so an
// agent is not paused for merely reading or discussing an auth error. It is a
// named var (not an inline literal in applyDefaults) so persistence can
// recognize "this list IS the default" and skip writing it — see
// redactedForPersist, which is what keeps the default set from being
// materialized into saved configs and pinned there forever (#4041).
var defaultLoginPatterns = []string{
	// claude: exact prompt strings from Claude Code's login screens.
	"Please run /login",
	"Not logged in",
	// gh / copilot / gemini: the commands their CLIs tell the user to run.
	"gh auth login",
	"claude login",
	// "copilot auth login" is the Copilot CLI's full logged-out instruction.
	// The bare 2-word fragment "copilot auth" false-positived: it also matched
	// `copilot auth status` (an auth CHECK) and incidental doc/comment mentions
	// (e.g. bin/copilot-models.mjs), pausing logged-IN agents — a live quality
	// agent flapped on `(?i)copilot auth` for days (restart_count 83). Tightening
	// to the full command matches the specificity of its `gh auth login` /
	// `gemini auth login` siblings and still catches genuine Copilot logouts.
	"copilot auth login",
	"gemini auth login",
	// bob: its API-key entry prompts.
	"Enter Bob-Shell API Key",
	"Paste your API key here",
}

// legacyDefaultLoginPatterns is the pre-#3959 default login-detector list,
// frozen verbatim (values AND order) as it shipped from the day the field
// existed until da9f6ff2. It exists only for migration: every hive that saved
// its config in that window has this exact list materialized as explicit
// values (Save() marshals defaults along with everything else), which
// permanently defeated the #3959 defaults fix because defaults only apply to
// an empty list (#4041). Never extend or reorder it — a byte-identical match
// is the evidence that the list carries no operator intent.
var legacyDefaultLoginPatterns = []string{
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

// IsLegacyDefaultLoginPatterns reports whether list is byte-identical (same
// entries, same order) to the pre-#3959 default login-pattern set. Exported
// because cmd/hive's legacy state-overrides migration replays a persisted
// sensing_login list over the loaded config, which is the same materialized-
// defaults hazard applyDefaults migrates in the config file itself (#4041).
func IsLegacyDefaultLoginPatterns(list []string) bool {
	return stringSlicesEqual(list, legacyDefaultLoginPatterns)
}

// stringSlicesEqual is an exact element-wise comparison (no normalization —
// migration must only ever fire on a byte-identical match).
func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
	Threshold int                `yaml:"threshold"`
	Cadences  map[string]Cadence `yaml:"cadences"`
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
	var raw map[string]Cadence
	if err := value.Decode(&raw); err != nil {
		return err
	}

	m.Cadences = make(map[string]Cadence)

	const thresholdKey = "threshold"
	if v, ok := raw[thresholdKey]; ok {
		var t int
		if _, err := fmt.Sscanf(v.Interval(), "%d", &t); err != nil {
			return fmt.Errorf("invalid threshold value %q: %w", v.Interval(), err)
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
