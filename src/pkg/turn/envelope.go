package turn

import (
	"encoding/json"
	"time"

	"github.com/hivecommons/hive/pkg/logscrub"
	"github.com/hivecommons/hive/pkg/toolapprove"
)

// ToJSON serializes the SessionEnvelope into JSON bytes.
//
// The conversation blob is a secret-bearing artifact: transcripts can contain
// tokens, repo contents, and credentials surfaced by tool output. Because the
// serialized envelope is the durable, handoff-able form of the session (it may
// be written to disk, a queue, or cross spokes), all content-bearing strings
// are scrubbed with the fleet's single reusable scrubber (logscrub) before
// serialization — scrub-on-persist, per the #4002 maintainer review. In-memory
// state (Clone / Step) is never scrubbed.
func (e SessionEnvelope) ToJSON() ([]byte, error) {
	return json.Marshal(e.scrubbedForPersist())
}

// ToPrettyJSON serializes the SessionEnvelope into indented JSON bytes,
// applying the same scrub-on-persist policy as ToJSON.
func (e SessionEnvelope) ToPrettyJSON() ([]byte, error) {
	return json.MarshalIndent(e.scrubbedForPersist(), "", "  ")
}

// scrubbedForPersist returns a deep copy of the envelope with credential-shaped
// substrings redacted from every content-bearing field.
func (e SessionEnvelope) scrubbedForPersist() SessionEnvelope {
	out := e.Clone()
	for i := range out.Messages {
		out.Messages[i] = scrubMessage(out.Messages[i])
	}
	for k, v := range out.Variables {
		out.Variables[k] = logscrub.ScrubString(v)
	}
	for i := range out.PendingApprovals {
		out.PendingApprovals[i].ToolCall.Arguments = logscrub.ScrubString(out.PendingApprovals[i].ToolCall.Arguments)
		out.PendingApprovals[i].Verdict.Rationale = logscrub.ScrubString(out.PendingApprovals[i].Verdict.Rationale)
	}
	for i := range out.Operations {
		if out.Operations[i].ToolCall != nil {
			out.Operations[i].ToolCall.Arguments = logscrub.ScrubString(out.Operations[i].ToolCall.Arguments)
		}
		if out.Operations[i].Verdict != nil {
			out.Operations[i].Verdict.Rationale = logscrub.ScrubString(out.Operations[i].Verdict.Rationale)
		}
		if out.Operations[i].OperatorDecision != nil {
			out.Operations[i].OperatorDecision.Rationale = logscrub.ScrubString(out.Operations[i].OperatorDecision.Rationale)
			out.Operations[i].OperatorDecision.Operator = logscrub.ScrubString(out.Operations[i].OperatorDecision.Operator)
		}
	}
	// The journal is persisted with the envelope and is equally secret-bearing:
	// Target can carry a branch name minted from a token-bearing remote URL,
	// ExternalRef can carry an authenticated URL, and Error is raw remote error
	// text — the most common accidental credential channel of the three. The
	// idempotency key itself is a hash and needs no scrubbing, and must not be
	// altered: scrubbing it would break re-entry matching.
	for i := range out.Journal.Entries {
		out.Journal.Entries[i].Repo = logscrub.ScrubString(out.Journal.Entries[i].Repo)
		out.Journal.Entries[i].Target = logscrub.ScrubString(out.Journal.Entries[i].Target)
		out.Journal.Entries[i].ExternalRef = logscrub.ScrubString(out.Journal.Entries[i].ExternalRef)
		out.Journal.Entries[i].Error = logscrub.ScrubString(out.Journal.Entries[i].Error)
	}
	return out
}

func scrubMessage(m Message) Message {
	m.Content = logscrub.ScrubString(m.Content)
	for i := range m.ToolCalls {
		m.ToolCalls[i].Arguments = logscrub.ScrubString(m.ToolCalls[i].Arguments)
	}
	for k, v := range m.Metadata {
		m.Metadata[k] = logscrub.ScrubString(v)
	}
	return m
}

// ParseEnvelope parses a SessionEnvelope from JSON bytes.
func ParseEnvelope(data []byte) (SessionEnvelope, error) {
	var env SessionEnvelope
	err := json.Unmarshal(data, &env)
	return env, err
}

// Clone creates a deep copy of the SessionEnvelope.
func (e SessionEnvelope) Clone() SessionEnvelope {
	data, _ := json.Marshal(e)
	var out SessionEnvelope
	_ = json.Unmarshal(data, &out)
	return out
}

// AddMessage appends a message to the conversation history and updates UpdatedAt.
func (e *SessionEnvelope) AddMessage(role Role, content string) Message {
	msg := Message{
		Role:      role,
		Content:   content,
		Timestamp: time.Now().UTC(),
	}
	e.Messages = append(e.Messages, msg)
	e.UpdatedAt = msg.Timestamp
	return msg
}

// AddToolCallMessage appends an assistant message containing tool calls.
func (e *SessionEnvelope) AddToolCallMessage(content string, calls []ToolCall) Message {
	msg := Message{
		Role:      RoleAssistant,
		Content:   content,
		ToolCalls: calls,
		Timestamp: time.Now().UTC(),
	}
	e.Messages = append(e.Messages, msg)
	e.UpdatedAt = msg.Timestamp
	return msg
}

// AddToolResultMessage appends a tool execution result message.
func (e *SessionEnvelope) AddToolResultMessage(callID, name, output, errStr string) Message {
	msg := Message{
		Role:       RoleTool,
		Content:    output,
		Name:       name,
		ToolCallID: callID,
		Timestamp:  time.Now().UTC(),
	}
	if errStr != "" {
		if msg.Metadata == nil {
			msg.Metadata = make(map[string]string)
		}
		msg.Metadata["error"] = errStr
		if output == "" {
			msg.Content = "Error: " + errStr
		}
	}
	e.Messages = append(e.Messages, msg)
	e.UpdatedAt = msg.Timestamp
	return msg
}

func (e *SessionEnvelope) AddToolApprovalOperation(call ToolCall, verdict toolapprove.Verdict) string {
	now := time.Now().UTC()
	id := "tool-approval:" + call.ID
	status := OperationStatusPending
	if verdict.AutoApproved() {
		status = OperationStatusApproved
	} else if verdict.Denied() {
		status = OperationStatusDenied
	}
	op := EnvelopeOperation{
		ID:        id,
		Kind:      OperationKindToolApproval,
		Status:    status,
		ToolCall:  &call,
		Verdict:   &verdict,
		CreatedAt: now,
	}
	if status != OperationStatusPending {
		op.SettledAt = now
	}
	e.Operations = append(e.Operations, op)
	e.UpdatedAt = now
	return id
}

func (e *SessionEnvelope) SettleToolApprovalOperation(id string, decision OperatorApprovalDecision) {
	now := time.Now().UTC()
	status := OperationStatusDenied
	if decision.Approved {
		status = OperationStatusApproved
	}
	for i := range e.Operations {
		if e.Operations[i].ID == id && e.Operations[i].Kind == OperationKindToolApproval {
			e.Operations[i].Status = status
			e.Operations[i].OperatorDecision = &decision
			e.Operations[i].SettledAt = now
			e.UpdatedAt = now
			return
		}
	}
}
