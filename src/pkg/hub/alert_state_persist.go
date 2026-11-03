package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Hub self-upgrades are instant (saas_sha_poller.go "hub auto-upgrade
// triggered"), so the hub process can restart several times in an hour —
// often while a fleet wave is rolling. alertState used to live only in
// memory, so every hub restart forgot which spoke start times it had already
// seen, which of them were upgrade rollouts, and when each condition began.
// The first evaluations after a restart then manufactured fleet alerts
// (#11826). This file persists that bookkeeping next to the acks file and
// defines the hub-restart grace window that keeps the rest quiet.

// alertStatePath is the on-disk file holding the crash-loop / condition
// bookkeeping. A var (not a const) so tests can point it at a temp dir.
var alertStatePath = "/data/saas/hub-alert-state.json"

const (
	// alertStateSaveInterval is how often the persistence loop flushes the
	// bookkeeping to disk (only when it changed).
	alertStateSaveInterval = time.Minute

	// alertHubRestartGrace is how long after the hub (re)starts that a NEW
	// crash-loop or offline condition is not raised. It covers the hub's own
	// downtime plus a few spoke heartbeats to refill registry freshness. A
	// condition that was already firing before the restart (its FirstSeen
	// was persisted) keeps firing — only alerts the restart itself could
	// have manufactured are held back.
	alertHubRestartGrace = 10 * time.Minute
)

// persistedAlertState is the JSON shape of alertStatePath.
type persistedAlertState struct {
	StartTimes    map[string][]alertStartObservation `json:"startTimes,omitempty"`
	UpgradeStarts map[string][]time.Time             `json:"upgradeStarts,omitempty"`
	FirstSeen     map[string]time.Time               `json:"firstSeen,omitempty"`
}

// markHubStart records when this hub process began serving, opening the
// restart grace window. Nil-safe.
func (a *alertState) markHubStart(at time.Time) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hubStartedAt = at
}

// suppressedByHubRestart reports whether a (hive, type) condition should be
// held back because the hub only just restarted and has no pre-restart
// record of it. A condition with a persisted FirstSeen is never suppressed.
func (a *alertState) suppressedByHubRestart(hiveID, alertType string, now time.Time) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hubStartedAt.IsZero() || now.Sub(a.hubStartedAt) >= alertHubRestartGrace {
		return false
	}
	_, known := a.firstSeen[alertAckKey(hiveID, alertType)]
	return !known
}

// snapshotBookkeeping deep-copies the restart-sensitive bookkeeping.
func (a *alertState) snapshotBookkeeping() persistedAlertState {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := persistedAlertState{
		StartTimes:    make(map[string][]alertStartObservation, len(a.startTimes)),
		UpgradeStarts: make(map[string][]time.Time, len(a.upgradeStarts)),
		FirstSeen:     make(map[string]time.Time, len(a.firstSeen)),
	}
	for k, v := range a.startTimes {
		out.StartTimes[k] = append([]alertStartObservation(nil), v...)
	}
	for k, v := range a.upgradeStarts {
		out.UpgradeStarts[k] = append([]time.Time(nil), v...)
	}
	for k, v := range a.firstSeen {
		out.FirstSeen[k] = v
	}
	return out
}

// loadBookkeeping restores persisted bookkeeping, dropping entries that have
// aged past every window that could still use them. Entries already present
// in memory win (loadAcks restores FirstSeen from the ack, which must keep
// matching it).
func (a *alertState) loadBookkeeping(p persistedAlertState, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for hiveID, obs := range p.StartTimes {
		if hiveID == "" {
			continue
		}
		if _, ok := a.startTimes[hiveID]; ok {
			continue
		}
		kept := make([]alertStartObservation, 0, len(obs))
		for _, o := range obs {
			if o.StartedAt.IsZero() || now.Sub(o.ObservedAt) > alertCrashLoopWindow {
				continue
			}
			kept = append(kept, o)
		}
		if len(kept) > alertStartTimesRetained {
			kept = kept[len(kept)-alertStartTimesRetained:]
		}
		if len(kept) > 0 {
			a.startTimes[hiveID] = kept
		}
	}
	for hiveID, marks := range p.UpgradeStarts {
		if hiveID == "" {
			continue
		}
		if _, ok := a.upgradeStarts[hiveID]; ok {
			continue
		}
		if kept := pruneUpgradeStarts(marks, now); len(kept) > 0 {
			a.upgradeStarts[hiveID] = kept
		}
	}
	for key, t := range p.FirstSeen {
		if t.IsZero() || alertAckKeyHive(key) == "" {
			continue
		}
		if _, ok := a.firstSeen[key]; ok {
			continue
		}
		a.firstSeen[key] = t
	}
}

// alertStateFileMu serialises writers of alertStatePath for the reason
// alertAcksFileMu exists.
var alertStateFileMu sync.Mutex

// saveAlertState persists the bookkeeping atomically (tmp+rename), skipping
// the write when the bytes are unchanged. Failures are logged, not fatal.
func (s *HubServer) saveAlertState() {
	if s == nil || s.alerts == nil {
		return
	}
	alertStateFileMu.Lock()
	defer alertStateFileMu.Unlock()
	snap := s.alerts.snapshotBookkeeping()
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		s.logger.Error("failed to marshal alert state for persistence", "error", err)
		return
	}
	prev, readErr := os.ReadFile(alertStatePath)
	if readErr == nil && bytes.Equal(prev, data) {
		return
	}
	if os.IsNotExist(readErr) && len(snap.StartTimes) == 0 && len(snap.UpgradeStarts) == 0 && len(snap.FirstSeen) == 0 {
		// Nothing to remember and nothing stale on disk to overwrite.
		return
	}
	if err := os.MkdirAll(filepath.Dir(alertStatePath), 0o755); err != nil {
		s.logger.Error("failed to create alert state dir", "error", err)
		return
	}
	tmpPath := alertStatePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		s.logger.Error("failed to write alert state tmp file", "error", err)
		return
	}
	if err := os.Rename(tmpPath, alertStatePath); err != nil {
		s.logger.Error("failed to rename alert state file", "error", err)
	}
}

// loadAlertState restores persisted bookkeeping on startup. A missing file is
// normal; a corrupt one is quarantined (same suffix as the acks file) so the
// hub starts fresh instead of re-logging on every boot.
func (s *HubServer) loadAlertState() {
	if s == nil || s.alerts == nil {
		return
	}
	data, err := os.ReadFile(alertStatePath)
	if err != nil {
		if !os.IsNotExist(err) {
			s.logger.Error("failed to read persisted alert state", "error", err)
		}
		return
	}
	var stored persistedAlertState
	if err := json.Unmarshal(data, &stored); err != nil {
		quarantine := alertStatePath + alertAcksQuarantineSuffix
		if renameErr := os.Rename(alertStatePath, quarantine); renameErr != nil {
			s.logger.Error("failed to quarantine corrupt alert state file",
				"error", renameErr, "parseError", err)
			return
		}
		s.logger.Error("persisted alert state was corrupt — quarantined and starting fresh",
			"error", err, "quarantined", quarantine)
		return
	}
	s.alerts.loadBookkeeping(stored, time.Now())
}

// startAlertStatePersistence opens the hub-restart grace window and flushes
// the bookkeeping every alertStateSaveInterval until ctx is cancelled, with a
// final flush on the way out. Launched by StartBackgroundPollers, so only the
// production lifecycle (never a plain test server) gets the grace window.
func (s *HubServer) startAlertStatePersistence(ctx context.Context) {
	if s == nil || s.alerts == nil {
		return
	}
	s.alerts.markHubStart(time.Now())
	ticker := time.NewTicker(alertStateSaveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.saveAlertState()
			return
		case <-ticker.C:
			s.saveAlertState()
		}
	}
}
