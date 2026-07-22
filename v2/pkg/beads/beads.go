package beads

import (
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
	dir       string
	fileMode  os.FileMode
	hiveID    string
	beads     map[string]*Bead
	writeFile func(string, []byte, os.FileMode) error
	rename    func(string, string) error
	mu        sync.RWMutex
}

// BatchInput is a fully validated work item prepared by an external evidence
// importer. SourceID is used only to reconnect dependencies within the batch.
type BatchInput struct {
	SourceID    string
	Title       string
	Type        BeadType
	Status      Status
	Priority    Priority
	Actor       string
	ExternalRef string
	Metadata    map[string]interface{}
	Notes       string
	DependsOn   []string
}

// BatchResult describes an idempotent batch import.
type BatchResult struct {
	Created int `json:"created"`
	Skipped int `json:"skipped"`
}

const (
	privateStoreDirMode  os.FileMode = 0o700
	privateStoreFileMode os.FileMode = 0o600
	sharedStoreDirMode   os.FileMode = os.ModeSetgid | 0o770
	sharedStoreFileMode  os.FileMode = 0o660
)

// NewStore opens an owner-private bead store.
func NewStore(dir string) (*Store, error) {
	return newStore(dir, privateStoreDirMode, privateStoreFileMode)
}

// NewSharedStore opens a group-writable role bead store shared across the
// node group — e.g. the dashboard/hub process minting an issue-sourced epic
// into the architect's store, or a role's UID-isolated agent writing its own
// beads. This matches the shared-node-group model used for /data/home/*
// (see pkg/agent/permissions_watcher DirPerms=0o770).
//
// NOTE the constant: os.Chmod takes an os.FileMode, where setgid is
// os.ModeSetgid (1<<29) and NOT the Unix octal 0o2000. Passing 0o2770
// silently requests plain 0770 — the setgid bit is dropped before the
// syscall, with no error — which is why sharedStoreDirMode is spelled
// os.ModeSetgid | 0o770.
func NewSharedStore(dir string) (*Store, error) {
	if info, err := os.Lstat(dir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("refusing shared beads dir symlink %s", dir)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspecting shared beads dir %s: %w", dir, err)
	}
	return newStore(dir, sharedStoreDirMode, sharedStoreFileMode)
}

func newStore(dir string, dirMode, fileMode os.FileMode) (*Store, error) {
	// MkdirAll's mode is clipped by the umask (and special bits are ignored
	// by mkdir on some platforms), so ensureStoreMode sets it explicitly.
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("creating beads dir %s: %w", dir, err)
	}
	if err := ensureStoreMode(dir, dirMode); err != nil {
		return nil, fmt.Errorf("protecting beads dir %s: %w", dir, err)
	}

	s := &Store{
		dir:       dir,
		fileMode:  fileMode,
		beads:     make(map[string]*Bead),
		writeFile: os.WriteFile,
		rename:    os.Rename,
	}

	if err := s.load(); err != nil {
		return nil, fmt.Errorf("loading beads from %s: %w", dir, err)
	}

	return s, nil
}

func ensureStoreMode(path string, mode os.FileMode) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	const specialMode = os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	if info.Mode().Perm() == mode.Perm() && info.Mode()&specialMode == mode&specialMode {
		return nil
	}
	return os.Chmod(path, mode)
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

// ImportBatch validates and persists all items with one atomic beads.json
// rename. Existing external references are skipped, making retries idempotent.
func (s *Store) ImportBatch(items []BatchInput) (BatchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := BatchResult{}
	existingByRef := make(map[string]*Bead, len(s.beads))
	for _, bead := range s.beads {
		if bead.ExternalRef != "" {
			existingByRef[bead.ExternalRef] = bead
		}
	}
	seenRefs := make(map[string]bool, len(items))
	seenSources := make(map[string]bool, len(items))
	for _, item := range items {
		if strings.TrimSpace(item.SourceID) == "" || strings.TrimSpace(item.Title) == "" || strings.TrimSpace(item.ExternalRef) == "" {
			return result, fmt.Errorf("batch item requires source ID, title, and external reference")
		}
		if !validBeadTypes[item.Type] {
			return result, fmt.Errorf("invalid bead type %q", item.Type)
		}
		if !validStatus(item.Status) || item.Priority < PriorityCritical || item.Priority > PriorityMinor {
			return result, fmt.Errorf("invalid status or priority for %q", item.ExternalRef)
		}
		if seenRefs[item.ExternalRef] {
			return result, fmt.Errorf("duplicate external reference %q in batch", item.ExternalRef)
		}
		if seenSources[item.SourceID] {
			return result, fmt.Errorf("duplicate source ID %q in batch", item.SourceID)
		}
		seenRefs[item.ExternalRef] = true
		seenSources[item.SourceID] = true
	}

	now := flexTime{time.Now().UTC()}
	createdIDs := make([]string, 0, len(items))
	sourceToID := make(map[string]string, len(items))
	for _, item := range items {
		if existing := existingByRef[item.ExternalRef]; existing != nil {
			sourceToID[item.SourceID] = existing.ID
			result.Skipped++
			continue
		}
		id := uuid.New().String()[:12]
		metadata := make(map[string]interface{}, len(item.Metadata)+1)
		for key, value := range item.Metadata {
			metadata[key] = value
		}
		if s.hiveID != "" {
			metadata[hiveIDMetadataKey] = s.hiveID
		}
		bead := &Bead{
			ID: id, Title: item.Title, Type: item.Type, Status: item.Status,
			Priority: item.Priority, Actor: item.Actor, ExternalRef: item.ExternalRef,
			Metadata: metadata, Notes: item.Notes, CreatedAt: now, UpdatedAt: now,
		}
		if item.Status == StatusDone || item.Status == StatusClosed {
			closedAt := now
			bead.ClosedAt = &closedAt
		}
		s.beads[id] = bead
		createdIDs = append(createdIDs, id)
		sourceToID[item.SourceID] = id
		result.Created++
	}
	for _, item := range items {
		if sourceToID[item.SourceID] == "" {
			continue
		}
		bead := s.beads[sourceToID[item.SourceID]]
		if bead == nil || !contains(createdIDs, bead.ID) {
			continue
		}
		for _, dependency := range item.DependsOn {
			if target := sourceToID[dependency]; target != "" {
				bead.DependsOn = append(bead.DependsOn, target)
			}
		}
	}
	if result.Created == 0 {
		return result, nil
	}
	if err := s.persist(nil); err != nil {
		for _, id := range createdIDs {
			delete(s.beads, id)
		}
		return BatchResult{}, err
	}
	return result, nil
}

// RollbackImportedExternalRefs removes only the exact external references
// created by a coordinated multi-store batch. Callers must preflight that the
// references did not exist before import; this is not a general delete API.
func (s *Store) RollbackImportedExternalRefs(refs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	wanted := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if strings.TrimSpace(ref) != "" {
			wanted[ref] = true
		}
	}
	removed := map[string]*Bead{}
	for id, bead := range s.beads {
		if bead != nil && wanted[bead.ExternalRef] {
			removed[id] = bead
			delete(s.beads, id)
		}
	}
	if len(removed) == 0 {
		return nil
	}
	if err := s.persist(nil); err != nil {
		for id, bead := range removed {
			s.beads[id] = bead
		}
		return err
	}
	return nil
}

func validStatus(status Status) bool {
	return status == StatusOpen || status == StatusInProgress || status == StatusBlocked || status == StatusDone || status == StatusClosed
}

func contains(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
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
	next, err := cloneBead(b)
	if err != nil {
		return fmt.Errorf("copy bead %s for update: %w", id, err)
	}
	fn(next)
	now := flexTime{time.Now().UTC()}
	// Stamp ClosedAt the first time a bead transitions into a terminal
	// status, so age/latency reporting has a durable close timestamp.
	isTerminal := next.Status == StatusDone || next.Status == StatusClosed
	if isTerminal && !wasTerminal && next.ClosedAt == nil {
		next.ClosedAt = &now
	}
	next.UpdatedAt = now
	candidate := cloneBeadMap(s.beads)
	candidate[id] = next
	if err := s.persistMap(candidate); err != nil {
		return err
	}
	*b = *next
	return nil
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

// CloseWithUpdate atomically persists terminal status together with caller
// metadata. It is used by controller-owned lifecycle projections so a crash
// cannot leave a terminal bead reopened or missing its terminal receipt.
func (s *Store) CloseWithUpdate(id string, fn func(b *Bead)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.beads[id]
	if !ok {
		return fmt.Errorf("bead %s not found", id)
	}
	next, err := cloneBead(b)
	if err != nil {
		return fmt.Errorf("copy bead %s for close: %w", id, err)
	}
	fn(next)
	now := flexTime{time.Now().UTC()}
	next.Status = StatusClosed
	next.ClosedAt = &now
	next.UpdatedAt = now
	candidate := cloneBeadMap(s.beads)
	candidate[id] = next
	if err := s.persistMap(candidate); err != nil {
		return err
	}
	*b = *next
	return nil
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
	return s.persistMap(s.beads)
}

func (s *Store) persistMap(source map[string]*Bead) error {
	var all []*Bead
	for _, b := range source {
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
	// s.fileMode is 0660 for shared stores so a bead file created by one
	// node-group member can be rewritten by another (e.g. the architect agent
	// and the dashboard both write the architect store); private stores stay
	// 0600. Matches the /data/home/* FilePerms model.
	if err := s.writeFile(tmpPath, data, s.fileMode); err != nil {
		return fmt.Errorf("writing tmp beads: %w", err)
	}
	if err := ensureStoreMode(tmpPath, s.fileMode); err != nil {
		return fmt.Errorf("protecting tmp beads: %w", err)
	}
	return s.rename(tmpPath, path)
}

func cloneBeadMap(source map[string]*Bead) map[string]*Bead {
	clone := make(map[string]*Bead, len(source))
	for id, bead := range source {
		clone[id] = bead
	}
	return clone
}

func cloneBead(source *Bead) (*Bead, error) {
	data, err := json.Marshal(source)
	if err != nil {
		return nil, err
	}
	var clone Bead
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, err
	}
	return &clone, nil
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
	f, err := os.OpenFile(archivePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, s.fileMode)
	if err != nil {
		return fmt.Errorf("opening archive file: %w", err)
	}
	defer f.Close()
	archiveInfo, err := f.Stat()
	if err != nil {
		return fmt.Errorf("statting archive file: %w", err)
	}
	if archiveInfo.Mode().Perm() != s.fileMode.Perm() {
		if err := f.Chmod(s.fileMode); err != nil {
			return fmt.Errorf("protecting archive file: %w", err)
		}
	}

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
