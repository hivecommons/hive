package integration

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const maxJournalRecordBytes = 1 << 20

// Store serializes local durable state only. Callers must do all forge/claim
// reads before entering it; no remote I/O belongs under this mutex.
type Store struct {
	mu            sync.Mutex
	path          string
	seen          map[string]string
	intents       map[string]Intent
	evidence      map[string]Evidence
	jobs          map[string]Job
	coverage      map[string]Coverage
	reservations  map[string]Reservation
	memberChanges map[string]Event
	poison        error
}

func (s *Store) HasEvent(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.seen[id]
	return ok
}

func (s *Store) EnsureIntent(i Intent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.intents[i.Key]; ok {
		return nil
	}
	return s.appendLocked(Event{Version: SchemaVersion, ID: "dispatch-intent/" + i.Key, At: i.ObservedAt, Intent: &i})
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, seen: map[string]string{}, intents: map[string]Intent{},
		evidence: map[string]Evidence{}, jobs: map[string]Job{}, coverage: map[string]Coverage{},
		reservations: map[string]Reservation{}, memberChanges: map[string]Event{}}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), maxJournalRecordBytes)
	for line := 1; scanner.Scan(); line++ {
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("integration journal line %d: %w", line, err)
		}
		if err := s.validate(e); err != nil {
			return nil, fmt.Errorf("integration journal line %d: %w", line, err)
		}
		s.apply(e)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if stat, err := f.Stat(); err != nil {
		return nil, err
	} else if stat.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], stat.Size()-1); err != nil {
			return nil, err
		}
		if last[0] != '\n' {
			return nil, fmt.Errorf("integration journal has an incomplete trailing record")
		}
	}
	return s, nil
}

func eventDigest(e Event) string {
	b, _ := json.Marshal(e)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func scopeKey(repo, base string) string { return repo + "#" + base }

func (s *Store) validate(e Event) error {
	if e.Version != SchemaVersion || e.ID == "" || e.At.IsZero() {
		return fmt.Errorf("invalid integration event envelope")
	}
	if old, ok := s.seen[e.ID]; ok && old != eventDigest(e) {
		return fmt.Errorf("event identity reused with different content")
	}
	n := 0
	for _, yes := range []bool{e.Intent != nil, e.Evidence != nil, e.Job != nil, e.Coverage != nil, e.Reservation != nil, e.Release != "", e.Invalidate != "", e.MemberChange != nil} {
		if yes {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("event must carry exactly one observation")
	}
	if change := e.MemberChange; change != nil && (change.Repo == "" || change.PR <= 0) {
		return fmt.Errorf("invalid member change")
	}
	if e.Intent != nil {
		if err := e.Intent.Validate(); err != nil {
			return err
		}
		if old, ok := s.intents[e.Intent.Key]; ok && e.Intent.Generation == old.Generation && eventDigest(Event{Intent: &old}) != eventDigest(Event{Intent: e.Intent}) {
			return fmt.Errorf("intent generation reused")
		}
		visiting, done := map[string]bool{}, map[string]bool{}
		var visit func(string) bool
		visit = func(key string) bool {
			if visiting[key] {
				return true
			}
			if done[key] {
				return false
			}
			visiting[key] = true
			i := s.intents[key]
			if key == e.Intent.Key {
				i = *e.Intent
			}
			for _, p := range i.Prerequisites {
				if visit(p.Key) {
					return true
				}
			}
			delete(visiting, key)
			done[key] = true
			return false
		}
		if visit(e.Intent.Key) {
			return fmt.Errorf("intent prerequisite cycle")
		}
	}
	if e.Evidence != nil {
		if err := e.Evidence.Subject.Validate(); err != nil && e.Evidence.State != Unknown {
			return err
		}
		if e.Evidence.Subject.Repo == "" || e.Evidence.Subject.Authority == "" {
			return fmt.Errorf("unknown candidate still requires authoritative identity")
		}
		switch e.Evidence.State {
		case Unknown, Waiting, Running, Passed, Rejected, Superseded:
		default:
			return fmt.Errorf("invalid evidence state")
		}
		if e.Evidence.ObservedAt.IsZero() || e.Evidence.Source == "" {
			return fmt.Errorf("evidence observation missing")
		}
		if e.Evidence.State == Passed && (e.Evidence.RunID <= 0 || e.Evidence.SuiteID <= 0 || len(e.Evidence.CheckIDs) == 0) {
			return fmt.Errorf("passed evidence requires run, suite and check identities")
		}
	}
	if j := e.Job; j != nil {
		if j.Repo == "" || j.ID <= 0 || j.RunID <= 0 || j.Attempt < 1 || !fullSHA(j.Head) || j.ObservedAt.IsZero() {
			return fmt.Errorf("invalid job identity")
		}
		if !j.CompletedAt.IsZero() && (j.StartedAt.IsZero() || j.CompletedAt.Before(j.StartedAt)) && j.RunnerID > 0 {
			return fmt.Errorf("invalid job execution interval")
		}
	}
	if c := e.Coverage; c != nil {
		if c.Repo == "" || c.Base == "" || c.Source == "" || c.From.IsZero() || c.Through.Before(c.From) {
			return fmt.Errorf("invalid telemetry coverage")
		}
	}
	if r := e.Reservation; r != nil {
		if r.Key == "" || r.Claim == "" || r.ClaimGeneration == 0 || r.IntentGeneration == 0 || !r.ExpiresAt.After(e.At) {
			return fmt.Errorf("invalid reservation")
		}
	}
	return nil
}

func (s *Store) apply(e Event) {
	if _, ok := s.seen[e.ID]; ok {
		return
	}
	s.seen[e.ID] = eventDigest(e)
	if i := e.Intent; i != nil {
		old, found := s.intents[i.Key]
		if !found || i.Generation > old.Generation {
			s.intents[i.Key] = cloneIntent(*i)
			delete(s.reservations, i.Key)
		}
	}
	if v := e.Evidence; v != nil {
		k := v.Subject.ID()
		old, found := s.evidence[k]
		if !found || (old.State != Superseded && v.ObservedAt.After(old.ObservedAt)) {
			next := cloneEvidence(*v)
			for _, member := range next.Subject.Members {
				change, known := s.memberChanges[fmt.Sprintf("%s#%d", next.Subject.Repo, member.PR)]
				if known && !next.ObservedAt.After(change.At) && (change.MemberChange.Head == "" || change.MemberChange.Head != member.Head) {
					next.State, next.InvalidatedBy, next.Reason = Superseded, change.ID, change.Reason
				}
			}
			s.evidence[k] = next
		}
	}
	if change := e.MemberChange; change != nil {
		key := fmt.Sprintf("%s#%d", change.Repo, change.PR)
		if previous, found := s.memberChanges[key]; !found || e.At.After(previous.At) {
			s.memberChanges[key] = e
		}
		for id, receipt := range s.evidence {
			for _, member := range receipt.Subject.Members {
				if receipt.Subject.Repo == change.Repo && member.PR == change.PR && (change.Head == "" || member.Head != change.Head) {
					receipt.State, receipt.InvalidatedBy, receipt.Reason = Superseded, e.ID, e.Reason
					s.evidence[id] = receipt
				}
			}
		}
	}
	if j := e.Job; j != nil {
		old, found := s.jobs[j.Key()]
		if !found || (!j.ObservedAt.Before(old.ObservedAt) && (old.Status != "completed" || j.Status == "completed")) {
			s.jobs[j.Key()] = cloneJob(*j)
		}
	}
	if c := e.Coverage; c != nil {
		k := scopeKey(c.Repo, c.Base)
		old, found := s.coverage[k]
		if !found || !c.Through.Before(old.Through) {
			s.coverage[k] = *c
		}
	}
	if r := e.Reservation; r != nil {
		s.reservations[r.Key] = *r
	}
	if e.Release != "" {
		if e.Release == "*" {
			clear(s.reservations)
		} else {
			r := s.reservations[e.Release]
			if r.Claim == e.ReleaseClaim && r.ClaimGeneration == e.ReleaseGeneration {
				delete(s.reservations, e.Release)
			}
		}
	}
	if e.Invalidate != "" {
		if old, ok := s.evidence[e.Invalidate]; ok {
			old.State, old.InvalidatedBy, old.Reason = Superseded, e.ID, e.Reason
			s.evidence[e.Invalidate] = old
		}
	}
}

func (s *Store) appendLocked(e Event) error {
	if s.poison != nil {
		return s.poison
	}
	if err := s.validate(e); err != nil {
		return err
	}
	if _, ok := s.seen[e.ID]; ok {
		return nil
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(b) >= maxJournalRecordBytes {
		return fmt.Errorf("integration event exceeds journal record bound")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(append(b, '\n'))
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		s.poison = writeErr
		return writeErr
	}
	if closeErr != nil {
		s.poison = closeErr
		return closeErr
	}
	s.apply(e)
	return nil
}

func (s *Store) InvalidateGroup(repo, authority, head, delivery, reason string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, v := range s.evidence {
		if v.Subject.Repo == repo && v.Subject.Authority == authority && v.Subject.TestedSHA == head && v.State != Superseded {
			if err := s.appendLocked(Event{Version: SchemaVersion, ID: delivery + "/" + id, At: now, Invalidate: id, Reason: reason}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) Append(e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(e)
}

type Snapshot struct {
	Version      int           `json:"version"`
	Intents      []Intent      `json:"intents"`
	Evidence     []Evidence    `json:"evidence"`
	Jobs         []Job         `json:"jobs"`
	Coverage     []Coverage    `json:"coverage"`
	Reservations []Reservation `json:"reservations"`
}

func (s *Store) Snapshot(now time.Time) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Snapshot{Version: SchemaVersion, Intents: []Intent{}, Evidence: []Evidence{}, Jobs: []Job{}, Coverage: []Coverage{}, Reservations: []Reservation{}}
	for _, v := range s.intents {
		out.Intents = append(out.Intents, cloneIntent(v))
	}
	for _, v := range s.evidence {
		out.Evidence = append(out.Evidence, cloneEvidence(v))
	}
	for _, v := range s.jobs {
		out.Jobs = append(out.Jobs, cloneJob(v))
	}
	for _, v := range s.coverage {
		out.Coverage = append(out.Coverage, v)
	}
	for _, v := range s.reservations {
		if v.ExpiresAt.After(now) {
			out.Reservations = append(out.Reservations, v)
		}
	}
	return out
}

func (s *Store) InvalidateMember(repo string, pr int, head, reason, delivery string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, seen := s.seen["member/"+delivery]; seen {
		return nil
	}
	return s.appendLocked(Event{Version: SchemaVersion, ID: "member/" + delivery, At: now, Reason: reason, MemberChange: &MemberChange{Repo: repo, PR: pr, Head: head}})
}
