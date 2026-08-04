package governor

import (
	"fmt"
	"log/slog"
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
	Agent    string
	Interval time.Duration
	Paused   bool
}

type ModeChange struct {
	Timestamp time.Time `json:"timestamp"`
	From      Mode      `json:"from"`
	To        Mode      `json:"to"`
	Reason    string    `json:"reason"`
}

type EvalSnapshot struct {
	Timestamp     int64             `json:"t"`
	Mode          Mode              `json:"govMode"`
	QueueIssues   int               `json:"govIssues"`
	QueuePRs      int               `json:"govPrs"`
	QueueTotal    int               `json:"govTotal"`
	QueueHold     int               `json:"govHold"`
	QueueActive   int               `json:"govActive"`
	SLAViolations int               `json:"sla_violations,omitempty"`
	AgentsKicked  []string          `json:"agents_kicked,omitempty"`
	Actionable    int               `json:"actionableCount"`
	OpenPRs       int               `json:"openPrCount"`
	Mergeable     int               `json:"mergeableCount"`
	BeadsWorkers  int               `json:"beadsWorkers"`
	BeadsSupervisor int             `json:"beadsSupervisor"`
	Repos         map[string]RepoSnapshot `json:"repos,omitempty"`
	AgentStats    map[string]map[string]any `json:"agentStats,omitempty"`
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
	ResetAt       time.Time        `json:"reset_at"`
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
}

const (
	modeHistoryCapacity = 100
	evalHistoryCapacity = 200
	kickHistoryCapacity = 500
)

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
			} else if cadence.Interval > 0 {
				g.logger.Info("agent cadence: active",
					"agent", agentName,
					"mode", g.state.Mode,
					"interval", cadence.Interval.String(),
				)
			}
		}
	}

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
	if mode, ok := g.cfg.Modes[modeName]; ok {
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

func (g *Governor) updateCadences() {
	modeName := modeToConfigKey(g.state.Mode)
	modeConfig, ok := g.cfg.Modes[modeName]
	if !ok {
		return
	}

	for agentName := range g.agents {
		cadenceStr, ok := modeConfig.Cadences[agentName]
		if !ok {
			continue
		}

		if cadenceStr == "pause" || cadenceStr == "paused" {
			g.state.Cadences[agentName] = AgentCadence{
				Agent:  agentName,
				Paused: true,
			}
			continue
		}

		dur, err := time.ParseDuration(cadenceStr)
		if err != nil {
			g.logger.Warn("invalid cadence duration",
				"agent", agentName,
				"mode", g.state.Mode,
				"value", cadenceStr,
				"error", err,
			)
			continue
		}

		g.state.Cadences[agentName] = AgentCadence{
			Agent:    agentName,
			Interval: dur,
		}
	}
}

func (g *Governor) agentsDueForKick() []string {
	now := time.Now()
	var due []string

	for agentName, cadence := range g.state.Cadences {
		if cadence.Paused {
			continue
		}
		if cadence.Interval == 0 {
			continue
		}
		if ac, ok := g.agents[agentName]; ok && ac.OnDemand {
			continue
		}

		lastKick, kicked := g.state.LastKick[agentName]
		if !kicked || now.Sub(lastKick) >= cadence.Interval {
			due = append(due, agentName)
		}
	}

	return due
}

func (g *Governor) RecordKick(agentName string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
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
		Mode:          g.state.Mode,
		QueueIssues:   g.state.QueueIssues,
		QueuePRs:      g.state.QueuePRs,
		QueueHold:     g.state.QueueHold,
		Cadences:      cadences,
		LastKick:      lastKick,
		LastEval:      g.state.LastEval,
		SLAViolations: g.state.SLAViolations,
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
		WeeklyLimit:   g.budget.WeeklyLimit,
		CurrentSpend:  g.budget.CurrentSpend,
		ByAgent:       byAgent,
		ByModel:       byModel,
		IgnoredAgents: ignored,
		ResetAt:       g.budget.ResetAt,
	}
}

func (g *Governor) UpdateBudget(totalTokens int64, byAgent map[string]int64, byModel map[string]int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.budget.CurrentSpend = totalTokens
	for k, v := range byAgent {
		g.budget.ByAgent[k] = v
	}
	for k, v := range byModel {
		g.budget.ByModel[k] = v
	}
}

func (g *Governor) SetBudgetLimit(limit int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.budget.WeeklyLimit = limit
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
