package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux"
)

const (
	busyNoActivityCondition        = "busy-no-activity"
	busyOverCeilingCondition       = "busy-over-ceiling"
	copilotQuestionFormCondition   = "copilot-question-form"
	busyUndeliverableThreshold     = 3
	transcriptLivenessScanInterval = 10 * time.Second
	// transcriptHotSessionAge bounds which Copilot sessions the refresher
	// re-stats every scan: a session whose events.jsonl changed within this
	// window may still be live. Older sessions are ended CLI runs whose
	// cached mtime is trusted until the next full rescan, so a home that
	// has accumulated thousands of sessions costs a handful of stats per
	// tick rather than one NFS GETATTR per session (hivecommons/hive: the
	// heartbeat collect spent 8–12 s in this walk on a 16k-session hive).
	transcriptHotSessionAge = 6 * time.Hour
	// transcriptFullRescanInterval is how often every session, cold ones
	// included, is re-stat'd, so a resumed old session is noticed well
	// inside the busy-no-activity threshold (60m by default).
	transcriptFullRescanInterval = 10 * time.Minute
	// busyNoActivityRecoveryKicks and busyNoActivityRecoveryFactor bound when
	// a busy-no-activity pane stops being visibility-only and is restarted
	// (#12505): twice the kicks that raise the condition, and twice the
	// configured silence threshold (60m by default). A live turn writes
	// transcript events, so only a wedged CLI clears both bars.
	busyNoActivityRecoveryKicks  = 2 * busyUndeliverableThreshold
	busyNoActivityRecoveryFactor = 2
)

const copilotQuestionFormMessage = "Copilot CLI asked the user a question; no human is attached — dismissed with Escape so the kick can be delivered"

type transcriptActivityScan struct {
	name string
	uid  int
}

type transcriptActivityResult struct {
	name string
	at   time.Time
	ok   bool
}

// transcriptSessionCache remembers, per agent, the events.jsonl mtime last
// observed for each session directory (zero when the directory has no
// events.jsonl) and when every session was last re-stat'd. Owned by the
// refresher goroutine; never shared.
type transcriptSessionCache struct {
	sessions map[string]time.Time
	lastFull time.Time
}

// newestCopilotTranscriptActivity reports the newest events.jsonl mtime under
// the agent's own Copilot session-state tree. Sub-agents write separate
// session directories under the same HOME, so scanning every session catches
// their activity without parsing JSON. Every session is stat'd; the refresher
// uses newestCopilotTranscriptActivityCached to skip cold ones.
func newestCopilotTranscriptActivity(home string) (time.Time, bool) {
	return newestCopilotTranscriptActivityCached(home, nil, time.Time{})
}

// newestCopilotTranscriptActivityCached is newestCopilotTranscriptActivity
// with a per-agent cache: between full rescans, a session whose cached mtime
// is older than transcriptHotSessionAge is trusted rather than stat'd. A
// session with no events.jsonl yet (zero cached mtime) is always re-stat'd:
// the CLI creates the directory a moment before the first event lands, and
// trusting that empty read would hide a brand-new session until the next
// full rescan. Sessions that disappeared from the directory are dropped from
// the cache. A nil cache stats everything.
func newestCopilotTranscriptActivityCached(home string, cache *transcriptSessionCache, now time.Time) (time.Time, bool) {
	root := filepath.Join(home, ".copilot", "session-state")
	entries, err := os.ReadDir(root)
	if err != nil {
		return time.Time{}, false
	}
	full := cache == nil || cache.lastFull.IsZero() || now.Sub(cache.lastFull) >= transcriptFullRescanInterval
	var seen map[string]bool
	if cache != nil {
		if cache.sessions == nil {
			cache.sessions = make(map[string]time.Time, len(entries))
		}
		seen = make(map[string]bool, len(entries))
	}
	var newest time.Time
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if cache != nil {
			seen[name] = true
			if prev, ok := cache.sessions[name]; ok && !full && !prev.IsZero() && now.Sub(prev) > transcriptHotSessionAge {
				if prev.After(newest) {
					newest = prev
				}
				continue
			}
		}
		var mod time.Time
		if info, err := os.Stat(filepath.Join(root, name, "events.jsonl")); err == nil {
			mod = info.ModTime()
		}
		if cache != nil {
			cache.sessions[name] = mod
		}
		if mod.After(newest) {
			newest = mod
		}
	}
	if cache != nil {
		for name := range cache.sessions {
			if !seen[name] {
				delete(cache.sessions, name)
			}
		}
		if full {
			cache.lastFull = now
		}
	}
	if newest.IsZero() {
		return time.Time{}, false
	}
	return newest, true
}

// markKickUndeliverableLocked records that a kick attempt found the pane busy
// or otherwise unsafe for input. reason says why delivery failed and is carried
// in the self-report. Caller holds m.mu.
func (m *Manager) markKickUndeliverableLocked(agent *AgentProcess, now time.Time, reason string) {
	if agent.KicksUndeliverable == 0 {
		agent.BusySince = now
	}
	agent.KicksUndeliverable++
	m.refreshBusyVisibilityLocked(agent, now)
	if agent.BusyCondition != "" {
		reason = agent.BusyCondition
	}
	if reason == "" {
		reason = "unknown"
	}
	// A pane that is busy because the agent is mid-turn is not a stall
	// (#12268): the governor re-kicks an agent whose previous turn is still
	// streaming, each attempt waits out the prompt timeout, and three of those
	// inside one long review turn used to file an upstream issue that cleared
	// itself the moment the turn ended. Only self-report once the agent's own
	// transcript has been silent for the busy-no-activity threshold, the same
	// bar busyCondition applies before it calls the pane stuck.
	silentFor, live := kickUndeliverableTranscriptLiveness(agent, now)
	if live {
		if m.logger != nil {
			m.logger.Info("kick undeliverable while agent turn in progress; not self-reporting",
				"agent", agent.Name,
				"reason", reason,
				"kicks_undeliverable", agent.KicksUndeliverable,
				"transcript_silent_for", roundDurationForHumans(silentFor))
		}
		return
	} else if silentFor > 0 {
		reason = fmt.Sprintf("%s; transcript silent for %s", reason, roundDurationForHumans(silentFor))
	}
	// Decision B on #12787: a stall that busyNoActivityStall is about to
	// restart is not yet a defect worth a fleet issue — most self-recover.
	// Until that restart has been attempted the condition stays a dashboard
	// warning (BusyCondition) and a log line; only kicks that keep failing
	// after the restart are self-reported.
	if agent.busyNoActivityRestartAt.IsZero() {
		if m.logger != nil {
			m.logger.Warn("kick undeliverable; busy-no-activity restart not yet attempted, not self-reporting",
				"agent", agent.Name,
				"reason", reason,
				"kicks_undeliverable", agent.KicksUndeliverable,
				"transcript_silent_for", roundDurationForHumans(silentFor))
		}
		return
	}
	// Self-report (#11868): the tracker applies the ≥3-consecutive threshold.
	m.observeCondition(crux.ClassKickFailedRepeatedly, agent.Name,
		busyNoActivityRestartFailedMessage(agent, now, reason), "")
}

// busyNoActivityRestartFailedMessage is the kick_failed_repeatedly self-report
// text once the stall restart has been attempted and kicks still fail. It
// carries the restart time and count so the fleet issue says the recovery
// already ran and did not help, not merely that kicks are failing.
func busyNoActivityRestartFailedMessage(agent *AgentProcess, now time.Time, reason string) string {
	restartAgo := now.Sub(agent.busyNoActivityRestartAt)
	if restartAgo < 0 {
		restartAgo = 0
	}
	return fmt.Sprintf("kick undeliverable %d times in a row after busy-no-activity restart did not recover the session (%s; restarted %s ago at %s, %d restart(s) this episode)",
		agent.KicksUndeliverable, reason, roundDurationForHumans(restartAgo),
		agent.busyNoActivityRestartAt.UTC().Format(time.RFC3339), agent.busyNoActivityRestarts)
}

// markBusyNoActivityRestartLocked records that busyNoActivityStall restarted
// the agent at now. Caller holds m.mu.
func (m *Manager) markBusyNoActivityRestartLocked(agent *AgentProcess, now time.Time) {
	agent.busyNoActivityRestartAt = now
	agent.busyNoActivityRestarts++
}

// reportBusyNoActivityRestartFailed self-reports a stall restart that itself
// errored: the recovery never ran, so there is nothing left to wait for.
func (m *Manager) reportBusyNoActivityRestartFailed(name, reason string, err error) {
	m.mu.RLock()
	agent, ok := m.agents[name]
	var kicks, restarts int
	if ok {
		kicks, restarts = agent.KicksUndeliverable, agent.busyNoActivityRestarts
	}
	m.mu.RUnlock()
	if !ok {
		return
	}
	m.observeCondition(crux.ClassKickFailedRepeatedly, name,
		fmt.Sprintf("kick undeliverable %d times in a row and busy-no-activity restart failed: %v (%s; %d restart(s) this episode)",
			kicks, err, reason, restarts), "")
}

// kickUndeliverableTranscriptLiveness reports how long the agent's transcript
// has been silent and whether that is short enough to call the busy pane a
// turn in progress. Agents without a transcript cache (non-Copilot backends,
// or a scan that has not run yet) report (0, false): the caller cannot tell a
// live turn from a stall, so it keeps self-reporting as before.
func kickUndeliverableTranscriptLiveness(agent *AgentProcess, now time.Time) (time.Duration, bool) {
	last := agent.LastTranscriptActivity
	if last.IsZero() {
		return 0, false
	}
	silentFor := now.Sub(last)
	if silentFor < 0 {
		silentFor = 0
	}
	return silentFor, silentFor < agent.Config.EffectiveBusyNoActivityThreshold()
}

// markCopilotQuestionFormLocked surfaces a question-form dismissal through the
// same dashboard busy-condition fields as other no-deliverable-kick states.
// Caller holds m.mu.
func (m *Manager) markCopilotQuestionFormLocked(agent *AgentProcess, now time.Time) {
	if agent.KicksUndeliverable == 0 {
		agent.BusySince = now
	}
	agent.KicksUndeliverable++
	agent.BusyCondition = copilotQuestionFormCondition
	agent.BusyConditionMessage = copilotQuestionFormMessage
	agent.busyConditionWarned = copilotQuestionFormCondition
}

// resetBusyVisibilityLocked clears the busy-delivery episode because a kick
// was delivered (or cancelled): the session is active again, so any
// kick_failed_repeatedly condition is cleared and the stall-restart marker
// dropped. Caller holds m.mu.
func (m *Manager) resetBusyVisibilityLocked(agent *AgentProcess) {
	if agent.KicksUndeliverable > 0 || !agent.busyNoActivityRestartAt.IsZero() {
		m.clearCondition(crux.ClassKickFailedRepeatedly, agent.Name)
	}
	agent.busyNoActivityRestartAt = time.Time{}
	agent.busyNoActivityRestarts = 0
	m.resetBusyCountersLocked(agent)
}

// resetBusyVisibilityForRestartLocked is the restart teardown's variant: the
// counters start over for the new session, but the stall-restart marker and
// any open self-report stay until a kick is actually delivered — a restart is
// the recovery attempt, not proof that it worked. Caller holds m.mu.
func (m *Manager) resetBusyVisibilityForRestartLocked(agent *AgentProcess) {
	m.resetBusyCountersLocked(agent)
}

func (m *Manager) resetBusyCountersLocked(agent *AgentProcess) {
	agent.KicksUndeliverable = 0
	agent.BusySince = time.Time{}
	agent.BusyCondition = ""
	agent.BusyConditionMessage = ""
	agent.busyConditionWarned = ""
}

// refreshBusyVisibilityLocked derives the visibility-only busy condition from
// cached transcript liveness. Caller holds m.mu.
func (m *Manager) refreshBusyVisibilityLocked(agent *AgentProcess, now time.Time) {
	condition, message := busyCondition(agent, now)
	agent.BusyCondition = condition
	agent.BusyConditionMessage = message
	if condition == "" {
		agent.busyConditionWarned = ""
		return
	}
	if agent.busyConditionWarned == condition {
		return
	}
	agent.busyConditionWarned = condition
	if m.logger == nil {
		return
	}
	m.logger.Warn("agent busy with no deliverable kick visibility condition",
		"agent", agent.Name,
		"condition", condition,
		"message", message,
		"kicks_undeliverable", agent.KicksUndeliverable,
		"busy_since", agent.BusySince.UTC().Format(time.RFC3339),
		"last_transcript_activity", formatTimeForLog(agent.LastTranscriptActivity))
}

// pendingTranscriptActivityScansLocked returns the Copilot agents whose
// transcript cache is stale. Caller holds m.mu; filesystem scans happen after
// releasing it.
func (m *Manager) pendingTranscriptActivityScansLocked(now time.Time) []transcriptActivityScan {
	scans := make([]transcriptActivityScan, 0, len(m.agents))
	for _, agent := range m.agents {
		if effectiveBackend(agent) != "copilot" {
			continue
		}
		if !agent.lastTranscriptScanAt.IsZero() && now.Sub(agent.lastTranscriptScanAt) < transcriptLivenessScanInterval {
			continue
		}
		scans = append(scans, transcriptActivityScan{name: agent.Name, uid: agent.UID})
	}
	return scans
}

// transcriptScanConcurrency bounds how many agents' session trees are walked
// at once. Each walk is dominated by network-filesystem round trips, so a few
// in parallel bring a many-agent pass back inside the scan interval without
// flooding the mount.
const transcriptScanConcurrency = 4

// scanAgentTranscript walks one agent's session tree; a variable so tests can
// observe how the passes are scheduled.
var scanAgentTranscript = func(scan transcriptActivityScan, cache *transcriptSessionCache, now time.Time) (time.Time, bool) {
	return newestCopilotTranscriptActivityCached(AgentHome(scan.name, scan.uid, "copilot"), cache, now)
}

// scanTranscriptActivity stats each agent's sessions, up to
// transcriptScanConcurrency agents at a time. caches is keyed by agent name
// and may be nil (every session is stat'd). Results keep the order of scans.
func scanTranscriptActivity(scans []transcriptActivityScan, caches map[string]*transcriptSessionCache, now time.Time) []transcriptActivityResult {
	results := make([]transcriptActivityResult, len(scans))
	perAgent := make([]*transcriptSessionCache, len(scans))
	for i, scan := range scans {
		if caches != nil {
			cache := caches[scan.name]
			if cache == nil {
				cache = &transcriptSessionCache{}
				caches[scan.name] = cache
			}
			perAgent[i] = cache
		}
	}
	sem := make(chan struct{}, transcriptScanConcurrency)
	var wg sync.WaitGroup
	for i, scan := range scans {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			at, ok := scanAgentTranscript(scan, perAgent[i], now)
			results[i] = transcriptActivityResult{name: scan.name, at: at, ok: ok}
		}()
	}
	wg.Wait()
	return results
}

// applyTranscriptActivityResultsLocked updates the transcript cache after an
// out-of-lock scan. Caller holds m.mu.
func (m *Manager) applyTranscriptActivityResultsLocked(results []transcriptActivityResult, scannedAt time.Time) {
	for _, result := range results {
		agent, ok := m.agents[result.name]
		if !ok {
			continue
		}
		agent.lastTranscriptScanAt = scannedAt
		if result.ok {
			agent.LastTranscriptActivity = result.at
		}
	}
}

// StartTranscriptActivityRefresh keeps every Copilot agent's
// LastTranscriptActivity fresh on its own goroutine, so readers such as
// AllStatuses (the dashboard status build and the hub heartbeat collect)
// only ever copy a cached value and never wait on the session-state walk.
// That walk stats one events.jsonl per session directory; on an NFS /data
// with thousands of accumulated sessions a cold walk takes 8–12 s, which
// blew the heartbeat's 10 s collect budget on every beat and left the hub
// reading the hive as unhealthy. Runs until ctx is cancelled.
func (m *Manager) StartTranscriptActivityRefresh(ctx context.Context) {
	ticker := time.NewTicker(transcriptLivenessScanInterval)
	defer ticker.Stop()
	m.runTranscriptActivityRefresh(ctx, ticker.C, time.Now)
}

// runTranscriptActivityRefresh is StartTranscriptActivityRefresh with the
// tick source and clock injected. It scans once immediately, then once per
// tick; scans never overlap because they run on this goroutine alone.
func (m *Manager) runTranscriptActivityRefresh(ctx context.Context, ticks <-chan time.Time, now func() time.Time) {
	caches := map[string]*transcriptSessionCache{}
	slow := false
	m.refreshTranscriptActivity(caches, now, &slow)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			m.refreshTranscriptActivity(caches, now, &slow)
		}
	}
}

// refreshTranscriptActivity runs one scan pass: pick the stale agents under
// a short read lock, stat their sessions with no lock held, then publish
// the results under a short write lock. Returns how many agents were
// scanned. slow tracks the previous pass's verdict so a walk that outlives
// the scan interval is logged on the transition, not every tick.
func (m *Manager) refreshTranscriptActivity(caches map[string]*transcriptSessionCache, now func() time.Time, slow *bool) int {
	started := now()
	m.mu.RLock()
	scans := m.pendingTranscriptActivityScansLocked(started)
	m.mu.RUnlock()
	if len(scans) == 0 {
		return 0
	}
	results := scanTranscriptActivity(scans, caches, started)
	m.mu.Lock()
	m.applyTranscriptActivityResultsLocked(results, started)
	live := make(map[string]bool, len(m.agents))
	for name := range m.agents {
		live[name] = true
	}
	m.mu.Unlock()
	for name := range caches {
		if !live[name] {
			delete(caches, name)
		}
	}
	if took := now().Sub(started); took > transcriptLivenessScanInterval {
		if slow != nil && !*slow && m.logger != nil {
			m.logger.Warn("copilot transcript activity scan outlived its interval; agent liveness reads may lag",
				"took", took.Round(time.Millisecond).String(), "agents", len(scans), "interval", transcriptLivenessScanInterval.String())
		}
		if slow != nil {
			*slow = true
		}
	} else if slow != nil {
		*slow = false
	}
	return len(results)
}

func busyCondition(agent *AgentProcess, now time.Time) (string, string) {
	threshold := agent.Config.EffectiveBusyNoActivityThreshold()
	lastActivity := agent.LastTranscriptActivity
	if lastActivity.IsZero() {
		lastActivity = agent.BusySince
	}
	silentFor := time.Duration(0)
	if !lastActivity.IsZero() {
		silentFor = now.Sub(lastActivity)
	}

	if ceiling := agent.Config.MaxTurnDuration; ceiling > 0 {
		start := agent.BusySince
		if !start.IsZero() && now.Sub(start) >= ceiling {
			return busyOverCeilingCondition, fmt.Sprintf(
				"pane shows Working for %s, exceeding max_turn_duration %s; %d kicks undeliverable",
				roundDurationForHumans(now.Sub(start)), roundDurationForHumans(ceiling), agent.KicksUndeliverable)
		}
	}

	if agent.KicksUndeliverable >= busyUndeliverableThreshold && !lastActivity.IsZero() && silentFor >= threshold {
		since := ""
		if !agent.BusySince.IsZero() {
			since = agent.BusySince.UTC().Format("15:04Z")
		}
		return busyNoActivityCondition, fmt.Sprintf(
			"pane shows Working but no transcript activity for %s; %d kicks undeliverable since %s",
			roundDurationForHumans(silentFor), agent.KicksUndeliverable, since)
	}

	return "", ""
}

// busyNoActivityStall reports why a pane that still shows Working or, for a
// live CLI, no input prompt, refuses every kick, and has produced no transcript
// activity for far longer than the busy-no-activity threshold should be
// restarted, or "" when it should not.
// Without this the condition was visibility-only: a wedged CLI refused kicks
// indefinitely and nothing broke the loop (#12505). Restarts share the
// session-liveness cooldown so a pane that wedges again is not churned.
//
// A live CLI pane that shows neither Working nor any input prompt is wedged
// the same way: every kick times out with "CLI did not reach input prompt"
// (#12916), and since #12860 only a restart attempt lets that self-report, so
// without this branch such a pane was neither recovered nor reported. A bare
// shell (no CLI marker) is left to crash recovery.
//
// A consent/blocking-prompt screen renders a "❯"-selected menu line, so it
// satisfies paneShowsInputPrompt, yet waitForInputPromptForAgentUnless never
// accepts it and every kick fails the same way (#12921). It is checked before
// the input-prompt test so it is not masked; only inference backends have a
// consent-dismissal path, so on every other backend it restarts here.
// Caller holds m.mu (read or write).
func busyNoActivityStall(agent *AgentProcess, pane string, now time.Time) string {
	if agent.KicksUndeliverable < busyNoActivityRecoveryKicks {
		return ""
	}
	paneState := "pane shows Working"
	if !paneShowsAgentWorking(pane) && !paneShowsActiveWork(pane) {
		switch {
		case !paneHasCLIMarker(pane):
			return ""
		case paneShowsConsentScreen(pane):
			// Inference backends re-run prompt dismissal for this instead
			// (dismissConsentIfStuck); a restart would loop to the same screen.
			if IsInferenceBackend(effectiveBackend(agent)) {
				return ""
			}
			paneState = "pane shows a consent or blocking prompt"
		case paneShowsInputPrompt(pane):
			return ""
		default:
			paneState = "pane shows no input prompt"
		}
	}
	if now.Sub(agent.lastSessionStallRestart) < sessionStallRestartCooldown {
		return ""
	}
	lastActivity := agent.LastTranscriptActivity
	if lastActivity.IsZero() {
		lastActivity = agent.BusySince
	}
	if lastActivity.IsZero() {
		return ""
	}
	silentFor := now.Sub(lastActivity)
	if silentFor < busyNoActivityRecoveryFactor*agent.Config.EffectiveBusyNoActivityThreshold() {
		return ""
	}
	return fmt.Sprintf("%s: %s with no transcript activity for %s; %d kicks undeliverable",
		busyNoActivityCondition, paneState, roundDurationForHumans(silentFor), agent.KicksUndeliverable)
}

func roundDurationForHumans(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	return d.Round(time.Minute).String()
}

func formatTimeForLog(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
