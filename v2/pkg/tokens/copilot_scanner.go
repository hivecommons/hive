package tokens

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// copilotEvent matches the Copilot CLI's events.jsonl format.
// Events have a type field and a data object with event-specific fields.
type copilotEvent struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

type copilotSessionStart struct {
	SessionID     string         `json:"sessionId"`
	SelectedModel string         `json:"selectedModel"`
	Context       copilotContext `json:"context"`
}

type copilotContext struct {
	Cwd string `json:"cwd"`
}

type copilotUserMessage struct {
	Content string `json:"content"`
}

type copilotShutdown struct {
	CurrentModel string                        `json:"currentModel"`
	ModelMetrics map[string]copilotModelMetric `json:"modelMetrics"`
}

type copilotModelMetric struct {
	Usage copilotUsage `json:"usage"`
}

type copilotUsage struct {
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CacheReadTokens  int64 `json:"cacheReadTokens"`
	CacheWriteTokens int64 `json:"cacheWriteTokens"`
}

type copilotToolComplete struct {
	Model string `json:"model"`
}

// ScanCopilotSessions reads Copilot CLI session files from the session-state
// directory and returns an AggregateSummary. The sessionsDir is typically
// ~/.copilot/session-state.
//
// When liveCapture is true, the MITM proxy is recording Copilot token usage
// live (per completion response) into the inference sink, so this scanner MUST
// NOT also accrue tokens from the session.shutdown ModelMetrics — doing so would
// double-count every Copilot session (once live via the sink, once at shutdown
// here). In that mode the scanner still contributes session presence, message
// counts, model, and last-active timestamps (which the live sink does not
// capture), but records zero tokens; the proxy sink is the single source of
// Copilot token totals.
func ScanCopilotSessions(sessionsDir string, liveCapture bool) (*AggregateSummary, error) {
	agg := &AggregateSummary{
		ByAgent:       make(map[string]int64),
		ByModel:       make(map[string]int64),
		ByAgentDetail: make(map[string]*AgentModelBucket),
		ByModelDetail: make(map[string]*AgentModelBucket),
	}

	if sessionsDir == "" {
		return agg, nil
	}

	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return agg, nil
	}

	now := time.Now()
	cutoff := now.Add(-maxSessionAgeDays * 24 * time.Hour)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		eventsFile := filepath.Join(sessionsDir, entry.Name(), "events.jsonl")
		info, err := os.Stat(eventsFile)
		if err != nil || info.ModTime().Before(cutoff) {
			continue
		}

		summary, err := parseCopilotSessionFile(eventsFile, liveCapture)
		if err != nil || summary == nil || summary.TotalTokens == 0 && summary.Messages == 0 {
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

func parseCopilotSessionFile(path string, liveCapture bool) (*SessionSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	summary := &SessionSummary{
		Agent: "unknown",
		Model: "unknown",
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, maxScanBufSizeClaude), maxScanBufSizeClaude)

	agentDetected := false
	agentScanCount := 0
	const maxAgentScan = 5

	for scanner.Scan() {
		var evt copilotEvent
		if err := json.Unmarshal(scanner.Bytes(), &evt); err != nil {
			continue
		}

		switch evt.Type {
		case "session.start":
			var start copilotSessionStart
			if json.Unmarshal(evt.Data, &start) == nil {
				if start.SessionID != "" {
					summary.SessionID = start.SessionID[:min(12, len(start.SessionID))]
				}
				if start.SelectedModel != "" {
					summary.Model = start.SelectedModel
				}
				if start.Context.Cwd != "" {
					agent := detectAgentFromCwd(start.Context.Cwd)
					if agent != "" {
						summary.Agent = agent
						agentDetected = true
					}
				}
			}

		case "user.message":
			if !agentDetected && agentScanCount < maxAgentScan {
				var msg copilotUserMessage
				if json.Unmarshal(evt.Data, &msg) == nil && msg.Content != "" {
					detected := DefaultAgentDetector(msg.Content)
					if detected != "unknown" {
						summary.Agent = detected
						agentDetected = true
					}
				}
				agentScanCount++
				if agentScanCount >= maxAgentScan {
					agentDetected = true
				}
			}
			summary.Messages++
			if ts := parseTimestampToUnixMilli(evt.Timestamp); ts > 0 {
				summary.LastActive = ts
			}

		case "assistant.message":
			summary.Messages++
			if ts := parseTimestampToUnixMilli(evt.Timestamp); ts > 0 {
				summary.LastActive = ts
			}

		case "tool.execution_complete":
			var tc copilotToolComplete
			if json.Unmarshal(evt.Data, &tc) == nil && tc.Model != "" && tc.Model != "unknown" {
				summary.Model = tc.Model
			}

		case "session.shutdown":
			var shutdown copilotShutdown
			if json.Unmarshal(evt.Data, &shutdown) == nil {
				if shutdown.CurrentModel != "" {
					summary.Model = shutdown.CurrentModel
				}
				// When the live-capture proxy is active it already recorded
				// every completion's tokens into the inference sink; accruing
				// the shutdown ModelMetrics here would double-count. Keep the
				// model (set above) but skip the token totals.
				if !liveCapture {
					for _, metrics := range shutdown.ModelMetrics {
						summary.InputTokens += metrics.Usage.InputTokens
						summary.OutputTokens += metrics.Usage.OutputTokens
						summary.CacheRead += metrics.Usage.CacheReadTokens
						summary.CacheCreate += metrics.Usage.CacheWriteTokens
					}
				}
			}
		}
	}

	summary.TotalTokens = summary.InputTokens + summary.OutputTokens + summary.CacheRead + summary.CacheCreate
	return summary, nil
}

// detectAgentFromCwd extracts the agent name from the working directory path.
// Copilot agents run in /data/agents/<name>/ directories.
func detectAgentFromCwd(cwd string) string {
	const agentPrefix = "/data/agents/"
	if idx := strings.Index(cwd, agentPrefix); idx >= 0 {
		remainder := cwd[idx+len(agentPrefix):]
		parts := strings.SplitN(remainder, "/", 2)
		if len(parts) > 0 && parts[0] != "" {
			return parts[0]
		}
	}
	return ""
}
