package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	maxJournalRecordBytes = 1 << 20
	maxJournalBytes       = 256 << 20
	maxJournalRecords     = 100000
)

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
	groupChanges  map[string]Event
	policies      *PolicySet
	drifts        map[string]Drift
	integrated    map[string]IntegrationReceipt
	poison        error
	offset        int64
	execution     *executionState
	executionOnly bool
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
	unlock, err := s.writerLock()
	if err != nil {
		return err
	}
	defer unlock()
	if previous, ok := s.intents[i.Key]; ok {
		if previous.Provenance == "dispatch-enumerator" && i.Provenance == previous.Provenance && !previous.ScopeKnown &&
			(previous.Plan != i.Plan || previous.PlanNode != i.PlanNode || previous.PlanRevision != i.PlanRevision) {
			now := i.ObservedAt
			i.Generation, i.ObservedAt = previous.Generation+1, previous.ObservedAt
			return s.appendLocked(Event{Version: SchemaVersion, ID: fmt.Sprintf("dispatch-intent/%s/%d", i.Key, i.Generation), At: now, Intent: &i})
		}
		return nil
	}
	return s.appendLocked(Event{Version: SchemaVersion, ID: "dispatch-intent/" + i.Key, At: i.ObservedAt, Intent: &i})
}

func (s *Store) Intent(key string) (Intent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	intent, found := s.intents[key]
	return cloneIntent(intent), found
}

func (s *Store) Intents() []Intent {
	s.mu.Lock()
	defer s.mu.Unlock()
	intents := make([]Intent, 0, len(s.intents))
	for _, intent := range s.intents {
		intents = append(intents, cloneIntent(intent))
	}
	return intents
}

func (s *Store) Reservation(key string, now time.Time) (Reservation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reservation, found := s.reservations[key]
	return reservation, found && reservation.ExpiresAt.After(now)
}

func (s *Store) ForTestedHead(repo, head string) []Evidence {
	s.mu.Lock()
	defer s.mu.Unlock()
	var receipts []Evidence
	for _, receipt := range s.evidence {
		if strings.EqualFold(receipt.Subject.Repo, repo) && receipt.Subject.TestedSHA == head {
			receipts = append(receipts, cloneEvidence(receipt))
		}
	}
	return receipts
}

func evidenceStateRank(state State) int {
	switch state {
	case Unknown:
		return 0
	case Waiting:
		return 1
	case Running:
		return 2
	case Passed:
		return 3
	case Rejected:
		return 4
	case Superseded:
		return 5
	default:
		return -1
	}
}

func (s *Store) ForFailure(repo, head string, suite, check int64) []Evidence {
	s.mu.Lock()
	defer s.mu.Unlock()
	var receipts []Evidence
	for _, receipt := range s.evidence {
		if !strings.EqualFold(receipt.Subject.Repo, repo) {
			continue
		}
		bound := receipt.RunID > 0 || receipt.SuiteID > 0 || len(receipt.CheckIDs) > 0
		checkKnown := false
		for _, id := range receipt.CheckIDs {
			if id == check && check > 0 {
				checkKnown = true
			}
		}
		if bound && (suite <= 0 || receipt.SuiteID != suite || !checkKnown) {
			continue
		}
		matched := receipt.Subject.TestedSHA == head
		if !matched && bound {
			for _, member := range receipt.Subject.Members {
				if member.Head == head {
					matched = true
				}
			}
		}
		if matched {
			receipts = append(receipts, cloneEvidence(receipt))
		}
	}
	return receipts
}

func Open(path string) (*Store, error) {
	return openStore(path, false)
}

// OpenExecution reuses the bounded fsync/replay journal but isolates v2
// candidate execution from the v1 admission/evidence file for safe rollback.
func OpenExecution(path string) (*Store, error) {
	return openStore(path, true)
}

func openStore(path string, executionOnly bool) (*Store, error) {
	s := &Store{path: path, seen: map[string]string{}, intents: map[string]Intent{},
		evidence: map[string]Evidence{}, jobs: map[string]Job{}, coverage: map[string]Coverage{},
		reservations: map[string]Reservation{}, memberChanges: map[string]Event{}, groupChanges: map[string]Event{}, integrated: map[string]IntegrationReceipt{}, drifts: map[string]Drift{}, executionOnly: executionOnly}
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	unlock, err := s.writerLock()
	if err != nil {
		return nil, err
	}
	unlock()
	return s, nil
}

func eventDigest(e Event) string {
	b, _ := json.Marshal(e)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func scopeKey(repo, base string) string { return strings.ToLower(repo) + "#" + base }

func (s *Store) validate(e Event) error {
	if e.Version != SchemaVersion || e.ID == "" || e.At.IsZero() {
		return fmt.Errorf("invalid integration event envelope")
	}
	if old, ok := s.seen[e.ID]; ok && old != eventDigest(e) {
		return fmt.Errorf("event identity reused with different content")
	}
	n := 0
	for _, yes := range []bool{e.Intent != nil, e.Evidence != nil, e.Job != nil, e.Coverage != nil, e.Reservation != nil, e.Release != "", e.Invalidate != "", e.MemberChange != nil, e.Integrated != nil, e.GroupChange != nil, e.PolicySet != nil, e.Drift != nil, e.Execution != nil} {
		if yes {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("event must carry exactly one observation")
	}
	if e.Execution != nil {
		if !s.executionOnly {
			return fmt.Errorf("execution v2 requires its own journal partition")
		}
		return s.validateExecution(*e.Execution)
	}
	if s.executionOnly && e.Job == nil && e.Coverage == nil && e.Integrated == nil {
		return fmt.Errorf("v1 admission/evidence cannot be written into execution v2")
	}
	if change := e.MemberChange; change != nil && (change.Repo == "" || change.PR <= 0) {
		return fmt.Errorf("invalid member change")
	}
	if drift := e.Drift; drift != nil && (drift.Key == "" || drift.Repo == "" || !fullSHA(drift.Head) || drift.IntentGeneration == 0 || drift.Source == "" || drift.ObservedAt.IsZero()) {
		return fmt.Errorf("invalid scope drift observation")
	}
	if change := e.GroupChange; change != nil && (change.Repo == "" || change.Authority == "" || !fullSHA(change.Head)) {
		return fmt.Errorf("invalid group invalidation")
	}
	if set := e.PolicySet; set != nil {
		for _, binding := range set.Active {
			if binding.Repo == "" || binding.Base == "" || binding.Fingerprint == "" {
				return fmt.Errorf("invalid policy binding")
			}
		}
	}
	if receipt := e.Integrated; receipt != nil && (receipt.Repo == "" || receipt.PR < 1 || !fullSHA(receipt.Head) || !fullSHA(receipt.MergeSHA) || receipt.MergedAt.IsZero() || receipt.Source == "") {
		return fmt.Errorf("invalid integrated receipt")
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
		for _, previous := range s.evidence {
			next := e.Evidence
			if (next.Source == "github-actions-bound-v1" || next.Source == "github-actions-attested-v1") && next.State != Unknown && next.RunID > 0 &&
				previous.ObservedState != Unknown && strings.EqualFold(previous.Subject.Repo, next.Subject.Repo) &&
				previous.Source == next.Source && previous.RunID == next.RunID && previous.Subject.Attempt == next.Subject.Attempt &&
				previous.Subject.Policy == next.Subject.Policy && previous.Subject.Workflow == next.Subject.Workflow &&
				previous.Subject.Authority == next.Subject.Authority && previous.Subject.ID() != next.Subject.ID() {
				return fmt.Errorf("workflow attempt changed immutable tested subject")
			}
		}
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
	if e.Execution != nil {
		s.applyExecution(e)
		return
	}
	if drift := e.Drift; drift != nil {
		if previous, found := s.drifts[drift.Key]; !found || drift.ObservedAt.After(previous.ObservedAt) {
			next := *drift
			next.Actual = append([]string(nil), drift.Actual...)
			next.OutsideAnticipated = append([]string(nil), drift.OutsideAnticipated...)
			s.drifts[drift.Key] = next
		}
	}
	if receipt := e.Integrated; receipt != nil {
		s.integrated[fmt.Sprintf("%s#%d", strings.ToLower(receipt.Repo), receipt.PR)] = *receipt
	}
	if i := e.Intent; i != nil {
		old, found := s.intents[i.Key]
		if !found || i.Generation > old.Generation {
			s.intents[i.Key] = cloneIntent(*i)
			delete(s.reservations, i.Key)
		}
	}
	if v := e.Evidence; v != nil {
		k := v.Subject.ID()
		supersededBy := ""
		var supersedingGeneration uint64
		supersedingAttempt := 0
		for id, prior := range s.evidence {
			old, next := prior.Subject, v.Subject
			sameProducer := id != k && strings.EqualFold(old.Repo, next.Repo) && old.Base == next.Base && old.Engine == next.Engine &&
				old.Authority == next.Authority && old.Policy == next.Policy && old.Producer == next.Producer && old.Workflow == next.Workflow
			completesUnknown := old.Generation == next.Generation && old.Attempt == next.Attempt && prior.State == Unknown && v.State != Unknown
			lateUnknown := old.Generation == next.Generation && old.Attempt == next.Attempt && prior.State != Unknown && v.State == Unknown
			if sameProducer && (completesUnknown || next.Generation > old.Generation || next.Generation == old.Generation && next.Attempt > old.Attempt) {
				prior.State, prior.InvalidatedBy, prior.Reason = Superseded, e.ID, "authoritative workflow attempt superseded"
				s.evidence[id] = prior
			} else if sameProducer && (lateUnknown || old.Generation > next.Generation || old.Generation == next.Generation && old.Attempt > next.Attempt) {
				if supersededBy == "" || old.Generation > supersedingGeneration || old.Generation == supersedingGeneration &&
					(old.Attempt > supersedingAttempt || old.Attempt == supersedingAttempt && id < supersededBy) {
					supersededBy, supersedingGeneration, supersedingAttempt = id, old.Generation, old.Attempt
				}
			}
		}
		old, found := s.evidence[k]
		progresses := v.ObservedAt.Equal(old.ObservedAt) && evidenceStateRank(v.State) > evidenceStateRank(old.State)
		if !found || old.State != Superseded && (v.ObservedAt.After(old.ObservedAt) || progresses) {
			next := cloneEvidence(*v)
			next.ObservedState = v.State
			if supersededBy != "" {
				next.State, next.InvalidatedBy, next.Reason = Superseded, supersededBy, "newer authoritative workflow attempt already observed"
			}
			for _, member := range next.Subject.Members {
				change, known := s.memberChanges[fmt.Sprintf("%s#%d", strings.ToLower(next.Subject.Repo), member.PR)]
				if known && !next.ObservedAt.After(change.At) && (change.MemberChange.Head == "" || change.MemberChange.Head != member.Head) {
					next.State, next.InvalidatedBy, next.Reason = Superseded, change.ID, change.Reason
				}
			}
			if change, found := s.groupChanges[groupChangeKey(next.Subject.Repo, next.Subject.Authority, next.Subject.TestedSHA)]; found {
				next.State, next.InvalidatedBy, next.Reason = Superseded, change.ID, change.Reason
			}
			if s.policies != nil && strings.HasPrefix(next.Subject.Policy, "workflow-policy-v1/") && !s.policyActive(next.Subject) {
				next.State, next.InvalidatedBy, next.Reason = Superseded, "active-policy-set", "producer policy no longer configured"
			}
			s.evidence[k] = next
		}
		for key, job := range s.jobs {
			if strings.EqualFold(job.Repo, v.Subject.Repo) && job.RunID == v.RunID && job.Attempt == v.Subject.Attempt {
				job.Candidates = appendCandidate(job.Candidates, k)
				s.jobs[key] = job
			}
		}
	}
	if change := e.GroupChange; change != nil {
		copyChange := *change
		e.GroupChange = &copyChange
		key := groupChangeKey(change.Repo, change.Authority, change.Head)
		s.groupChanges[key] = e
		for id, receipt := range s.evidence {
			if groupChangeKey(receipt.Subject.Repo, receipt.Subject.Authority, receipt.Subject.TestedSHA) == key {
				receipt.State, receipt.InvalidatedBy, receipt.Reason = Superseded, e.ID, e.Reason
				s.evidence[id] = receipt
			}
		}
	}
	if set := e.PolicySet; set != nil {
		s.policies = &PolicySet{Active: append([]PolicyBinding(nil), set.Active...)}
		for id, receipt := range s.evidence {
			if strings.HasPrefix(receipt.Subject.Policy, "workflow-policy-v1/") && !s.policyActive(receipt.Subject) {
				receipt.State, receipt.InvalidatedBy, receipt.Reason = Superseded, e.ID, "producer policy changed or removed"
				s.evidence[id] = receipt
			}
		}
	}
	if change := e.MemberChange; change != nil {
		copyChange := *change
		e.MemberChange = &copyChange
		key := fmt.Sprintf("%s#%d", strings.ToLower(change.Repo), change.PR)
		if previous, found := s.memberChanges[key]; !found || e.At.After(previous.At) {
			s.memberChanges[key] = e
		}
		for id, receipt := range s.evidence {
			for _, member := range receipt.Subject.Members {
				if strings.EqualFold(receipt.Subject.Repo, change.Repo) && member.PR == change.PR && !receipt.ObservedAt.After(e.At) && (change.Head == "" || member.Head != change.Head) {
					receipt.State, receipt.InvalidatedBy, receipt.Reason = Superseded, e.ID, e.Reason
					s.evidence[id] = receipt
				}
			}
		}
	}
	if j := e.Job; j != nil {
		old, found := s.jobs[j.Key()]
		wasRunning := old.Status == "in_progress" || old.Status == "running"
		regressesRunning := wasRunning && j.Status != "in_progress" && j.Status != "running" && j.Status != "completed"
		if !found || (!j.ObservedAt.Before(old.ObservedAt) && !regressesRunning && (old.Status != "completed" || j.Status == "completed")) {
			next := cloneJob(*j)
			if old.RunnerID > 0 && (wasRunning || old.Status == "completed" && old.Conclusion != "skipped") {
				if next.RunnerID == 0 {
					next.RunnerID = old.RunnerID
				}
				if next.StartedAt.IsZero() {
					next.StartedAt = old.StartedAt
				}
				if next.CompletedAt.IsZero() && old.Status == "completed" {
					next.CompletedAt = old.CompletedAt
				}
			}
			for _, id := range old.Candidates {
				next.Candidates = appendCandidate(next.Candidates, id)
			}
			for id, receipt := range s.evidence {
				if strings.EqualFold(receipt.Subject.Repo, j.Repo) && receipt.RunID == j.RunID && receipt.Subject.Attempt == j.Attempt {
					next.Candidates = appendCandidate(next.Candidates, id)
				}
			}
			s.jobs[j.Key()] = next
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
	if len(s.seen) >= maxJournalRecords && e.Release != "*" {
		return fmt.Errorf("integration journal index bound reached; observations unavailable, retain audit and disable enforcement")
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(b) >= maxJournalRecordBytes {
		return fmt.Errorf("integration event exceeds journal record bound")
	}
	if s.offset+int64(len(b)+1) > maxJournalBytes {
		return fmt.Errorf("integration journal byte bound reached; observations unavailable, retain audit and disable enforcement")
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
	s.offset += int64(len(b) + 1)
	return nil
}

func (s *Store) InvalidateGroup(repo, authority, head, delivery, reason string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.writerLock()
	if err != nil {
		return err
	}
	defer unlock()
	if _, exists := s.seen["group/"+delivery]; exists {
		return nil
	}
	return s.appendLocked(Event{Version: SchemaVersion, ID: "group/" + delivery, At: now, Reason: reason, GroupChange: &GroupChange{Repo: repo, Authority: authority, Head: head}})
}

func (s *Store) Append(e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.writerLock()
	if err != nil {
		return err
	}
	defer unlock()
	return s.appendLocked(e)
}

type Snapshot struct {
	Version      int                  `json:"version"`
	Intents      []Intent             `json:"intents"`
	Evidence     []Evidence           `json:"evidence"`
	Jobs         []Job                `json:"jobs"`
	Coverage     []Coverage           `json:"coverage"`
	Reservations []Reservation        `json:"reservations"`
	Integrated   []IntegrationReceipt `json:"integrated"`
	Drifts       []Drift              `json:"drifts"`
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
	for _, v := range s.integrated {
		out.Integrated = append(out.Integrated, v)
	}
	for _, v := range s.drifts {
		v.Actual = append([]string(nil), v.Actual...)
		v.OutsideAnticipated = append([]string(nil), v.OutsideAnticipated...)
		out.Drifts = append(out.Drifts, v)
	}
	return out
}

func (s *Store) Integrated(repo string, pr int) (IntegrationReceipt, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	receipt, ok := s.integrated[fmt.Sprintf("%s#%d", strings.ToLower(repo), pr)]
	return receipt, ok
}

func (s *Store) InvalidateMember(repo string, pr int, head, reason, delivery string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.writerLock()
	if err != nil {
		return err
	}
	defer unlock()
	if _, seen := s.seen["member/"+delivery]; seen {
		return nil
	}
	return s.appendLocked(Event{Version: SchemaVersion, ID: "member/" + delivery, At: now, Reason: reason, MemberChange: &MemberChange{Repo: repo, PR: pr, Head: head}})
}
