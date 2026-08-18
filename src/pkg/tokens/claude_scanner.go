package tokens

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// claudeRawEntry matches Claude Code's native JSONL format where token usage
// is nested inside message.usage rather than at the top level.
//
// Claude Code session files live at ~/.claude/projects/<project-hash>/*.jsonl
// and contain entries like:
//
//	{"type":"assistant","timestamp":"2025-01-01T00:00:00Z","message":{"model":"claude-sonnet-4-20250514","usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":20,"cache_creation_input_tokens":10}}}
//	{"type":"human","timestamp":"2025-01-01T00:01:00Z","message":{"text":"scan for issues"}}
type claudeRawEntry struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp,omitempty"`
	Message   json.RawMessage `json:"message,omitempty"`
	// Also support the flat format used by the existing collector as a fallback.
	Role         string `json:"role,omitempty"`
	Model        string `json:"model,omitempty"`
	InputTokens  int64  `json:"input_tokens,omitempty"`
	OutputTokens int64  `json:"output_tokens,omitempty"`
	CacheRead    int64  `json:"cache_read,omitempty"`
	CacheCreate  int64  `json:"cache_creation,omitempty"`
}

// claudeMessagePayload is the nested message structure in Claude Code's JSONL.
type claudeMessagePayload struct {
	Model string             `json:"model,omitempty"`
	Usage claudeUsagePayload `json:"usage,omitempty"`
	Text  string             `json:"text,omitempty"`
	Role  string             `json:"role,omitempty"`
}

// claudeUsagePayload is the token usage block inside a Claude Code assistant message.
type claudeUsagePayload struct {
	InputTokens             int64 `json:"input_tokens"`
	OutputTokens            int64 `json:"output_tokens"`
	CacheReadInputTokens    int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

const (
	// maxSessionAgeDays is how far back to scan Claude session files.
	maxSessionAgeDays = 30
	// maxScanBufSizeClaude is the buffer size for scanning large JSONL lines.
	maxScanBufSizeClaude = 10 * 1024 * 1024
)

// ScanClaudeSessions reads Claude Code's native session JSONL files from the
// given projects directory and returns an AggregateSummary compatible with the
// existing collector output. The projectsDir is typically ~/.claude/projects.
//
// It scans all *.jsonl files in all subdirectories (project hashes), plus any
// subagent files in */subagents/*.jsonl.
func ScanClaudeSessions(projectsDir string, agentDetector func(string) string) (*AggregateSummary, error) {
	agg := &AggregateSummary{
		ByAgent:       make(map[string]int64),
		ByModel:       make(map[string]int64),
		ByAgentDetail: make(map[string]*AgentModelBucket),
		ByModelDetail: make(map[string]*AgentModelBucket),
	}

	if projectsDir == "" {
		return agg, nil
	}

	// Find all JSONL files: <projectsDir>/*/*.jsonl and <projectsDir>/*/subagents/*.jsonl
	patterns := []string{
		filepath.Join(projectsDir, "*", "*.jsonl"),
		filepath.Join(projectsDir, "*", "subagents", "*.jsonl"),
	}

	var files []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		files = append(files, matches...)
	}

	now := time.Now()
	cutoff := now.Add(-maxSessionAgeDays * 24 * time.Hour)

	for _, file := range files {
		// Skip old files based on modification time
		info, err := os.Stat(file)
		if err != nil || info.ModTime().Before(cutoff) {
			continue
		}

		summary, err := parseClaudeSessionFile(file, agentDetector)
		if err != nil || summary == nil {
			continue
		}
		if summary.TotalTokens == 0 {
			continue
		}

		agg.Sessions = append(agg.Sessions, *summary)
		agg.TotalTokens += summary.TotalTokens
		agg.TotalInput += summary.InputTokens
		agg.TotalOutput += summary.OutputTokens
		agg.TotalCacheRead += summary.CacheRead
		agg.TotalCacheCreate += summary.CacheCreate
		agg.TotalMessages += summary.Messages
		agg.ByAgent[summary.Agent] += summary.TotalTokens
		agg.ByModel[summary.Model] += summary.TotalTokens

		// Per-agent detail
		ab, ok := agg.ByAgentDetail[summary.Agent]
		if !ok {
			ab = &AgentModelBucket{}
			agg.ByAgentDetail[summary.Agent] = ab
		}
		ab.Input += summary.InputTokens
		ab.Output += summary.OutputTokens
		ab.CacheRead += summary.CacheRead
		ab.CacheCreate += summary.CacheCreate
		ab.Messages += summary.Messages
		ab.Sessions++

		// Per-model detail
		mb, ok := agg.ByModelDetail[summary.Model]
		if !ok {
			mb = &AgentModelBucket{}
			agg.ByModelDetail[summary.Model] = mb
		}
		mb.Input += summary.InputTokens
		mb.Output += summary.OutputTokens
		mb.CacheRead += summary.CacheRead
		mb.CacheCreate += summary.CacheCreate
		mb.Messages += summary.Messages
		mb.Sessions++
	}

	agg.SessionCount = len(agg.Sessions)
	return agg, nil
}

// parseClaudeSessionFile reads a single Claude Code session JSONL file and
// extracts token usage from the nested message.usage structure.
func parseClaudeSessionFile(path string, agentDetector func(string) string) (*SessionSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	summary := &SessionSummary{
		SessionID: sessionID,
		Agent:     "unknown",
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, maxScanBufSizeClaude), maxScanBufSizeClaude)

	firstHumanMsg := ""
	var lastTimestamp int64

	for scanner.Scan() {
		var raw claudeRawEntry
		if err := json.Unmarshal(scanner.Bytes(), &raw); err != nil {
			continue
		}

		switch raw.Type {
		case "assistant":
			// Parse nested message payload for model and usage
			var msg claudeMessagePayload
			if len(raw.Message) > 0 {
				if err := json.Unmarshal(raw.Message, &msg); err == nil {
					if msg.Model != "" && summary.Model == "" {
						summary.Model = msg.Model
					}
					summary.InputTokens += msg.Usage.InputTokens
					summary.OutputTokens += msg.Usage.OutputTokens
					summary.CacheRead += msg.Usage.CacheReadInputTokens
					summary.CacheCreate += msg.Usage.CacheCreationInputTokens
				}
			}
			summary.Messages++
			lastTimestamp = parseTimestampToUnixMilli(raw.Timestamp)

		case "human":
			// Extract first human message for agent detection
			if firstHumanMsg == "" && len(raw.Message) > 0 {
				var msg claudeMessagePayload
				if json.Unmarshal(raw.Message, &msg) == nil && msg.Text != "" {
					firstHumanMsg = msg.Text
				}
				// Also try direct string unmarshal for simpler message formats
				if firstHumanMsg == "" {
					var text string
					if json.Unmarshal(raw.Message, &text) == nil && text != "" {
						firstHumanMsg = text
					}
				}
			}
			summary.Messages++
			lastTimestamp = parseTimestampToUnixMilli(raw.Timestamp)

		default:
			// Flat format fallback: entries with top-level input_tokens/output_tokens
			if raw.InputTokens > 0 || raw.OutputTokens > 0 {
				summary.InputTokens += raw.InputTokens
				summary.OutputTokens += raw.OutputTokens
				summary.CacheRead += raw.CacheRead
				summary.CacheCreate += raw.CacheCreate
				if raw.Model != "" && summary.Model == "" {
					summary.Model = raw.Model
				}
			}
			if raw.Role == "user" || raw.Role == "assistant" {
				summary.Messages++
				lastTimestamp = time.Now().UnixMilli()
			}
			if raw.Role == "user" && firstHumanMsg == "" {
				// Try Message field as a string for flat-format files
				if raw.Message != nil {
					var text string
					if json.Unmarshal(raw.Message, &text) == nil {
						firstHumanMsg = text
					}
				}
			}
		}
	}

	summary.TotalTokens = summary.InputTokens + summary.OutputTokens + summary.CacheRead + summary.CacheCreate
	summary.LastActive = lastTimestamp

	if agentDetector != nil && firstHumanMsg != "" {
		summary.Agent = agentDetector(firstHumanMsg)
	}

	return summary, nil
}

// parseTimestampToUnixMilli converts an ISO 8601 timestamp string to Unix milliseconds.
// Returns 0 if parsing fails.
func parseTimestampToUnixMilli(ts string) int64 {
	if ts == "" {
		return 0
	}
	// Handle Z suffix
	ts = strings.Replace(ts, "Z", "+00:00", 1)
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		// Try with nanosecond precision
		t, err = time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return 0
		}
	}
	return t.UnixMilli()
}

// MergeAggregates combines two AggregateSummary results into the first one.
// This is used to merge Claude native session data with flat-format metrics data.
func MergeAggregates(dst, src *AggregateSummary) {
	if src == nil {
		return
	}

	// Deduplicate by session ID — if a session already exists in dst, skip it
	existing := make(map[string]bool, len(dst.Sessions))
	for _, s := range dst.Sessions {
		existing[s.SessionID] = true
	}

	for _, sess := range src.Sessions {
		if existing[sess.SessionID] {
			continue
		}

		dst.Sessions = append(dst.Sessions, sess)
		dst.TotalTokens += sess.TotalTokens
		dst.TotalInput += sess.InputTokens
		dst.TotalOutput += sess.OutputTokens
		dst.TotalCacheRead += sess.CacheRead
		dst.TotalCacheCreate += sess.CacheCreate
		dst.TotalMessages += sess.Messages
		dst.ByAgent[sess.Agent] += sess.TotalTokens
		dst.ByModel[sess.Model] += sess.TotalTokens

		// Per-agent detail
		ab, ok := dst.ByAgentDetail[sess.Agent]
		if !ok {
			ab = &AgentModelBucket{}
			dst.ByAgentDetail[sess.Agent] = ab
		}
		ab.Input += sess.InputTokens
		ab.Output += sess.OutputTokens
		ab.CacheRead += sess.CacheRead
		ab.CacheCreate += sess.CacheCreate
		ab.Messages += sess.Messages
		ab.Sessions++

		// Per-model detail
		mb, ok := dst.ByModelDetail[sess.Model]
		if !ok {
			mb = &AgentModelBucket{}
			dst.ByModelDetail[sess.Model] = mb
		}
		mb.Input += sess.InputTokens
		mb.Output += sess.OutputTokens
		mb.CacheRead += sess.CacheRead
		mb.CacheCreate += sess.CacheCreate
		mb.Messages += sess.Messages
		mb.Sessions++
	}

	dst.SessionCount = len(dst.Sessions)
}

// EnhancedAgentDetector uses the HIVE_AGENT environment variable that the agent
// manager sets in each tmux session. The tmux session name is "hive-<agent>",
// and HIVE_AGENT is set to the agent name. The projectsDir path structure
// encodes the working directory, which typically contains the agent name.
func EnhancedAgentDetector(sessionPath string, fallbackDetector func(string) string) func(string) string {
	return func(firstMsg string) string {
		// Check if the session path contains an agent name hint
		// Claude projects dir structure: ~/.claude/projects/<hash-of-working-dir>/
		// The working dir for agents is typically /data/agents/<agent-name>/
		lower := strings.ToLower(sessionPath)
		agents := ConfiguredAgentNames()
		for _, agent := range agents {
			if strings.Contains(lower, agent) {
				return agent
			}
		}

		// Fall back to message-based detection
		if fallbackDetector != nil {
			return fallbackDetector(firstMsg)
		}
		return "unknown"
	}
}

// AgentFromTmuxEnv attempts to detect the agent name by checking if the session
// file's parent directory path contains a known agent work directory pattern.
// Agent work dirs follow the pattern /data/agents/<name>/, and Claude creates
// project hashes from the working directory path.
func AgentFromTmuxEnv(filePath string) string {
	// The file path structure for Claude sessions started from agent work dirs:
	// ~/.claude/projects/-data-agents-<name>/session.jsonl
	// The project hash replaces / with - in the path.
	dir := filepath.Dir(filePath)
	base := filepath.Base(dir)
	baseLower := strings.ToLower(base)

	// Check for -data-agents-<name> pattern in the project directory name
	const agentDirPrefix = "-data-agents-"
	idx := strings.Index(baseLower, agentDirPrefix)
	if idx >= 0 {
		remainder := baseLower[idx+len(agentDirPrefix):]
		// The agent name is the next path segment (until the next -)
		parts := strings.SplitN(remainder, "-", 2)
		if len(parts) > 0 && parts[0] != "" {
			return parts[0]
		}
	}

	return ""
}

// HiveAgentDetector creates an agent detector that first checks the file path
// for agent name hints (from the Claude project directory structure), then falls
// back to keyword-based detection from the first user message.
func HiveAgentDetector(filePath string) func(string) string {
	return func(firstMsg string) string {
		// Try path-based detection first
		if agent := AgentFromTmuxEnv(filePath); agent != "" {
			return agent
		}
		// Fall back to keyword detection
		return DefaultAgentDetector(firstMsg)
	}
}

// ScanClaudeSessionsWithPathDetection is like ScanClaudeSessions but uses
// the file path to help determine which agent owns each session.
func ScanClaudeSessionsWithPathDetection(projectsDir string) (*AggregateSummary, error) {
	agg := &AggregateSummary{
		ByAgent:       make(map[string]int64),
		ByModel:       make(map[string]int64),
		ByAgentDetail: make(map[string]*AgentModelBucket),
		ByModelDetail: make(map[string]*AgentModelBucket),
	}

	if projectsDir == "" {
		return agg, nil
	}

	patterns := []string{
		filepath.Join(projectsDir, "*", "*.jsonl"),
		filepath.Join(projectsDir, "*", "subagents", "*.jsonl"),
	}

	var files []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		files = append(files, matches...)
	}

	now := time.Now()
	cutoff := now.Add(-maxSessionAgeDays * 24 * time.Hour)

	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil || info.ModTime().Before(cutoff) {
			continue
		}

		detector := HiveAgentDetector(file)
		summary, err := parseClaudeSessionFile(file, detector)
		if err != nil || summary == nil || summary.TotalTokens == 0 {
			continue
		}

		agg.Sessions = append(agg.Sessions, *summary)
		agg.TotalTokens += summary.TotalTokens
		agg.TotalInput += summary.InputTokens
		agg.TotalOutput += summary.OutputTokens
		agg.TotalCacheRead += summary.CacheRead
		agg.TotalCacheCreate += summary.CacheCreate
		agg.TotalMessages += summary.Messages
		agg.ByAgent[summary.Agent] += summary.TotalTokens
		agg.ByModel[summary.Model] += summary.TotalTokens

		ab, ok := agg.ByAgentDetail[summary.Agent]
		if !ok {
			ab = &AgentModelBucket{}
			agg.ByAgentDetail[summary.Agent] = ab
		}
		ab.Input += summary.InputTokens
		ab.Output += summary.OutputTokens
		ab.CacheRead += summary.CacheRead
		ab.CacheCreate += summary.CacheCreate
		ab.Messages += summary.Messages
		ab.Sessions++

		mb, ok := agg.ByModelDetail[summary.Model]
		if !ok {
			mb = &AgentModelBucket{}
			agg.ByModelDetail[summary.Model] = mb
		}
		mb.Input += summary.InputTokens
		mb.Output += summary.OutputTokens
		mb.CacheRead += summary.CacheRead
		mb.CacheCreate += summary.CacheCreate
		mb.Messages += summary.Messages
		mb.Sessions++
	}

	agg.SessionCount = len(agg.Sessions)
	return agg, nil
}
