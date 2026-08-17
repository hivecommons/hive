package turn

import (
	"encoding/json"
	"time"
)

// ToJSON serializes the SessionEnvelope into JSON bytes.
func (e SessionEnvelope) ToJSON() ([]byte, error) {
	return json.Marshal(e)
}

// ToPrettyJSON serializes the SessionEnvelope into indented JSON bytes.
func (e SessionEnvelope) ToPrettyJSON() ([]byte, error) {
	return json.MarshalIndent(e, "", "  ")
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
