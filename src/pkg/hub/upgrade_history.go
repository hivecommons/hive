package hub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// upgradeHistoryDir is where the hub persists each hive's recent upgrade
// attempts, one file per hive beside the timeline store. A var so tests can
// point it at a temp dir.
var upgradeHistoryDir = "/data/saas/upgrade-history"

// defaultUpgradeHistoryMax is how many attempts are kept per hive. Two weeks
// of daily windows is enough to answer "when did this hive last actually
// roll, and what happened on the days it did not" without a log dive.
const defaultUpgradeHistoryMax = 14

// upgradeHistoryMax is the retention cap, overridable with
// HIVE_HUB_UPGRADE_HISTORY_MAX.
func upgradeHistoryMax() int {
	return envInt("HIVE_HUB_UPGRADE_HISTORY_MAX", defaultUpgradeHistoryMax)
}

// upgradeHistoryRecentFor bounds how long a closed attempt still speaks for
// the hive's current upgrade state: a window missed this morning reads as
// "missed" all day; by the time the next window opens it is history only.
const upgradeHistoryRecentFor = 24 * time.Hour

// hiveUpgradeHistory is the on-disk shape of one hive's history file.
type hiveUpgradeHistory struct {
	HiveID   string                 `json:"hive_id"`
	Attempts []spoke.UpgradeAttempt `json:"attempts"`
}

// upgradeHistoryStore records the outcome of every upgrade attempt and every
// held scheduled window per hive, so a missed window is never silent (#14856:
// three hives missed two daily windows each and nothing on either dashboard
// said so). Like timelineStore its mutex is a LEAF — nothing under it reaches
// for HubServer.mu — and it does its own file I/O, so callers may hold s.mu
// only when they already do I/O there (the hold recorder does).
type upgradeHistoryStore struct {
	mu   sync.RWMutex
	byID map[string][]spoke.UpgradeAttempt
}

func newUpgradeHistoryStore() *upgradeHistoryStore {
	return &upgradeHistoryStore{byID: make(map[string][]spoke.UpgradeAttempt)}
}

func upgradeHistoryPath(hiveID string) string {
	if hiveID == "" || !isValidName(hiveID) ||
		strings.Contains(hiveID, "..") || strings.ContainsAny(hiveID, `/\`) {
		return ""
	}
	return filepath.Join(upgradeHistoryDir, hiveID+".json")
}

// recent returns a display copy, oldest first, without resolved schedule holds.
func (u *upgradeHistoryStore) recent(hiveID string) []spoke.UpgradeAttempt {
	if u == nil {
		return nil
	}
	u.mu.RLock()
	defer u.mu.RUnlock()
	src := u.byID[hiveID]
	if len(src) == 0 {
		return nil
	}
	return spoke.UpgradeHistoryForDisplay(src, time.Time{})
}

// open returns the hive's currently open attempt, if any.
func (u *upgradeHistoryStore) open(hiveID string) (spoke.UpgradeAttempt, bool) {
	if u == nil {
		return spoke.UpgradeAttempt{}, false
	}
	u.mu.RLock()
	defer u.mu.RUnlock()
	list := u.byID[hiveID]
	if n := len(list); n > 0 && list[n-1].Open() {
		return list[n-1], true
	}
	return spoke.UpgradeAttempt{}, false
}

// start opens an attempt toward target. A retry of an attempt that is already
// open (same hive, still unresolved) does not open a second row: the hub
// re-arms the same instruction every stale cycle, and a re-targeted retry
// (the channel moved mid-attempt) is still the same attempt — the target is
// updated so the row names what finally landed.
func (u *upgradeHistoryStore) start(hiveID, from, target, trigger string, at time.Time) {
	if u == nil || hiveID == "" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	list := u.byID[hiveID]
	if n := len(list); n > 0 && list[n-1].Open() {
		if at.Sub(list[n-1].StartedAt) < upgradeHistoryRecentFor {
			if target != "" {
				list[n-1].Target = target
			}
			if list[n-1].From == "" {
				list[n-1].From = from
			}
			u.byID[hiveID] = list
			u.persistLocked(hiveID)
			return
		}
		// An attempt left open for a day was closed by a path that does not
		// report here (latch dropped on a channel switch, hub state reset…);
		// never let it swallow the new attempt.
		list[n-1].Outcome = spoke.UpgradeOutcomeMissed
		list[n-1].Reason = "superseded by a newer attempt"
		list[n-1].EndedAt = at.UTC()
	}
	list = append(list, spoke.UpgradeAttempt{
		Target:    target,
		From:      from,
		Trigger:   trigger,
		StartedAt: at.UTC(),
	})
	u.byID[hiveID] = trimUpgradeHistory(list, upgradeHistoryMax())
	u.persistLocked(hiveID)
}

// end closes the open attempt with an outcome. With nothing open, a done
// outcome records a one-row attempt (the hub observed a rollout it did not
// arm); a missed outcome with nothing open is dropped — there is no window
// to blame.
func (u *upgradeHistoryStore) end(hiveID, outcome, reason, from, target string, at time.Time) {
	if u == nil || hiveID == "" || outcome == "" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	list := u.byID[hiveID]
	n := len(list)
	if n > 0 && list[n-1].Open() {
		a := &list[n-1]
		a.Outcome = outcome
		a.Reason = reason
		a.EndedAt = at.UTC()
		if a.Target == "" {
			a.Target = target
		}
		if a.From == "" {
			a.From = from
		}
		u.byID[hiveID] = list
		u.persistLocked(hiveID)
		return
	}
	if outcome != spoke.UpgradeOutcomeDone {
		return
	}
	list = append(list, spoke.UpgradeAttempt{
		Target:    target,
		From:      from,
		Trigger:   spoke.UpgradeTriggerObserved,
		StartedAt: at.UTC(),
		EndedAt:   at.UTC(),
		Outcome:   outcome,
		Reason:    reason,
	})
	u.byID[hiveID] = trimUpgradeHistory(list, upgradeHistoryMax())
	u.persistLocked(hiveID)
}

// windowMissed records a daily/weekly window as delayed, not a failed attempt.
// One row per window: a hold reason that changes while the same window is
// open updates the row rather than adding another, and an attempt already
// open for this hive means the window did fire, so nothing is recorded.
func (u *upgradeHistoryStore) windowMissed(hiveID, from, reason string, at time.Time) {
	if u == nil || hiveID == "" || reason == "" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	list := u.byID[hiveID]
	if n := len(list); n > 0 {
		last := &list[n-1]
		if last.Open() {
			return
		}
		if last.Trigger == spoke.UpgradeTriggerWindow && sameUpgradeWindow(last.StartedAt, at) {
			last.Outcome = spoke.UpgradeOutcomeDelayed
			last.Reason = reason
			last.EndedAt = at.UTC()
			u.byID[hiveID] = list
			u.persistLocked(hiveID)
			return
		}
	}
	list = append(list, spoke.UpgradeAttempt{
		From:      from,
		Trigger:   spoke.UpgradeTriggerWindow,
		StartedAt: at.UTC(),
		EndedAt:   at.UTC(),
		Outcome:   spoke.UpgradeOutcomeDelayed,
		Reason:    reason,
	})
	u.byID[hiveID] = trimUpgradeHistory(list, upgradeHistoryMax())
	u.persistLocked(hiveID)
}

// forget drops a hive's history (hive deleted).
func (u *upgradeHistoryStore) forget(hiveID string) {
	if u == nil {
		return
	}
	u.mu.Lock()
	delete(u.byID, hiveID)
	u.mu.Unlock()
	if p := upgradeHistoryPath(hiveID); p != "" {
		_ = os.Remove(p)
	}
}

// sameUpgradeWindow reports whether two instants fall in the same scheduled
// window: the same calendar day in the schedule's timezone.
func sameUpgradeWindow(a, b time.Time) bool {
	loc, err := autoUpgradeLocation()
	if err != nil {
		loc = time.UTC
	}
	ay, am, ad := a.In(loc).Date()
	by, bm, bd := b.In(loc).Date()
	return ay == by && am == bm && ad == bd
}

func trimUpgradeHistory(list []spoke.UpgradeAttempt, max int) []spoke.UpgradeAttempt {
	if max < 1 {
		max = 1
	}
	if len(list) <= max {
		return list
	}
	trimmed := make([]spoke.UpgradeAttempt, max)
	copy(trimmed, list[len(list)-max:])
	return trimmed
}

// persistLocked writes one hive's history atomically. Caller holds u.mu.
// Failures are swallowed: history must never fail a heartbeat or a sweep.
func (u *upgradeHistoryStore) persistLocked(hiveID string) {
	path := upgradeHistoryPath(hiveID)
	if path == "" {
		return
	}
	if err := os.MkdirAll(upgradeHistoryDir, 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(hiveUpgradeHistory{HiveID: hiveID, Attempts: u.byID[hiveID]}, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

// load reads every persisted history back at hub startup.
func (u *upgradeHistoryStore) load() {
	entries, err := os.ReadDir(upgradeHistoryDir)
	if err != nil {
		return
	}
	max := upgradeHistoryMax()
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(upgradeHistoryDir, name))
		if err != nil {
			continue
		}
		var h hiveUpgradeHistory
		if json.Unmarshal(data, &h) != nil || h.HiveID == "" {
			continue
		}
		u.byID[h.HiveID] = trimUpgradeHistory(h.Attempts, max)
	}
}

// Hub-side recording hooks. Each is safe without s.mu.

// noteUpgradeStarted opens a history row for an instructed upgrade.
func (s *HubServer) noteUpgradeStarted(hiveID, from, target, trigger string) {
	if s == nil || s.upgradeHistory == nil {
		return
	}
	s.upgradeHistory.start(hiveID, historyRef(from), historyRef(target), trigger, time.Now())
}

// noteUpgradeEnded closes the open row for hiveID.
func (s *HubServer) noteUpgradeEnded(hiveID, outcome, reason, from, target string) {
	if s == nil || s.upgradeHistory == nil {
		return
	}
	s.upgradeHistory.end(hiveID, outcome, reason, historyRef(from), historyRef(target), time.Now())
}

// noteUpgradeDone / noteUpgradeMissed are the outcome-specific spellings for
// callers that should not need the spoke vocabulary.
func (s *HubServer) noteUpgradeDone(hiveID, reason, from, target string) {
	s.noteUpgradeEnded(hiveID, spoke.UpgradeOutcomeDone, reason, from, target)
}

func (s *HubServer) noteUpgradeMissed(hiveID, reason, from, target string) {
	s.noteUpgradeEnded(hiveID, spoke.UpgradeOutcomeMissed, reason, from, target)
}

// noteUpgradeWindowMissed records a held scheduled window.
func (s *HubServer) noteUpgradeWindowMissed(hiveID, from, reason string, at time.Time) {
	if s == nil || s.upgradeHistory == nil {
		return
	}
	s.upgradeHistory.windowMissed(hiveID, historyRef(from), reason, at)
}

// historyRef trims a commit to its short form for history rows; a channel
// name ("candidate") or an empty target passes through untouched.
func historyRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if len(ref) > StandardSHALen && isHexString(ref) {
		return ref[:StandardSHALen]
	}
	return ref
}

func isHexString(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// upgradeTriggerForSchedule maps an auto-upgrade schedule onto the history
// trigger vocabulary: a daily/weekly window is "scheduled", everything else
// the hub fires on its own is "instant".
func upgradeTriggerForSchedule(mode, channel string) string {
	switch effectiveAutoUpgradeSchedule(mode, channel) {
	case AutoUpgradeModeDaily, AutoUpgradeModeWeekly:
		return spoke.UpgradeTriggerScheduled
	}
	return spoke.UpgradeTriggerInstant
}
