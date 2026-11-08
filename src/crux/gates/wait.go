package gates

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Wait blocks until gate id is answered or expired, ctx ends, or poll has
// passed (poll <= 0 waits without a window), and returns the gate as it then
// stands: a record still StatusWaiting means the window closed first. A
// gate with a timeout is expired by Wait itself when the timeout passes,
// so a waiter never needs another caller to look for it to flip.
func (s *Service) Wait(ctx context.Context, id string, poll time.Duration) (Record, error) {
	var window <-chan time.Time
	if poll > 0 {
		window = s.after(poll)
	}
	for {
		s.mu.Lock()
		rec, changes, err := s.loadLocked(id)
		var resolved chan struct{}
		if err == nil && rec.Waiting() {
			resolved = s.watcherLocked(id)
		}
		s.mu.Unlock()
		s.emit(changes)
		if err != nil || !rec.Waiting() {
			return rec, err
		}
		var expiry <-chan time.Time
		if rec.ExpiresAt != nil {
			expiry = s.after(rec.ExpiresAt.Sub(s.now()))
		}
		select {
		case <-resolved:
		case <-expiry:
		case <-window:
			return rec, nil
		case <-ctx.Done():
			return rec, ctx.Err()
		}
	}
}

// answerLocked applies one answer to rec; wrote reports whether a new
// answer was recorded (false for an exact replay).
func (s *Service) answerLocked(caller Caller, rec Record, choice, note string, override bool) (Record, bool, error) {
	if !rec.Waiting() {
		if rec.Status == StatusAnswered && rec.Answer == choice && rec.Note == note && rec.AnsweredByPrincipal == caller.Principal {
			return rec, false, nil
		}
		return rec, false, fmt.Errorf("%w: %s is %s", ErrClosed, rec.ID, rec.Status)
	}
	if !rec.HasChoice(choice) {
		return rec, false, fmt.Errorf("%w: %q is not one of %s", ErrInvalidChoice, choice, strings.Join(rec.Choices, ", "))
	}
	self := rec.OpenedByPrincipal == caller.Principal
	if self && !override {
		return rec, false, fmt.Errorf("%w: %s opened %s; answering it needs the explicit operator override", ErrSelfAnswer, caller.Principal, rec.ID)
	}
	now := s.now().UTC()
	rec.Status = StatusAnswered
	rec.Answer = choice
	rec.AnsweredBy = caller.Name
	rec.AnsweredByPrincipal = caller.Principal
	rec.AnsweredAt = &now
	rec.Note = note
	rec.OperatorOverride = self
	err := s.resolveLocked(rec, JournalEntry{At: now, Op: OpAnswered, ID: rec.ID, Status: rec.Status, By: caller.Principal, Answer: choice, Override: self})
	if err != nil {
		return Record{}, false, err
	}
	return rec, true, nil
}

// loadLocked reads id and expires it when due.
func (s *Service) loadLocked(id string) (Record, []change, error) {
	rec, err := s.st.get(id)
	if err != nil {
		return Record{}, nil, err
	}
	return s.expireLocked(rec)
}

// expireLocked moves a waiting gate whose timeout has passed to
// StatusExpired: no answer, no answerer, only the time it expired.
func (s *Service) expireLocked(rec Record) (Record, []change, error) {
	if !rec.Waiting() || rec.ExpiresAt == nil {
		return rec, nil, nil
	}
	now := s.now().UTC()
	if now.Before(*rec.ExpiresAt) {
		return rec, nil, nil
	}
	rec.Status = StatusExpired
	rec.ExpiredAt = &now
	if err := s.resolveLocked(rec, JournalEntry{At: now, Op: OpExpired, ID: rec.ID, Status: rec.Status}); err != nil {
		return Record{}, nil, err
	}
	return rec, []change{{OpExpired, rec}}, nil
}

// resolveLocked makes a resolution durable, journals it and wakes waiters.
func (s *Service) resolveLocked(rec Record, entry JournalEntry) error {
	if err := s.st.put(rec); err != nil {
		return err
	}
	if ch, ok := s.watchers[rec.ID]; ok {
		close(ch)
		delete(s.watchers, rec.ID)
	}
	return s.st.appendJournal(entry)
}

// watcherLocked returns the channel closed when id resolves.
func (s *Service) watcherLocked(id string) chan struct{} {
	ch, ok := s.watchers[id]
	if !ok {
		ch = make(chan struct{})
		s.watchers[id] = ch
	}
	return ch
}

// emit tells the observer about durable changes, outside the lock.
func (s *Service) emit(changes []change) {
	if len(changes) == 0 {
		return
	}
	s.mu.Lock()
	fn := s.observer
	s.mu.Unlock()
	if fn == nil {
		return
	}
	for _, c := range changes {
		fn(c.op, c.rec)
	}
}

func newID() string {
	var b [idRandomBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("gates: crypto/rand: %v", err))
	}
	return IDPrefix + hex.EncodeToString(b[:])
}
