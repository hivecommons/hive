package turn

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kubestellar/hive/pkg/convergence/mutation"
)

// OpKind classifies a journaled operation by the kind of side effect it has on
// the outside world. Only side-effectful kinds need idempotency protection;
// read-only operations are replayable for free and are not journaled.
type OpKind string

const (
	// OpPRCreate opens a pull request. Non-idempotent at the GitHub API level:
	// a replayed create yields a second PR.
	OpPRCreate OpKind = "pr_create"
	// OpPREdit edits an existing pull request (title/body/base).
	OpPREdit OpKind = "pr_edit"
	// OpComment posts an issue or PR comment. The canonical duplicate-effect
	// shape: replay yields a visible double-post.
	OpComment OpKind = "comment"
	// OpPush pushes commits to a branch.
	OpPush OpKind = "push"
	// OpLabel adds or removes labels on an issue or PR. Naturally idempotent on
	// GitHub's side, but journaled anyway so replay accounting is uniform.
	OpLabel OpKind = "label"
	// OpApprovalWait is a suspended operator-approval wait (#4000). It performs
	// no external effect itself; it exists so a turn blocked on an operator
	// decision is a journaled, re-enterable position rather than in-process
	// suspended state. See the shape note in the stage-2 design section.
	OpApprovalWait OpKind = "approval_wait"
)

// sideEffectful reports whether a kind mutates external state and therefore
// must be replay-protected by its idempotency key.
func (k OpKind) sideEffectful() bool {
	return k != OpApprovalWait
}

// OpStatus is the journaled lifecycle position of a single operation. The
// three states exist precisely so that re-entry can distinguish "never
// started" from "may have happened" from "definitely happened".
type OpStatus string

const (
	// OpIntended is written BEFORE the effect is attempted. A record found in
	// this state on re-entry is the ambiguous case: the process died between
	// declaring intent and recording the outcome, so the effect may or may not
	// have landed. It must be reconciled (looked up), never blindly replayed.
	OpIntended OpStatus = "intended"
	// OpSucceeded is written AFTER the effect landed, carrying the external
	// result reference. Re-entry short-circuits these: already done.
	OpSucceeded OpStatus = "succeeded"
	// OpFailed is written after a terminal failure. Re-entry may retry.
	OpFailed OpStatus = "failed"
)

// JournalEntry is the durable record of one side-effectful operation inside a
// turn. It is written to the envelope twice: once as OpIntended before the
// effect is attempted, and once again — same IdempotencyKey, same slot — as
// OpSucceeded or OpFailed after. That before/after pair is what makes a turn
// re-enterable at an operation boundary instead of only at a turn boundary.
type JournalEntry struct {
	// IdempotencyKey is the stable, content-derived identity of the logical
	// effect. See DeriveIdempotencyKey for the derivation and its rationale.
	IdempotencyKey string   `json:"idempotency_key"`
	Kind           OpKind   `json:"kind"`
	Status         OpStatus `json:"status"`
	// Repo is the "owner/name" the effect targets, empty for non-repo effects.
	Repo string `json:"repo,omitempty"`
	// Target is the secondary subject within the repo (issue/PR number, branch).
	Target string `json:"target,omitempty"`
	// ToolCallID ties the entry back to the model-requested tool call that
	// produced it, so a transcript and its journal can be cross-read.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ExternalRef is the identity the remote assigned to the effect (PR URL,
	// comment ID, pushed SHA). Populated only on OpSucceeded. This is what a
	// re-entering process returns instead of re-performing the effect.
	ExternalRef string `json:"external_ref,omitempty"`
	// Error records a terminal failure reason on OpFailed.
	Error string `json:"error,omitempty"`
	// Attempts counts how many times execution has been entered for this key,
	// across process lifetimes. >1 means a re-entry occurred.
	Attempts   int       `json:"attempts"`
	IntendedAt time.Time `json:"intended_at"`
	SettledAt  time.Time `json:"settled_at,omitempty"`
}

// Done reports whether the effect is known to have landed.
func (e JournalEntry) Done() bool { return e.Status == OpSucceeded }

// Ambiguous reports whether the entry is in the crash-window state: intent was
// recorded but no outcome was. The effect may or may not exist remotely, so it
// must be reconciled against the remote rather than replayed or skipped.
func (e JournalEntry) Ambiguous() bool { return e.Status == OpIntended }

// OpIntent describes a side-effectful operation a turn is about to perform.
// It is the input to the journal; the key is derived from it.
type OpIntent struct {
	Kind OpKind
	Repo string
	// Target is the issue/PR number or branch the effect applies to.
	Target string
	// Body is the semantic payload of the effect — comment text, PR
	// title+body, label set, pushed tree-ish. It participates in the key so
	// that two genuinely different effects of the same kind on the same target
	// are distinct operations, while a replay of the same effect is not.
	Body string
	// ToolCallID is the originating model tool call, recorded but deliberately
	// NOT part of the key. See DeriveIdempotencyKey.
	ToolCallID string
}

// idempotencyKeyVersion prefixes every derived key. Bumping it deliberately
// invalidates all prior keys, which is the escape hatch if the derivation
// itself ever has to change: old journals stay readable, new keys never
// collide with them.
const idempotencyKeyVersion = "v1"

// DeriveIdempotencyKey computes the stable identity of a logical side effect.
//
// Deprecated: turn operation identities are now derived by
// mutation.DeriveLogicalID, the canonical operation-id helper shared with the
// durable convergence journal. This wrapper remains only for the unwired
// SessionEnvelope prototype until callers migrate to the mutation journal
// directly.
//
// Design (RFC #4002 stage 2). The key must satisfy two opposing constraints:
//
//  1. STABLE ACROSS RE-ENTRY — recomputing it in a fresh process, from the
//     persisted envelope alone, must yield the same value. This rules out
//     anything ambient: no timestamps, no random nonces, no PIDs, no
//     process-local counters, no attempt numbers.
//  2. UNIQUE PER LOGICAL EFFECT — two different intended effects must not
//     collide, or the journal would suppress a real second operation and the
//     turn would silently under-perform (a failure mode strictly worse than a
//     duplicate, because it is invisible).
//
// The derivation delegates to mutation.DeriveLogicalID, the same helper used
// by pkg/convergence/mutation's durable journal, over the semantic content of
// the effect and nothing else:
//
//	sha256(version | session | kind | repo | target | body)
//
// Field-by-field rationale:
//
//   - SessionID scopes the key to one task's turn sequence. Without it, two
//     different agents posting the identical comment on the same issue would
//     collide and the second would be suppressed. Handoff preserves SessionID,
//     so this stays stable across processes — which is the whole point.
//   - Kind + Repo + Target locate the effect.
//   - Body is the content hash. It is what distinguishes "post comment A" from
//     "post comment B" on the same issue, and equally what makes a replay of
//     "post comment A" recognizable as the same operation.
//
// Deliberately EXCLUDED:
//
//   - ToolCallID. Model-assigned call IDs are freshly generated on every
//     inference, so a re-entry that re-runs the LLM step would produce a new
//     ID for a semantically identical effect and defeat the whole mechanism.
//     The ID is recorded on the entry for traceability but never keyed on.
//   - Timestamps and attempt counters, per constraint 1.
//
// On the "natural GitHub idempotency surrogate" alternative: GitHub exposes no
// idempotency-key header on the writes we care about (unlike Stripe). The
// available surrogates are all *query-after-the-fact* — search for an open PR
// from head branch X, list comments and match the body. Those are genuinely
// useful and this design uses them, but as RECONCILIATION for the ambiguous
// OpIntended window (see Reconciler), not as the key itself: they cost a round
// trip, they are eventually consistent, and they cannot be computed offline.
// The shared content hash is the primary key; the remote query is the
// tie-breaker for the one state where the local journal cannot know the answer.
//
// Lineage: at-least-once delivery over non-idempotent GitHub writes is exactly
// the duplicate-work class the fleet eradicated one incident at a time in
// #3768 → #3792 → #3980 → #3987. Each of those was the same bug — a task
// re-entering a pool and re-performing an effect whose completion was not
// durably recorded — patched at a different layer. This journal is the
// generalization: record the effect, not the attempt.
func DeriveIdempotencyKey(sessionID string, in OpIntent) string {
	return mutation.DeriveLogicalID([]string{
		idempotencyKeyVersion,
		sessionID,
		string(in.Kind),
		in.Repo,
		in.Target,
		in.Body,
	}, nil)
}

// Journal is the ordered list of operation records carried inside the
// envelope. Order is the operation-boundary sequence of the turn; the map-free
// slice representation keeps the JSON stable and diffable.
//
// Deprecated: use pkg/convergence/mutation.Journal for durable operation
// journaling. pkg/turn must not grow a second idempotency implementation.
type Journal struct {
	Entries []JournalEntry `json:"entries,omitempty"`
}

// Lookup returns the entry for a key and whether it exists.
func (j *Journal) Lookup(key string) (JournalEntry, bool) {
	for _, e := range j.Entries {
		if e.IdempotencyKey == key {
			return e, true
		}
	}
	return JournalEntry{}, false
}

// RecordIntent writes (or re-writes) the pre-execution OpIntended record for a
// key and returns the entry plus the prior entry if one existed. Calling it a
// second time for the same key increments Attempts — that counter is the
// journal's own evidence that a re-entry happened.
func (j *Journal) RecordIntent(key string, in OpIntent, now time.Time) (JournalEntry, JournalEntry, bool) {
	for i := range j.Entries {
		if j.Entries[i].IdempotencyKey != key {
			continue
		}
		prior := j.Entries[i]
		// A settled entry is never downgraded back to intended; callers must
		// consult Lookup first. Only the attempt counter moves.
		if prior.Status == OpIntended {
			j.Entries[i].Attempts++
		}
		return j.Entries[i], prior, true
	}
	e := JournalEntry{
		IdempotencyKey: key,
		Kind:           in.Kind,
		Status:         OpIntended,
		Repo:           in.Repo,
		Target:         in.Target,
		ToolCallID:     in.ToolCallID,
		Attempts:       1,
		IntendedAt:     now,
	}
	j.Entries = append(j.Entries, e)
	return e, JournalEntry{}, false
}

// Settle writes the post-execution outcome for a key.
func (j *Journal) Settle(key string, status OpStatus, externalRef, errStr string, now time.Time) {
	for i := range j.Entries {
		if j.Entries[i].IdempotencyKey != key {
			continue
		}
		j.Entries[i].Status = status
		j.Entries[i].ExternalRef = externalRef
		j.Entries[i].Error = errStr
		j.Entries[i].SettledAt = now
		return
	}
}

// Ambiguous returns every entry stuck in the intended-but-unsettled state.
// A re-entering process must reconcile each of these before proceeding.
func (j *Journal) Ambiguous() []JournalEntry {
	var out []JournalEntry
	for _, e := range j.Entries {
		if e.Ambiguous() {
			out = append(out, e)
		}
	}
	return out
}

// EffectCount returns the number of successfully landed effects, optionally
// filtered by kind. Tests assert exactly-once against this.
func (j *Journal) EffectCount(kinds ...OpKind) int {
	want := map[OpKind]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	n := 0
	for _, e := range j.Entries {
		if !e.Done() {
			continue
		}
		if len(want) > 0 && !want[e.Kind] {
			continue
		}
		n++
	}
	return n
}

// Summary renders a stable, sorted one-line-per-effect digest of the landed
// effects. Two runs that produced identical effects produce identical
// summaries, which is how the replay test compares a killed-and-resumed run
// against its unkilled positive control.
func (j *Journal) Summary() string {
	var lines []string
	for _, e := range j.Entries {
		if !e.Done() {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s %s/%s -> %s", e.Kind, e.Repo, e.Target, e.ExternalRef))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
