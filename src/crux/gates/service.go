package gates

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Config is how a Service is built. Only Dir is required.
type Config struct {
	// Dir is where pending, resolved and the journal live (DefaultDir).
	Dir string
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
	// After fires once d has passed; nil uses time.After. Wait uses it for
	// both the caller's poll window and a gate's expiry.
	After func(d time.Duration) <-chan time.Time
	// NewID mints a gate id; nil draws IDPrefix plus 16 random hex digits.
	NewID func() string
}

// Observer is told about every change to a gate after it is durable:
// op is OpOpened, OpAnswered or OpExpired.
type Observer func(op string, rec Record)

// Service is the gate store for one directory. It is safe for concurrent
// use within one process; a gates directory is served by one process.
type Service struct {
	st    store
	now   func() time.Time
	after func(time.Duration) <-chan time.Time
	newID func() string

	mu       sync.Mutex
	keys     map[string]string        // idempotency key -> id
	watchers map[string]chan struct{} // id -> closed when the gate resolves
	observer Observer
}

// change is a durable transition the observer has not yet been told about.
type change struct {
	op  string
	rec Record
}

// New opens the gates directory, creating it, and rebuilds the idempotency
// index from every record on disk, pending and resolved — which is what
// makes a gate opened before a restart answerable, and re-opening it
// idempotent, after one.
func New(cfg Config) (*Service, error) {
	if strings.TrimSpace(cfg.Dir) == "" {
		return nil, fmt.Errorf("%w: gates directory is required", ErrBadRequest)
	}
	s := &Service{st: store{dir: cfg.Dir}, now: cfg.Now, after: cfg.After, newID: cfg.NewID,
		keys: map[string]string{}, watchers: map[string]chan struct{}{}}
	if s.now == nil {
		s.now = time.Now
	}
	if s.after == nil {
		s.after = time.After
	}
	if s.newID == nil {
		s.newID = newID
	}
	if err := s.st.init(); err != nil {
		return nil, err
	}
	recs, err := s.st.list()
	if err != nil {
		return nil, err
	}
	for _, rec := range recs {
		if !rec.Waiting() {
			if err := s.st.dropStalePending(rec.ID); err != nil {
				return nil, err
			}
		}
		if rec.IdempotencyKey != "" {
			s.keys[rec.IdempotencyKey] = rec.ID
		}
	}
	return s, nil
}

// SetObserver installs fn as the change observer (crux serve publishes
// gate.* events from it). nil removes it.
func (s *Service) SetObserver(fn Observer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observer = fn
}

// Visible reports whether caller may read rec: the operator reads every
// gate, anyone else only the gates it opened.
func Visible(rec Record, caller Caller) bool {
	return caller.Operator || rec.OpenedByPrincipal == caller.Principal
}

// Open records a new waiting gate for caller and returns it with
// created=true. When the same principal already opened the same gate (the
// derived idempotency key matches) nothing is written and that gate's
// current record — waiting, answered or expired — is returned with
// created=false.
func (s *Service) Open(caller Caller, req Request) (Record, bool, error) {
	n, err := normalize(req)
	if err != nil {
		return Record{}, false, err
	}
	key := DeriveIdempotencyKey(caller.Principal, n)
	s.mu.Lock()
	if id, ok := s.keys[key]; ok {
		rec, changes, err := s.loadLocked(id)
		s.mu.Unlock()
		s.emit(changes)
		return rec, false, err
	}
	now := s.now().UTC()
	rec := Record{
		ID: s.newID(), Status: StatusWaiting, Subject: n.Subject, Context: n.Context, Choices: n.Choices,
		Timeout: FormatTimeout(n.Timeout), OpenedBy: n.OpenedBy, OpenedByPrincipal: caller.Principal,
		OpenedAt: now, IdempotencyKey: key, Run: n.Run,
	}
	if rec.OpenedBy == "" {
		rec.OpenedBy = caller.Name
	}
	entry := JournalEntry{At: now, Op: OpOpened, ID: rec.ID, Status: rec.Status, By: caller.Principal}
	if n.Timeout > 0 {
		exp := now.Add(n.Timeout)
		rec.ExpiresAt = &exp
		entry.ExpiresAt = exp.Format(time.RFC3339Nano)
	}
	if err := s.st.put(rec); err != nil {
		s.mu.Unlock()
		return Record{}, false, err
	}
	err = s.st.appendJournal(entry)
	s.keys[key] = rec.ID
	s.mu.Unlock()
	if err != nil {
		return rec, true, err
	}
	s.emit([]change{{OpOpened, rec}})
	return rec, true, nil
}

// Get returns the saved gate, expiring it first when its timeout passed.
func (s *Service) Get(id string) (Record, error) {
	s.mu.Lock()
	rec, changes, err := s.loadLocked(id)
	s.mu.Unlock()
	s.emit(changes)
	return rec, err
}

// ListFilter narrows List. Zero values match everything.
type ListFilter struct {
	// Status is one of the Status* values.
	Status string
	// Caller limits the result to the gates Visible to it.
	Caller *Caller
}

// List returns the gates matching f, oldest first, expiring any whose
// timeout passed.
func (s *Service) List(f ListFilter) ([]Record, error) {
	s.mu.Lock()
	recs, err := s.st.list()
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	var changes []change
	out := make([]Record, 0, len(recs))
	for _, rec := range recs {
		rec, c, err := s.expireLocked(rec)
		changes = append(changes, c...)
		if err != nil {
			s.mu.Unlock()
			s.emit(changes)
			return nil, err
		}
		if (f.Status == "" || rec.Status == f.Status) && (f.Caller == nil || Visible(rec, *f.Caller)) {
			out = append(out, rec)
		}
	}
	s.mu.Unlock()
	s.emit(changes)
	sort.Slice(out, func(i, j int) bool {
		if out[i].OpenedAt.Equal(out[j].OpenedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].OpenedAt.Before(out[j].OpenedAt)
	})
	return out, nil
}

// Answer records caller's decision on gate id. Only an operator answers;
// the choice must be one the gate offers; the principal that opened the
// gate may answer it only with a.Override, which the record and journal
// carry as operator_override. Repeating the exact answer that was recorded
// (same choice, note and principal) returns the record unchanged, so a
// retried request is not an error; any other answer to a closed gate is
// ErrClosed.
func (s *Service) Answer(caller Caller, id string, a Answer) (Record, error) {
	if !caller.Operator {
		return Record{}, fmt.Errorf("%w: gates are answered by the operator, not %s", ErrForbidden, caller.Principal)
	}
	note, err := normalizeNote(a.Note)
	if err != nil {
		return Record{}, err
	}
	choice := strings.TrimSpace(a.Choice)
	s.mu.Lock()
	rec, changes, err := s.loadLocked(id)
	if err == nil {
		var wrote bool
		rec, wrote, err = s.answerLocked(caller, rec, choice, note, a.Override)
		if wrote {
			changes = append(changes, change{OpAnswered, rec})
		}
	}
	s.mu.Unlock()
	s.emit(changes)
	return rec, err
}
