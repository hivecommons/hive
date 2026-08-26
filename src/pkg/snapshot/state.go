package snapshot

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/kubestellar/hive/pkg/config"
)

const maxStateAge = 7 * 24 * time.Hour

type PersistedState struct {
	SavedAt          time.Time                            `json:"saved_at"`
	Agents           map[string]AgentState                `json:"agents"`
	GovernorMode     string                               `json:"governor_mode"`
	BudgetLimit      int64                                `json:"budget_limit"`
	BudgetIgnored    []string                             `json:"budget_ignored"`
	BudgetIgnoreAll  bool                                 `json:"budget_ignore_all,omitempty"`
	CadenceOverrides map[string]map[string]config.Cadence `json:"cadence_overrides,omitempty"`
	LastKicks        map[string]time.Time                 `json:"last_kicks,omitempty"`
	BudgetSpend      int64                                `json:"budget_spend,omitempty"`
	BudgetResetAt    time.Time                            `json:"budget_reset_at,omitempty"`
	BudgetByAgent    map[string]int64                     `json:"budget_by_agent,omitempty"`
	BudgetByModel    map[string]int64                     `json:"budget_by_model,omitempty"`
	// BudgetWindowBaseline is the lifetime token total at the start of the
	// current budget window (see governor.BudgetInfo.WindowBaseline).
	BudgetWindowBaseline int64            `json:"budget_window_baseline,omitempty"`
	KickHistory          []GovKickEntry   `json:"kick_history,omitempty"`
	LastEval             time.Time        `json:"last_eval,omitempty"`
	ACMMLevel            *int             `json:"acmm_level,omitempty"`
	ConfigOverrides      *ConfigOverrides `json:"config_overrides,omitempty"`
	Breaker              *BreakerState    `json:"breaker,omitempty"`
}

// BreakerState persists the fleet breaker so an engaged kill-switch survives
// the frequent pod restarts. The agents it holds paused restore paused from
// their own persisted per-agent pause; this records that the breaker owns them
// so a later release resumes exactly that set. Absent (nil) means the breaker
// was never engaged — the common case — and adds nothing to the state file.
type BreakerState struct {
	Engaged bool     `json:"engaged"`
	Paused  []string `json:"paused,omitempty"`
}

type ConfigOverrides struct {
	ProjectRepos        []string       `json:"project_repos,omitempty"`
	EvalIntervalS       *int           `json:"eval_interval_s,omitempty"`
	Thresholds          map[string]int `json:"thresholds,omitempty"`
	SensingGHRate       []string       `json:"sensing_gh_rate,omitempty"`
	SensingCLIExclude   []string       `json:"sensing_cli_exclude,omitempty"`
	SensingLogin        []string       `json:"sensing_login,omitempty"`
	SensingTTL          *int           `json:"sensing_ttl,omitempty"`
	SensingPullback     *int           `json:"sensing_pullback,omitempty"`
	ExemptLabels        []string       `json:"exempt_labels,omitempty"`
	NtfyServer          string         `json:"ntfy_server,omitempty"`
	NtfyTopic           string         `json:"ntfy_topic,omitempty"`
	DiscordWebhook      string         `json:"discord_webhook,omitempty"`
	HealthcheckInterval *int           `json:"healthcheck_interval,omitempty"`
	RestartCooldown     *int           `json:"restart_cooldown,omitempty"`
	ModelLock           *bool          `json:"model_lock,omitempty"`
	LogMaxSizeMB        *int           `json:"log_max_size_mb,omitempty"`
	LogMaxAgeDays       *int           `json:"log_max_age_days,omitempty"`
	LogMaxBackups       *int           `json:"log_max_backups,omitempty"`
	LogCompress         *bool          `json:"log_compress,omitempty"`
	LogLevel            string         `json:"log_level,omitempty"`
}

type AgentState struct {
	Paused        bool       `json:"paused"`
	PausedAt      *time.Time `json:"paused_at,omitempty"`
	PausedReason  string     `json:"paused_reason,omitempty"`
	PausedTrigger string     `json:"paused_trigger,omitempty"`
	// PausedBy is the acting user behind the pause when one is known (the
	// authenticated dashboard user); empty for system-initiated pauses.
	// Persisted so pause provenance survives restarts (#4041).
	PausedBy        string           `json:"paused_by,omitempty"`
	PinnedCLI       string           `json:"pinned_cli,omitempty"`
	PinnedModel     string           `json:"pinned_model,omitempty"`
	ModelOverride   string           `json:"model_override,omitempty"`
	BackendOverride string           `json:"backend_override,omitempty"`
	RestartCount    int              `json:"restart_count"`
	DisplayName     string           `json:"display_name,omitempty"`
	Description     string           `json:"description,omitempty"`
	Enabled         *bool            `json:"enabled,omitempty"`
	ClearOnKick     *bool            `json:"clear_on_kick,omitempty"`
	StaleTimeout    *int             `json:"stale_timeout,omitempty"`
	RestartStrategy string           `json:"restart_strategy,omitempty"`
	LaunchCmd       string           `json:"launch_cmd,omitempty"`
	LastKick        *time.Time       `json:"last_kick,omitempty"`
	KickHistory     []AgentKickEntry `json:"kick_history,omitempty"`
}

type GovKickEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Agent     string    `json:"agent"`
}

type AgentKickEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Agent     string    `json:"agent"`
	Snippet   string    `json:"snippet"`
}

func SaveState(path string, state *PersistedState, logger *slog.Logger) error {
	state.SavedAt = time.Now()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("writing temp state file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("renaming state file: %w", err)
	}

	logger.Info("state persisted", "path", path)
	return nil
}

func LoadState(path string, logger *slog.Logger) (*PersistedState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading state file: %w", err)
	}

	var state PersistedState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parsing state file: %w", err)
	}

	if time.Since(state.SavedAt) > maxStateAge {
		logger.Info("state file too old, ignoring", "saved_at", state.SavedAt, "age", time.Since(state.SavedAt))
		return nil, nil
	}

	logger.Info("state restored", "saved_at", state.SavedAt, "agents", len(state.Agents))
	return &state, nil
}
