package blocker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Entry is what the ledger remembers about one item.
type Entry struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	// Blocker is the structured blocker, nil while the item only has a
	// plain needs-human reason on record (kept so the loop guard can
	// compare the next one).
	Blocker *Blocker `json:"blocker,omitempty"`
	// Reason is the normalised needs-human reason last reported.
	Reason     string    `json:"reason,omitempty"`
	ReportedAt time.Time `json:"reported_at,omitempty"`
	// DelegatedUnparkAt is when "Let hive decide" last lifted the park;
	// ReasonAtUnpark is the reason it answered.
	DelegatedUnparkAt time.Time `json:"delegated_unpark_at,omitempty"`
	ReasonAtUnpark    string    `json:"reason_at_unpark,omitempty"`
	// Reapplied is the loop guard: the same reason came back after
	// DelegatedUnparkAt.
	Reapplied bool `json:"reapplied,omitempty"`
	// ClearedAt is when the blocker was lifted (the permission was
	// granted); reports from before it are stale and are not re-recorded.
	ClearedAt time.Time `json:"cleared_at,omitempty"`
}

// stale reports whether an observation at `at` predates the last clear.
func (e Entry) stale(at time.Time) bool {
	return !e.ClearedAt.IsZero() && !at.After(e.ClearedAt)
}

// Blocks reports whether the entry parks the item on a blocker (as opposed
// to merely remembering a reason).
func (e Entry) Blocks() bool { return e.Blocker != nil && e.Blocker.Kind != "" }

// Key is "owner/repo#N" lower-cased.
func Key(repo string, number int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
}

// Ledger is the per-item blocker record, persisted as one JSON file.
type Ledger struct {
	mu      sync.Mutex
	path    string
	entries map[string]Entry
	now     func() time.Time
	loaded  bool
}

// NewLedger returns a ledger backed by path (loaded lazily; a missing or
// unreadable file starts empty). An empty path keeps it in memory.
func NewLedger(path string) *Ledger {
	return &Ledger{path: path, entries: map[string]Entry{}, now: time.Now}
}

// SetNow overrides the clock (tests).
func (l *Ledger) SetNow(now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
}

func (l *Ledger) load() {
	if l.loaded {
		return
	}
	l.loaded = true
	if l.path == "" {
		return
	}
	data, err := os.ReadFile(l.path)
	if err != nil {
		return
	}
	var file struct {
		Entries []Entry `json:"entries"`
	}
	if json.Unmarshal(data, &file) != nil {
		return
	}
	for _, e := range file.Entries {
		l.entries[Key(e.Repo, e.Number)] = e
	}
}

func (l *Ledger) save() error {
	if l.path == "" {
		return nil
	}
	entries := make([]Entry, 0, len(l.entries))
	for _, e := range l.entries {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		return Key(entries[i].Repo, entries[i].Number) < Key(entries[j].Repo, entries[j].Number)
	})
	data, err := json.MarshalIndent(struct {
		Entries []Entry `json:"entries"`
	}{entries}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// Observe reads one comment or body on the item and records what it says:
// a structured blocker when text carries the marker or the prose infers
// one, else (when text is a needs-human report) the plain reason for the
// loop guard. It returns the resulting entry and whether text changed it.
func (l *Ledger) Observe(repo string, number int, text string, at time.Time) (Entry, bool) {
	if strings.TrimSpace(text) == "" {
		return Entry{}, false
	}
	if e, ok := l.Get(repo, number); ok && !at.IsZero() && e.stale(at) {
		return e, false
	}
	b, ok := Detect(text, at)
	if !ok {
		if !needsHumanRE.MatchString(stripMarkers(text)) {
			e, _ := l.Get(repo, number)
			return e, false
		}
		return l.RecordReason(repo, number, NormalizeReason(text), at), true
	}
	return l.Record(repo, number, b, NormalizeReason(text)), true
}

// Record stores a structured blocker on the item. A reason that matches
// the one a delegated un-park answered, arriving after it, also sets the
// loop guard.
func (l *Ledger) Record(repo string, number int, b Blocker, reason string) Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.load()
	if b.Since.IsZero() {
		b.Since = l.now()
	}
	k := Key(repo, number)
	e := l.entries[k]
	if e.stale(b.Since) {
		return e
	}
	// The observation time drives the loop guard; Since stays the first
	// report of this kind so the row's age does not reset on re-reports.
	observedAt := b.Since
	e.Repo, e.Number = repo, number
	if e.Blocker != nil && e.Blocker.Kind == b.Kind && !e.Blocker.Since.IsZero() && e.Blocker.Since.Before(b.Since) {
		b.Since = e.Blocker.Since
	}
	e.Blocker = &b
	e.Reason = reason
	e.ReportedAt = observedAt
	e.Reapplied = e.Reapplied || l.reapplied(e, reason, observedAt)
	l.entries[k] = e
	_ = l.save()
	return e
}

// RecordReason remembers a plain needs-human reason (no structured kind).
// When it is the reason a delegated un-park already answered, the loop
// guard trips and the item is parked on HumanOnlyReasked.
func (l *Ledger) RecordReason(repo string, number int, reason string, at time.Time) Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.load()
	if at.IsZero() {
		at = l.now()
	}
	k := Key(repo, number)
	e := l.entries[k]
	if e.stale(at) {
		return e
	}
	e.Repo, e.Number = repo, number
	e.Reason = reason
	e.ReportedAt = at
	if l.reapplied(e, reason, at) {
		e.Reapplied = true
		if e.Blocker == nil {
			e.Blocker = &Blocker{Kind: HumanOnlyReasked, Detail: reason, Source: SourceLoopGuard, Since: at}
		}
	}
	l.entries[k] = e
	_ = l.save()
	return e
}

func (l *Ledger) reapplied(e Entry, reason string, at time.Time) bool {
	return !e.DelegatedUnparkAt.IsZero() && at.After(e.DelegatedUnparkAt) && reason != "" && reason == e.ReasonAtUnpark
}

// NoteDelegatedUnpark records that "Let hive decide" lifted the park,
// answering the reason on record. The blocker (if any) is kept: a
// capability gap is not answered by a direction.
func (l *Ledger) NoteDelegatedUnpark(repo string, number int, at time.Time) Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.load()
	if at.IsZero() {
		at = l.now()
	}
	k := Key(repo, number)
	e := l.entries[k]
	e.Repo, e.Number = repo, number
	e.DelegatedUnparkAt = at
	e.ReasonAtUnpark = e.Reason
	e.Reapplied = false
	l.entries[k] = e
	_ = l.save()
	return e
}

// Get returns the entry for the item.
func (l *Ledger) Get(repo string, number int) (Entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.load()
	e, ok := l.entries[Key(repo, number)]
	return e, ok
}

// Blocking returns the item's blocker when it has one.
func (l *Ledger) Blocking(repo string, number int) (Blocker, bool) {
	e, ok := l.Get(repo, number)
	if !ok || !e.Blocks() {
		return Blocker{}, false
	}
	return *e.Blocker, true
}

// Clear forgets the item entirely (it closed, or a person resolved it).
func (l *Ledger) Clear(repo string, number int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.load()
	delete(l.entries, Key(repo, number))
	_ = l.save()
}

// ClearKind lifts every blocker of kind k (the permission was granted) and
// returns how many items it released. The entries stay so the loop guard
// keeps its history; only the blocker goes.
func (l *Ledger) ClearKind(k Kind) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.load()
	n := 0
	for key, e := range l.entries {
		if e.Blocker != nil && e.Blocker.Kind == k {
			e.Blocker = nil
			e.Reapplied = false
			e.ClearedAt = l.now()
			l.entries[key] = e
			n++
		}
	}
	if n > 0 {
		_ = l.save()
	}
	return n
}

// Blockers lists every current blocker, sorted by item key.
func (l *Ledger) Blockers() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.load()
	out := make([]Entry, 0, len(l.entries))
	for _, e := range l.entries {
		if e.Blocks() {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return Key(out[i].Repo, out[i].Number) < Key(out[j].Repo, out[j].Number) })
	return out
}

// Capabilities summarises the capability gaps the ledger knows: the
// missing App permissions and how many items each blocks.
type Capabilities struct {
	Missing      []string       `json:"missing"`
	BlockedItems int            `json:"blocked_items"`
	ByPermission map[string]int `json:"by_permission,omitempty"`
}

// Capabilities computes the summary.
func (l *Ledger) Capabilities() Capabilities {
	c := Capabilities{Missing: []string{}, ByPermission: map[string]int{}}
	for _, e := range l.Blockers() {
		p := e.Blocker.Kind.Permission()
		if p == "" {
			continue
		}
		c.BlockedItems++
		c.ByPermission[p]++
	}
	for p := range c.ByPermission {
		c.Missing = append(c.Missing, p)
	}
	sort.Strings(c.Missing)
	return c
}

// Shared ledger: one per process, so the webhook (pkg/github), the stage
// derivation and the dashboard read the same record without wiring.

// LedgerPath is where the shared ledger persists; tests point it elsewhere
// before the first Shared() call or use SetShared.
var LedgerPath = "/data/item-blockers.json"

var (
	sharedMu sync.Mutex
	shared   *Ledger
)

// Shared returns the process-wide ledger.
func Shared() *Ledger {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if shared == nil {
		shared = NewLedger(LedgerPath)
	}
	return shared
}

// SetShared replaces the process-wide ledger (tests) and returns the
// previous one.
func SetShared(l *Ledger) *Ledger {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	prev := shared
	shared = l
	return prev
}

// ErrNotBlocked is returned by callers that expected a blocker on an item.
var ErrNotBlocked = errors.New("item has no recorded blocker")
