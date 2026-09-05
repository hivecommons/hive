package spoke

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// AgentActivityFor gathers the per-agent liveness evidence the hub needs to
// tell a deliberately-paused agent from one that is running but unable to work.
func AgentActivityFor(mgr *agent.Manager, cfg *config.Config, govState governor.State, currentMode, name string, proc *agent.AgentProcess, onDemandFromPack map[string]bool) AgentActivity {
	act := AgentActivity{
		Paused: proc.Paused,
		// Pause provenance (#4041): ride WHO/WHY/WHEN to the hub so the fleet
		// view can tell a deliberate owner quiesce from a malfunction.
		PausedTrigger:  proc.PausedTrigger,
		PausedReason:   proc.PausedReason,
		PausedBy:       proc.PausedBy,
		PausedAt:       proc.PausedAt,
		NeedsLogin:     proc.NeedsLogin,
		QuotaExhausted: proc.QuotaExhausted,
		LastActivityAt: proc.LastPaneChange,
		// A missing tmux session is only meaningful for an agent the manager
		// believes is running; SessionMissing enforces that itself.
		SessionMissing: mgr.SessionMissing(name),
	}
	if proc.StartedAt != nil {
		act.StartedAt = *proc.StartedAt
	}
	act.KickInterval = HeartbeatKickInterval(govState, name, proc, onDemandFromPack)

	// EXPECTED leg: does the governor's current mode schedule this agent on a
	// kicking cadence right now? Shared with the dashboard's offByCadence via
	// config.ExpectedActive so the two never disagree.
	if cfg != nil {
		onDemandAgent := false
		enabled := false
		if ac, ok := cfg.Agents[name]; ok {
			onDemandAgent = ac.OnDemand
			enabled = ac.Enabled
		}
		act.ExpectedActive = cfg.ExpectedActive(name, currentMode, onDemandAgent, onDemandFromPack)
		act.Enabled = enabled
	}

	// ABLE leg: the exact ACMM capability gates the spoke enforces. ok=false
	// (unknown agent) leaves all three false, read hub-side as UNKNOWN.
	if canIssue, canPR, canMerge, ok := mgr.AgentCapabilities(name); ok {
		act.CanOpenIssue = canIssue
		act.CanOpenPR = canPR
		act.CanMerge = canMerge
	}

	// Backend lets the hub interpret NeedsLogin (interactive vs inference).
	if backend, ok := mgr.EffectiveBackend(name); ok {
		act.Backend = backend
	}
	if total, last24h, lastRestartAt, lastReason, ok := mgr.RestartTelemetry(name); ok {
		act.Restarts = AgentRestartTelemetry{
			Total:      total,
			Last24h:    last24h,
			LastReason: lastReason,
		}
		if !lastRestartAt.IsZero() {
			act.Restarts.LastRestartAt = lastRestartAt.UTC().Format(time.RFC3339)
		}
	}

	return act
}

func HeartbeatKickInterval(govState governor.State, name string, proc *agent.AgentProcess, onDemandFromPack map[string]bool) time.Duration {
	if proc == nil || !proc.Config.UsesGovernorKick() || proc.Config.OnDemand || onDemandFromPack[name] {
		return 0
	}
	cadence, ok := govState.Cadences[name]
	if !ok || cadence.Paused || cadence.Interval <= 0 {
		return 0
	}
	return cadence.Interval
}

func QuotaExhaustedAgentCount(agents []AgentSummary) int {
	count := 0
	for _, a := range agents {
		if a.QuotaExhausted && !a.Paused &&
			!strings.EqualFold(a.State, "paused") &&
			strings.EqualFold(a.State, "running") {
			count++
		}
	}
	return count
}

func QuotaExhaustedProcessCount(statuses map[string]*agent.AgentProcess) int {
	count := 0
	for _, proc := range statuses {
		if proc != nil && proc.QuotaExhausted && !proc.Paused && proc.State == agent.StateRunning {
			count++
		}
	}
	return count
}

func QuotaExhaustedAgentReason(count int) string {
	if count <= 0 {
		return ""
	}
	return fmt.Sprintf("%d agent(s) out of provider quota", count)
}

// InferenceBudgetProvider supplies the current provider spending-limit latch.
type InferenceBudgetProvider func() (cause string, since, lastRebuff time.Time, rebuffs int)

func ProviderLimitHeartbeatFields(agents []AgentSummary, budget InferenceBudgetProvider) (reason string, rebuffs int, hiveWide bool, agentNames []string) {
	if budget != nil {
		errMsg, _, _, rebuffs := budget()
		if errMsg != "" {
			if rebuffs > 1 {
				return fmt.Sprintf("provider spending limit reached — %d refused calls: %s", rebuffs, errMsg), rebuffs, true, nil
			}
			return "provider spending limit reached — " + errMsg, rebuffs, true, nil
		}
	}
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		if a.QuotaExhausted && !a.Paused && !strings.EqualFold(a.State, agentStatePaused) {
			names = append(names, a.Name)
		}
	}
	sort.Strings(names)
	return QuotaExhaustedAgentReason(len(names)), 0, false, names
}
