package prfollowup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// StatsFile is the cumulative follow-up counter file kept in the pointer
// directory. It is not a pointer (its name does not carry pointerFilePrefix),
// so the sweep and HandoffSection never read it as one.
const StatsFile = "stats.json"

// statsFileMode matches the pointer files' mode (turn.FileStore).
const statsFileMode = 0o660

// ReasonKickRefused is the counter key for every fallback whose reason was a
// refusal from the agent manager (the Outcome keeps the manager's full
// error). Collapsing them keeps the counter's keys to a small fixed set.
const ReasonKickRefused = "kick refused"

// knownFallbackReasons are the counter keys a fallback reason is kept under
// verbatim.
var knownFallbackReasons = map[string]bool{
	ReasonExpired: true, ReasonSessionGone: true, ReasonSessionMoved: true,
	ReasonCapReached: true, ReasonNoResumer: true, ReasonSuperseded: true,
}

// Stats are the cumulative PR follow-up counters, per event unless noted.
type Stats struct {
	// Resumed counts events delivered into the live authoring session.
	Resumed int `json:"resumed"`
	// Fallback counts events handed to the fresh-dispatch path, by reason.
	Fallback map[string]int `json:"fallback,omitempty"`
	// Skipped counts PRs entering a skipped state (draft, fork, escalated),
	// by reason: once per transition, not per tick.
	Skipped map[string]int `json:"skipped,omitempty"`
	// Deferred counts routing attempts that found the session busy (each
	// retry counts).
	Deferred int `json:"deferred"`
	// HandoffsQueued counts human-feedback events queued for a fresh kick;
	// HandoffsDelivered counts those a kick has since carried.
	HandoffsQueued    int `json:"handoffs_queued"`
	HandoffsDelivered int `json:"handoffs_delivered"`
	// Pruned counts pointers deleted by the sweep, by reason.
	Pruned    map[string]int `json:"pruned,omitempty"`
	UpdatedAt time.Time      `json:"updated_at"`
}

func bump(m *map[string]int, key string, n int) {
	if n <= 0 {
		return
	}
	if *m == nil {
		*m = map[string]int{}
	}
	(*m)[key] += n
}

func (s *Stats) addFallback(reason string, n int) {
	if !knownFallbackReasons[reason] {
		reason = ReasonKickRefused
	}
	bump(&s.Fallback, reason, n)
}

func (s *Stats) addSkipped(reason string) { bump(&s.Skipped, reason, 1) }

func (s *Stats) addPruned(reason string) { bump(&s.Pruned, reason, 1) }

func (s Stats) isZero() bool {
	return s.Resumed == 0 && s.Deferred == 0 && s.HandoffsQueued == 0 && s.HandoffsDelivered == 0 &&
		len(s.Fallback) == 0 && len(s.Skipped) == 0 && len(s.Pruned) == 0
}

func (s *Stats) merge(d Stats) {
	s.Resumed += d.Resumed
	s.Deferred += d.Deferred
	s.HandoffsQueued += d.HandoffsQueued
	s.HandoffsDelivered += d.HandoffsDelivered
	for k, v := range d.Fallback {
		bump(&s.Fallback, k, v)
	}
	for k, v := range d.Skipped {
		bump(&s.Skipped, k, v)
	}
	for k, v := range d.Pruned {
		bump(&s.Pruned, k, v)
	}
}

// ReadStats returns the cumulative counters in dir. A missing file is zero
// counters, not an error.
func ReadStats(dir string) (Stats, error) {
	var s Stats
	data, err := os.ReadFile(filepath.Join(dir, StatsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return Stats{}, fmt.Errorf("parsing %s: %w", StatsFile, err)
	}
	return s, nil
}

// addStats folds delta into the counters in dir. Callers hold storeMu. A
// corrupt counter file is restarted from zero rather than blocking the count.
func addStats(dir string, delta Stats, now time.Time) error {
	if delta.isZero() {
		return nil
	}
	s, err := ReadStats(dir)
	if err != nil {
		s = Stats{}
	}
	s.merge(delta)
	s.UpdatedAt = now
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return err
	}
	tmp := filepath.Join(dir, StatsFile+".tmp")
	if err := os.WriteFile(tmp, data, statsFileMode); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, StatsFile))
}
