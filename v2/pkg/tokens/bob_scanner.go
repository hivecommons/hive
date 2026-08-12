package tokens

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// bobChatSession is the top-level structure of a bobshell chat recording file.
// Files live at $HOME/.bob/tmp/<uuid>/chats/<session-id>.json.
//
// Verified schema (from live bobshell recordings on disk):
//
//	{
//	  "sessionId": "abc-123",
//	  "projectHash": "...",
//	  "startTime": "2026-01-31T05:43:35.306Z",
//	  "lastUpdated": "2026-01-31T05:45:00.000Z",
//	  "messages": [
//	    {"id":"...","timestamp":"...","type":"user","content":"..."},
//	    {"id":"...","timestamp":"...","type":"bob-shell","content":"...",
//	     "model":"premium",
//	     "tokens":{"input":8200,"output":285,"cached":8166,"thoughts":0},
//	     "toolCalls":[...]}
//	  ]
//	}
type bobChatSession struct {
	SessionID   string           `json:"sessionId"`
	StartTime   string           `json:"startTime"`
	LastUpdated string           `json:"lastUpdated"`
	Messages    []bobChatMessage `json:"messages"`
}

type bobChatMessage struct {
	// Type is the role discriminator: "user", "bob-shell", etc.
	Type    string `json:"type"`
	Content string `json:"content"`
	// Model is per-message (e.g. "premium", "standard").
	Model string `json:"model,omitempty"`
	// Tokens carries usage for assistant ("bob-shell") messages.
	// User messages typically omit this field entirely.
	Tokens *bobTokens `json:"tokens,omitempty"`

	// Legacy/alternative fields from older or future bobshell versions.
	Role  string          `json:"role,omitempty"`
	Usage *bobLegacyUsage `json:"usage,omitempty"`
}

// bobTokens is the primary token usage shape in bobshell recordings.
//
// Billing note on `cached`: in the observed data, `input` already INCLUDES
// `cached` tokens (e.g. input=8200, cached=8166 means 34 new input tokens
// plus 8166 cache-hit tokens, totalling 8200 reported as "input"). Therefore
// we do NOT add `cached` on top of `input` — doing so would approximately
// double-count. We record `cached` separately in the AgentModelBucket's
// CacheRead field for visibility, but the budget-relevant token count is
// input + output only.
type bobTokens struct {
	Input    int64 `json:"input"`
	Output   int64 `json:"output"`
	Cached   int64 `json:"cached"`
	Thoughts int64 `json:"thoughts"`
}

// bobLegacyUsage is a fallback for hypothetical older bobshell versions that
// may use OpenAI-style field names. Checked only when the primary `tokens`
// field is absent.
type bobLegacyUsage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// charsPerToken is the estimation factor when explicit token counts are
// unavailable: ~4 characters per token for English-heavy code work.
const charsPerToken = 4

// maxBobChatFileSize limits how large a single chat file we'll parse.
const maxBobChatFileSize = 50 * 1024 * 1024 // 50 MB

// maxBobSessionAge limits how far back to scan.
const maxBobSessionAge = 30 * 24 * time.Hour

// ScanBobSessions reads bobshell's chat recording files from the given
// directory (typically /data/home/.bob or ~/.bob) and returns an
// AggregateSummary. It walks tmp/*/chats/*.json looking for session recordings.
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

		var inputTokens, outputTokens, cachedTokens int64
		hasExplicitUsage := false
		messageCount := 0
		// Per-message model; use last seen as session model.
		var sessionModel string

		for i := range sess.Messages {
			msg := &sess.Messages[i]
			msgType := msg.effectiveType()

			if msg.Model != "" {
				sessionModel = msg.Model
			}

			in, out, cached := msg.effectiveTokensReal()
			if in > 0 || out > 0 {
				hasExplicitUsage = true
				inputTokens += in
				outputTokens += out
				cachedTokens += cached
			}

			if msgType == "user" || msgType == "bob-shell" || msgType == "assistant" {
				messageCount++
			}
		}

		// Fall back to estimation when no explicit usage is recorded.
		if !hasExplicitUsage {
			for i := range sess.Messages {
				msg := &sess.Messages[i]
				chars := int64(len(msg.Content))
				estimated := chars / charsPerToken
				if estimated < 1 && chars > 0 {
					estimated = 1
				}
				switch msg.effectiveType() {
				case "user":
					inputTokens += estimated
				case "bob-shell", "assistant":
					outputTokens += estimated
				}
			}
		}

		total := inputTokens + outputTokens
		if total == 0 {
			continue
		}

		model := sessionModel
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
		agg.ByAgentDetail[agentName].CacheRead += cachedTokens
		agg.ByAgentDetail[agentName].Messages += messageCount
		agg.ByAgentDetail[agentName].Sessions++

		if agg.ByModelDetail[model] == nil {
			agg.ByModelDetail[model] = &AgentModelBucket{}
		}
		agg.ByModelDetail[model].Input += inputTokens
		agg.ByModelDetail[model].Output += outputTokens
		agg.ByModelDetail[model].CacheRead += cachedTokens
		agg.ByModelDetail[model].Messages += messageCount
		agg.ByModelDetail[model].Sessions++

		sessionID := sess.SessionID
		if sessionID == "" {
			sessionID = extractBobSessionID(path)
		}
		agg.Sessions = append(agg.Sessions, SessionSummary{
			SessionID:    sessionID,
			Agent:        agentName,
			Model:        model,
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			CacheRead:    cachedTokens,
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

// effectiveType normalizes the message type/role. Bobshell uses "type" as the
// discriminator; legacy formats may use "role" instead.
func (m *bobChatMessage) effectiveType() string {
	if m.Type != "" {
		return m.Type
	}
	return m.Role
}

// effectiveTokensReal returns the best available token counts from a message.
// Primary path: tokens.{input, output, cached}.
// Fallback: usage.{input_tokens, output_tokens} or usage.{prompt_tokens, completion_tokens}.
func (m *bobChatMessage) effectiveTokensReal() (input, output, cached int64) {
	if m.Tokens != nil {
		// Primary bobshell format. `input` already includes `cached` —
		// see the billing note on bobTokens. We report cached separately
		// for visibility only.
		return m.Tokens.Input, m.Tokens.Output, m.Tokens.Cached
	}
	if m.Usage != nil {
		in := m.Usage.InputTokens
		out := m.Usage.OutputTokens
		if in == 0 && m.Usage.PromptTokens > 0 {
			in = m.Usage.PromptTokens
		}
		if out == 0 && m.Usage.CompletionTokens > 0 {
			out = m.Usage.CompletionTokens
		}
		return in, out, 0
	}
	return 0, 0, 0
}

// extractBobSessionID pulls the UUID directory component from a path like
// .bob/tmp/<uuid>/chats/session.json.
func extractBobSessionID(path string) string {
	dir := filepath.Dir(path) // .../chats
	dir = filepath.Dir(dir)   // .../<uuid>
	base := filepath.Base(dir)
	if base == "." || base == "/" || base == "tmp" {
		return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return base
}
