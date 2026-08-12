package tokens

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// bobChatSession is the top-level structure of a bobshell 1.0.6 chat recording
// file. Files live at $HOME/.bob/tmp/<uuid>/chats/<session-id>.json and contain
// an array of messages with optional per-message token usage.
//
// Observed format (bobshell 1.0.6, IBM watsonx backend):
//
//	{
//	  "id": "session-uuid",
//	  "model": "granite-3.1-8b-instruct",
//	  "messages": [
//	    {"role":"user","content":"..."},
//	    {"role":"assistant","content":"...","usage":{"input_tokens":100,"output_tokens":50}}
//	  ]
//	}
//
// When explicit usage is absent (older bobshell versions or recording disabled),
// the scanner estimates tokens from message content length (~4 chars/token).
type bobChatSession struct {
	ID       string           `json:"id"`
	Model    string           `json:"model"`
	Messages []bobChatMessage `json:"messages"`
}

type bobChatMessage struct {
	Role    string      `json:"role"`
	Content string      `json:"content"`
	Usage   bobMsgUsage `json:"usage,omitempty"`
}

type bobMsgUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	// IBM's watsonx sometimes uses these field names instead.
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// charsPerToken is the estimation factor when explicit token counts are
// unavailable: ~4 characters per token for English-heavy code work.
const charsPerToken = 4

// maxBobChatFileSize limits how large a single chat file we'll parse. Files
// larger than this are skipped to avoid OOM on runaway recordings.
const maxBobChatFileSize = 50 * 1024 * 1024 // 50 MB

// maxBobSessionAge limits how far back to scan. Sessions older than this are
// skipped (they predate the budget window anyway).
const maxBobSessionAge = 30 * 24 * time.Hour

// ScanBobSessions reads bobshell's chat recording files from the given
// directory (typically /data/home/.bob) and returns an AggregateSummary.
// It walks tmp/*/chats/*.json looking for session recordings.
func ScanBobSessions(bobHomeDir string) (*AggregateSummary, error) {
	agg := &AggregateSummary{
		ByAgent:       make(map[string]int64),
		ByModel:       make(map[string]int64),
		ByAgentDetail: make(map[string]*AgentModelBucket),
		ByModelDetail: make(map[string]*AgentModelBucket),
	}

	if bobHomeDir == "" {
		return agg, nil
	}

	chatsGlob := filepath.Join(bobHomeDir, "tmp", "*", "chats", "*.json")
	matches, err := filepath.Glob(chatsGlob)
	if err != nil {
		return agg, err
	}

	cutoff := time.Now().Add(-maxBobSessionAge)

	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		if info.ModTime().Before(cutoff) {
			continue
		}
		if info.Size() > maxBobChatFileSize {
			continue
		}

		sess, err := parseBobChatFile(path)
		if err != nil || sess == nil {
			continue
		}

		var inputTokens, outputTokens int64
		hasExplicitUsage := false
		messageCount := 0

		for _, msg := range sess.Messages {
			in, out := msg.Usage.effectiveTokens()
			if in > 0 || out > 0 {
				hasExplicitUsage = true
				inputTokens += in
				outputTokens += out
			}
			if msg.Role == "assistant" || msg.Role == "user" {
				messageCount++
			}
		}

		// Fall back to estimation when no explicit usage is recorded.
		if !hasExplicitUsage {
			for _, msg := range sess.Messages {
				chars := int64(len(msg.Content))
				estimated := chars / charsPerToken
				if estimated < 1 && chars > 0 {
					estimated = 1
				}
				switch msg.Role {
				case "user":
					inputTokens += estimated
				case "assistant":
					outputTokens += estimated
				}
			}
		}

		total := inputTokens + outputTokens
		if total == 0 {
			continue
		}

		model := sess.Model
		if model == "" {
			model = "bob-unknown"
		}

		const agentName = "bob"
		agg.TotalTokens += total
		agg.TotalMessages += messageCount
		agg.SessionCount++
		agg.ByAgent[agentName] += total
		agg.ByModel[model] += total

		if agg.ByAgentDetail[agentName] == nil {
			agg.ByAgentDetail[agentName] = &AgentModelBucket{}
		}
		agg.ByAgentDetail[agentName].Input += inputTokens
		agg.ByAgentDetail[agentName].Output += outputTokens
		agg.ByAgentDetail[agentName].Messages += messageCount
		agg.ByAgentDetail[agentName].Sessions++

		if agg.ByModelDetail[model] == nil {
			agg.ByModelDetail[model] = &AgentModelBucket{}
		}
		agg.ByModelDetail[model].Input += inputTokens
		agg.ByModelDetail[model].Output += outputTokens
		agg.ByModelDetail[model].Messages += messageCount
		agg.ByModelDetail[model].Sessions++

		// Use the file path's UUID component as session ID.
		sessionID := extractBobSessionID(path)
		agg.Sessions = append(agg.Sessions, SessionSummary{
			SessionID:    sessionID,
			Agent:        agentName,
			Model:        model,
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			TotalTokens:  total,
			Messages:     messageCount,
		})
	}

	return agg, nil
}

func parseBobChatFile(path string) (*bobChatSession, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var sess bobChatSession
	if err := json.Unmarshal(data, &sess); err != nil {
		// Try as a JSON array of messages (alternative format).
		var msgs []bobChatMessage
		if err2 := json.Unmarshal(data, &msgs); err2 != nil {
			return nil, err
		}
		sess.Messages = msgs
	}

	return &sess, nil
}

// effectiveTokens returns the best available token counts, preferring
// input_tokens/output_tokens over prompt_tokens/completion_tokens.
func (u bobMsgUsage) effectiveTokens() (input, output int64) {
	input = u.InputTokens
	output = u.OutputTokens
	if input == 0 && u.PromptTokens > 0 {
		input = u.PromptTokens
	}
	if output == 0 && u.CompletionTokens > 0 {
		output = u.CompletionTokens
	}
	return
}

// extractBobSessionID pulls the UUID directory component from a path like
// .bob/tmp/<uuid>/chats/session.json.
func extractBobSessionID(path string) string {
	dir := filepath.Dir(path) // .../chats
	dir = filepath.Dir(dir)   // .../<uuid>
	base := filepath.Base(dir)
	if base == "." || base == "/" || base == "tmp" {
		// Fallback: use the filename without extension.
		return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return base
}
