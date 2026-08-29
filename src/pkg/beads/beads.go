package beads

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusOpen       Status = "open"
	StatusInProgress Status = "in_progress"
	StatusBlocked    Status = "blocked"
	StatusDone       Status = "done"
	StatusClosed     Status = "closed"
)

type BeadType string

const (
	TypeBug      BeadType = "bug"
	TypeFeature  BeadType = "feature"
	TypeTask     BeadType = "task"
	TypeEpic     BeadType = "epic"
	TypeChore    BeadType = "chore"
	TypeDecision BeadType = "decision"
	TypeAdvisory BeadType = "advisory"
)

type Priority int

const (
	PriorityCritical Priority = 0
	PriorityHigh     Priority = 1
	PriorityMedium   Priority = 2
	PriorityLow      Priority = 3
	PriorityMinor    Priority = 4
)

// flexTime wraps time.Time with lenient JSON parsing that accepts
// RFC3339 and common short forms like "2006-01-02T15:04Z".
type flexTime struct{ time.Time }

var flexTimeFormats = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04Z",
	"2006-01-02T15:04-07:00",
	"2006-01-02T15:04:05",
	"2006-01-02",
	// Agents write beads through the bd CLI but occasionally hand-edit or
	// template a human-format timestamp (observed live on torch-spyre:
	// "2026-07-23 16:04 EDT"). Accept the common space-separated shapes so a
	// well-meaning-but-wrong value still parses. time.Parse resolves a bare
	// zone abbreviation to offset 0 when it isn't the local zone — hours-level
	// imprecision, which beats the alternative below.
	"2006-01-02 15:04:05 MST",
	"2006-01-02 15:04 MST",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
}

func (ft *flexTime) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		return nil
	}
	for _, layout := range flexTimeFormats {
		if t, err := time.Parse(layout, s); err == nil {
			ft.Time = t
			return nil
		}
	}
	// FAIL-SOFT, deliberately. This unmarshals inside the store-wide
	// json.Unmarshal of beads.json, so returning an error here poisons the
	// ENTIRE agent ledger over one malformed field: the store is dropped from
	// beadStores, the hive runs on a partial ledger for the life of the
	// process, and the hub raises "N bead store(s) failed to load at startup"
	// (observed live: torch-spyre ran degraded for a month over a single
	// hand-written timestamp). A zero time is strictly less wrong — the bead
	// stays visible with unknown age, and zero-CreatedAt beads simply sort
	// oldest. Callers that must distinguish "absent" from "unparseable" can't
	// anyway: both arrive as the zero value, same as the "" case above.
	ft.Time = time.Time{}
	return nil
}

func (ft flexTime) MarshalJSON() ([]byte, error) {
	return json.Marshal(ft.Format(time.RFC3339Nano))
}

type Bead struct {
	ID          string                 `json:"id"`
	Title       string                 `json:"title"`
	Type        BeadType               `json:"type"`
	Status      Status                 `json:"status"`
	Priority    Priority               `json:"priority"`
	Actor       string                 `json:"actor"`
	ExternalRef string                 `json:"external_ref,omitempty"`
	Metadata    map[string]interface{} `json:"metadata,omitempty"`
	Notes       string                 `json:"notes,omitempty"`
	CreatedAt   flexTime               `json:"created_at"`
	UpdatedAt   flexTime               `json:"updated_at"`
	ClosedAt    *flexTime              `json:"closed_at,omitempty"`
	// LastSeenAt is when this bead's underlying condition was last CONFIRMED
	// to still hold — set by Upsert every time an agent re-files the same
	// finding. It is deliberately distinct from UpdatedAt, which any edit
	// touches: staleness pruning needs "nobody has re-reported this", not
	// "nothing has written to this". nil means the bead predates Upsert and is
	// never pruned for staleness.
	LastSeenAt *flexTime `json:"last_seen_at,omitempty"`
	DependsOn  []string  `json:"depends_on,omitempty"`
}

// Meta returns a metadata value as a string, or "" if missing/non-string.
func (b *Bead) Meta(key string) string {
	if v, ok := b.Metadata[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprintf("%v", v)
	}
	return ""
}

const maxBeadCount = 5000

type Store struct {
	dir    string
	hiveID string
	beads  map[string]*Bead
	// retired holds the IDs of beads that were removed from the live map after
	// reaching a TERMINAL state — archived by lifecycle culling, or evicted by
	// evictOldClosed once the store passes maxBeadCount. Both removal paths only
	// ever remove closed/done beads, so membership here means "this bead existed
	// and was satisfied", which is materially different from an ID that was never
	// seen. Dependency resolution needs that distinction: without it a satisfied
	// dependency that later gets culled resolves nowhere and permanently withholds
	// its dependent (kubestellar/hive#3845 review).
	retired map[string]bool
	mu      sync.RWMutex
}

func NewStore(dir string) (*Store, error) {
	// 0770 (group-writable), not 0755: agent bead dirs under /data/beads/<agent>
	// are owned by that agent's UID but must be writable by other node-group
	// members — e.g. the dashboard/hub process minting an issue-sourced epic into
	// the architect's store. This matches the shared-node-group model used for
	// /data/home/* (see pkg/agent/permissions_watcher DirPerms=0o770).
	if err := os.MkdirAll(dir, 0770); err != nil {
		return nil, fmt.Errorf("creating beads dir %s: %w", dir, err)
	}
	// The hive process runs as dev (1001) but agents write beads as their own
	// UIDs (2001+) sharing only the node group — the dir must be group-writable
	// with setgid, and MkdirAll's mode is clipped by the umask, so set it
	// explicitly. Best-effort: an already-correct or foreign-owned dir is fine.
	//
	// NOTE the constant: os.Chmod takes an os.FileMode, where setgid is
	// os.ModeSetgid (1<<29) and NOT the Unix octal 0o2000. Passing 0o2770 here
	// silently requests plain 0770 — the setgid bit is dropped before the
	// syscall, with no error — which is why the dir came out drwxrwx--- and the
	// regression test caught it only on Linux (where the assertion is guarded).
	//
	// Re-group BEFORE the chmod (fma incident): on OpenShift the PVC root
	// carries the namespace's random fsGroup with setgid, so a freshly created
	// dir INHERITS that foreign gid — and at 0770 the hive server (which drops
	// the fsGroup supplementary group at privilege drop) can never traverse
	// it, silently losing the whole store at the next boot. The creator owns
	// the dir and shares the node group with every hive UID, so pin the group
	// to the creator's egid. Best-effort, like the chmod; chown can clear
	// setgid on some platforms, so it must come first.
	_ = os.Chown(dir, -1, os.Getegid())
	_ = os.Chmod(dir, 0o770|os.ModeSetgid)

	s := &Store{
		dir:     dir,
		beads:   make(map[string]*Bead),
		retired: make(map[string]bool),
	}

	if err := s.load(); err != nil {
		return nil, fmt.Errorf("loading beads from %s: %w", dir, err)
	}

	return s, nil
}

// SetHiveID configures the Hive ID that will be stamped into new bead metadata.
func (s *Store) SetHiveID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hiveID = id
}

// hiveIDMetadataKey is the metadata key used to record which hive instance created a bead.
const hiveIDMetadataKey = "hive_id"

var validBeadTypes = map[BeadType]bool{
	TypeBug: true, TypeFeature: true, TypeTask: true,
	TypeEpic: true, TypeChore: true, TypeDecision: true, TypeAdvisory: true,
}

func (s *Store) Create(title string, beadType BeadType, priority Priority, actor string, externalRef string) (*Bead, error) {
	if !validBeadTypes[beadType] {
		return nil, fmt.Errorf("invalid bead type %q", beadType)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.lockAndRefresh()()

	now := flexTime{time.Now().UTC()}
	metadata := make(map[string]interface{})
	if s.hiveID != "" {
		metadata[hiveIDMetadataKey] = s.hiveID
	}

	b := &Bead{
		ID:          uuid.New().String()[:12],
		Title:       title,
		Type:        beadType,
		Status:      StatusOpen,
		Priority:    priority,
		Actor:       actor,
		ExternalRef: externalRef,
		Metadata:    metadata,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	s.beads[b.ID] = b
	s.evictOldClosed()
	return b, s.persist(b)
}

// upsertTitleKey collapses a title to a match key for Upsert: lowercase,
// letters only, digits and punctuation dropped. Agents re-file a persistent
// finding with cosmetic drift ("run #3279" -> "run #3291"), and matching on the
// exact string would create a fresh bead for every re-report — which is how
// advisory beads accumulated forever. Nothing semantic is folded, so two
// findings that differ in WORDS never collide.
func upsertTitleKey(title string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(title) {
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return strings.TrimSpace(strings.ToLower(title))
	}
	return b.String()
}

// Upsert records a finding without duplicating it: if an OPEN bead of the same
// type already carries an equivalent title, its LastSeenAt is refreshed (and
// its priority raised if this report is more severe) and that bead is returned;
// otherwise a new bead is created with LastSeenAt set.
//
// This is what lets advisory agents re-file the same finding every cycle: the
// re-report is the signal that the condition still holds, and it is exactly
// that signal PruneStaleAdvisoryBeads uses to retire findings nobody reports
// anymore. Closed/done beads never match, so a condition that recurs after
// being resolved opens a fresh bead.
func (s *Store) Upsert(title string, beadType BeadType, priority Priority, actor string, externalRef string) (*Bead, error) {
	if !validBeadTypes[beadType] {
		return nil, fmt.Errorf("invalid bead type %q", beadType)
	}
	key := upsertTitleKey(title)

	s.mu.RLock()
	var match *Bead
	for _, b := range s.beads {
		if b.Type != beadType {
			continue
		}
		if b.Status == StatusClosed || b.Status == StatusDone {
			continue
		}
		if upsertTitleKey(b.Title) != key {
			continue
		}
		match = b
		break
	}
	s.mu.RUnlock()

	if match != nil {
		if err := s.Update(match.ID, func(b *Bead) {
			now := flexTime{time.Now().UTC()}
			b.LastSeenAt = &now
			// Priority is an ORDER: Critical is 0, Minor is 4. A more severe
			// re-report must be able to raise the bead, never lower it.
			if priority < b.Priority {
				b.Priority = priority
			}
		}); err != nil {
			return nil, err
		}
		return s.Get(match.ID)
	}

	created, err := s.Create(title, beadType, priority, actor, externalRef)
	if err != nil {
		return nil, err
	}
	if err := s.SetLastSeenAt(created.ID, time.Now()); err != nil {
		return nil, err
	}
	return s.Get(created.ID)
}

// SetLastSeenAt stamps when a bead's condition was last confirmed to hold.
// Upsert does this on every re-report; this is the explicit form for callers
// that already hold a bead ID (and the only way to set the stamp to a time
// other than now, which staleness tests need). flexTime stays unexported, so
// this is also the package's sole entry point for writing the field.
func (s *Store) SetLastSeenAt(id string, t time.Time) error {
	return s.Update(id, func(b *Bead) {
		ft := flexTime{t.UTC()}
		b.LastSeenAt = &ft
	})
}

// LastSeen returns when the bead's condition was last confirmed, and whether it
// has ever been stamped. Callers outside the package cannot read the unexported
// flexTime, so this is how they ask.
func (b *Bead) LastSeen() (time.Time, bool) {
	if b == nil || b.LastSeenAt == nil {
		return time.Time{}, false
	}
	return b.LastSeenAt.Time, true
}

func (s *Store) evictOldClosed() {
	if len(s.beads) <= maxBeadCount {
		return
	}
	var closedIDs []string
	for id, b := range s.beads {
		if b.Status == StatusClosed || b.Status == StatusDone {
			closedIDs = append(closedIDs, id)
		}
	}
	sort.Slice(closedIDs, func(i, j int) bool {
		return s.beads[closedIDs[i]].UpdatedAt.Before(s.beads[closedIDs[j]].UpdatedAt.Time)
	})
	for _, id := range closedIDs {
		if len(s.beads) <= maxBeadCount {
			break
		}
		// Record the eviction the same way Archive does before dropping it.
		// Eviction only ever takes CLOSED/DONE beads, so losing the ID silently
		// would turn a satisfied dependency into an unresolvable one and
		// permanently withhold whatever depended on it.
		//
		// If the archive cannot be written, KEEP the bead. Evicting anyway would
		// retire it in memory only, and the next restart would forget — the same
		// silent withholding, just deferred. Eviction is an opportunistic memory
		// bound, so skipping one bead until the next Create is the cheap side of
		// this trade.
		if !s.appendArchiveEntry(s.beads[id]) {
			continue
		}
		delete(s.beads, id)
		s.retired[id] = true
	}
}

// appendArchiveEntry writes one archive record, reporting whether the record
// actually reached disk. Callers use that to decide whether removing the bead
// is safe: an in-memory-only retirement is forgotten on restart. The record
// contents come from newArchivedBead — the same builder Archive() uses — so
// eviction preserves exactly what archival preserves (#3971).
func (s *Store) appendArchiveEntry(b *Bead) bool {
	if b == nil {
		return false
	}
	data, err := json.Marshal(newArchivedBead(b))
	if err != nil {
		return false
	}
	f, err := os.OpenFile(filepath.Join(s.dir, archiveFileName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return false
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close() // best-effort cleanup; the write already failed
		return false
	}
	// This function's whole contract is "did the record actually reach disk" —
	// a deferred, error-ignored Close would let a failed flush report success,
	// so the caller deletes the in-memory bead believing it is safely archived.
	if err := f.Close(); err != nil {
		return false
	}
	return true
}

func (s *Store) Update(id string, fn func(b *Bead)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.lockAndRefresh()()

	b, ok := s.beads[id]
	if !ok {
		return fmt.Errorf("bead %s not found", id)
	}

	wasTerminal := b.Status == StatusDone || b.Status == StatusClosed
	fn(b)
	now := flexTime{time.Now().UTC()}
	isTerminal := b.Status == StatusDone || b.Status == StatusClosed
	if isTerminal && !wasTerminal && b.ClosedAt == nil {
		b.ClosedAt = &now
	}
	b.UpdatedAt = now

	return s.persist(b)
}

func (s *Store) Claim(id string) error {
	return s.Update(id, func(b *Bead) {
		b.Status = StatusInProgress
	})
}

func (s *Store) Close(id string) error {
	return s.Update(id, func(b *Bead) {
		now := flexTime{time.Now().UTC()}
		b.Status = StatusClosed
		b.ClosedAt = &now
	})
}

func (s *Store) Get(id string) (*Bead, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	b, ok := s.beads[id]
	if !ok {
		return nil, fmt.Errorf("bead %s not found", id)
	}
	return b, nil
}

type ListFilter struct {
	Status      *Status
	Actor       *string
	ExternalRef *string
}

func (s *Store) List(filter ListFilter) []*Bead {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*Bead
	for _, b := range s.beads {
		if filter.Status != nil && b.Status != *filter.Status {
			continue
		}
		if filter.Actor != nil && b.Actor != *filter.Actor {
			continue
		}
		if filter.ExternalRef != nil && b.ExternalRef != *filter.ExternalRef {
			continue
		}
		result = append(result, b)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.Before(result[j].CreatedAt.Time)
	})

	return result
}

// ReadEach invokes fn once per bead matching filter, in the same
// creation-ordered sequence as List, WHILE HOLDING the store's read lock.
//
// Why this exists (kubestellar/hive#3845). List returns the store's LIVE
// *Bead pointers and releases the lock before the caller reads them, so every
// field read off a List result races any concurrent Update — which mutates
// beads in place under the write lock (Status and DependsOn via the caller's
// fn, plus UpdatedAt/ClosedAt). That is latent for an occasional reader, but
// the contributor-neutral admission gate reads every bead in every store on
// every ReadyQueue and selectTask pass, concurrently with the inception watcher
// and the planning decomposer calling Close/AddDependency. `go test -race`
// reproduces it within a few hundred iterations. Reading inside the lock removes
// the race at its source rather than narrowing the window.
//
// Two rules for fn, enforced by nothing but this comment:
//
//   - It MUST NOT retain the *Bead (or alias its DependsOn/Metadata) past the
//     call. Copy out whatever is needed; the pointer is only stable while the
//     lock is held.
//   - It MUST NOT call back into the store. sync.RWMutex is not reentrant, so a
//     nested Get/List/Update deadlocks.
//
// fn should therefore be a cheap projection, never I/O.
func (s *Store) ReadEach(filter ListFilter, fn func(*Bead)) {
	if fn == nil {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*Bead
	for _, b := range s.beads {
		if filter.Status != nil && b.Status != *filter.Status {
			continue
		}
		if filter.Actor != nil && b.Actor != *filter.Actor {
			continue
		}
		if filter.ExternalRef != nil && b.ExternalRef != *filter.ExternalRef {
			continue
		}
		result = append(result, b)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.Before(result[j].CreatedAt.Time)
	})

	for _, b := range result {
		fn(b)
	}
}

// Plan-review metadata keys. These mirror the convention written by
// pkg/planning (planning.MetaParentEpic / planning.MetaPlanStatus /
// planning.PlanStatusApproved); they are duplicated here as plain strings so
// the low-level beads package can honor the plan-review gate WITHOUT importing
// planning (which would create an import cycle: planning already imports beads).
// Keep these values in sync with pkg/planning.
const (
	// metaParentEpic on a child bead holds the ID of the epic it decomposes.
	// A bead without this key is a normal bead and is never gated.
	metaParentEpic = "parent_epic"
	// metaPlanStatus on an epic records its plan-review state.
	metaPlanStatus = "plan_status"
	// planStatusApproved is the only plan_status that releases an epic's
	// children through Ready(). Any other value (e.g. "draft"), or the parent
	// being missing, keeps the children hidden.
	planStatusApproved = "approved"
)

// Ready returns the open beads a claimant may work on now, honoring the
// plan-review gate (Phase 2 planning intelligence): a child bead of a
// decomposed epic (one carrying a parent_epic metadata key) is EXCLUDED unless
// that parent epic's plan_status metadata is "approved". Normal beads — those
// with no parent_epic — are never affected by the gate.
//
// This is the readiness hook that feeds `bd ready` and governor claim
// selection, so gating here transparently prevents an agent from claiming a
// not-yet-reviewed plan's sub-tasks. Approval is a human action performed via
// planning.ApprovePlan / the /api/plan/{epic}/approve endpoint.
func (s *Store) Ready(actor string) []*Bead {
	status := StatusOpen
	filter := ListFilter{Status: &status}
	if actor != "" {
		filter.Actor = &actor
	}
	candidates := s.List(filter)

	// Resolve plan gating under a read lock so the parent lookups are
	// consistent with the snapshot returned by List.
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := candidates[:0:0] // fresh slice, don't alias List's return
	for _, b := range candidates {
		if s.planGated(b) {
			continue
		}
		result = append(result, b)
	}
	return result
}

// planGated reports whether bead b must be withheld from Ready() because it is a
// child of an epic whose plan has not been approved. It reads only string
// metadata by convention (no planning import). A bead with no parent_epic key is
// never gated. A child whose parent cannot be found is gated (fail-closed): the
// plan record is incomplete, so the child is not yet safe to claim.
//
// Caller must hold s.mu (at least RLock).
func (s *Store) planGated(b *Bead) bool {
	parentID := metaString(b, metaParentEpic)
	if parentID == "" {
		return false // normal bead, not a decomposed child
	}
	parent, ok := s.beads[parentID]
	if !ok {
		return true // fail-closed: parent epic missing
	}
	return metaString(parent, metaPlanStatus) != planStatusApproved
}

// metaString reads a string metadata value from a bead without locking. It
// mirrors Bead.Meta but is used internally where the store lock is already held
// and where only genuine string values should count (a non-string value is
// treated as absent for gating purposes).
func metaString(b *Bead, key string) string {
	if b == nil || b.Metadata == nil {
		return ""
	}
	if v, ok := b.Metadata[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func (s *Store) FindByExternalRef(ref string) *Bead {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, b := range s.beads {
		if b.ExternalRef == ref {
			return b
		}
	}
	return nil
}

func (s *Store) AddDependency(beadID, dependsOnID string) error {
	return s.Update(beadID, func(b *Bead) {
		for _, dep := range b.DependsOn {
			if dep == dependsOnID {
				return
			}
		}
		b.DependsOn = append(b.DependsOn, dependsOnID)
	})
}

func (s *Store) SetMetadata(id, key, value string) error {
	return s.Update(id, func(b *Bead) {
		if b.Metadata == nil {
			b.Metadata = make(map[string]interface{})
		}
		b.Metadata[key] = value
	})
}

func (s *Store) UnsetMetadata(id, key string) error {
	return s.Update(id, func(b *Bead) {
		if b.Metadata != nil {
			delete(b.Metadata, key)
		}
	})
}

const beadsFileName = "beads.json"

// Reload re-reads beads from disk. Agents write directly to the JSON file
// via the bd CLI, so the in-memory store can become stale.
func (s *Store) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beads = make(map[string]*Bead)
	return s.load()
}

func (s *Store) load() error {
	// Retirement is reconstructed FIRST, and unconditionally. archive.jsonl is
	// an independent append-only log, so a directory can hold a complete archive
	// with no beads.json at all — a store whose beads were every one of them
	// culled, a restore that kept only the log, external tooling rewriting the
	// live file. Loading it after the missing-file return meant exactly those
	// stores came back with an EMPTY retired set, so every archived dependency
	// read as "never resolved" rather than "satisfied then culled" — silently
	// withholding the dependents forever, which is the failure retirement exists
	// to prevent.
	s.loadRetired()

	path := filepath.Join(s.dir, beadsFileName)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	var beads []*Bead
	if err := json.Unmarshal(data, &beads); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}

	for _, b := range beads {
		s.beads[b.ID] = b
	}
	return nil
}

// loadRetired repopulates the retired set from archive.jsonl so a restart does
// not forget that a culled dependency was satisfied. Best-effort: a missing or
// partly-corrupt archive degrades to "fewer known-retired IDs", which is the
// same conservative state as before this existed.
func (s *Store) loadRetired() {
	f, err := os.Open(filepath.Join(s.dir, archiveFileName))
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }() // read-only fd; nothing to lose on close error
	// bufio.Reader, not Scanner: a Scanner stops PERMANENTLY on ErrTooLong, so a
	// single oversized entry would discard every retirement recorded after it
	// rather than just that one. Bead titles are unbounded and agent-influenced,
	// so that line is reachable. ReadString has no length cap; an over-long line
	// simply fails to parse as JSON and is skipped like any other bad line.
	r := bufio.NewReader(f)
	for {
		raw, err := r.ReadString('\n')
		if line := bytes.TrimSpace([]byte(raw)); len(line) > 0 {
			var entry ArchivedBead
			if jsonErr := json.Unmarshal(line, &entry); jsonErr == nil && entry.ID != "" {
				s.retired[entry.ID] = true
			}
		}
		if err != nil {
			return // io.EOF or a read failure: take what we parsed
		}
	}
}

// IsRetired reports whether the given bead ID was removed from the live map
// after reaching a terminal state. Callers use it to tell a satisfied-then-culled
// dependency from a reference that never resolved.
func (s *Store) IsRetired(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.retired[id]
}

// RetiredIDs returns a detached copy of the retired set — the whole-set view, for
// inspection and assertions. The dependency gate does NOT use it: it resolves one
// ID at a time via IsRetired, because the retired set is rebuilt from the entire
// archive history and only grows, so copying it per selection would be an
// unbounded cost to answer a question only a dangling edge ever asks.
func (s *Store) RetiredIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.retired))
	for id := range s.retired {
		out = append(out, id)
	}
	return out
}

func (s *Store) persist(_ *Bead) error {
	var all []*Bead
	for _, b := range s.beads {
		all = append(all, b)
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].CreatedAt.Before(all[j].CreatedAt.Time)
	})

	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling beads: %w", err)
	}

	path := filepath.Join(s.dir, beadsFileName)
	// A UNIQUE temp name per writer (#4742): the old fixed beads.json.tmp let
	// two concurrent processes write the same temp file, so one rename won the
	// race and the other failed with ENOENT — and whichever snapshot landed
	// last silently dropped the other process's beads. The flock in
	// lockAndRefresh serializes cooperating writers; the unique name keeps
	// even a non-cooperating writer from corrupting an in-flight persist.
	tmp, err := os.CreateTemp(s.dir, beadsFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating tmp beads file: %w", err)
	}
	tmpPath := tmp.Name()
	// 0660 (group-writable) so a bead file created by one node-group member can be
	// rewritten by another (e.g. the architect agent and the dashboard both write
	// the architect store). Matches the /data/home/* FilePerms model. CreateTemp
	// makes 0600, so widen explicitly.
	_ = tmp.Chmod(0660)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()        // best-effort cleanup; the write error is what's returned
		_ = os.Remove(tmpPath) // best-effort cleanup; the write error is what's returned
		return fmt.Errorf("writing tmp beads: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath) // best-effort cleanup; the close error is what's returned
		return fmt.Errorf("closing tmp beads: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath) // best-effort cleanup; the rename error is what's returned
		return err
	}
	return nil
}

func (s *Store) CloseAll(reason string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.lockAndRefresh()()

	now := flexTime{time.Now().UTC()}
	closed := 0
	for _, b := range s.beads {
		if b.Status == StatusOpen || b.Status == StatusInProgress || b.Status == StatusBlocked {
			b.Status = StatusClosed
			b.ClosedAt = &now
			b.UpdatedAt = now
			if b.Metadata == nil {
				b.Metadata = make(map[string]interface{})
			}
			b.Metadata["close_reason"] = reason
			closed++
		}
	}

	if closed > 0 {
		if err := s.persist(nil); err != nil {
			return closed, err
		}
	}
	return closed, nil
}

func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.beads)
}

const archiveFileName = "archive.jsonl"

// ArchivedBead is the compact representation written to the archive log.
//
// It carries the audit fields — Status, Actor, Metadata, CreatedAt — alongside
// the identity fields. Earlier versions dropped these (#3971), which destroyed
// the merge-outcome metadata (pr_merged, pr_ref, fix_attempts) and the
// who/what/when needed to compute the ACMM advisor's MergeSuccessRate from
// history (#3972). Heavy free-text fields (Notes) are still intentionally
// excluded to keep archive entries compact.
type ArchivedBead struct {
	ID          string                 `json:"id"`
	Title       string                 `json:"title"`
	Type        BeadType               `json:"type"`
	Status      Status                 `json:"status,omitempty"`
	Priority    Priority               `json:"priority"`
	Actor       string                 `json:"actor,omitempty"`
	ExternalRef string                 `json:"external_ref,omitempty"`
	Metadata    map[string]interface{} `json:"metadata,omitempty"`
	CreatedAt   time.Time              `json:"created_at,omitempty"`
	ClosedAt    time.Time              `json:"closed_at,omitempty"`
	ArchivedAt  time.Time              `json:"archived_at"`
}

// newArchivedBead builds the archive record for a bead. It is the single
// source of truth for what an archive entry contains, shared by Archive() and
// the eviction path's appendArchiveEntry() so the two removal paths cannot
// preserve different data (#3971): before this existed, each built its own
// entry inline, and both silently dropped the audit fields.
func newArchivedBead(b *Bead) ArchivedBead {
	entry := ArchivedBead{
		ID:          b.ID,
		Title:       b.Title,
		Type:        b.Type,
		Status:      b.Status,
		Priority:    b.Priority,
		Actor:       b.Actor,
		ExternalRef: b.ExternalRef,
		Metadata:    b.Metadata,
		CreatedAt:   b.CreatedAt.Time,
		ArchivedAt:  time.Now().UTC(),
	}
	if b.ClosedAt != nil {
		entry.ClosedAt = b.ClosedAt.Time
	}
	return entry
}

// Archive writes the bead's audit record (including status, actor, metadata,
// and timestamps — see ArchivedBead) to archive.jsonl, then removes it from
// the store. Notes are dropped to keep the archive compact.
func (s *Store) Archive(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.lockAndRefresh()()

	b, ok := s.beads[id]
	if !ok {
		return fmt.Errorf("bead %s not found", id)
	}

	data, err := json.Marshal(newArchivedBead(b))
	if err != nil {
		return fmt.Errorf("marshaling archive entry: %w", err)
	}

	archivePath := filepath.Join(s.dir, archiveFileName)
	f, err := os.OpenFile(archivePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("opening archive file: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close() // best-effort cleanup; the write error is what's returned
		return fmt.Errorf("writing archive entry: %w", err)
	}
	// The archive is the durability record for a bead about to be deleted from
	// memory below — an ignored Close error here would let a failed flush look
	// like a successful archive, and the record would be gone from both places.
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing archive file: %w", err)
	}

	delete(s.beads, id)
	// Retire ONLY a bead that actually reached a terminal state. The retired set
	// means "this existed and was satisfied", and a consumer treats membership as
	// a satisfied dependency — so retiring an archived-but-open bead would assert
	// completion that never happened. Archiving an open bead is still allowed
	// (the audit line is written either way); it just does not claim satisfaction.
	if b.Status == StatusClosed || b.Status == StatusDone {
		s.retired[id] = true
	}
	return s.persist(nil)
}

// AllClosed returns all beads with done or closed status.
func (s *Store) AllClosed() []*Bead {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*Bead
	for _, b := range s.beads {
		if b.Status == StatusDone || b.Status == StatusClosed {
			result = append(result, b)
		}
	}
	return result
}

// HasOpenBead checks if a bead with the given ID exists and is not done/closed.
func (s *Store) HasOpenBead(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	b, ok := s.beads[id]
	if !ok {
		return false
	}
	return b.Status != StatusDone && b.Status != StatusClosed
}

// Unsynthesized returns all done/closed beads that have not yet been
// synthesized into wiki facts (i.e., missing the "synthesized_at" metadata key).
func (s *Store) Unsynthesized() []*Bead {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*Bead
	for _, b := range s.beads {
		if b.Status != StatusDone && b.Status != StatusClosed {
			continue
		}
		if _, ok := b.Metadata["synthesized_at"]; ok {
			continue
		}
		result = append(result, b)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.Before(result[j].CreatedAt.Time)
	})

	return result
}
