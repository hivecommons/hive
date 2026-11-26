package schedule

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	// FileName is the schedule file inside the meetings data directory.
	FileName = "schedule.json"
	// fileVersion is the schedule.json format version.
	fileVersion   = 1
	storeDirMode  = 0o700
	storeFileMode = 0o600
	// maxEvents bounds the stored events; the oldest finished ones are
	// pruned first.
	maxEvents = 2000
)

type storeFile struct {
	Version int     `json:"version"`
	Events  []Event `json:"events"`
}

// Store persists events in one JSON file, rewritten atomically on every
// change. Safe for concurrent use.
type Store struct {
	path string
	now  func() time.Time

	mu     sync.Mutex
	events map[string]Event // by Key
}

// NewStore opens <dir>/schedule.json, creating dir if needed. A missing
// file is an empty schedule; an unreadable one is an error, so a damaged
// schedule is never silently replaced.
func NewStore(dir string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(dir, storeDirMode); err != nil {
		return nil, fmt.Errorf("schedule: creating dir: %w", err)
	}
	s := &Store{path: filepath.Join(dir, FileName), now: now, events: map[string]Event{}}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("schedule: reading %s: %w", FileName, err)
	}
	var f storeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("schedule: decoding %s: %w", FileName, err)
	}
	for _, e := range f.Events {
		if e.UID == "" {
			continue
		}
		e.ID = EventID(e.UID, e.RecurrenceID)
		s.events[Key(e.UID, e.RecurrenceID)] = e
	}
	return s, nil
}

// Path is the schedule file.
func (s *Store) Path() string { return s.path }

// saveLocked writes every event; s.mu is held.
func (s *Store) saveLocked() error {
	f := storeFile{Version: fileVersion, Events: s.sortedLocked()}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.path, data)
}

func (s *Store) sortedLocked() []Event {
	out := make([]Event, 0, len(s.events))
	for _, e := range s.events {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// writeAtomic writes data to a temp file beside path and renames it over.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".schedule-*")
	if err != nil {
		return fmt.Errorf("schedule: temp file: %w", err)
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, storeFileMode)
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		_ = os.Remove(name)
		return fmt.Errorf("schedule: writing %s: %w", FileName, werr)
	}
	return nil
}

// List returns every event, by start time.
func (s *Store) List() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sortedLocked()
}

// Get returns one event by its id.
func (s *Store) Get(id string) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.ID == id {
			return e, nil
		}
	}
	return Event{}, fmt.Errorf("%w: %s", ErrNotFound, id)
}

// Update applies fn to one event and persists it. fn returning an error
// leaves the event unchanged.
func (s *Store) Update(id string, fn func(*Event) error) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.events {
		if e.ID != id {
			continue
		}
		next := e
		if err := fn(&next); err != nil {
			return e, err
		}
		next.UpdatedAt = s.now().UTC()
		s.events[k] = next
		if err := s.saveLocked(); err != nil {
			s.events[k] = e
			return e, err
		}
		return next, nil
	}
	return Event{}, fmt.Errorf("%w: %s", ErrNotFound, id)
}

// Remove deletes one event by its id when check accepts it (nil accepts
// any). It returns the removed event.
func (s *Store) Remove(id string, check func(Event) error) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.events {
		if e.ID != id {
			continue
		}
		if check != nil {
			if err := check(e); err != nil {
				return e, err
			}
		}
		delete(s.events, k)
		if err := s.saveLocked(); err != nil {
			s.events[k] = e
			return e, err
		}
		return e, nil
	}
	return Event{}, fmt.Errorf("%w: %s", ErrNotFound, id)
}

// Upsert stores a normalized event under the SEQUENCE rule: a new event is
// added; an existing one is replaced only by a higher SEQUENCE (equal or
// lower returns the stored event and ErrStaleSequence). merge builds the
// replacement from the stored event and the incoming one.
func (s *Store) Upsert(ev Event, merge func(old *Event, ev Event) Event) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := Key(ev.UID, ev.RecurrenceID)
	now := s.now().UTC()
	old, ok := s.events[k]
	if ok && ev.Sequence <= old.Sequence {
		return old, ErrStaleSequence
	}
	var next Event
	if ok {
		next = merge(&old, ev)
	} else {
		if s.pruneLocked(maxEvents - 1); len(s.events) >= maxEvents {
			return Event{}, fmt.Errorf("%w: the schedule is full (%d events)", ErrConflict, maxEvents)
		}
		next = merge(nil, ev)
		next.CreatedAt = now
	}
	next.UpdatedAt = now
	s.events[k] = next
	if err := s.saveLocked(); err != nil {
		if ok {
			s.events[k] = old
		} else {
			delete(s.events, k)
		}
		return Event{}, err
	}
	return next, nil
}

// Cancel applies an iCalendar CANCEL: the event with this UID and
// RECURRENCE-ID is marked cancelled unless the stored SEQUENCE is higher.
// A newer cancellation also advances that event's SEQUENCE.
// Cancelling a series cancels its overridden occurrences too; cancelling
// one occurrence that has no override of its own excludes it from the
// series. It returns the changed events.
func (s *Store) Cancel(uid string, recurrenceID *time.Time, sequence int, reason string) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	before := make(map[string]Event, len(s.events))
	for k, e := range s.events {
		before[k] = e
	}
	var changed []Event
	cancel := func(k string, e Event, nextSequence int) {
		if e.Status != StatusCancelled {
			e.Status, e.StatusReason = StatusCancelled, reason
		} else if nextSequence == e.Sequence {
			return
		}
		e.Sequence, e.UpdatedAt = nextSequence, now
		s.events[k] = e
		changed = append(changed, e)
	}
	k := Key(uid, recurrenceID)
	e, ok := s.events[k]
	switch {
	case ok && sequence < e.Sequence:
		return nil, ErrStaleSequence
	case ok:
		cancel(k, e, sequence)
		if recurrenceID == nil {
			for ok2, o := range s.events {
				if o.UID == uid && o.RecurrenceID != nil {
					// Overrides have their own SEQUENCE.
					cancel(ok2, o, o.Sequence)
				}
			}
		}
	case recurrenceID != nil:
		series, sok := s.events[Key(uid, nil)]
		if !sok {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, uid)
		}
		if !containsTime(series.ExDates, *recurrenceID) && len(series.ExDates) < maxExDates {
			series.ExDates = append(series.ExDates, recurrenceID.UTC())
			series.UpdatedAt = now
			s.events[Key(uid, nil)] = series
			changed = append(changed, series)
		}
	default:
		return nil, fmt.Errorf("%w: %s", ErrNotFound, uid)
	}
	if len(changed) == 0 {
		return nil, nil
	}
	if err := s.saveLocked(); err != nil {
		s.events = before
		return nil, err
	}
	return changed, nil
}

// overrides returns the RECURRENCE-IDs of the overridden occurrences of a
// series: those run as their own events.
func (s *Store) overrides(uid string) []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.overridesLocked(uid)
}

// overridesLocked returns the current overrides while s.mu is held.
func (s *Store) overridesLocked(uid string) []time.Time {
	var out []time.Time
	for _, e := range s.events {
		if e.UID == uid && e.RecurrenceID != nil {
			out = append(out, *e.RecurrenceID)
		}
	}
	return out
}

// Prune removes finished events last changed before cutoff.
func (s *Store) Prune(cutoff time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := map[string]Event{}
	for k, e := range s.events {
		if e.Status.Terminal() && e.UpdatedAt.Before(cutoff) {
			removed[k] = e
			delete(s.events, k)
		}
	}
	if len(removed) == 0 {
		return 0, nil
	}
	if err := s.saveLocked(); err != nil {
		for k, e := range removed {
			s.events[k] = e
		}
		return 0, err
	}
	return len(removed), nil
}

// pruneLocked keeps the store at or under limit events by dropping the
// oldest finished ones.
func (s *Store) pruneLocked(limit int) {
	if len(s.events) <= limit {
		return
	}
	var done []Event
	for _, e := range s.events {
		if e.Status.Terminal() {
			done = append(done, e)
		}
	}
	sort.Slice(done, func(i, j int) bool { return done[i].UpdatedAt.Before(done[j].UpdatedAt) })
	for _, e := range done {
		if len(s.events) <= limit {
			return
		}
		delete(s.events, Key(e.UID, e.RecurrenceID))
	}
}
