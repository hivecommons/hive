package agent

import (
	"encoding/json"
	"io"
	"strings"
	"time"
)

// claudeTranscriptLine is the subset of a Claude Code
// ~/.claude/projects/<cwd>/<session>.jsonl record the kick log renders.
type claudeTranscriptLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// claudeContentBlock is one block of a message's content array.
type claudeContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// renderClaudeTranscript renders a Claude Code session JSONL as plain text:
// user and assistant turns, every tool_use with its full input and every
// tool_result with its full output. Thinking blocks and bookkeeping records
// (summaries, snapshots) are skipped.
func renderClaudeTranscript(r io.Reader, since time.Time) (string, error) {
	var w transcriptWriter
	toolNames := map[string]string{}
	err := forEachTranscriptLine(r, func(line []byte) error {
		var rec claudeTranscriptLine
		if err := json.Unmarshal(line, &rec); err != nil {
			return err
		}
		if (rec.Type != "user" && rec.Type != "assistant") || rec.Message == nil || len(rec.Message.Content) == 0 {
			return nil
		}
		ts := parseTranscriptTime(rec.Timestamp)
		if beforeKick(ts, since) {
			return nil
		}
		role := rec.Type + ":"
		var text string
		if json.Unmarshal(rec.Message.Content, &text) == nil {
			if strings.TrimSpace(text) != "" {
				w.entry(ts, role, text)
			}
			return nil
		}
		var blocks []claudeContentBlock
		if err := json.Unmarshal(rec.Message.Content, &blocks); err != nil {
			return err
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					w.entry(ts, role, b.Text)
				}
			case "tool_use":
				toolNames[b.ID] = b.Name
				w.toolCall(ts, b.Name, b.ID, b.Input)
			case "tool_result":
				status := "ok"
				if b.IsError {
					status = "failed"
				}
				w.entry(ts, "tool result: "+toolNames[b.ToolUseID]+" ("+b.ToolUseID+") "+status, claudeToolResultText(b.Content))
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return w.String(), nil
}

// claudeToolResultText flattens a tool_result's content: a string, or an
// array of text (and image) blocks.
func claudeToolResultText(raw json.RawMessage) string {
	var blocks []claudeContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return transcriptText(raw)
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "\n")
}
