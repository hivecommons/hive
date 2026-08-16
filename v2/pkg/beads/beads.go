package beads

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	return fmt.Errorf("parsing time %q: no matching format", s)
}

func (ft flexTime) MarshalJSON() ([]byte, error) {
	return json.Marshal(ft.Time.Format(time.RFC3339Nano))
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
	DependsOn   []string               `json:"depends_on,omitempty"`
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
	mu     sync.RWMutex
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
	_ = os.Chmod(dir, 0o770|os.ModeSetgid)

	s := &Store{
		dir:   dir,
		beads: make(map[string]*Bead),
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
		delete(s.beads, id)
	}
}

func (s *Store) Update(id string, fn func(b *Bead)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

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
	tmpPath := path + ".tmp"
	// 0660 (group-writable) so a bead file created by one node-group member can be
	// rewritten by another (e.g. the architect agent and the dashboard both write
	// the architect store). Matches the /data/home/* FilePerms model.
	if err := os.WriteFile(tmpPath, data, 0660); err != nil {
		return fmt.Errorf("writing tmp beads: %w", err)
	}
	return os.Rename(tmpPath, path)
}

func (s *Store) CloseAll(reason string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

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
type ArchivedBead struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Type        BeadType  `json:"type"`
	Priority    Priority  `json:"priority"`
	ExternalRef string    `json:"external_ref,omitempty"`
	ClosedAt    time.Time `json:"closed_at,omitempty"`
	ArchivedAt  time.Time `json:"archived_at"`
}

// Archive writes a compact summary of the bead to archive.jsonl, then removes
// it from the store. This preserves an audit trail without the full bead weight.
func (s *Store) Archive(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, ok := s.beads[id]
	if !ok {
		return fmt.Errorf("bead %s not found", id)
	}

	entry := ArchivedBead{
		ID:          b.ID,
		Title:       b.Title,
		Type:        b.Type,
		Priority:    b.Priority,
		ExternalRef: b.ExternalRef,
		ArchivedAt:  time.Now().UTC(),
	}
	if b.ClosedAt != nil {
		entry.ClosedAt = b.ClosedAt.Time
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshaling archive entry: %w", err)
	}

	archivePath := filepath.Join(s.dir, archiveFileName)
	f, err := os.OpenFile(archivePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("opening archive file: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("writing archive entry: %w", err)
	}

	delete(s.beads, id)
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
