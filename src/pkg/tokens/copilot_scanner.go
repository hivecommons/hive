package tokens

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
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
	// TokenDetails, when present (Copilot CLI >= 1.0.8x), carries the
	// disjoint per-category counts. Its "input" bucket is the UNCACHED prompt
	// tokens, whereas Usage.InputTokens is the whole prompt including the
	// cache_read / cache_write portions — see copilotModelMetric.splits.
	TokenDetails map[string]copilotTokenDetail `json:"tokenDetails,omitempty"`
}

type copilotTokenDetail struct {
	TokenCount *int64 `json:"tokenCount"`
}

type copilotUsage struct {
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CacheReadTokens  int64 `json:"cacheReadTokens"`
	CacheWriteTokens int64 `json:"cacheWriteTokens"`
}

// copilotTokenDetailInput is the tokenDetails key for uncached prompt tokens.
const copilotTokenDetailInput = "input"

// splits returns the disjoint (input, output, cacheRead, cacheWrite) counts
// for one model's shutdown metrics. Copilot CLI's usage.inputTokens is the
// FULL prompt size — it already includes cacheReadTokens and cacheWriteTokens
// (observed on every captured session: inputTokens == tokenDetails.input +
// cacheRead + cacheWrite). Pricing input at the uncached list rate and then
// adding the cache buckets again double-charged ~97% of every prompt, inflating
// Copilot estimates several-fold (#13470). Prefer the explicit tokenDetails
// bucket; without it, subtract the cache portions when they fit inside
// inputTokens (older CLI builds), else leave the split as reported.
func (m copilotModelMetric) splits() (input, output, cacheRead, cacheWrite int64) {
	u := m.Usage
	input, output, cacheRead, cacheWrite = u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens
	if d, ok := m.TokenDetails[copilotTokenDetailInput]; ok && d.TokenCount != nil && *d.TokenCount >= 0 {
		return *d.TokenCount, output, cacheRead, cacheWrite
	}
	if cached := cacheRead + cacheWrite; cached > 0 && input >= cached {
		input -= cached
	}
	return input, output, cacheRead, cacheWrite
}

type copilotToolComplete struct {
	Model string `json:"model"`
}

// CopilotScanOptions tunes ScanCopilotSessionTree.
type CopilotScanOptions struct {
	// LiveCaptureSinceMs, when > 0, is the Unix-ms moment the MITM proxy began
	// recording Copilot token usage live into the inference sink. Shutdown
	// token totals of sessions still active at/after that moment are deferred
	// to the sink (never double-counted); sessions that ended earlier keep
	// theirs. 0 disables the deferral.
	LiveCaptureSinceMs int64
	// LiveCaptured reports whether the sink actually recorded live usage for
	// (agent, model). The proxy only sniffs OpenAI-shaped /chat/completions
	// responses; Claude models driven by Copilot CLI go through /v1/messages
	// and are never captured, so deferring THEIR shutdown totals silently drops
	// the whole spend (#13470). When nil every model is treated as captured
	// (the legacy all-or-nothing deferral).
	LiveCaptured func(agent, model string) bool
	// DefaultAgent, when non-empty, is the sandbox that owns every session in
	// the scanned tree (the per-agent HOME layout, hivecommons/hive#9444) and
	// overrides cwd / keyword detection.
	DefaultAgent string
}

// copilotSessionStateRel is the Copilot CLI session-state path under a HOME.
const copilotSessionStateRel = ".copilot/session-state"

// copilotPerAgentHomesDirName is the directory under the shared HOME root that
// holds the per-agent HOMEs (pkg/agent interactiveHomeRootName).
const copilotPerAgentHomesDirName = "agents"

// CopilotSessionTree is one Copilot session-state directory to scan plus the
// agent that owns it ("" for the legacy shared directory).
type CopilotSessionTree struct {
	Dir   string
	Agent string
}

// CopilotSessionTrees returns the configured shared session-state directory
// plus every per-agent tree beside it. Since hivecommons/hive#9444 each
// per-UID agent has its own HOME under <shared-home>/agents/<name>, so the
// CLI writes <shared-home>/agents/<name>/.copilot/session-state — a tree the
// shared-dir scan never saw, which left every Copilot session uncounted and
// the per-sandbox cost table showing only proxy-sniffed side calls (#13470).
// The per-agent trees are derived only when sharedDir has the canonical
// <home>/.copilot/session-state shape.
func CopilotSessionTrees(sharedDir string) []CopilotSessionTree {
	if sharedDir == "" {
		return nil
	}
	trees := []CopilotSessionTree{{Dir: sharedDir}}
	rel := filepath.FromSlash(copilotSessionStateRel)
	if !strings.HasSuffix(filepath.Clean(sharedDir), rel) {
		return trees
	}
	home := strings.TrimSuffix(filepath.Clean(sharedDir), rel)
	home = filepath.Clean(home)
	matches, err := filepath.Glob(filepath.Join(home, copilotPerAgentHomesDirName, "*", rel))
	if err != nil {
		return trees
	}
	sort.Strings(matches)
	for _, m := range matches {
		agentHome := strings.TrimSuffix(m, string(filepath.Separator)+rel)
		agent := filepath.Base(agentHome)
		if agent == "" || agent == "." || strings.HasPrefix(agent, ".") {
			continue
		}
		trees = append(trees, CopilotSessionTree{Dir: m, Agent: agent})
	}
	return trees
}

// ScanCopilotSessions reads Copilot CLI session files from one session-state
// directory (typically ~/.copilot/session-state) and returns an
// AggregateSummary. It is ScanCopilotSessionTree with the legacy
// all-or-nothing live-capture deferral — see CopilotScanOptions.
func ScanCopilotSessions(sessionsDir string, liveCaptureSinceMs int64) (*AggregateSummary, error) {
	return ScanCopilotSessionTree(sessionsDir, CopilotScanOptions{LiveCaptureSinceMs: liveCaptureSinceMs})
}

// ScanCopilotSessionTree reads every <sessionsDir>/<id>/events.jsonl and
// aggregates per-session token usage from the session.shutdown ModelMetrics.
//
// Live-capture deferral is per MODEL: for a session active at/after
// opts.LiveCaptureSinceMs, a model's shutdown tokens are dropped only when
// opts.LiveCaptured(agent, model) says the sink recorded that model live;
// everything else is kept. Either way the scanner always contributes session
// presence, message counts, model, and timestamps (which the sink lacks).
func ScanCopilotSessionTree(sessionsDir string, opts CopilotScanOptions) (*AggregateSummary, error) {
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

		summary, perModel, err := parseCopilotSessionFileDetailed(eventsFile)
		if err != nil || summary == nil {
			continue
		}
		if opts.DefaultAgent != "" {
			summary.Agent = opts.DefaultAgent
		}
		if opts.LiveCaptureSinceMs > 0 && summary.LastActive >= opts.LiveCaptureSinceMs {
			// Deciding here — not inside the parser — means we can use the
			// session's own LastActive, which is only known after parsing.
			applyCopilotLiveDeferral(summary, perModel, opts.LiveCaptured)
		}
		if summary.TotalTokens == 0 && summary.Messages == 0 {
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

// copilotModelSplit is one model's disjoint token counts from a session's
// shutdown metrics.
type copilotModelSplit struct {
	Input, Output, CacheRead, CacheWrite int64
}

// applyCopilotLiveDeferral removes from summary the shutdown tokens of every
// model the live sink already recorded for this agent (all models when
// captured is nil), so the two sources never double-count.
func applyCopilotLiveDeferral(summary *SessionSummary, perModel map[string]copilotModelSplit, captured func(agent, model string) bool) {
	for model, sp := range perModel {
		if captured != nil && !captured(summary.Agent, model) {
			continue
		}
		summary.InputTokens -= sp.Input
		summary.OutputTokens -= sp.Output
		summary.CacheRead -= sp.CacheRead
		summary.CacheCreate -= sp.CacheWrite
	}
	summary.TotalTokens = summary.InputTokens + summary.OutputTokens + summary.CacheRead + summary.CacheCreate
}

func parseCopilotSessionFile(path string) (*SessionSummary, error) {
	summary, _, err := parseCopilotSessionFileDetailed(path)
	return summary, err
}

// parseCopilotSessionFileDetailed parses one events.jsonl into a session
// summary plus the per-model shutdown token splits (keyed by model id) that
// the live-capture deferral needs.
func parseCopilotSessionFileDetailed(path string) (*SessionSummary, map[string]copilotModelSplit, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }() // read-only fd; nothing to lose on close error

	summary := &SessionSummary{
		Agent: "unknown",
		Model: "unknown",
		// Native session-file token usage lands in one lump at session.shutdown,
		// so there is no intra-session time distribution to recover here. Live
		// MITM capture writes separate BackendCopilot sessions with timelines.
		Backend: BackendCopilot,
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, maxScanBufSizeClaude), maxScanBufSizeClaude)

	agentDetected := false
	agentScanCount := 0
	const maxAgentScan = 5
	perModel := map[string]copilotModelSplit{}

	for scanner.Scan() {
		var evt copilotEvent
		if err := json.Unmarshal(scanner.Bytes(), &evt); err != nil {
			continue
		}

		// Session start time: the earliest event with a parseable timestamp.
		if summary.FirstActive == 0 {
			if ts := parseTimestampToUnixMilli(evt.Timestamp); ts > 0 {
				summary.FirstActive = ts
			}
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
				// Always accrue the shutdown token totals here. The caller
				// (ScanCopilotSessions) decides per-session whether to keep them
				// or zero them out for double-count avoidance, based on whether
				// the session was active after live capture started — a decision
				// that needs the fully-parsed LastActive timestamp.
				for model, metrics := range shutdown.ModelMetrics {
					in, out, cr, cw := metrics.splits()
					summary.InputTokens += in
					summary.OutputTokens += out
					summary.CacheRead += cr
					summary.CacheCreate += cw
					sp := perModel[model]
					sp.Input += in
					sp.Output += out
					sp.CacheRead += cr
					sp.CacheWrite += cw
					perModel[model] = sp
				}
			}
		}
	}

	summary.TotalTokens = summary.InputTokens + summary.OutputTokens + summary.CacheRead + summary.CacheCreate
	return summary, perModel, nil
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
