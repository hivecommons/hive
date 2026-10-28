package agent

import (
	"encoding/json"
	"io"
	"strings"
	"time"
)

// copilotTranscriptEvent is the subset of a Copilot CLI
// ~/.copilot/session-state/<id>/events.jsonl record the kick log renders.
// Payload fields whose shape varies across CLI releases stay raw.
type copilotTranscriptEvent struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Data      struct {
		Content      json.RawMessage `json:"content"`
		ToolCallID   string          `json:"toolCallId"`
		ToolName     string          `json:"toolName"`
		Arguments    json.RawMessage `json:"arguments"`
		ToolRequests []struct {
			ToolCallID string          `json:"toolCallId"`
			Name       string          `json:"name"`
			Arguments  json.RawMessage `json:"arguments"`
		} `json:"toolRequests"`
		Success *bool           `json:"success"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
		// ShellExecution ({"exitCode":N}) and ToolTelemetry are read by the
		// repeat-failure reader (repeat_failure_copilot.go); raw so a shape
		// change never breaks the kick log.
		ShellExecution json.RawMessage `json:"shellExecution"`
		ToolTelemetry  json.RawMessage `json:"toolTelemetry"`
	} `json:"data"`
}

// copilotToolResult is the usual shape of tool.execution_complete's result;
// detailedContent holds the untruncated output when the CLI recorded one.
type copilotToolResult struct {
	Content         string `json:"content"`
	DetailedContent string `json:"detailedContent"`
}

// renderCopilotTranscript renders a Copilot CLI events.jsonl as plain text:
// user and assistant turns, every tool call with its full arguments and every
// tool result with its full output.
func renderCopilotTranscript(r io.Reader, since time.Time) (string, error) {
	var w transcriptWriter
	toolNames := map[string]string{}
	rendered := map[string]bool{}
	call := func(ts time.Time, id, name string, args json.RawMessage) {
		if id != "" {
			if name != "" {
				toolNames[id] = name
			}
			if rendered[id] {
				return
			}
			rendered[id] = true
		}
		w.toolCall(ts, name, id, args)
	}
	err := forEachTranscriptLine(r, func(line []byte) error {
		var ev copilotTranscriptEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return err
		}
		ts := parseTranscriptTime(ev.Timestamp)
		if beforeKick(ts, since) {
			return nil
		}
		switch ev.Type {
		case "user.message":
			w.entry(ts, "user:", transcriptText(ev.Data.Content))
		case "assistant.message":
			if text := transcriptText(ev.Data.Content); strings.TrimSpace(text) != "" {
				w.entry(ts, "assistant:", text)
			}
			for _, req := range ev.Data.ToolRequests {
				call(ts, req.ToolCallID, req.Name, req.Arguments)
			}
		case "tool.execution_start":
			call(ts, ev.Data.ToolCallID, ev.Data.ToolName, ev.Data.Arguments)
		case "tool.execution_complete":
			name := ev.Data.ToolName
			if name == "" {
				name = toolNames[ev.Data.ToolCallID]
			}
			status := "ok"
			if ev.Data.Success != nil && !*ev.Data.Success {
				status = "failed"
			}
			output := transcriptText(ev.Data.Result)
			var res copilotToolResult
			if json.Unmarshal(ev.Data.Result, &res) == nil {
				output = res.DetailedContent
				if output == "" {
					output = res.Content
				}
			}
			var errText string
			if e := transcriptText(ev.Data.Error); e != "" {
				errText = "error: " + e
			}
			w.entry(ts, "tool result: "+name+" ("+ev.Data.ToolCallID+") "+status, output, errText)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return w.String(), nil
}
