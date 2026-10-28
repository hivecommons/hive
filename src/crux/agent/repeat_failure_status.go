package agent

import (
	"context"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
)

// Operator surface of the repeat-failure check (hivecommons/hive#14550, C11,
// C12, C14; issue #14558): the collector feeds scrubbed command records to
// the detector, and every collection refreshes each agent's
// RepeatFailureStatus, the field the dashboard row renders. It is a separate
// field on purpose: watchdog conditions are a fixed set the reconciler owns,
// and BusyCondition is one recomputed string. Monitor only (C10): nothing
// here pauses, kicks or interrupts an agent.

// RepeatFailureCheckState is an agent's repeat-failure check state. Empty
// means the check is off, and the dashboard shows nothing (C14). Defined in
// package crux so observers (dashboard, hub) read it without importing
// this package.
type RepeatFailureCheckState = crux.RepeatFailureCheckState

const (
	RepeatFailureCheckOn         = crux.RepeatFailureCheckOn
	RepeatFailureCheckNotCovered = crux.RepeatFailureCheckNotCovered
	RepeatFailureCheckNoData     = crux.RepeatFailureCheckNoData
)

// Row texts for the check states (C5, C11, C12).
const (
	RepeatFailureCheckOnMessage         = crux.RepeatFailureCheckOnMessage
	RepeatFailureCheckNotCoveredMessage = crux.RepeatFailureCheckNotCoveredMessage
	RepeatFailureCheckNoDataMessage     = crux.RepeatFailureCheckNoDataMessage
)

// repeatFailureNoteCommandMax bounds the command shown in a row note, in runes.
const repeatFailureNoteCommandMax = 160

// RepeatFailureStatus is what an agent's dashboard row shows for the check.
// The zero value (check off) shows nothing.
type RepeatFailureStatus = crux.RepeatFailureStatus

// FormatRepeatFailureNote renders the row note for the detector's notes,
// newest first: the most recent command and how many others repeat.
func FormatRepeatFailureNote(notes []RepeatFailureNote, window time.Duration) string {
	if len(notes) == 0 {
		return ""
	}
	n := notes[0]
	result := "(failed)"
	if n.Result.ExitCode != nil {
		result = fmt.Sprintf("(exit %d)", *n.Result.ExitCode)
	}
	minutes := int((window + time.Minute - 1) / time.Minute)
	note := fmt.Sprintf("Repeating failure: `%s` failed %d times in %d min %s", truncateRepeatFailureCommand(n.Command, repeatFailureNoteCommandMax), n.Count, minutes, result)
	switch others := len(notes) - 1; {
	case others == 1:
		note += " and 1 other"
	case others > 1:
		note += fmt.Sprintf(" and %d others", others)
	}
	return note
}

func truncateRepeatFailureCommand(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	r := []rune(s)
	return string(r[:limit-1]) + "…"
}

// RepeatFailureToolKind classifies a tool call a backend reports for the
// not-receiving-data safeguard (C12). Only shell calls count: file reads and
// edits alone are not shell-tool activity.
type RepeatFailureToolKind int

const (
	RepeatFailureToolShell RepeatFailureToolKind = iota + 1
	RepeatFailureToolFileRead
	RepeatFailureToolFileEdit
	RepeatFailureToolOther
)

// repeatFailureAgentState is the safeguard's view of one agent.
type repeatFailureAgentState struct {
	lastRecord time.Time
	// kick is the turn the shell activity below belongs to.
	kick time.Time
	// running counts shell calls started and not yet finished in kick.
	running int
	// pendingSince is the first shell call that finished in kick with no
	// command record since. A still-running command never sets it.
	pendingSince time.Time
}

// repeatFailureMonitor holds the detector and the per-agent safeguard state.
// Lock order: Manager.mu before repeatFailureMonitor.mu, never the reverse.
type repeatFailureMonitor struct {
	mu          sync.Mutex
	detector    *RepeatFailureDetector
	detectorKey string
	window      time.Duration
	agents      map[string]*repeatFailureAgentState
	// copilot holds each Copilot agent's session-log reader
	// (repeat_failure_copilot.go).
	copilot map[string]*copilotRecordReader
}

// SetRepeatFailureConfigResolver injects the live repeat_failure block so the
// detector and safeguard follow config edits without a restart. A nil
// resolver (or a nil block) uses the defaults. The on/off switch stays with
// SetRepeatFailureEnabledResolver.
func (m *Manager) SetRepeatFailureConfigResolver(fn func() *config.RepeatFailureConfig) {
	if fn == nil {
		m.repeatFailureConfigResolver.Store(nil)
		return
	}
	m.repeatFailureConfigResolver.Store(&fn)
}

// repeatFailureConfig returns the live block with the switch forced on; only
// called once repeatFailureEnabled has said the check is on.
func (m *Manager) repeatFailureConfig() *config.RepeatFailureConfig {
	cfg := config.RepeatFailureConfig{}
	if fnp := m.repeatFailureConfigResolver.Load(); fnp != nil && *fnp != nil {
		if live := (*fnp)(); live != nil {
			cfg = *live
		}
	}
	cfg.Enabled = true
	return &cfg
}

// StartRepeatFailureMonitor runs RunRepeatFailureMonitor on a ticker at
// RepeatFailureCollectInterval until ctx ends. While the check is off each
// tick only clears the row state, touching no file.
func (m *Manager) StartRepeatFailureMonitor(ctx context.Context) {
	ticker := time.NewTicker(RepeatFailureCollectInterval)
	defer ticker.Stop()
	m.RunRepeatFailureMonitor(ctx, ticker.C)
}

// RunRepeatFailureMonitor collects command records and refreshes every
// agent's RepeatFailureStatus on each tick until ctx ends. Production passes
// a ticker at RepeatFailureCollectInterval; tests call
// CollectRepeatFailures directly.
func (m *Manager) RunRepeatFailureMonitor(ctx context.Context, tick <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick:
			m.CollectRepeatFailures(now)
		}
	}
}

// CollectRepeatFailures runs one collection and refreshes the row status. With
// the check off it touches no file and clears every status, so no row shows a
// note or check state (C14).
func (m *Manager) CollectRepeatFailures(now time.Time) {
	mon := &m.repeatFailure
	if !m.repeatFailureEnabled() {
		mon.mu.Lock()
		mon.detector, mon.detectorKey, mon.agents, mon.copilot = nil, "", nil, nil
		mon.mu.Unlock()
		m.mu.Lock()
		for _, a := range m.agents {
			a.RepeatFailure = RepeatFailureStatus{}
		}
		m.mu.Unlock()
		return
	}
	cfg := m.repeatFailureConfig()
	mon.configure(cfg)
	m.readCopilotRepeatFailureRecords()
	collector := NewRepeatFailureCollector(cfg, m.repeatFailureRecordPaths, func(rec RepeatFailureRecord) {
		m.recordRepeatFailure(rec, now)
	}, m.logger)
	collector.Collect()
	m.refreshRepeatFailureStatus(cfg.EffectiveNoDataAfter(), now)
}

// configure (re)builds the detector when threshold, window or exclusions
// change. Exclusions are normalized like commands (C9).
func (mon *repeatFailureMonitor) configure(cfg *config.RepeatFailureConfig) {
	exclude := make([]string, 0, len(cfg.EffectiveExclude()))
	for _, p := range cfg.EffectiveExclude() {
		if p = NormalizeRepeatFailureCommand(p); p != "" {
			exclude = append(exclude, p)
		}
	}
	key := fmt.Sprintf("%d|%s|%q", cfg.EffectiveThreshold(), cfg.EffectiveWindow(), exclude)
	mon.mu.Lock()
	defer mon.mu.Unlock()
	if mon.detector == nil || mon.detectorKey != key {
		mon.detector = NewRepeatFailureDetector(cfg.EffectiveThreshold(), cfg.EffectiveWindow(), exclude)
		mon.detectorKey = key
		mon.window = cfg.EffectiveWindow()
	}
	if mon.agents == nil {
		mon.agents = make(map[string]*repeatFailureAgentState)
	}
}

// repeatFailureRecordPaths maps each agent to its raw record file. Agents
// that share a HOME (contributor local mode) share a file whose records
// cannot be told apart, so they are skipped rather than misattributed.
func (m *Manager) repeatFailureRecordPaths() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	paths := make(map[string]string, len(m.agents))
	owners := make(map[string]int, len(m.agents))
	for name, a := range m.agents {
		home := AgentHome(name, a.UID, effectiveBackend(a))
		if home == "" {
			continue
		}
		p := RepeatFailureRecordPath(home)
		paths[name] = p
		owners[p]++
	}
	for name, p := range paths {
		if owners[p] > 1 {
			delete(paths, name)
		}
	}
	return paths
}

// recordRepeatFailure feeds one scrubbed record to the detector. A raised
// note is logged once, when it is raised (C11). now is the collection time, so
// records reach the detector in arrival order whatever their own clock says.
func (m *Manager) recordRepeatFailure(rec RepeatFailureRecord, now time.Time) {
	command := rec.Command
	if command == "" {
		command = NormalizeRepeatFailureCommand(rec.RawCommand)
	}
	result := RepeatFailureResult{ExitCode: rec.ExitCode, Failed: rec.Failed}
	mon := &m.repeatFailure
	mon.mu.Lock()
	st := mon.agentState(rec.Agent)
	st.lastRecord = now
	st.pendingSince = time.Time{}
	var note *RepeatFailureNote
	if mon.detector != nil {
		note = mon.detector.Record(rec.Agent, command, result, now)
	}
	window := mon.window
	mon.mu.Unlock()
	if note != nil {
		m.logger.Warn("repeat-failure: agent is repeating a failing command (monitor only; the agent is not paused)",
			"agent", rec.Agent, "backend", rec.Backend, "route", rec.Route,
			"note", FormatRepeatFailureNote([]RepeatFailureNote{*note}, window))
	}
}

// agentState returns the safeguard state for agent. Caller holds mon.mu.
func (mon *repeatFailureMonitor) agentState(agent string) *repeatFailureAgentState {
	if mon.agents == nil {
		mon.agents = make(map[string]*repeatFailureAgentState)
	}
	st := mon.agents[agent]
	if st == nil {
		st = &repeatFailureAgentState{}
		mon.agents[agent] = st
	}
	return st
}

// ObserveRepeatFailureTool reports one tool call of a covered backend's turn
// to the not-receiving-data safeguard (C12): finished false when the call
// starts, true when it ends. Only shell calls count; a still-running command
// does not start the timer. Each covered backend's producer reports the
// shell-activity signal named in its coverage-table row.
func (m *Manager) ObserveRepeatFailureTool(agentName string, kind RepeatFailureToolKind, finished bool, at time.Time) {
	if kind != RepeatFailureToolShell || !m.repeatFailureEnabled() {
		return
	}
	m.mu.RLock()
	a := m.agents[agentName]
	var kick time.Time
	if a != nil && a.LastKick != nil {
		kick = *a.LastKick
	}
	m.mu.RUnlock()
	if a == nil {
		return
	}
	mon := &m.repeatFailure
	mon.mu.Lock()
	defer mon.mu.Unlock()
	st := mon.agentState(agentName)
	if !st.kick.Equal(kick) {
		st.kick, st.running, st.pendingSince = kick, 0, time.Time{}
	}
	if !finished {
		st.running++
		return
	}
	if st.running > 0 {
		st.running--
	}
	if st.pendingSince.IsZero() && at.After(st.lastRecord) {
		st.pendingSince = at
	}
}

// refreshRepeatFailureStatus recomputes every agent's row status and forgets
// agents that are gone.
func (m *Manager) refreshRepeatFailureStatus(noDataAfter time.Duration, now time.Time) {
	mon := &m.repeatFailure
	m.mu.Lock()
	defer m.mu.Unlock()
	mon.mu.Lock()
	defer mon.mu.Unlock()
	for name := range mon.agents {
		if _, ok := m.agents[name]; !ok {
			delete(mon.agents, name)
			if mon.detector != nil {
				mon.detector.Forget(name)
			}
		}
	}
	for name, a := range m.agents {
		var notes []RepeatFailureNote
		if mon.detector != nil {
			notes = mon.detector.Notes(name, now)
		}
		a.RepeatFailure = RepeatFailureStatus{
			Check: repeatFailureCheckState(RepeatFailureBackendCovered(effectiveBackend(a)), repeatFailureTurnKick(a), mon.agents[name], noDataAfter, now),
			Note:  FormatRepeatFailureNote(notes, mon.window),
		}
	}
}

// repeatFailureTurnKick returns the kick of the agent's turn in progress, or
// nil when no turn is running. Caller holds m.mu.
func repeatFailureTurnKick(a *AgentProcess) *time.Time {
	if a.State != StateRunning || a.LastKick == nil || a.KickOutcome.Settled(a.LastKick) {
		return nil
	}
	return a.LastKick
}

// repeatFailureCheckState decides an agent's check state (C5, C12). A covered
// agent is "not receiving data" only when its turn in progress finished a
// shell call at least noDataAfter ago, no record has arrived since, and no
// shell call is still running.
func repeatFailureCheckState(covered bool, turnKick *time.Time, st *repeatFailureAgentState, noDataAfter time.Duration, now time.Time) RepeatFailureCheckState {
	if !covered {
		return RepeatFailureCheckNotCovered
	}
	if turnKick == nil || st == nil || !st.kick.Equal(*turnKick) || st.running > 0 || st.pendingSince.IsZero() {
		return RepeatFailureCheckOn
	}
	if now.Sub(st.pendingSince) >= noDataAfter {
		return RepeatFailureCheckNoData
	}
	return RepeatFailureCheckOn
}
