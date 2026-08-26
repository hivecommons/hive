package outcome

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Sentinel errors. Callers match with errors.Is; the wrapped detail names the
// offending key and generations for logs.
var (
	// ErrUnauthorizedActor: the actor is not in the configured maintainer
	// principal set. Agents, contributors, observers, the admission sweep,
	// bead lifecycle, events, and webhooks are NEVER in that set — this is the
	// accepted #4249 mutator rule made structural.
	ErrUnauthorizedActor = errors.New("actor is not an authorized outcome mutator")
	// ErrGenerationConflict: the caller named a stale expected generation.
	// The mutation is REFUSED, never merged — a stale writer is fenced, in
	// the same shape as the hub rotate/retire refusal.
	ErrGenerationConflict = errors.New("expected generation does not match current desired generation")
	// ErrOutcomeExists: create named an already-declared outcome.
	ErrOutcomeExists = errors.New("outcome is already declared")
	// ErrOutcomeNotFound: the named outcome has never been declared.
	ErrOutcomeNotFound = errors.New("outcome is not declared")
	// ErrOutcomeRetired: the outcome is terminally retired; no verb applies.
	ErrOutcomeRetired = errors.New("outcome is retired")
	// ErrInvalidTransition: the verb does not apply in the current state
	// (e.g. accepting an already-accepted generation).
	ErrInvalidTransition = errors.New("transition not valid in current state")
)

// ledgerFileMode is 0660 (group-writable) for the same reason beads.Store.persist
// uses it: the file lives under the shared /data root and more than one
// node-group member may legitimately rewrite it.
const ledgerFileMode = 0o660

// ledgerFormatVersion is the persisted schema version, written so a future
// widening is a deliberate, reviewable migration rather than an accident.
const ledgerFormatVersion = 1

// Receipt is the write-only audit record handed to the Mirror after a
// mutation durably persists. The intended mirror posts a human-readable
// comment to the governing GitHub issue; whatever it does, NOTHING in this
// package ever reads a mirror back — GitHub content is exhaust, not authority.
type Receipt struct {
	Verb       string
	Key        string
	Generation int
	State      State
	Actor      string
	Date       time.Time
	Spec       string
}

// Options configures a Ledger at Open time.
type Options struct {
	// Principals is the exact set of actors authorized to mutate outcomes
	// (per accepted #4249: maintainer/product principals only, initially
	// "clubanderson", extensible via config). Empty means NO mutation is
	// authorized — the safe default; the ledger stays readable.
	Principals []string
	// Mirror, when non-nil, receives a Receipt after each successful durable
	// mutation. It is fire-and-forget: it returns nothing, its failures are
	// its own to log, and it can never veto or roll back a mutation.
	Mirror func(Receipt)
	// Now supplies transition timestamps; nil means time.Now().UTC. A seam
	// for tests, exactly like the hub store's clock convention.
	Now func() time.Time
}

// Ledger is the durable outcome authority: an in-memory index over one JSON
// file, reloaded on boot, rewritten atomically on every mutation. No process
// memory is authoritative — restart reconstruction IS the file.
type Ledger struct {
	path       string
	mu         sync.Mutex
	records    map[string]*Record
	principals map[string]bool
	mirror     func(Receipt)
	now        func() time.Time
}

// persistedLedger is the on-disk shape, named separately so the file format is
// a deliberate artifact (the persistedGenerations convention).
type persistedLedger struct {
	Version int      `json:"version"`
	Records []Record `json:"records"`
}

// Open loads the ledger at path, creating an empty one if the file does not
// exist. A file that exists but cannot be parsed or validated is a REFUSAL:
// Open fails, the bytes are left exactly as found for operator inspection,
// and no handle exists through which a write could replace them — corrupt
// durable state must be visible and can never invent unmanaged intent or a
// fresh generation-1 truth (the accepted #4249 failure behavior, following
// the hub generations quarantine rationale).
func Open(path string, opts Options) (*Ledger, error) {
	l := &Ledger{
		path:       path,
		records:    make(map[string]*Record),
		principals: make(map[string]bool, len(opts.Principals)),
		mirror:     opts.Mirror,
		now:        opts.Now,
	}
	for _, p := range opts.Principals {
		p = strings.TrimSpace(p)
		if p != "" {
			l.principals[p] = true
		}
	}
	if l.now == nil {
		l.now = func() time.Time { return time.Now().UTC() }
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading outcome ledger %s: %w", path, err)
	}
	var persisted persistedLedger
	if err := json.Unmarshal(data, &persisted); err != nil {
		return nil, fmt.Errorf("outcome ledger %s is unparseable and is left untouched for inspection: %w", path, err)
	}
	for i := range persisted.Records {
		rec := persisted.Records[i]
		key := rec.Ref.Key()
		if key == "" {
			return nil, fmt.Errorf("outcome ledger %s holds a record with an invalid ref %+v", path, rec.Ref)
		}
		if _, dup := l.records[key]; dup {
			return nil, fmt.Errorf("outcome ledger %s holds conflicting records for %s", path, key)
		}
		if rec.Generation < 1 {
			return nil, fmt.Errorf("outcome ledger %s holds %s at impossible generation %d", path, key, rec.Generation)
		}
		stored := rec.clone()
		l.records[key] = &stored
	}
	return l, nil
}

// Get returns a detached copy of the record for ref. The copy is the
// projection seam: readers hold immutable values and can never race a writer.
func (l *Ledger) Get(ref Ref) (Record, bool) {
	return l.GetByKey(ref.Key())
}

// GetByKey is Get for callers holding a canonical key string.
func (l *Ledger) GetByKey(key string) (Record, bool) {
	if key == "" {
		return Record{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.records[key]
	if !ok {
		return Record{}, false
	}
	return rec.clone(), true
}

// List returns detached copies of every record, sorted by key for
// deterministic projections and tests.
func (l *Ledger) List() []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Record, 0, len(l.records))
	for _, rec := range l.records {
		out = append(out, rec.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref.Key() < out[j].Ref.Key() })
	return out
}

// Create declares a new outcome at generation 1, proposed.
func (l *Ledger) Create(ref Ref, spec string, workRefs []string, actor string) (Record, error) {
	if err := ref.Validate(); err != nil {
		return Record{}, err
	}
	if err := validateWorkRefs(workRefs); err != nil {
		return Record{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.authorize(actor); err != nil {
		return Record{}, err
	}
	key := ref.Key()
	if _, exists := l.records[key]; exists {
		return Record{}, fmt.Errorf("%s: %w", key, ErrOutcomeExists)
	}
	now := l.now()
	rec := &Record{
		Ref:        ref,
		Generation: 1,
		State:      StateProposed,
		Spec:       spec,
		WorkRefs:   append([]string(nil), workRefs...),
		History: []Transition{{
			Verb: "create", Generation: 1, Actor: actor, Date: now, Spec: spec,
		}},
	}
	return l.commit(key, rec, "create")
}

// Accept records maintainer acceptance of EXACTLY the named generation.
// Naming any other generation is a refused CAS conflict: acceptance can never
// drift onto a revision the principal did not read.
func (l *Ledger) Accept(ref Ref, expectedGeneration int, actor string) (Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, err := l.mutable(ref, expectedGeneration, actor)
	if err != nil {
		return Record{}, err
	}
	if rec.State != StateProposed {
		return Record{}, fmt.Errorf("accept %s at generation %d in state %s: %w",
			ref.Key(), rec.Generation, rec.State, ErrInvalidTransition)
	}
	rec.State = StateAccepted
	rec.History = append(rec.History, Transition{
		Verb: "accept", Generation: rec.Generation, Actor: actor, Date: l.now(),
	})
	return l.commit(ref.Key(), rec, "accept")
}

// Supersede advances the desired generation to N+1 with a new proposed spec.
// The prior generation is retained in History (its spec snapshot lives on the
// create/supersede transition that introduced it) and remains visible as
// stale; it can never again authorize a transition because comparison is
// against the CURRENT generation only.
func (l *Ledger) Supersede(ref Ref, expectedGeneration int, spec string, workRefs []string, actor string) (Record, error) {
	if err := validateWorkRefs(workRefs); err != nil {
		return Record{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, err := l.mutable(ref, expectedGeneration, actor)
	if err != nil {
		return Record{}, err
	}
	rec.Generation++
	rec.State = StateProposed
	rec.Spec = spec
	if len(workRefs) > 0 {
		rec.WorkRefs = append([]string(nil), workRefs...)
	}
	rec.History = append(rec.History, Transition{
		Verb: "supersede", Generation: rec.Generation, Actor: actor, Date: l.now(), Spec: spec,
	})
	return l.commit(ref.Key(), rec, "supersede")
}

// Retire terminally closes the outcome. No verb applies afterwards.
func (l *Ledger) Retire(ref Ref, expectedGeneration int, actor string) (Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, err := l.mutable(ref, expectedGeneration, actor)
	if err != nil {
		return Record{}, err
	}
	rec.State = StateRetired
	rec.History = append(rec.History, Transition{
		Verb: "retire", Generation: rec.Generation, Actor: actor, Date: l.now(),
	})
	return l.commit(ref.Key(), rec, "retire")
}

// authorize enforces the accepted mutator rule. Callers hold l.mu.
func (l *Ledger) authorize(actor string) error {
	if !l.principals[strings.TrimSpace(actor)] {
		return fmt.Errorf("%q: %w", actor, ErrUnauthorizedActor)
	}
	return nil
}

// mutable resolves the record for a mutation: authorized actor, declared
// outcome, not retired, and a CAS match on the expected generation. It
// returns a WORKING COPY; the stored record is replaced only by commit, so a
// failed persist leaves memory and disk agreeing.
func (l *Ledger) mutable(ref Ref, expectedGeneration int, actor string) (*Record, error) {
	if err := l.authorize(actor); err != nil {
		return nil, err
	}
	key := ref.Key()
	if key == "" {
		return nil, ref.Validate()
	}
	stored, ok := l.records[key]
	if !ok {
		return nil, fmt.Errorf("%s: %w", key, ErrOutcomeNotFound)
	}
	if stored.State == StateRetired {
		return nil, fmt.Errorf("%s: %w", key, ErrOutcomeRetired)
	}
	if stored.Generation != expectedGeneration {
		return nil, fmt.Errorf("%s: expected generation %d, current is %d: %w",
			key, expectedGeneration, stored.Generation, ErrGenerationConflict)
	}
	work := stored.clone()
	return &work, nil
}

// commit persists the mutated record atomically, installs it in memory only
// after the rename lands (persist-then-install, the hub retirement idiom),
// and then emits the write-only mirror receipt. Callers hold l.mu.
func (l *Ledger) commit(key string, rec *Record, verb string) (Record, error) {
	prior, hadPrior := l.records[key]
	l.records[key] = rec
	if err := l.persistLocked(); err != nil {
		if hadPrior {
			l.records[key] = prior
		} else {
			delete(l.records, key)
		}
		return Record{}, err
	}
	out := rec.clone()
	if l.mirror != nil {
		last := rec.History[len(rec.History)-1]
		l.mirror(Receipt{
			Verb:       verb,
			Key:        key,
			Generation: rec.Generation,
			State:      rec.State,
			Actor:      last.Actor,
			Date:       last.Date,
			Spec:       rec.Spec,
		})
	}
	return out, nil
}

// persistLocked writes the whole ledger: marshal → *.tmp → rename, exactly
// the beads.Store.persist idiom. Callers hold l.mu, which is what keeps two
// writers from interleaving bytes in the same tmp path.
func (l *Ledger) persistLocked() error {
	all := make([]Record, 0, len(l.records))
	for _, rec := range l.records {
		all = append(all, rec.clone())
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Ref.Key() < all[j].Ref.Key() })

	data, err := json.MarshalIndent(persistedLedger{Version: ledgerFormatVersion, Records: all}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling outcome ledger: %w", err)
	}
	tmpPath := l.path + ".tmp"
	if err := os.WriteFile(tmpPath, data, ledgerFileMode); err != nil {
		return fmt.Errorf("writing tmp outcome ledger: %w", err)
	}
	return os.Rename(tmpPath, l.path)
}
