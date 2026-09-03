package mutation

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

// Claim lifecycle states, exactly the accepted #4255 lifecycle.
const (
	// StateActiveMutation: the holder owns the resource at the entry's epoch
	// and consumes one mutation-writer slot. The ONLY state that may
	// authorize an external effect.
	StateActiveMutation = "ActiveMutation"
	// StateWaiting: the holder entered a CI/review/publication/external or
	// human-decision wait. Capacity is released and the epoch is fenced the
	// moment this state is entered; any further mutation requires
	// reacquisition at a strictly higher epoch.
	StateWaiting = "Waiting"
	// StateReleased: complete/abandon/revoke/expiry. The entry is retained
	// with its final epoch for fencing history; the slot is freed.
	StateReleased = "Released"
)

// Sentinel errors; callers match with errors.Is.
var (
	// ErrClaimHeld: an overlapping claim is actively held; the waiter
	// consumes no capacity and simply does not acquire.
	ErrClaimHeld = errors.New("an overlapping mutation claim is actively held")
	// ErrNoCapacity: the repository's mutation-writer slots are all consumed.
	ErrNoCapacity = errors.New("no mutation-writer capacity is available")
	// ErrStaleEpoch: the presented epoch is not the entry's current epoch, or
	// the entry is not in a state that epoch may act in. Every stale-owner
	// mutation, progress, completion, and acknowledgment attempt lands here.
	ErrStaleEpoch = errors.New("stale mutation epoch is fenced")
	// ErrInvalidClaim: the claim fails Validate and can never be held.
	ErrInvalidClaim = errors.New("mutation claim is invalid")
)

// ledgerFileMode matches the outcome/proof/beads persistence idiom.
const ledgerFileMode = 0o660
const ledgerLockFileSuffix = ".lock"

// ledgerFormatVersion is the persisted schema version.
const ledgerFormatVersion = 1

// DefaultMaxWritersPerRepo is the accepted default capacity: one
// mutation-writer slot per canonical repository.
const DefaultMaxWritersPerRepo = 1

// Entry is one durable claim record: the canonical claim, its holder, its
// monotonic epoch (per key, never reused — it survives Released and Waiting
// so a reacquisition is ALWAYS strictly higher), its lifecycle state, and its
// window.
type Entry struct {
	Claim  Claim  `json:"claim"`
	Holder string `json:"holder"`
	// Epoch increases monotonically per claim key on every acquisition and
	// reacquisition. It is the C2 fencing token.
	Epoch      uint64    `json:"epoch"`
	State      string    `json:"state"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// persistedLedger is the on-disk shape.
type persistedLedger struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

// Ledger is the durable claim/lease owner: an in-memory index over one JSON
// file, reloaded on boot, rewritten atomically on every accepted transition.
// Process memory is never authority — restart reconstruction IS the file.
// Every transition is a CAS on {key, expected epoch, expected state} under
// the ledger mutex, which is the accepted serialization rule for racing
// acquirers: one deterministic winner, no interleaved bytes.
type Ledger struct {
	path       string
	maxWriters int
	mu         sync.Mutex
	entries    map[string]*Entry
}

// OpenLedger loads the claim ledger at path, creating an empty one when the
// file does not exist. maxWritersPerRepo <= 0 selects the accepted default.
// A file that exists but cannot be parsed or validated is a REFUSAL: the
// bytes are left exactly as found for inspection and no handle exists through
// which a write could replace them — corrupt durable ownership must be
// visible and can never silently reset (the accepted #4249/#4251 failure
// behavior).
//
// Restart reconciliation: an entry read back in ActiveMutation whose window
// expired while the process was down reconciles to Waiting — fenced, never
// silently Released, never re-authorized at the old epoch.
func OpenLedger(path string, maxWritersPerRepo int) (*Ledger, error) {
	if maxWritersPerRepo <= 0 {
		maxWritersPerRepo = DefaultMaxWritersPerRepo
	}
	l := &Ledger{path: path, maxWriters: maxWritersPerRepo, entries: make(map[string]*Entry)}
	if err := l.reloadLocked(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Ledger) reloadLocked() error {
	data, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		l.entries = make(map[string]*Entry)
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading mutation claim ledger %s: %w", l.path, err)
	}
	var persisted persistedLedger
	if err := json.Unmarshal(data, &persisted); err != nil {
		return fmt.Errorf("mutation claim ledger %s is unparseable and is left untouched for inspection: %w", l.path, err)
	}
	entries := make(map[string]*Entry, len(persisted.Entries))
	for i := range persisted.Entries {
		e := persisted.Entries[i]
		if err := e.Claim.Validate(); err != nil {
			return fmt.Errorf("mutation claim ledger %s holds an invalid claim: %w", l.path, err)
		}
		key := e.Claim.Key()
		if _, dup := entries[key]; dup {
			return fmt.Errorf("mutation claim ledger %s holds conflicting entries for %s", l.path, key)
		}
		cp := e
		entries[key] = &cp
	}
	l.entries = entries
	return nil
}

func (l *Ledger) lockAndRefreshLocked() (func(), error) {
	lockPath := l.path + ledgerLockFileSuffix
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o770); err != nil {
		return nil, fmt.Errorf("creating mutation claim ledger lock directory: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, ledgerFileMode)
	if err != nil {
		return nil, fmt.Errorf("opening mutation claim ledger lock %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("locking mutation claim ledger %s: %w", lockPath, err)
	}
	if err := l.reloadLocked(); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// reconcileExpiredLocked fences an ActiveMutation entry whose window has
// passed: it moves to Waiting at its OWN epoch (retained for history), so the
// old owner is fenced and the slot is freed, but ownership is never silently
// released. Callers hold l.mu.
func (l *Ledger) reconcileExpiredLocked(now time.Time) bool {
	changed := false
	for _, e := range l.entries {
		if e.State == StateActiveMutation && now.After(e.ExpiresAt) {
			e.State = StateWaiting
			changed = true
		}
	}
	return changed
}

// activeWritersLocked counts consumed mutation-writer slots in a repo:
// ONLY ActiveMutation consumes; Waiting and Released are structural zeros.
func (l *Ledger) activeWritersLocked(repo string) int {
	n := 0
	for _, e := range l.entries {
		if e.State == StateActiveMutation && e.Claim.Repo == repo {
			n++
		}
	}
	return n
}

// Acquire takes (or retakes) a claim for holder, minting a strictly higher
// epoch than the key has ever carried. The rules, in order:
//
//  1. An invalid claim can never be held.
//  2. Any actively held overlapping claim refuses acquisition (ErrClaimHeld):
//     exactly one owner per overlap set. The refused acquirer consumes no
//     capacity — it simply does not acquire.
//  3. A repo with all writer slots consumed refuses (ErrNoCapacity), unless
//     the acquisition is the reacquisition of this exact key (its own prior
//     hold is the one being replaced).
//  4. Otherwise the entry moves to ActiveMutation at epoch prev+1, persisted
//     before the grant is returned (durable-before-authoritative).
func (l *Ledger) Acquire(c Claim, holder string, ttl time.Duration, now time.Time) (Entry, error) {
	if err := c.Validate(); err != nil {
		return Entry{}, fmt.Errorf("%w: %v", ErrInvalidClaim, err)
	}
	if holder == "" {
		return Entry{}, fmt.Errorf("%w: a claim requires a holder identity", ErrInvalidClaim)
	}
	key := c.Key()
	l.mu.Lock()
	defer l.mu.Unlock()
	unlock, err := l.lockAndRefreshLocked()
	if err != nil {
		return Entry{}, err
	}
	defer unlock()
	if l.reconcileExpiredLocked(now) {
		if err := l.persistLocked(); err != nil {
			return Entry{}, err
		}
	}

	for otherKey, e := range l.entries {
		if e.State != StateActiveMutation {
			continue
		}
		if otherKey == key {
			return Entry{}, fmt.Errorf("%s is actively held by %s at epoch %d: %w", key, e.Holder, e.Epoch, ErrClaimHeld)
		}
		if e.Claim.Overlaps(c) {
			return Entry{}, fmt.Errorf("%s overlaps active %s (held by %s at epoch %d): %w",
				key, otherKey, e.Holder, e.Epoch, ErrClaimHeld)
		}
	}
	if l.activeWritersLocked(c.Repo) >= l.maxWriters {
		return Entry{}, fmt.Errorf("repo %s writers exhausted (max %d): %w", c.Repo, l.maxWriters, ErrNoCapacity)
	}

	var prevEpoch uint64
	if prior, ok := l.entries[key]; ok {
		prevEpoch = prior.Epoch
	}
	next := &Entry{
		Claim:      c,
		Holder:     holder,
		Epoch:      prevEpoch + 1,
		State:      StateActiveMutation,
		AcquiredAt: now,
		ExpiresAt:  now.Add(ttl),
	}
	prior, hadPrior := l.entries[key]
	l.entries[key] = next
	if err := l.persistLocked(); err != nil {
		if hadPrior {
			l.entries[key] = prior
		} else {
			delete(l.entries, key)
		}
		return Entry{}, err
	}
	return *next, nil
}

// transitionLocked applies one CAS state transition for key at expectedEpoch.
func (l *Ledger) transition(key string, expectedEpoch uint64, from map[string]bool, to string, now time.Time) (Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	unlock, err := l.lockAndRefreshLocked()
	if err != nil {
		return Entry{}, err
	}
	defer unlock()
	if l.reconcileExpiredLocked(now) {
		if err := l.persistLocked(); err != nil {
			return Entry{}, err
		}
	}
	e, ok := l.entries[key]
	if !ok || e.Epoch != expectedEpoch || !from[e.State] {
		return Entry{}, fmt.Errorf("%s: expected epoch %d in %v: %w", key, expectedEpoch, keysOf(from), ErrStaleEpoch)
	}
	priorState := e.State
	e.State = to
	if err := l.persistLocked(); err != nil {
		e.State = priorState
		return Entry{}, err
	}
	return *e, nil
}

// Wait moves an active hold into the Waiting state: the writer slot is freed
// and the epoch is fenced — before another mutation the holder MUST
// reacquire, which mints a strictly higher epoch.
func (l *Ledger) Wait(key string, expectedEpoch uint64, now time.Time) (Entry, error) {
	return l.transition(key, expectedEpoch, map[string]bool{StateActiveMutation: true}, StateWaiting, now)
}

// Release retires a hold on any release path (complete, abandon, revoke).
// The entry is retained with its final epoch for fencing history.
func (l *Ledger) Release(key string, expectedEpoch uint64, now time.Time) (Entry, error) {
	return l.transition(key, expectedEpoch,
		map[string]bool{StateActiveMutation: true, StateWaiting: true}, StateReleased, now)
}

// ValidateEpoch reports whether epoch is the CURRENT authorized epoch for
// key: the entry exists, is in ActiveMutation, is unexpired, and carries
// exactly this epoch. This is the check the executor applies at the actual
// mutation boundary and again before acknowledgment. Duplicate or
// out-of-order commands cannot manufacture authority: the answer is computed
// from current durable state, never from the caller's history.
func (l *Ledger) ValidateEpoch(key string, epoch uint64, now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	unlock, err := l.lockAndRefreshLocked()
	if err != nil {
		return err
	}
	defer unlock()
	if l.reconcileExpiredLocked(now) {
		if err := l.persistLocked(); err != nil {
			return err
		}
	}
	e, ok := l.entries[key]
	if !ok {
		return fmt.Errorf("%s has no claim entry: %w", key, ErrStaleEpoch)
	}
	if e.State != StateActiveMutation {
		return fmt.Errorf("%s is %s, not actively held: %w", key, e.State, ErrStaleEpoch)
	}
	if e.Epoch != epoch {
		return fmt.Errorf("%s current epoch is %d, presented %d: %w", key, e.Epoch, epoch, ErrStaleEpoch)
	}
	return nil
}

// Get returns a copy of the entry stored under key.
func (l *Ledger) Get(key string) (Entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	unlock, err := l.lockAndRefreshLocked()
	if err == nil {
		defer unlock()
	}
	e, ok := l.entries[key]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

func (l *Ledger) persistLocked() error {
	all := make([]Entry, 0, len(l.entries))
	for _, e := range l.entries {
		all = append(all, *e)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Claim.Key() < all[j].Claim.Key() })
	data, err := json.MarshalIndent(persistedLedger{Version: ledgerFormatVersion, Entries: all}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling mutation claim ledger: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o770); err != nil {
		return fmt.Errorf("creating mutation claim ledger directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(l.path), filepath.Base(l.path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("writing tmp mutation claim ledger: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing tmp mutation claim ledger: %w", err)
	}
	if err := tmp.Chmod(ledgerFileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod tmp mutation claim ledger: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing tmp mutation claim ledger: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing tmp mutation claim ledger: %w", err)
	}
	if err := os.Rename(tmpPath, l.path); err != nil {
		return fmt.Errorf("renaming tmp mutation claim ledger: %w", err)
	}
	cleanup = false
	if dir, err := os.Open(filepath.Dir(l.path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
