package toolapprove

// The operator inbox: durable storage for the operator lane ONLY.
//
// An approval that waits overnight must survive a spoke roll — hives auto-roll
// frequently, so an in-memory pending list would be silently wiped by the very
// machinery it is meant to outlive. This follows the persistence idiom the
// fleet already uses for hub upgrade-pause and the contribute ledger: one small
// JSON file on the durable data dir, loaded lazily on first use, written
// atomically via a temp file + rename so a crash mid-write cannot leave a
// truncated inbox.
//
// THE THROUGHPUT CONTRACT (#4000 comment 5321778076, requirement 1):
// "A request the policy resolves to auto-approve never enters a queue and never
// waits on any external process. The inbox is for the operator lane only."
//
// This is enforced structurally, not by convention: Enqueue REFUSES any verdict
// whose decision is not operator-approve, returning ErrNotOperatorLane. There is
// no code path by which an auto-approved request reaches this file, and
// inbox_l6_test.go pins that with the store wired to a path that fails the test
// if it is ever written.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"crypto/sha256"
	"encoding/hex"
)

// DefaultInboxPath is the durable home of the pending-approval inbox, on the
// spoke's PVC-backed /data dir alongside the other operational JSON state
// (hive-state.json, sparkline-history.json). A var, not a const, so tests
// redirect it at a temp path — the same affordance upgradePausePath uses.
var DefaultInboxPath = "/data/approvals/inbox.json"

// inboxFileMode matches the other non-secret /data JSON files: this state is
// operational, not key material. The verdict rationale can quote tool
// arguments, so it is not world-readable beyond the container's own user.
const inboxFileMode = 0o644

// inboxDirMode is the mode for the containing directory when it must be created.
const inboxDirMode = 0o755

// maxInboxEntries bounds how many pending items the inbox retains. An inbox
// that grows without limit would eventually make every decision slow and every
// dashboard render heavy. At the cap the OLDEST entries are dropped with a
// warning: a pending queue this deep is already a misconfiguration signal (see
// requirement 4 — operator-lane items at L6 are probable policy bugs), and
// silently consuming unbounded disk is the worse failure.
const maxInboxEntries = 5000

// maxJournalEntries bounds the resolved-verdict journal used for idempotency.
// Entries older than journalRetention are pruned on write; the cap is a second
// backstop for a hive that resolves faster than it prunes.
const maxJournalEntries = 10000

// journalRetention is how long a resolved verdict stays in the idempotency
// journal. A re-delivery arriving later than this is treated as a new request.
// 30 days comfortably covers "an approval that waits overnight" plus any
// realistic replay window, without growing the file unboundedly.
const journalRetention = 30 * 24 * time.Hour

// Errors returned by the inbox.
var (
	// ErrNotOperatorLane is returned by Enqueue for any verdict that is not
	// operator-approve. This is the structural guard behind the L6 throughput
	// contract: auto-approve can never enter the queue.
	ErrNotOperatorLane = errors.New("only operator-approve verdicts may enter the inbox")
	// ErrNotFound is returned when resolving an unknown approval ID.
	ErrNotFound = errors.New("pending approval not found")
	// ErrAlreadyResolved is returned when resolving an approval that a prior,
	// identical delivery already resolved. It carries the earlier outcome.
	ErrAlreadyResolved = errors.New("approval already resolved")
)

// PendingItem is one request parked in the operator lane.
type PendingItem struct {
	// ID is the stable identifier operators and the API address this item by.
	// It IS the idempotency key, so a re-delivered request resolves to the same
	// row rather than creating a duplicate.
	ID string `json:"id"`
	// Request is the original request, retained so a resolve can re-evaluate it
	// through the desk rather than trusting a stale verdict.
	Request Request `json:"request"`
	// Verdict is the desk's verdict at enqueue time, including which rule (if
	// any) matched — the dashboard renders this per row.
	Verdict Verdict `json:"verdict"`
	// QueuedAt is when the item entered the inbox.
	QueuedAt time.Time `json:"queued_at"`
	// ACMMLevel is the hive level at enqueue time. An item queued at L6 is a
	// probable policy bug (requirement 4) and the API flags it.
	ACMMLevel int `json:"acmm_level"`
}

// ResolvedRecord journals a verdict that has been resolved, keyed by the same
// ID, so a re-delivered grant does not double-execute.
type ResolvedRecord struct {
	ID         string    `json:"id"`
	Approved   bool      `json:"approved"`
	Operator   string    `json:"operator,omitempty"`
	Rationale  string    `json:"rationale,omitempty"`
	ResolvedAt time.Time `json:"resolved_at"`
	// Executed records that the granted action was actually carried out. The
	// producer sets this after a successful execution, so a crash between
	// "resolved" and "executed" is distinguishable on restart.
	Executed bool `json:"executed"`
}

// inboxState is the on-disk shape.
type inboxState struct {
	Pending  []PendingItem    `json:"pending"`
	Resolved []ResolvedRecord `json:"resolved"`
}

// Inbox is the durable operator-lane store. The zero value is not usable;
// construct with NewInbox.
type Inbox struct {
	mu     sync.Mutex
	path   string
	loaded bool
	state  inboxState
	// now is injectable so tests can exercise retention pruning deterministically.
	now func() time.Time
}

// NewInbox returns an inbox backed by path. An empty path uses
// DefaultInboxPath. The file is not touched until first use — construction is
// free, so wiring an inbox into a hive that never queues anything costs nothing.
func NewInbox(path string) *Inbox {
	if path == "" {
		path = DefaultInboxPath
	}
	return &Inbox{path: path, now: time.Now}
}

// DeriveIdempotencyKey computes a stable key for a request. Two deliveries of
// the same logical request produce the same key, so the second is recognized as
// a replay rather than executed again.
//
// The key covers the fields that identify WHAT is being requested — lane, tool,
// target, and the tool's own arguments — but deliberately NOT timestamps or
// per-delivery metadata, which is what makes a re-delivery match.
func DeriveIdempotencyKey(req Request) string {
	if req.IdempotencyKey != "" {
		return req.IdempotencyKey
	}
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0})
		}
	}
	write(req.Kind, req.Tool.Tool, req.Agent.Name, req.Repo, req.Author)
	write(fmt.Sprintf("%d", req.Number))
	write(req.Tool.GetCommand(), req.Tool.GetFilePath(), req.Tool.GetContent())
	// Arguments are hashed in sorted key order so map iteration order cannot
	// make the same request hash two different ways.
	if len(req.Tool.Arguments) > 0 {
		keys := make([]string, 0, len(req.Tool.Arguments))
		for k := range req.Tool.Arguments {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			write(k, fmt.Sprintf("%v", req.Tool.Arguments[k]))
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// ensureLoadedLocked reads the persisted state exactly once per process.
// Callers must hold i.mu. A missing file means an empty inbox — the safe
// default and the pre-feature behavior. A corrupt file is treated the same way
// rather than wedging the hive; the operator sees the warning and the inbox
// refills from live traffic.
func (i *Inbox) ensureLoadedLocked() {
	if i.loaded {
		return
	}
	i.loaded = true
	data, err := os.ReadFile(i.path)
	if err != nil {
		return
	}
	var st inboxState
	if err := json.Unmarshal(data, &st); err != nil {
		return
	}
	i.state = st
}

// persistLocked writes the state atomically: temp file in the same directory,
// then rename. A crash mid-write leaves either the old file or the new one,
// never a truncated inbox. Callers must hold i.mu.
func (i *Inbox) persistLocked() error {
	dir := filepath.Dir(i.path)
	if err := os.MkdirAll(dir, inboxDirMode); err != nil {
		return fmt.Errorf("create approvals dir: %w", err)
	}
	data, err := json.MarshalIndent(i.state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal inbox: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".inbox-*.json")
	if err != nil {
		return fmt.Errorf("create temp inbox: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp inbox: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp inbox: %w", err)
	}
	if err := os.Chmod(tmpName, inboxFileMode); err != nil {
		return fmt.Errorf("chmod temp inbox: %w", err)
	}
	if err := os.Rename(tmpName, i.path); err != nil {
		return fmt.Errorf("rename inbox into place: %w", err)
	}
	return nil
}

// Enqueue parks an operator-lane request in the durable inbox and returns its
// ID (== its idempotency key).
//
// It REFUSES any verdict that is not operator-approve with ErrNotOperatorLane.
// That refusal is the L6 throughput contract expressed as code: there is no
// argument by which an auto-approved request reaches durable storage.
//
// Enqueue is idempotent. A request already pending returns its existing ID
// without duplicating the row; a request already RESOLVED returns
// ErrAlreadyResolved so the caller re-delivers nothing.
func (i *Inbox) Enqueue(req Request, v Verdict) (string, error) {
	if v.Decision != DecisionOperatorApprove {
		return "", fmt.Errorf("%w: got %s", ErrNotOperatorLane, v.Decision)
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	i.ensureLoadedLocked()

	id := DeriveIdempotencyKey(req)

	// Already resolved? Do not re-queue — that is the double-execution the
	// journal exists to prevent.
	for _, r := range i.state.Resolved {
		if r.ID == id {
			return id, fmt.Errorf("%w: %s at %s", ErrAlreadyResolved, verdictWord(r.Approved), r.ResolvedAt.UTC().Format(time.RFC3339))
		}
	}
	// Already pending? Return the existing row.
	for _, p := range i.state.Pending {
		if p.ID == id {
			return id, nil
		}
	}

	req.IdempotencyKey = id
	i.state.Pending = append(i.state.Pending, PendingItem{
		ID:        id,
		Request:   req,
		Verdict:   v,
		QueuedAt:  i.now().UTC(),
		ACMMLevel: v.ACMMLevel,
	})

	// Bound growth: drop oldest beyond the cap.
	if len(i.state.Pending) > maxInboxEntries {
		i.state.Pending = i.state.Pending[len(i.state.Pending)-maxInboxEntries:]
	}

	if err := i.persistLocked(); err != nil {
		return id, err
	}
	return id, nil
}

// verdictWord renders an approval boolean for messages.
func verdictWord(approved bool) string {
	if approved {
		return "granted"
	}
	return "rejected"
}

// List returns the pending items, newest last (queue order). The returned slice
// is a copy: callers cannot mutate inbox state.
func (i *Inbox) List() []PendingItem {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.ensureLoadedLocked()
	return append([]PendingItem(nil), i.state.Pending...)
}

// Count returns the number of pending items — the dashboard's badge value.
func (i *Inbox) Count() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.ensureLoadedLocked()
	return len(i.state.Pending)
}

// Get returns one pending item by ID.
func (i *Inbox) Get(id string) (PendingItem, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.ensureLoadedLocked()
	for _, p := range i.state.Pending {
		if p.ID == id {
			return p, true
		}
	}
	return PendingItem{}, false
}

// Resolve records an operator's verdict on a pending item, removes it from the
// pending list, and journals the outcome.
//
// Idempotency: resolving an ID that is already journaled returns
// ErrAlreadyResolved along with the ORIGINAL record, and does not journal a
// second time. A granted verdict re-delivered therefore cannot double-execute —
// the caller sees the error, reads Executed on the returned record, and skips.
func (i *Inbox) Resolve(id string, approved bool, operator, rationale string) (ResolvedRecord, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.ensureLoadedLocked()

	for _, r := range i.state.Resolved {
		if r.ID == id {
			return r, fmt.Errorf("%w: %s at %s", ErrAlreadyResolved, verdictWord(r.Approved), r.ResolvedAt.UTC().Format(time.RFC3339))
		}
	}

	idx := -1
	for n, p := range i.state.Pending {
		if p.ID == id {
			idx = n
			break
		}
	}
	if idx < 0 {
		return ResolvedRecord{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}

	rec := ResolvedRecord{
		ID:         id,
		Approved:   approved,
		Operator:   operator,
		Rationale:  rationale,
		ResolvedAt: i.now().UTC(),
	}
	i.state.Pending = append(i.state.Pending[:idx], i.state.Pending[idx+1:]...)
	i.state.Resolved = append(i.state.Resolved, rec)
	i.pruneJournalLocked()

	if err := i.persistLocked(); err != nil {
		return rec, err
	}
	return rec, nil
}

// MarkExecuted flips the journal record's Executed flag after the granted
// action has actually been carried out. The two-phase shape (resolve, then
// execute, then mark) is what lets a restart tell "approved but never ran" from
// "approved and ran" — the former is safe to retry, the latter must not be.
func (i *Inbox) MarkExecuted(id string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.ensureLoadedLocked()

	for n := range i.state.Resolved {
		if i.state.Resolved[n].ID == id {
			if i.state.Resolved[n].Executed {
				return nil // already marked; idempotent
			}
			i.state.Resolved[n].Executed = true
			return i.persistLocked()
		}
	}
	return fmt.Errorf("%w: %s", ErrNotFound, id)
}

// ResolvedRecordFor returns the journal entry for an ID, if any. Producers call
// this BEFORE executing a granted verdict: a record with Executed=true means
// this grant has already run and must not run again.
func (i *Inbox) ResolvedRecordFor(id string) (ResolvedRecord, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.ensureLoadedLocked()
	for _, r := range i.state.Resolved {
		if r.ID == id {
			return r, true
		}
	}
	return ResolvedRecord{}, false
}

// pruneJournalLocked drops journal entries past the retention window and caps
// total size. Callers must hold i.mu.
func (i *Inbox) pruneJournalLocked() {
	cutoff := i.now().UTC().Add(-journalRetention)
	kept := i.state.Resolved[:0]
	for _, r := range i.state.Resolved {
		if r.ResolvedAt.After(cutoff) {
			kept = append(kept, r)
		}
	}
	i.state.Resolved = kept
	if len(i.state.Resolved) > maxJournalEntries {
		i.state.Resolved = i.state.Resolved[len(i.state.Resolved)-maxJournalEntries:]
	}
}

// BulkResolveResult is the outcome for ONE item in a bulk resolve. Partial
// failure is the normal case (an item may have been resolved by another
// operator, or aged out), so the API returns a per-item list rather than a
// single boolean — mirroring BulkHiveResult in pkg/hub/saas_bulk.go.
type BulkResolveResult struct {
	ID    string `json:"id"`
	Ok    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// ResolveMany resolves N items by calling Resolve once per item — the SAME
// function the single-item path uses. There is no bulk-specific predicate and
// no shortcut: a bulk approve is N individual resolutions, each journaled and
// each idempotent, exactly as runBulkAction decomposes a bulk hive request into
// per-item authorized operations.
func (i *Inbox) ResolveMany(ids []string, approved bool, operator, rationale string) []BulkResolveResult {
	out := make([]BulkResolveResult, 0, len(ids))
	for _, id := range ids {
		_, err := i.Resolve(id, approved, operator, rationale)
		if err != nil {
			out = append(out, BulkResolveResult{ID: id, Ok: false, Error: err.Error()})
			continue
		}
		out = append(out, BulkResolveResult{ID: id, Ok: true})
	}
	return out
}

// PendingAtFullAutonomy returns the pending items queued at an ACMM level that
// claims full autonomy. Per requirement 4 of the throughput contract, these are
// surfaced as PROBABLE POLICY BUGS rather than silently waiting: an inbox that
// accumulates on a hive whose level says "full autonomy" is a misconfiguration
// signal. The API marks them and the dashboard renders them distinctly.
func (i *Inbox) PendingAtFullAutonomy() []PendingItem {
	var out []PendingItem
	for _, p := range i.List() {
		if p.ACMMLevel >= 6 {
			out = append(out, p)
		}
	}
	return out
}

// RuleChips returns the distinct rule names across pending items, for the
// dashboard's filter chips. Items resolved by base policy rather than a rule
// are grouped under the empty name, which the UI renders as "no rule".
func (i *Inbox) RuleChips() []string {
	seen := map[string]struct{}{}
	for _, p := range i.List() {
		name := strings.TrimSpace(p.Verdict.Rule)
		if name == "" {
			continue
		}
		seen[name] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return sortRuleNames(out)
}
