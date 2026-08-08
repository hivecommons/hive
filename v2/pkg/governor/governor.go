package governor

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/v2/pkg/config"
)

type Mode string

const (
	ModeSurge Mode = "SURGE"
	ModeBusy  Mode = "BUSY"
	ModeQuiet Mode = "QUIET"
	ModeIdle  Mode = "IDLE"
)

type AgentCadence struct {
	Agent          string
	Interval       time.Duration
	Schedule       config.Cadence
	Paused         bool
	LastOccurrence time.Time
}

type ModeChange struct {
	Timestamp time.Time `json:"timestamp"`
	From      Mode      `json:"from"`
	To        Mode      `json:"to"`
	Reason    string    `json:"reason"`
}

type EvalSnapshot struct {
	Timestamp       int64                     `json:"t"`
	Mode            Mode                      `json:"govMode"`
	QueueIssues     int                       `json:"govIssues"`
	QueuePRs        int                       `json:"govPrs"`
	QueueTotal      int                       `json:"govTotal"`
	QueueHold       int                       `json:"govHold"`
	QueueActive     int                       `json:"govActive"`
	SLAViolations   int                       `json:"sla_violations,omitempty"`
	AgentsKicked    []string                  `json:"agents_kicked,omitempty"`
	Actionable      int                       `json:"actionableCount"`
	OpenPRs         int                       `json:"openPrCount"`
	Mergeable       int                       `json:"mergeableCount"`
	BeadsWorkers    int                       `json:"beadsWorkers"`
	BeadsSupervisor int                       `json:"beadsSupervisor"`
	Repos           map[string]RepoSnapshot   `json:"repos,omitempty"`
	AgentStats      map[string]map[string]any `json:"agentStats,omitempty"`
}

type RepoSnapshot struct {
	Issues int `json:"issues"`
	PRs    int `json:"prs"`
}

type KickRecord struct {
	Timestamp time.Time `json:"timestamp"`
	Agent     string    `json:"agent"`
}

type BudgetInfo struct {
	WeeklyLimit   int64            `json:"weekly_limit"`
	CurrentSpend  int64            `json:"current_spend"`
	ByAgent       map[string]int64 `json:"by_agent"`
	ByModel       map[string]int64 `json:"by_model"`
	IgnoredAgents []string         `json:"ignored_agents"`
	// IgnoreAll disables budget kick-suppression for every agent while
	// keeping the limit, window, and alerts intact (the dashboard's
	// global "ignore budget" toggle).
	IgnoreAll bool `json:"ignore_all"`
	// ResetAt is the START of the current budget window; the window ends
	// at ResetAt + BudgetWindowDuration.
	ResetAt time.Time `json:"reset_at"`
	// WindowBaseline is the lifetime token total observed when the current
	// window opened. Spend inside the window is lifetime total minus this.
	WindowBaseline int64 `json:"window_baseline"`
}

// BudgetWindowDuration is the DEFAULT length of one budget accounting
// window; the "weekly" limit applies to spend accumulated within it. As of
// #2323 the effective window is the configured governor.budget.period_days
// (see budgetWindowDuration); this constant is the fallback used when that
// setting is unset/zero.
const BudgetWindowDuration = defaultBudgetWindowDays * 24 * time.Hour

// defaultBudgetWindowDays is the fallback budget-window length in days,
// matching config.defaultBudgetPeriodDays. Used when period_days is unset.
const defaultBudgetWindowDays = 7

// BudgetWarnPct is the DEFAULT percent of the weekly limit at which the soft
// budget warning fires. As of #2323 the effective threshold is the
// configured governor.budget.critical_pct (see budgetWarnPct); this constant
// is the fallback used when that setting is unset/zero.
const BudgetWarnPct = 90

// percentDenominator converts a percentage into a fraction of a whole.
const percentDenominator = 100

// hoursPerDay converts a day count into hours for time.Duration math.
const hoursPerDay = 24

// budgetWindowDuration returns the effective budget-window length. It reads
// the configured governor.budget.period_days (#2323 — previously this value
// was persisted but never read, so the window was silently pinned to 7 days)
// and falls back to the BudgetWindowDuration default when the setting is
// unset/zero (a 0-day window would roll every eval and break budgeting).
// Callers must hold g.mu.
func (g *Governor) budgetWindowDuration() time.Duration {
	if days := g.cfg.Budget.PeriodDays; days > 0 {
		return time.Duration(days) * hoursPerDay * time.Hour
	}
	return BudgetWindowDuration
}

// budgetWarnPct returns the effective soft-warning threshold as an integer
// percent (0-100). It reads the configured governor.budget.critical_pct
// (#2323 — previously persisted but never read, so warnings were silently
// pinned to 90%) and falls back to the BudgetWarnPct default when the
// setting is unset/zero (a 0% threshold would warn immediately). Callers
// must hold g.mu.
func (g *Governor) budgetWarnPct() int {
	if pct := g.cfg.Budget.CriticalPct; pct > 0 {
		return pct
	}
	return BudgetWarnPct
}

// BudgetTransitions reports budget threshold state from a single
// UpdateBudgetFromTotals call. WarnCrossed and ExhaustedCrossed are
// one-shot: they fire at most once per window (flags reset when the window
// rolls). The Active fields reflect the current spend level every call so
// callers can clear alerts that no longer apply.
type BudgetTransitions struct {
	WarnCrossed      bool
	ExhaustedCrossed bool
	Rolled           bool
	WarnActive       bool
	ExhaustedActive  bool
}

type State struct {
	Mode          Mode                    `json:"mode"`
	QueueIssues   int                     `json:"queue_issues"`
	QueuePRs      int                     `json:"queue_prs"`
	QueueHold     int                     `json:"queue_hold"`
	Cadences      map[string]AgentCadence `json:"-"`
	LastKick      map[string]time.Time    `json:"last_kick"`
	LastEval      time.Time               `json:"last_eval"`
	SLAViolations int                     `json:"sla_violations"`
	// BudgetExhausted mirrors the budget gate as of the last eval: the
	// weekly limit is set and window spend has reached it, so kicks for
	// non-exempt agents are suppressed.
	BudgetExhausted bool `json:"budget_exhausted"`
}

const (
	modeHistoryCapacity = 100
	evalHistoryCapacity = 200
	kickHistoryCapacity = 500
)

// Cadence sentinel values the config layer emits. "pause"/"paused" mark an
// agent the operator does not want timer-kicked in a mode; "off" is the
// dashboard's spelling for the same thing. Kept as named constants so the
// governor and the dashboard's display logic agree on the exact strings.
const (
	cadenceValuePause  = "pause"
	cadenceValuePaused = "paused"
	cadenceValueOff    = "off"
)

// idleModeKey is the mode whose cadence entries serve as the per-agent
// fallback when the current mode defines none. This mirrors the dashboard's
// display chain (current mode → idle), so the interval the governor actually
// kicks on is always the interval the agent card shows.
const idleModeKey = "idle"

type Governor struct {
	cfg    config.GovernorConfig
	agents map[string]config.AgentConfig
	state  State
	mu     sync.RWMutex
	logger *slog.Logger

	modeHistory []ModeChange
	evalHistory []EvalSnapshot
	kickHistory []KickRecord
	budget      BudgetInfo
	now         func() time.Time

	// resumeKicks records the last crash-recovery resume kick granted per
	// agent (see AllowResumeKick). In-memory only: after a process restart
	// the startup path re-kicks every eligible agent anyway, so persisting
	// this would not tighten the bound.
	resumeKicks map[string]time.Time

	// One-shot alert flags for the current budget window; reset when the
	// window rolls so each window alerts at most once per threshold.
	budgetWarned           bool
	budgetExhaustedAlerted bool
}

func New(cfg config.GovernorConfig, agents map[string]config.AgentConfig, logger *slog.Logger) *Governor {
	// Record the initial mode so the timeline always has at least one entry.
	initialChange := ModeChange{
		Timestamp: time.Now(),
		From:      "",
		To:        ModeIdle,
		Reason:    "startup",
	}

	return &Governor{
		cfg:    cfg,
		agents: agents,
		state: State{
			Mode:     ModeIdle,
			Cadences: make(map[string]AgentCadence),
			LastKick: make(map[string]time.Time),
		},
		logger:      logger,
		modeHistory: []ModeChange{initialChange},
		evalHistory: make([]EvalSnapshot, 0, evalHistoryCapacity),
		kickHistory: make([]KickRecord, 0, kickHistoryCapacity),
		resumeKicks: make(map[string]time.Time),
		now:         time.Now,
		budget: BudgetInfo{
			ByAgent: make(map[string]int64),
			ByModel: make(map[string]int64),
		},
	}
}

func (g *Governor) UpdateConfig(cfg config.GovernorConfig) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cfg = cfg
}

func (g *Governor) UpdateAgents(agents map[string]config.AgentConfig) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.agents = agents
}

func (g *Governor) Evaluate(queueIssues, queuePRs, queueHold, slaViolations int) []string {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.state.QueueIssues = queueIssues
	g.state.QueuePRs = queuePRs
	g.state.QueueHold = queueHold
	g.state.SLAViolations = slaViolations
	g.state.LastEval = time.Now()

	// Queue pressure is actionable issues PLUS open PRs. Issues alone let the
	// governor idle while a large PR backlog sat unmerged (observed 2026-08-03:
	// 23 open PRs, 1 actionable issue → idle cadence → merge sweeps too rare
	// to drain the queue). Held items stay excluded — they are waiting on a
	// human by definition and faster kicks cannot move them.
	newMode := g.computeMode(queueIssues + queuePRs)
	modeChanged := newMode != g.state.Mode
	if modeChanged {
		g.logger.Info("governor mode change",
			"from", g.state.Mode,
			"to", newMode,
			"issues", queueIssues,
			"prs", queuePRs,
			"hold", queueHold,
			"sla_violations", slaViolations,
		)
		change := ModeChange{
			Timestamp: time.Now(),
			From:      g.state.Mode,
			To:        newMode,
			Reason:    fmt.Sprintf("queue_depth=%d", queueIssues),
		}
		g.appendModeHistory(change)
		g.state.Mode = newMode
	}

	g.updateCadences()

	if modeChanged {
		for agentName, cadence := range g.state.Cadences {
			if cadence.Paused {
				g.logger.Info("agent cadence: paused by mode",
					"agent", agentName,
					"mode", g.state.Mode,
				)
			} else if cadence.Schedule.Mode() != config.CadenceModeInterval {
				g.logger.Info("agent cadence: active time-of-day schedule",
					"agent", agentName,
					"mode", g.state.Mode,
					"schedule", cadence.Schedule.HumanSummary(),
				)
			} else if cadence.Interval > 0 {
				g.logger.Info("agent cadence: active",
					"agent", agentName,
					"mode", g.state.Mode,
					"interval", cadence.Interval.String(),
				)
			}
		}
	}

	g.state.BudgetExhausted = g.budgetExhausted()

	due := g.agentsDueForKick()

	snap := EvalSnapshot{
		Timestamp:     time.Now().UnixMilli(),
		Mode:          g.state.Mode,
		QueueIssues:   queueIssues,
		QueuePRs:      queuePRs,
		QueueTotal:    queueIssues + queuePRs + queueHold,
		QueueHold:     queueHold,
		QueueActive:   queueIssues + queuePRs,
		SLAViolations: slaViolations,
		AgentsKicked:  due,
		Actionable:    queueIssues,
		OpenPRs:       queuePRs,
	}
	g.appendEvalHistory(snap)

	return due
}

func (g *Governor) computeMode(queueDepth int) Mode {
	type modeEntry struct {
		name      Mode
		threshold int
	}

	entries := []modeEntry{
		{ModeSurge, g.thresholdFor("surge")},
		{ModeBusy, g.thresholdFor("busy")},
		{ModeQuiet, g.thresholdFor("quiet")},
	}

	for _, e := range entries {
		if queueDepth > e.threshold {
			return e.name
		}
	}
	return ModeIdle
}

func (g *Governor) thresholdFor(modeName string) int {
	// A mode entry may exist only for cadences with threshold unset
	// (zero). Zero thresholds would make every non-empty queue surge,
	// so fall through to the defaults in that case.
	if mode, ok := g.cfg.Modes[modeName]; ok && mode.Threshold > 0 {
		return mode.Threshold
	}
	switch modeName {
	case "surge":
		return 20
	case "busy":
		return 10
	case "quiet":
		return 2
	default:
		return 0
	}
}

// updateCadences recomputes every agent's effective cadence for the current
// mode. Resolution chain per agent: current-mode entry → idle-mode entry →
// none (no timer kicks) — the same chain the dashboard uses to display the
// agent's interval, so kick timing and the "next run" card always agree.
//
// The result REPLACES the previous cadence state. Before #2573 an agent
// missing from the current mode's cadence map silently KEPT the interval
// from whichever mode last defined one, so a hive leaving a short-cadence
// mode kept kicking on the short interval while the dashboard displayed the
// configured (longer) one — burning backend tokens faster than any cadence
// the operator could see or set.
func (g *Governor) updateCadences() {
	modeName := modeToConfigKey(g.state.Mode)
	cadences := make(map[string]AgentCadence, len(g.agents))

	for agentName := range g.agents {
		cadence, ok := g.resolveCadence(modeName, agentName)
		if !ok || strings.EqualFold(strings.TrimSpace(cadence.Interval()), cadenceValueOff) {
			// No cadence configured for this agent in this mode (or an
			// explicit "off"): no timer kicks. Leaving the agent out of the
			// map — rather than keeping a stale entry — is the fix.
			continue
		}

		if cadence.IsPaused() {
			cadences[agentName] = AgentCadence{
				Agent:  agentName,
				Paused: true,
			}
			continue
		}

		if err := cadence.Validate(); err != nil {
			g.logger.Warn("invalid cadence — agent will receive no timer kicks until fixed",
				"agent", agentName,
				"mode", g.state.Mode,
				"value", cadence.String(),
				"error", err,
			)
			continue
		}

		entry := AgentCadence{Agent: agentName, Schedule: cadence}
		if cadence.Mode() == config.CadenceModeInterval {
			dur, err := time.ParseDuration(cadence.Interval())
			if err != nil {
				g.logger.Warn("invalid cadence duration — agent will receive no timer kicks until fixed",
					"agent", agentName,
					"mode", g.state.Mode,
					"value", cadence.Interval(),
					"error", err,
				)
				continue
			}
			entry.Interval = dur
			if ac, ok := g.agents[agentName]; ok && ac.ReplicaIndex > 1 && ac.ReplicaCount > 1 && g.state.LastKick[agentName].IsZero() {
				offset := time.Duration(int64(dur) * int64(ac.ReplicaIndex-1) / int64(ac.ReplicaCount))
				g.state.LastKick[agentName] = g.now().Add(-dur + offset)
			}
		}
		cadences[agentName] = entry
	}

	g.state.Cadences = cadences
}

// resolveCadence returns the configured cadence string for one agent in one
// mode, falling back to the idle mode's entry when the mode defines none.
func (g *Governor) resolveCadence(modeName, agentName string) (config.Cadence, bool) {
	baseName := agentName
	if ac, ok := g.agents[agentName]; ok && ac.ReplicaOf != "" {
		baseName = ac.ReplicaOf
	}
	if mode, ok := g.cfg.Modes[modeName]; ok {
		if c, ok := mode.Cadences[agentName]; ok {
			return c, true
		}
		if baseName != agentName {
			if c, ok := mode.Cadences[baseName]; ok {
				return c, true
			}
		}
	}
	if modeName == idleModeKey {
		return "", false
	}
	if mode, ok := g.cfg.Modes[idleModeKey]; ok {
		if c, ok := mode.Cadences[agentName]; ok {
			return c, true
		}
		if baseName != agentName {
			if c, ok := mode.Cadences[baseName]; ok {
				return c, true
			}
		}
	}
	return "", false
}

// budgetExhausted reports whether the weekly budget gate is closed.
// WeeklyLimit == 0 means budgeting is entirely off; IgnoreAll keeps the
// limit and alerts but opens the kick gate. Caller must hold g.mu.
func (g *Governor) budgetExhausted() bool {
	return g.budget.WeeklyLimit > 0 && !g.budget.IgnoreAll && g.budget.CurrentSpend >= g.budget.WeeklyLimit
}

func (g *Governor) agentsDueForKick() []string {
	now := g.now()
	var due []string

	exhausted := g.budgetExhausted()
	// IgnoredAgents are exempt from budget suppression: they keep getting
	// kicked even when the weekly budget is exhausted.
	exempt := make(map[string]bool, len(g.budget.IgnoredAgents))
	for _, name := range g.budget.IgnoredAgents {
		exempt[name] = true
	}
	suppressed := 0

	for agentName, cadence := range g.state.Cadences {
		if cadence.Paused {
			continue
		}
		if cadence.Interval == 0 && cadence.Schedule.Mode() == config.CadenceModeInterval {
			continue
		}
		if ac, ok := g.agents[agentName]; ok && ac.OnDemand {
			continue
		}
		if ac, ok := g.agents[agentName]; ok && !ac.UsesGovernorKick() {
			continue
		}
		if exhausted && !exempt[agentName] {
			suppressed++
			continue
		}

		lastKick := g.state.LastKick[agentName]
		if cadence.Schedule.Mode() != config.CadenceModeInterval {
			// Time-of-day cadences are exact wall-clock schedules. Governor modes
			// only decide whether this schedule is active; they never scale the
			// schedule's times. A short catch-up window grants at most one kick
			// after downtime, and comparing the scheduled occurrence to LastKick
			// dedupes repeated governor ticks inside the same minute.
			if occurrence, ok := cadence.Schedule.DueOccurrence(lastKick, now, config.CadenceCatchUpWindow); ok {
				cadence.LastOccurrence = occurrence
				g.state.Cadences[agentName] = cadence
				due = append(due, agentName)
			}
			continue
		}
		if lastKick.IsZero() || now.Sub(lastKick) >= cadence.Interval {
			due = append(due, agentName)
		}
	}

	if exhausted {
		g.logger.Info("budget exhausted — suppressing kicks",
			"spend", g.budget.CurrentSpend,
			"limit", g.budget.WeeklyLimit,
			"suppressed", suppressed,
		)
	}

	return due
}

// budgetExempt reports whether the named agent is on the budget exemption
// list (kicked even when the weekly budget is exhausted). Caller must hold g.mu.
func (g *Governor) budgetExempt(agentName string) bool {
	for _, name := range g.budget.IgnoredAgents {
		if name == agentName {
			return true
		}
	}
	return false
}

// AllowResumeKick reports whether a crash-restarted agent may receive an
// immediate "resume" kick ahead of its scheduled cadence slot, and records
// the grant. The early kick exists so an agent whose CLI crashed mid-task
// resumes promptly instead of idling until the next scheduled kick.
//
// Before #2573 this was UNCONDITIONAL: every crash restart earned a kick, so
// a crash-looping CLI was restarted AND kicked on every governor eval cycle
// (default 5 minutes) — burning backend tokens ("Bob coins") continuously on
// agents the operator had set to multi-hour cadences, and bypassing the
// budget gate as well. The gates, in order:
//
//   - the agent must have an active cadence in the current mode: a mode
//     "pause"/"off" (or no cadence at all) means the operator wants no timer
//     work, and a crash must not override that;
//   - on-demand and non-kick-channel agents never get resume kicks (they are
//     never timer-kicked at all);
//   - the budget gate must be open for the agent, with the same exemption
//     list as scheduled kicks;
//   - at most ONE resume kick per cadence interval: a crash loop gets its
//     first resume, then waits for the next scheduled slot, bounding worst-
//     case kick frequency at two per interval (one scheduled + one resume)
//     instead of one per eval cycle.
func (g *Governor) AllowResumeKick(agentName string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	cadence, ok := g.state.Cadences[agentName]
	if !ok || cadence.Paused || (cadence.Interval <= 0 && cadence.Schedule.Mode() == config.CadenceModeInterval) {
		return false
	}
	if ac, ok := g.agents[agentName]; ok && (ac.OnDemand || !ac.UsesGovernorKick()) {
		return false
	}
	if g.budgetExhausted() && !g.budgetExempt(agentName) {
		return false
	}
	if cadence.Schedule.Mode() != config.CadenceModeInterval {
		return false
	}
	if last, ok := g.resumeKicks[agentName]; ok && g.now().Sub(last) < cadence.Interval {
		return false
	}
	g.resumeKicks[agentName] = g.now()
	return true
}

func (g *Governor) RecordKick(agentName string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.state.LastKick[agentName] = now
	g.appendKickHistory(KickRecord{Timestamp: now, Agent: agentName})
}

func (g *Governor) GetState() State {
	g.mu.RLock()
	defer g.mu.RUnlock()
	// Return a copy
	cadences := make(map[string]AgentCadence, len(g.state.Cadences))
	for k, v := range g.state.Cadences {
		cadences[k] = v
	}
	lastKick := make(map[string]time.Time, len(g.state.LastKick))
	for k, v := range g.state.LastKick {
		lastKick[k] = v
	}
	return State{
		Mode:            g.state.Mode,
		QueueIssues:     g.state.QueueIssues,
		QueuePRs:        g.state.QueuePRs,
		QueueHold:       g.state.QueueHold,
		Cadences:        cadences,
		LastKick:        lastKick,
		LastEval:        g.state.LastEval,
		SLAViolations:   g.state.SLAViolations,
		BudgetExhausted: g.state.BudgetExhausted,
	}
}

func modeToConfigKey(m Mode) string {
	switch m {
	case ModeSurge:
		return "surge"
	case ModeBusy:
		return "busy"
	case ModeQuiet:
		return "quiet"
	case ModeIdle:
		return "idle"
	default:
		return "idle"
	}
}

func (g *Governor) FormatStatus() string {
	s := g.GetState()
	return fmt.Sprintf("mode=%s issues=%d prs=%d hold=%d sla_violations=%d",
		s.Mode, s.QueueIssues, s.QueuePRs, s.QueueHold, s.SLAViolations)
}

func (g *Governor) appendModeHistory(change ModeChange) {
	if len(g.modeHistory) >= modeHistoryCapacity {
		g.modeHistory = g.modeHistory[1:]
	}
	g.modeHistory = append(g.modeHistory, change)
}

func (g *Governor) appendEvalHistory(snap EvalSnapshot) {
	if len(g.evalHistory) >= evalHistoryCapacity {
		g.evalHistory = g.evalHistory[1:]
	}
	g.evalHistory = append(g.evalHistory, snap)
}

func (g *Governor) appendKickHistory(record KickRecord) {
	if len(g.kickHistory) >= kickHistoryCapacity {
		g.kickHistory = g.kickHistory[1:]
	}
	g.kickHistory = append(g.kickHistory, record)
}

func (g *Governor) ModeHistory() []ModeChange {
	g.mu.RLock()
	defer g.mu.RUnlock()
	result := make([]ModeChange, len(g.modeHistory))
	copy(result, g.modeHistory)
	return result
}

func (g *Governor) EvalHistory() []EvalSnapshot {
	g.mu.RLock()
	defer g.mu.RUnlock()
	result := make([]EvalSnapshot, len(g.evalHistory))
	copy(result, g.evalHistory)
	return result
}

// AttachAgentStats attaches resolved stat values to the most recent eval snapshot.
func (g *Governor) AttachAgentStats(stats map[string]map[string]any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.evalHistory) == 0 {
		return
	}
	g.evalHistory[len(g.evalHistory)-1].AgentStats = stats
}

// AttachRepoSnapshots attaches per-repo issue/PR counts to the most recent eval snapshot.
func (g *Governor) AttachRepoSnapshots(repos map[string]RepoSnapshot) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.evalHistory) == 0 {
		return
	}
	g.evalHistory[len(g.evalHistory)-1].Repos = repos
}

// SeedEvalHistory loads previously persisted eval snapshots so sparkline
// history survives container restarts.
func (g *Governor) SeedEvalHistory(snapshots []EvalSnapshot) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(snapshots) > evalHistoryCapacity {
		snapshots = snapshots[len(snapshots)-evalHistoryCapacity:]
	}
	g.evalHistory = make([]EvalSnapshot, len(snapshots), evalHistoryCapacity)
	copy(g.evalHistory, snapshots)
}

// SeedModeHistory loads previously persisted mode changes so the mode
// timeline survives container restarts.
func (g *Governor) SeedModeHistory(changes []ModeChange) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(changes) > modeHistoryCapacity {
		changes = changes[len(changes)-modeHistoryCapacity:]
	}
	g.modeHistory = make([]ModeChange, len(changes), modeHistoryCapacity)
	copy(g.modeHistory, changes)
}

func (g *Governor) SeedQueueState(issues, prs, hold, slaViolations int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.state.QueueIssues = issues
	g.state.QueuePRs = prs
	g.state.QueueHold = hold
	g.state.SLAViolations = slaViolations
}

func (g *Governor) SeedLastKicks(kicks map[string]time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, v := range kicks {
		g.state.LastKick[k] = v
	}
}

// ClearLastKicks resets all LastKick timestamps so every agent is "due"
// on the next eval cycle. Paused and on-demand agents are still skipped
// by agentsDueForKick() — this just clears the timing gate.
func (g *Governor) ClearLastKicks() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.state.LastKick = make(map[string]time.Time)
}

func (g *Governor) SeedKickHistory(records []KickRecord) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(records) > kickHistoryCapacity {
		records = records[len(records)-kickHistoryCapacity:]
	}
	g.kickHistory = make([]KickRecord, len(records), kickHistoryCapacity)
	copy(g.kickHistory, records)
}

func (g *Governor) SeedBudget(spend int64, byAgent map[string]int64, byModel map[string]int64, resetAt time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.budget.CurrentSpend = spend
	for k, v := range byAgent {
		g.budget.ByAgent[k] = v
	}
	for k, v := range byModel {
		g.budget.ByModel[k] = v
	}
	g.budget.ResetAt = resetAt
}

func (g *Governor) SetMode(m Mode) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.state.Mode = m
}

func (g *Governor) SeedLastEval(t time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.state.LastEval = t
}

func (g *Governor) KickHistory() []KickRecord {
	g.mu.RLock()
	defer g.mu.RUnlock()
	result := make([]KickRecord, len(g.kickHistory))
	copy(result, g.kickHistory)
	return result
}

func (g *Governor) GetBudget() BudgetInfo {
	g.mu.RLock()
	defer g.mu.RUnlock()
	byAgent := make(map[string]int64, len(g.budget.ByAgent))
	for k, v := range g.budget.ByAgent {
		byAgent[k] = v
	}
	byModel := make(map[string]int64, len(g.budget.ByModel))
	for k, v := range g.budget.ByModel {
		byModel[k] = v
	}
	ignored := make([]string, len(g.budget.IgnoredAgents))
	copy(ignored, g.budget.IgnoredAgents)
	return BudgetInfo{
		WeeklyLimit:    g.budget.WeeklyLimit,
		CurrentSpend:   g.budget.CurrentSpend,
		ByAgent:        byAgent,
		ByModel:        byModel,
		IgnoredAgents:  ignored,
		IgnoreAll:      g.budget.IgnoreAll,
		ResetAt:        g.budget.ResetAt,
		WindowBaseline: g.budget.WindowBaseline,
	}
}

// UpdateBudgetFromTotals refreshes budget spend from the token collector's
// lifetime totals. Session-file scans are cumulative, so window spend is
// derived by subtracting the baseline captured when the window opened.
// Rolls the window forward once BudgetWindowDuration has elapsed and
// reports threshold crossings so callers can alert exactly once per window.
func (g *Governor) UpdateBudgetFromTotals(totalTokens int64, byAgent map[string]int64, byModel map[string]int64) BudgetTransitions {
	g.mu.Lock()
	defer g.mu.Unlock()

	var trans BudgetTransitions
	now := time.Now()
	if g.budget.ResetAt.IsZero() {
		// First run or legacy snapshot without a window: open one now.
		g.budget.ResetAt = now
		g.budget.WindowBaseline = totalTokens
	} else if now.Sub(g.budget.ResetAt) >= g.budgetWindowDuration() {
		g.budget.ResetAt = now
		g.budget.WindowBaseline = totalTokens
		g.budgetWarned = false
		g.budgetExhaustedAlerted = false
		trans.Rolled = true
		if g.logger != nil {
			g.logger.Info("budget window rolled",
				"reset_at", now,
				"baseline", totalTokens,
			)
		}
	}

	if totalTokens < g.budget.WindowBaseline {
		// Lifetime totals shrank (session files pruned); re-baseline so
		// spend never goes negative.
		g.budget.WindowBaseline = totalTokens
	}
	g.budget.CurrentSpend = totalTokens - g.budget.WindowBaseline

	// ByAgent/ByModel remain lifetime totals: window-relative breakdowns
	// would need per-key baselines and these maps are informational only.
	for k, v := range byAgent {
		g.budget.ByAgent[k] = v
	}
	for k, v := range byModel {
		g.budget.ByModel[k] = v
	}

	// WeeklyLimit == 0 disables budgeting entirely: no thresholds, no alerts.
	if g.budget.WeeklyLimit > 0 {
		warnThreshold := g.budget.WeeklyLimit * int64(g.budgetWarnPct()) / percentDenominator
		trans.WarnActive = g.budget.CurrentSpend >= warnThreshold
		trans.ExhaustedActive = g.budget.CurrentSpend >= g.budget.WeeklyLimit
		if trans.WarnActive && !g.budgetWarned {
			g.budgetWarned = true
			trans.WarnCrossed = true
		}
		if trans.ExhaustedActive && !g.budgetExhaustedAlerted {
			g.budgetExhaustedAlerted = true
			trans.ExhaustedCrossed = true
		}
	}

	return trans
}

// SeedBudgetWindowBaseline restores the window baseline from a persisted
// snapshot so the current window's spend survives restarts.
func (g *Governor) SeedBudgetWindowBaseline(baseline int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.budget.WindowBaseline = baseline
}

func (g *Governor) SetBudgetLimit(limit int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.budget.WeeklyLimit = limit
}

// SetBudgetIgnoreAll toggles the global budget-suppression bypass.
func (g *Governor) SetBudgetIgnoreAll(ignore bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.budget.IgnoreAll = ignore
}

func (g *Governor) SetBudgetIgnored(agents []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.budget.IgnoredAgents = make([]string, len(agents))
	copy(g.budget.IgnoredAgents, agents)
}

func (g *Governor) SetBudgetResetAt(t time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.budget.ResetAt = t
}
