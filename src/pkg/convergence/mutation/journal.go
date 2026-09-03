package mutation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// EffectCreatePR is the ONE external effect this vertical implements, exactly
// as named by the accepted #4255 C2 binding: GitHub pull-request creation for
// the assigned task. The identifier is versioned; a semantics change is a new
// effect kind, never a silent reinterpretation of journal entries.
const EffectCreatePR = "github.create-pr/v1"

// Operation statuses: the reconstructable record distinguishing an external
// effect that was planned, applied, not applied, or left uncertain.
const (
	// StatusPlanned: intent is durably recorded; the effect may not yet have
	// been attempted, or an attempt is in flight.
	StatusPlanned = "Planned"
	// StatusApplied: authoritative external state (or an acknowledged
	// current-epoch result) proves the effect took place. Terminal truth —
	// a later duplicate, out-of-order, or stale-epoch result can never
	// regress it.
	StatusApplied = "Applied"
	// StatusNotApplied: authoritative external state proves the effect did
	// NOT take place; the same logical operation may retry through a new
	// authorized attempt under the current epoch.
	StatusNotApplied = "NotApplied"
	// StatusUnknown: the external result is ambiguous or the observer was
	// unavailable. No retry is authorized until reconciliation against
	// authoritative external state resolves the SAME logical operation.
	StatusUnknown = "Unknown"
)

// Journal sentinel errors; callers match with errors.Is.
var (
	// ErrNeedsReconciliation: the operation has an unresolved attempt
	// (Planned or Unknown); authoritative external state must reconcile the
	// same logical operation before any retry is authorized. This is what
	// makes retry-after-uncertainty reconcile rather than duplicate.
	ErrNeedsReconciliation = errors.New("operation must be reconciled against authoritative external state before retry")
	// ErrAlreadyApplied: the effect already took place; a new attempt can
	// never be authorized for the same logical operation.
	ErrAlreadyApplied = errors.New("operation effect is already applied")
	// ErrResultRegression: a result or acknowledgment arrived that would
	// regress terminal journal truth or overwrite a newer attempt from an
	// older epoch.
	ErrResultRegression = errors.New("result cannot regress terminal journal truth")
	// ErrUnknownOperation: no journal entry exists for the logical ID.
	ErrUnknownOperation = errors.New("no journal entry exists for this operation")
	// ErrInvalidEffect: the effect description fails validation.
	ErrInvalidEffect = errors.New("effect description is invalid")
)

const journalFileMode = 0o660
const journalFormatVersion = 1

// DeriveLogicalID is the canonical operation-id derivation for Hive effects.
// The caller supplies the already-ordered, load-bearing identity fields and any
// additional named inputs; owner, holder, epoch, attempt id, and model tool-call
// ids must stay out of both arguments so a retry or reassignment adopts the same
// durable operation instead of minting a duplicate.
func DeriveLogicalID(parts []string, inputs map[string]string) string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0})
		}
	}
	write(parts...)
	names := make([]string, 0, len(inputs))
	for k := range inputs {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		write(k, inputs[k])
	}
	return "op:" + hex.EncodeToString(h.Sum(nil))
}

// Effect is the complete description of one desired external effect. Its
// LogicalID deliberately EXCLUDES the owner and epoch: retry or reassignment
// of the same desired effect must find the same journal entry, and a
// replacement owner can never mint a second logical ID for unchanged inputs.
// Changing any load-bearing input (desired generation, subject head, ...)
// produces a DIFFERENT logical operation.
type Effect struct {
	// OutcomeKey is the accepted #4249/#4251 outcome identity
	// ("project/repo@outcome") this effect serves.
	OutcomeKey string `json:"outcome_key"`
	// DesiredGeneration is the outcome's desired generation at planning time.
	DesiredGeneration int `json:"desired_generation"`
	// Transition names the Hive transition performing the effect.
	Transition string `json:"transition"`
	// Subject is the canonical source-aware work key ("owner/repo#N").
	Subject string `json:"subject"`
	// ClaimKey is the canonical mutation claim key authorizing the effect.
	ClaimKey string `json:"claim_key"`
	// Kind is the effect kind; this vertical only ever writes EffectCreatePR.
	Kind string `json:"kind"`
	// Inputs are ALL load-bearing effect inputs (head branch, base branch,
	// repository, title digest, ...), canonicalized by the caller.
	Inputs map[string]string `json:"inputs"`
}

// Validate reports why an Effect cannot be journaled.
func (e Effect) Validate() error {
	if e.OutcomeKey == "" || e.Transition == "" || e.Subject == "" || e.ClaimKey == "" {
		return fmt.Errorf("%w: outcome key, transition, subject, and claim key are all required", ErrInvalidEffect)
	}
	if e.DesiredGeneration < 1 {
		return fmt.Errorf("%w: desired generation %d is impossible", ErrInvalidEffect, e.DesiredGeneration)
	}
	if e.Kind != EffectCreatePR {
		return fmt.Errorf("%w: effect kind %q is not implemented by this vertical (only %s)", ErrInvalidEffect, e.Kind, EffectCreatePR)
	}
	if len(e.Inputs) == 0 {
		return fmt.Errorf("%w: an external effect declares its inputs", ErrInvalidEffect)
	}
	return nil
}

// LogicalID derives the stable logical operation identity: a sha256 over the
// canonical serialization of every field above — and nothing else. Returns ""
// for an effect that fails Validate ("" is never an ID).
func (e Effect) LogicalID() string {
	if err := e.Validate(); err != nil {
		return ""
	}
	return DeriveLogicalID([]string{e.OutcomeKey, fmt.Sprintf("%d", e.DesiredGeneration), e.Transition, e.Subject, e.ClaimKey, e.Kind}, e.Inputs)
}

// Attempt is one authorized attempt at the effect, recorded INSIDE the
// operation entry with its owner epoch — never in the logical ID.
type Attempt struct {
	Epoch  uint64    `json:"epoch"`
	Holder string    `json:"holder"`
	At     time.Time `json:"at"`
	// Outcome is the attempt's locally observed conclusion: "applied",
	// "unknown", or "not-applied" once known; empty while in flight.
	Outcome string `json:"outcome,omitempty"`
}

// Operation is one durable journal entry for one logical operation.
type Operation struct {
	LogicalID string    `json:"logical_id"`
	Effect    Effect    `json:"effect"`
	Status    string    `json:"status"`
	Attempts  []Attempt `json:"attempts"`
	// Result is bounded provenance of the observed external result (e.g. the
	// created PR URL/number) — enough to audit, not a log archive.
	Result string `json:"result,omitempty"`
	// UpdatedAt is the instant of the last accepted journal transition.
	UpdatedAt time.Time `json:"updated_at"`
}

type persistedJournal struct {
	Version    int         `json:"version"`
	Operations []Operation `json:"operations"`
}

// Journal is the durable idempotent operation record: an in-memory index over
// one JSON file, reloaded on boot, rewritten atomically on every accepted
// transition, serialized per logical-ID under the journal mutex. Process
// memory is never authority.
type Journal struct {
	path string
	mu   sync.Mutex
	ops  map[string]*Operation
}

// OpenJournal loads the operation journal at path, creating an empty one when
// the file does not exist; a corrupt file is a refusal that leaves the bytes
// untouched for inspection.
func OpenJournal(path string) (*Journal, error) {
	j := &Journal{path: path, ops: make(map[string]*Operation)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return j, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading mutation journal %s: %w", path, err)
	}
	var persisted persistedJournal
	if err := json.Unmarshal(data, &persisted); err != nil {
		return nil, fmt.Errorf("mutation journal %s is unparseable and is left untouched for inspection: %w", path, err)
	}
	for i := range persisted.Operations {
		op := persisted.Operations[i]
		want := op.Effect.LogicalID()
		if want == "" || want != op.LogicalID {
			return nil, fmt.Errorf("mutation journal %s holds an entry whose id does not derive from its effect", path)
		}
		if _, dup := j.ops[op.LogicalID]; dup {
			return nil, fmt.Errorf("mutation journal %s holds conflicting entries for %s", path, op.LogicalID)
		}
		cp := op.clone()
		j.ops[op.LogicalID] = &cp
	}
	return j, nil
}

func (o Operation) clone() Operation {
	out := o
	out.Attempts = append([]Attempt(nil), o.Attempts...)
	out.Effect.Inputs = make(map[string]string, len(o.Effect.Inputs))
	for k, v := range o.Effect.Inputs {
		out.Effect.Inputs[k] = v
	}
	return out
}

// Get returns a detached copy of the operation for a logical ID.
func (j *Journal) Get(id string) (Operation, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	op, ok := j.ops[id]
	if !ok {
		return Operation{}, false
	}
	return op.clone(), true
}

// Begin persists operation INTENT — durably, BEFORE the external effect — and
// records the authorized attempt with its owner epoch inside the entry. The
// rules, in order:
//
//  1. An invalid effect is refused.
//  2. A new logical ID creates the entry (Planned) with attempt #1.
//  3. An existing entry already Applied refuses a new attempt
//     (ErrAlreadyApplied): the same desired effect can never run twice.
//  4. An existing entry with an unresolved attempt (Planned/Unknown) refuses
//     (ErrNeedsReconciliation): retry after uncertainty must reconcile the
//     SAME logical operation against authoritative external state first.
//  5. An entry reconciled to NotApplied accepts a new attempt — the
//     replacement owner ADOPTS the existing entry under its (newer) epoch;
//     unchanged inputs can never mint a second logical ID because the ID is
//     derived from the effect alone.
func (j *Journal) Begin(e Effect, epoch uint64, holder string, now time.Time) (Operation, error) {
	if err := e.Validate(); err != nil {
		return Operation{}, err
	}
	id := e.LogicalID()
	j.mu.Lock()
	defer j.mu.Unlock()
	if op, ok := j.ops[id]; ok {
		switch op.Status {
		case StatusApplied:
			return Operation{}, fmt.Errorf("%s: %w", id, ErrAlreadyApplied)
		case StatusPlanned, StatusUnknown:
			return Operation{}, fmt.Errorf("%s is %s: %w", id, op.Status, ErrNeedsReconciliation)
		}
		// NotApplied: adopt the SAME entry under the new attempt's epoch.
		work := op.clone()
		work.Status = StatusPlanned
		work.Attempts = append(work.Attempts, Attempt{Epoch: epoch, Holder: holder, At: now})
		work.UpdatedAt = now
		return j.commit(id, work)
	}
	work := Operation{
		LogicalID: id,
		Effect:    e,
		Status:    StatusPlanned,
		Attempts:  []Attempt{{Epoch: epoch, Holder: holder, At: now}},
		UpdatedAt: now,
	}
	return j.commit(id, work)
}

// RecordResult records the observed conclusion of the LATEST attempt, keyed
// by the attempt's epoch. Rules:
//
//   - Only the latest attempt's epoch may record: an older epoch's late
//     result can never overwrite the current attempt (ErrResultRegression).
//   - An Applied entry is terminal: nothing regresses it, though the
//     IDENTICAL applied acknowledgment deduplicates silently.
//   - status must be StatusApplied, StatusNotApplied, or StatusUnknown.
func (j *Journal) RecordResult(id string, epoch uint64, status, result string, now time.Time) (Operation, error) {
	if status != StatusApplied && status != StatusNotApplied && status != StatusUnknown {
		return Operation{}, fmt.Errorf("%w: %q is not a recordable status", ErrInvalidEffect, status)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	op, ok := j.ops[id]
	if !ok {
		return Operation{}, fmt.Errorf("%s: %w", id, ErrUnknownOperation)
	}
	if op.Status == StatusApplied {
		if status == StatusApplied && op.Result == result {
			return op.clone(), nil // duplicate acknowledgment deduplicates
		}
		return Operation{}, fmt.Errorf("%s is Applied: %w", id, ErrResultRegression)
	}
	last := op.Attempts[len(op.Attempts)-1]
	if last.Epoch != epoch {
		return Operation{}, fmt.Errorf("%s latest attempt epoch %d, presented %d: %w",
			id, last.Epoch, epoch, ErrResultRegression)
	}
	work := op.clone()
	work.Status = status
	work.Attempts[len(work.Attempts)-1].Outcome = attemptOutcome(status)
	if result != "" {
		work.Result = result
	}
	work.UpdatedAt = now
	return j.commit(id, work)
}

// ExternalState is one authoritative observation of whether the effect took
// place, produced by querying the external system itself (for EffectCreatePR:
// the existing open-PR-by-head lookup CreatePR already performs).
type ExternalState struct {
	// Known reports whether the observation was authoritative.
	Known bool
	// Applied reports, when Known, whether the effect exists externally.
	Applied bool
	// Result is bounded provenance of the found effect (PR URL/number).
	Result string
}

// Reconcile resolves the SAME logical operation after timeout, crash,
// restart, or reassignment, from current authoritative external state —
// BEFORE any retry:
//
//   - Known+Applied  → StatusApplied, recording the exact found effect,
//     WITHOUT repeating it;
//   - Known+!Applied → StatusNotApplied, authorizing a retry attempt through
//     Begin under the current epoch;
//   - !Known         → StatusUnknown: no duplicate retry is authorized.
//
// Reconciliation may be performed by any current owner (adoption); it never
// regresses an Applied entry to NotApplied — terminal truth stands, and a
// disagreeing later observation is a refusal, not a rewrite.
func (j *Journal) Reconcile(id string, state ExternalState, now time.Time) (Operation, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	op, ok := j.ops[id]
	if !ok {
		return Operation{}, fmt.Errorf("%s: %w", id, ErrUnknownOperation)
	}
	if op.Status == StatusApplied {
		if state.Known && !state.Applied {
			return Operation{}, fmt.Errorf("%s is Applied but external state disagrees: %w", id, ErrResultRegression)
		}
		return op.clone(), nil
	}
	work := op.clone()
	switch {
	case state.Known && state.Applied:
		work.Status = StatusApplied
		if state.Result != "" {
			work.Result = state.Result
		}
	case state.Known:
		work.Status = StatusNotApplied
	default:
		work.Status = StatusUnknown
	}
	if n := len(work.Attempts); n > 0 && work.Attempts[n-1].Outcome == "" {
		work.Attempts[n-1].Outcome = attemptOutcome(work.Status)
	}
	work.UpdatedAt = now
	return j.commit(id, work)
}

func attemptOutcome(status string) string {
	switch status {
	case StatusApplied:
		return "applied"
	case StatusNotApplied:
		return "not-applied"
	default:
		return "unknown"
	}
}

// commit installs the updated operation and persists atomically, rolling back
// the in-memory index when the durable write fails. Callers hold j.mu.
func (j *Journal) commit(id string, work Operation) (Operation, error) {
	prior, hadPrior := j.ops[id]
	cp := work.clone()
	j.ops[id] = &cp
	if err := j.persistLocked(); err != nil {
		if hadPrior {
			j.ops[id] = prior
		} else {
			delete(j.ops, id)
		}
		return Operation{}, err
	}
	return work.clone(), nil
}

func (j *Journal) persistLocked() error {
	all := make([]Operation, 0, len(j.ops))
	for _, op := range j.ops {
		all = append(all, op.clone())
	}
	sort.Slice(all, func(i, k int) bool { return all[i].LogicalID < all[k].LogicalID })
	data, err := json.MarshalIndent(persistedJournal{Version: journalFormatVersion, Operations: all}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling mutation journal: %w", err)
	}
	tmpPath := j.path + ".tmp"
	if err := os.WriteFile(tmpPath, data, journalFileMode); err != nil {
		return fmt.Errorf("writing tmp mutation journal: %w", err)
	}
	return os.Rename(tmpPath, j.path)
}
