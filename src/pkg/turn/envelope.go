// Package turn prototypes a re-entrant, conversation-as-state agent turn.
//
// It is deliberately not wired into hive's tmux agent loop. The package is a
// bounded spike for RFC #4002: all state needed to resume a contribute-shaped
// turn is carried by SessionEnvelope, and Step returns a structured TurnOutput.
package turn

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/hivecommons/hive/pkg/logscrub"
)

const EnvelopeVersion = 1

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type Message struct {
	Role       Role              `json:"role"`
	Content    string            `json:"content"`
	Name       string            `json:"name,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Timestamp  time.Time         `json:"timestamp"`
}

type Status string

const (
	StatusActive    Status = "active"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

type Verdict string

const (
	VerdictShipped      Verdict = "shipped"
	VerdictNoWorkNeeded Verdict = "no_work_needed"
	VerdictFailed       Verdict = "failed"
)

// TurnPlan is the ordered work for a single headless contribute turn. It lives
// in the envelope rather than on a Runner so a fresh process does not need the
// process that produced the plan in order to resume it.
type TurnPlan struct {
	Operations []PlannedOperation `json:"operations,omitempty"`
	Verdict    Verdict            `json:"verdict"`
	Rationale  string             `json:"rationale,omitempty"`
}

type PlannedOperation struct {
	// IdempotencyKey is bound once, when the plan first enters the envelope.
	// It remains stable even if scrub-on-persist redacts credential-shaped text
	// from the operation body.
	IdempotencyKey string   `json:"idempotency_key"`
	Intent         OpIntent `json:"intent"`
}

// SessionEnvelope is the complete, serializable state of a prototype turn.
// It can be discarded in memory after every Step and reconstructed from JSON.
type SessionEnvelope struct {
	Version   int       `json:"version"`
	SessionID string    `json:"session_id"`
	Agent     string    `json:"agent,omitempty"`
	TaskRef   string    `json:"task_ref,omitempty"`
	Status    Status    `json:"status"`
	Messages  []Message `json:"messages,omitempty"`
	Plan      *TurnPlan `json:"plan,omitempty"`
	Journal   Journal   `json:"journal,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TurnOutput is the structured return from one re-entrant Step invocation.
type TurnOutput struct {
	Status    Status         `json:"status"`
	Done      bool           `json:"done"`
	Verdict   Verdict        `json:"verdict,omitempty"`
	Rationale string         `json:"rationale,omitempty"`
	Effects   []JournalEntry `json:"effects,omitempty"`
}

func (e SessionEnvelope) Validate() error {
	if e.Version != EnvelopeVersion {
		return fmt.Errorf("turn: envelope version %d, want %d", e.Version, EnvelopeVersion)
	}
	if e.SessionID == "" {
		return fmt.Errorf("turn: session ID is required")
	}
	return nil
}

func (e SessionEnvelope) Clone() SessionEnvelope {
	data, _ := json.Marshal(e)
	var out SessionEnvelope
	_ = json.Unmarshal(data, &out)
	return out
}

// ToJSON is the persistence boundary. The durable envelope may cross process
// or spoke boundaries, so every content-bearing field is scrubbed here while
// the in-memory envelope remains untouched.
func (e SessionEnvelope) ToJSON() ([]byte, error) {
	return json.Marshal(e.scrubbedForPersist())
}

func ParseEnvelope(data []byte) (SessionEnvelope, error) {
	var env SessionEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return SessionEnvelope{}, fmt.Errorf("turn: parse envelope: %w", err)
	}
	if err := env.Validate(); err != nil {
		return SessionEnvelope{}, err
	}
	return env, nil
}

func (e SessionEnvelope) scrubbedForPersist() SessionEnvelope {
	out := e.Clone()
	out.Agent = logscrub.ScrubString(out.Agent)
	out.TaskRef = logscrub.ScrubString(out.TaskRef)
	for i := range out.Messages {
		out.Messages[i].Content = logscrub.ScrubString(out.Messages[i].Content)
		out.Messages[i].Name = logscrub.ScrubString(out.Messages[i].Name)
		for key, value := range out.Messages[i].Metadata {
			out.Messages[i].Metadata[key] = logscrub.ScrubString(value)
		}
	}
	if out.Plan != nil {
		out.Plan.Rationale = logscrub.ScrubString(out.Plan.Rationale)
		for i := range out.Plan.Operations {
			op := &out.Plan.Operations[i].Intent
			op.Repo = logscrub.ScrubString(op.Repo)
			op.Target = logscrub.ScrubString(op.Target)
			op.Body = logscrub.ScrubString(op.Body)
		}
	}
	for i := range out.Journal.Entries {
		entry := &out.Journal.Entries[i]
		entry.Repo = logscrub.ScrubString(entry.Repo)
		entry.Target = logscrub.ScrubString(entry.Target)
		entry.ExternalRef = logscrub.ScrubString(entry.ExternalRef)
		entry.Error = logscrub.ScrubString(entry.Error)
	}
	return out
}
