package snapshot

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/watchdog"
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
	// Watchdog persists the RFC #4665 reconciler's per-agent backoff /
	// crash-loop / condition state, so a pod restart neither forgets an
	// escalated crash loop nor replays a backoff ladder from the top
	// (RFC open question 2: the state rides the existing state file).
	Watchdog map[string]watchdog.PersistedAgent `json:"watchdog,omitempty"`
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
	PausedBy          string              `json:"paused_by,omitempty"`
	PinnedCLI         string              `json:"pinned_cli,omitempty"`
	PinnedModel       string              `json:"pinned_model,omitempty"`
	ModelOverride     string              `json:"model_override,omitempty"`
	BackendOverride   string              `json:"backend_override,omitempty"`
	RestartCount      int                 `json:"restart_count"`
	RestartEvents     []AgentRestartEvent `json:"restart_events,omitempty"`
	LastRestartReason string              `json:"last_restart_reason,omitempty"`
	DisplayName       string              `json:"display_name,omitempty"`
	Description       string              `json:"description,omitempty"`
	Enabled           *bool               `json:"enabled,omitempty"`
	ClearOnKick       *bool               `json:"clear_on_kick,omitempty"`
	StaleTimeout      *int                `json:"stale_timeout,omitempty"`
	RestartStrategy   string              `json:"restart_strategy,omitempty"`
	LaunchCmd         string              `json:"launch_cmd,omitempty"`
	LastKick          *time.Time          `json:"last_kick,omitempty"`
	KickHistory       []AgentKickEntry    `json:"kick_history,omitempty"`
	// TurnLoss records what teardowns discarded from this agent's in-flight
	// turns. RestartCount above says how OFTEN an agent was restarted;
	// this says what those restarts COST, which is RFC #4002's open question 3
	// and the input its step-4 feasibility call depends on.
	//
	// It rides the existing state file rather than a sidecar because the RFC
	// explicitly cautions against adding durable store number five, and because
	// the measurement is worthless unless it survives the very event it
	// measures.
	TurnLoss *AgentTurnLoss `json:"turn_loss,omitempty"`
}

type AgentRestartEvent struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// AgentTurnLoss is the persisted form of agent.TurnLoss. It is duplicated here
// rather than imported because pkg/snapshot does not depend on pkg/agent (and
// must not — the manager is the thing being snapshotted); cmd/hive converts at
// the boundary, exactly as it already does for KickHistory.
type AgentTurnLoss struct {
	// Interruptions is every teardown that hit a turn with output still pending.
	Interruptions int `json:"interruptions"`
	// Producing is the subset where the pane changed AFTER the kick landed —
	// the agent was observably working. This is the honest headline: teardowns
	// that certainly discarded work, as opposed to teardowns that merely could
	// have.
	Producing int `json:"producing"`
	// UpperBoundS is the summed time-since-kick across interruptions, in
	// seconds. Named for what it is: the most these teardowns could have cost,
	// never a claim about what they did cost.
	UpperBoundS float64 `json:"upper_bound_s"`
	// Bytes is the summed archived scrollback across interruptions, a proxy for
	// the volume of turn output discarded.
	Bytes int64 `json:"bytes"`
	// Recent is a bounded tail of individual records, oldest first, so an
	// operator can see the SHAPE of the loss (a few long turns vs. many short
	// ones) rather than only its total.
	Recent []AgentTurnInterruption `json:"recent,omitempty"`
}

// AgentTurnInterruption is one teardown that killed a turn in flight.
//
// Durations are seconds as floats rather than time.Duration: this lands in a
// file operators read with `jq`, where a nanosecond integer is not legible.
type AgentTurnInterruption struct {
	At         time.Time `json:"at"`
	Reason     string    `json:"reason"`
	SinceKickS float64   `json:"since_kick_s"`
	// SinceOutputS is nil when the pane poller never observed a change, which
	// means UNKNOWN and must never be read as "idle".
	SinceOutputS *float64 `json:"since_output_s,omitempty"`
	Producing    bool     `json:"producing"`
	Bytes        int      `json:"bytes"`
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
