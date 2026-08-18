package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// journeyStore holds per-hive nudge state (what was last sent, and any admin
// snooze) and persists it to the PVC so a hub restart neither restarts every
// escalation ladder from scratch nor forgets an admin's exemption.
//
// It is deliberately a separate file from hub-banners.json: banners are
// ephemeral delivery state an admin can clear wholesale, whereas journey state
// is a record of what each owner has already been told.
type journeyStore struct {
	mu     sync.RWMutex
	states map[string]*JourneyState
	// dirty tracks whether anything changed since the last save, so a
	// heartbeat storm that produces no nudges does not rewrite the file.
	dirty bool
}

func newJourneyStore() *journeyStore {
	return &journeyStore{states: make(map[string]*JourneyState)}
}

// get returns a copy of the state for a hive, or nil when none is recorded.
// A copy so callers cannot mutate shared state without the lock.
//
// Nil-receiver safe: a HubServer built directly (as tests and some internal
// paths do) has no store, and a missing store simply means "nothing was ever
// sent" rather than a crash on a read path.
func (js *journeyStore) get(hiveID string) *JourneyState {
	if js == nil {
		return nil
	}
	js.mu.RLock()
	defer js.mu.RUnlock()
	st, ok := js.states[hiveID]
	if !ok || st == nil {
		return nil
	}
	cp := *st
	return &cp
}

// recordSent stores that a banner for this stage/severity was delivered now.
func (js *journeyStore) recordSent(hiveID string, n nudge, acmmLevel int, now time.Time) {
	if js == nil {
		return
	}
	js.mu.Lock()
	defer js.mu.Unlock()
	st, ok := js.states[hiveID]
	if !ok || st == nil {
		st = &JourneyState{}
		js.states[hiveID] = st
	}
	st.LastStage = n.Stage.String()
	st.LastSeverity = n.Severity.String()
	st.LastSentAt = now.UTC().Format(time.RFC3339)
	if n.Stage == StageACMMLevel {
		st.LastACMMLevel = acmmLevel
	}
	js.dirty = true
}

// setSnooze records (or clears, when until is zero) an admin exemption.
func (js *journeyStore) setSnooze(hiveID string, until time.Time, by, reason string) {
	if js == nil {
		return
	}
	js.mu.Lock()
	defer js.mu.Unlock()
	st, ok := js.states[hiveID]
	if !ok || st == nil {
		st = &JourneyState{}
		js.states[hiveID] = st
	}
	if until.IsZero() {
		st.SnoozedUntil = ""
		st.SnoozedBy = ""
		st.SnoozeReason = ""
	} else {
		st.SnoozedUntil = until.UTC().Format(time.RFC3339)
		st.SnoozedBy = by
		st.SnoozeReason = reason
	}
	js.dirty = true
}

// save writes the store to disk atomically (tmp + rename) so a crash mid-write
// never leaves a half-parsed file. A no-op when nothing changed.
func (js *journeyStore) save() error {
	if js == nil {
		return nil
	}
	js.mu.Lock()
	if !js.dirty {
		js.mu.Unlock()
		return nil
	}
	snapshot := make(map[string]*JourneyState, len(js.states))
	for id, st := range js.states {
		snapshot[id] = st
	}
	js.dirty = false
	js.mu.Unlock()

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal journey state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(journeyStatePath), 0o755); err != nil {
		return fmt.Errorf("create journey state dir: %w", err)
	}
	tmpPath := journeyStatePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("write journey state: %w", err)
	}
	if err := os.Rename(tmpPath, journeyStatePath); err != nil {
		return fmt.Errorf("rename journey state: %w", err)
	}
	return nil
}

// load reads persisted state from the PVC. A missing file is normal (no nudges
// sent yet) and is not an error. Expired snoozes are dropped on load so a stale
// exemption does not silently outlive its window.
func (js *journeyStore) load(now time.Time) error {
	if js == nil {
		return nil
	}
	data, err := os.ReadFile(journeyStatePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read journey state: %w", err)
	}
	var stored map[string]*JourneyState
	if err := json.Unmarshal(data, &stored); err != nil {
		return fmt.Errorf("parse journey state: %w", err)
	}
	js.mu.Lock()
	defer js.mu.Unlock()
	for id, st := range stored {
		if st == nil {
			continue
		}
		if st.SnoozedUntil != "" && !st.snoozeActive(now) {
			st.SnoozedUntil = ""
			st.SnoozedBy = ""
			st.SnoozeReason = ""
		}
		js.states[id] = st
	}
	return nil
}

// parseSnoozeUntil validates an admin-supplied snooze duration and converts it
// to an absolute expiry. An empty/zero duration means "clear the snooze".
// Durations beyond maxJourneySnoozeDuration are rejected rather than silently
// clamped, so an admin who typed the wrong unit finds out.
func parseSnoozeUntil(raw string, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "0" {
		return time.Time{}, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid duration %q: use a Go duration such as 720h", raw)
	}
	if d <= 0 {
		return time.Time{}, nil
	}
	if d > maxJourneySnoozeDuration {
		return time.Time{}, fmt.Errorf("snooze %s exceeds the maximum of %s", d, maxJourneySnoozeDuration)
	}
	return now.Add(d), nil
}
