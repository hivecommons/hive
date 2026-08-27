package turn

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

type OpKind string

const (
	OpPRCreate OpKind = "pr_create"
	OpPREdit   OpKind = "pr_edit"
	OpComment  OpKind = "comment"
	OpPush     OpKind = "push"
	OpLabel    OpKind = "label"
)

type OpStatus string

const (
	// OpIntended is persisted before the effect. On recovery it means the
	// effect may have landed and must be reconciled, never blindly replayed.
	OpIntended  OpStatus = "intended"
	OpSucceeded OpStatus = "succeeded"
	OpFailed    OpStatus = "failed"
)

type OpIntent struct {
	Kind       OpKind `json:"kind"`
	Repo       string `json:"repo,omitempty"`
	Target     string `json:"target,omitempty"`
	Body       string `json:"body,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type JournalEntry struct {
	IdempotencyKey string    `json:"idempotency_key"`
	Kind           OpKind    `json:"kind"`
	Status         OpStatus  `json:"status"`
	Repo           string    `json:"repo,omitempty"`
	Target         string    `json:"target,omitempty"`
	ToolCallID     string    `json:"tool_call_id,omitempty"`
	ExternalRef    string    `json:"external_ref,omitempty"`
	Error          string    `json:"error,omitempty"`
	Attempts       int       `json:"attempts"`
	IntendedAt     time.Time `json:"intended_at"`
	SettledAt      time.Time `json:"settled_at,omitempty"`
}

func (e JournalEntry) Done() bool      { return e.Status == OpSucceeded }
func (e JournalEntry) Ambiguous() bool { return e.Status == OpIntended }

const idempotencyKeyVersion = "v1"

// DeriveIdempotencyKey is stable across processes and model retries. A model's
// ToolCallID is deliberately excluded because inference can mint a new call ID
// for the same semantic effect after re-entry.
func DeriveIdempotencyKey(sessionID string, in OpIntent) string {
	h := sha256.New()
	for _, part := range []string{
		idempotencyKeyVersion,
		sessionID,
		string(in.Kind),
		in.Repo,
		in.Target,
		in.Body,
	} {
		_, _ = fmt.Fprintf(h, "%d:%s|", len(part), part)
	}
	return idempotencyKeyVersion + "-" + hex.EncodeToString(h.Sum(nil))[:32]
}

type Journal struct {
	Entries []JournalEntry `json:"entries,omitempty"`
}

func (j *Journal) Lookup(key string) (JournalEntry, bool) {
	for _, entry := range j.Entries {
		if entry.IdempotencyKey == key {
			return entry, true
		}
	}
	return JournalEntry{}, false
}

func (j *Journal) recordIntent(key string, in OpIntent, now time.Time) JournalEntry {
	for i := range j.Entries {
		if j.Entries[i].IdempotencyKey != key {
			continue
		}
		j.Entries[i].Status = OpIntended
		j.Entries[i].Error = ""
		j.Entries[i].SettledAt = time.Time{}
		j.Entries[i].Attempts++
		return j.Entries[i]
	}
	entry := JournalEntry{
		IdempotencyKey: key,
		Kind:           in.Kind,
		Status:         OpIntended,
		Repo:           in.Repo,
		Target:         in.Target,
		ToolCallID:     in.ToolCallID,
		Attempts:       1,
		IntendedAt:     now,
	}
	j.Entries = append(j.Entries, entry)
	return entry
}

func (j *Journal) settle(key string, status OpStatus, externalRef, errText string, now time.Time) {
	for i := range j.Entries {
		if j.Entries[i].IdempotencyKey != key {
			continue
		}
		j.Entries[i].Status = status
		j.Entries[i].ExternalRef = externalRef
		j.Entries[i].Error = errText
		j.Entries[i].SettledAt = now
		return
	}
}

func (j *Journal) Ambiguous() []JournalEntry {
	var entries []JournalEntry
	for _, entry := range j.Entries {
		if entry.Ambiguous() {
			entries = append(entries, entry)
		}
	}
	return entries
}

func (j *Journal) Summary() string {
	var lines []string
	for _, entry := range j.Entries {
		if entry.Done() {
			lines = append(lines, fmt.Sprintf("%s %s/%s -> %s", entry.Kind, entry.Repo, entry.Target, entry.ExternalRef))
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
