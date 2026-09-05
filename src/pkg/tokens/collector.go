package tokens

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type SessionEntry struct {
	Type          string `json:"type"`
	Model         string `json:"model,omitempty"`
	CacheCreation int64  `json:"cache_creation,omitempty"`
	CacheRead     int64  `json:"cache_read,omitempty"`
	InputTokens   int64  `json:"input_tokens,omitempty"`
	OutputTokens  int64  `json:"output_tokens,omitempty"`
	Message       string `json:"message,omitempty"`
	Role          string `json:"role,omitempty"`
	// Timestamp is the per-entry event time as an ISO 8601 / RFC 3339 string
	// (the same wire format the Claude and Copilot session files use). It is
	// optional; entries without one simply don't contribute to the session's
	// FirstActive/LastActive bracket.
	Timestamp string `json:"timestamp,omitempty"`
	// Agent, when set, pins the session to a specific agent instead of
	// relying on keyword detection from the first user message. Inference
	// (bare-mode) agents set this so the translator-written usage records
	// attribute cleanly. Normal Claude/Copilot session files omit it.
	Agent string `json:"agent,omitempty"`
	// Backend pins collector-generated session files (for example MITM
	// Copilot live capture) to their producing backend.
	Backend string `json:"backend,omitempty"`
	// Coalesced carries UsageEvent coalescing metadata when the inference sink
	// rewrites a bounded per-request timeline.
	Coalesced int `json:"coalesced,omitempty"`
}

// UsageEvent is one timestamped slice of token usage inside a session — the
// per-assistant-message grain that the Claude session files record and that
// every scanner previously summed away. Retaining it is what makes intra-session
// analysis possible: burn-rate curves over the life of a run, and (see
// pkg/dashboard/repo_cost.go) attributing a session's cost to the repos an agent
// actually touched while it was spending.
//
// TimestampMs is unix-milliseconds. An event whose source line carried no
// parseable timestamp is still emitted with TimestampMs 0 so its tokens are
// never lost; consumers that need ordering must treat 0 as "unknown time" and
// refuse to place it in an interval rather than sorting it to the front.
type UsageEvent struct {
	TimestampMs int64  `json:"ts_ms"`
	Model       string `json:"model,omitempty"`
	// Coalesced counts how many raw per-message events this entry represents.
	// It is 1 for an untouched event and >1 for a bucket produced by
	// coalescing (see maxUsageEventsPerSession). It is never 0 for a real event.
	Coalesced   int   `json:"coalesced,omitempty"`
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cache_read"`
	CacheCreate int64 `json:"cache_create"`
}

// Total is the sum of the four token counts, matching how SessionSummary
// computes TotalTokens.
func (u UsageEvent) Total() int64 {
	return u.Input + u.Output + u.CacheRead + u.CacheCreate
}

type SessionSummary struct {
	SessionID    string `json:"session_id"`
	Agent        string `json:"agent"`
	Model        string `json:"model"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	CacheRead    int64  `json:"cache_read"`
	CacheCreate  int64  `json:"cache_create"`
	TotalTokens  int64  `json:"total_tokens"`
	Messages     int    `json:"messages"`
	// FirstActive / LastActive bracket the session in time: the earliest and
	// latest event timestamps seen while parsing, as unix-milliseconds stamps
	// (0 when the scanner could not determine them).
	FirstActive int64 `json:"first_active,omitempty"`
	LastActive  int64 `json:"last_active,omitempty"`

	// Backend names the tool that produced this session ("claude", "copilot",
	// "bob", "inference", ""). Consumers use it to tell which sessions carry a
	// usable Usage timeline; "" means an older/flat-format session of unknown
	// provenance and must be treated as NOT time-resolved.
	Backend string `json:"backend,omitempty"`

	// Usage is the time-ordered per-message/per-request usage timeline,
	// populated only by scanners/sinks that can observe the grain (Claude
	// sessions and MITM-captured Copilot completions). It is ADDITIVE: the
	// summed fields above remain the
	// authoritative session totals and are unchanged by its presence. When
	// non-empty its token sums equal the summed fields exactly, so a consumer
	// may use either but must never add both.
	//
	// Bounded by maxUsageEventsPerSession via coalescing, never truncation:
	// tokens are preserved even when individual message boundaries are not.
	Usage []UsageEvent `json:"usage,omitempty"`

	// UsageCoalesced is how many raw per-message events were folded into
	// coarser buckets to keep Usage within maxUsageEventsPerSession. 0 means
	// the timeline is at full per-message fidelity. It is reported rather than
	// hidden because a silently-degraded timeline would make an interval join
	// look more precise than it is.
	UsageCoalesced int `json:"usage_coalesced,omitempty"`
}

// stripUsageTimelines returns a shallow copy of agg whose sessions carry no
// Usage timeline, for persistence only.
//
// The timeline is a LIVE-ONLY structure. It is rebuilt from the source session
// JSONL on every scan, so persisting it buys nothing on restart — but it would
// cost a great deal: up to maxUsageEventsPerSession events per Claude session
// at ~60-90 bytes of JSON each, re-marshalled and rewritten to
// /data/token-summary.json every scanInterval. On a hive with many sessions
// that turns a modest snapshot into a multi-megabyte write every 30 seconds,
// for data that is discarded and recomputed at the next scan anyway.
//
// UsageCoalesced is likewise dropped: it describes a timeline that is not
// present, and keeping it would imply a fidelity claim about nothing.
//
// The summed token fields are untouched, so a restored snapshot is exactly as
// complete as it was before the timeline existed. A restored session therefore
// has Backend set but Usage empty, which the repo-cost join treats as
// not-time-resolved and reports under backend_unsupported rather than
// silently dropping or misattributing its tokens unless its source can
// rebuild the timeline again (as live Copilot capture does).
func stripUsageTimelines(agg *AggregateSummary) *AggregateSummary {
	if agg == nil {
		return nil
	}
	out := *agg
	out.Sessions = make([]SessionSummary, len(agg.Sessions))
	copy(out.Sessions, agg.Sessions)
	for i := range out.Sessions {
		out.Sessions[i].Usage = nil
		out.Sessions[i].UsageCoalesced = 0
	}
	return &out
}

// UsageTotal sums the retained usage timeline. It exists so tests and consumers
// can assert the timeline reconciles against TotalTokens without duplicating the
// summation. Returns 0 for a session with no timeline.
func (s *SessionSummary) UsageTotal() int64 {
	var t int64
	for _, u := range s.Usage {
		t += u.Total()
	}
	return t
}

// AgentModelBucket holds per-agent or per-model token breakdown.
type AgentModelBucket struct {
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cache_read"`
	CacheCreate int64 `json:"cache_create"`
	Messages    int   `json:"messages"`
	Sessions    int   `json:"sessions"`
}

type AggregateSummary struct {
	TotalTokens      int64                        `json:"total_tokens"`
	TotalInput       int64                        `json:"total_input"`
	TotalOutput      int64                        `json:"total_output"`
	TotalCacheRead   int64                        `json:"total_cache_read"`
	TotalCacheCreate int64                        `json:"total_cache_create"`
	TotalMessages    int                          `json:"total_messages"`
	ByAgent          map[string]int64             `json:"by_agent"`
	ByModel          map[string]int64             `json:"by_model"`
	ByAgentDetail    map[string]*AgentModelBucket `json:"by_agent_detail"`
	ByModelDetail    map[string]*AgentModelBucket `json:"by_model_detail"`
	Sessions         []SessionSummary             `json:"sessions"`
	SessionCount     int                          `json:"session_count"`
}

// Diagnostics is the collector's non-secret explanation channel for consumers
// that need to distinguish "zero tokens because nothing used a model" from
// "zero tokens because metering itself is unhealthy".
type Diagnostics struct {
	LastScanError        string `json:"last_scan_error,omitempty"`
	LastClaudeScanError  string `json:"last_claude_scan_error,omitempty"`
	LastCopilotScanError string `json:"last_copilot_scan_error,omitempty"`
	LastBobScanError     string `json:"last_bob_scan_error,omitempty"`
	LiveCaptureEnabled   bool   `json:"live_capture_enabled,omitempty"`
}

const (
	defaultScanInterval = 30 * time.Second
)

// defaultPersistPath is the PVC location of the token summary snapshot.
// It is a var (not a const) only so tests can point it at a temp path;
// production always uses the fixed /data/token-summary.json location.
var defaultPersistPath = "/data/token-summary.json"

type Collector struct {
	sessionsDir               string
	claudeSessionsDir         string
	copilotSessionsDir        string
	bobSessionsDir            string
	copilotLiveCaptureSinceMs int64
	persistPath               string
	detector                  func(string) string
	logger                    *slog.Logger
	mu                        sync.RWMutex
	latest                    *AggregateSummary
	errorStreaks              map[string]int
	diagnostics               Diagnostics
	scanInterval              time.Duration
	prevSessionCount          int
	prevTotalTokens           int64
	prevByAgent               map[string]int64

	// loadOnce guards the one-time restore of the persisted snapshot. The
	// load is deferred until first read (see Summary) instead of happening in
	// NewCollector so callers may redirect defaultPersistPath or call
	// SetPersistPath after construction — otherwise the constructor eagerly
	// reads the live /data/token-summary.json on a hive host and tests see
	// production data instead of a clean initial state (#4585).
	loadOnce sync.Once
}

func NewCollector(sessionsDir string, logger *slog.Logger) *Collector {
	// A nil logger must not become a latent panic: loadSnapshot logs on the
	// first successful restore, which only happens on hosts where the snapshot
	// file exists — exactly the environments where a crash hurts most (#4664).
	if logger == nil {
		logger = slog.Default()
	}
	c := &Collector{
		sessionsDir:  sessionsDir,
		persistPath:  defaultPersistPath,
		detector:     DefaultAgentDetector,
		logger:       logger,
		scanInterval: defaultScanInterval,
		prevByAgent:  make(map[string]int64),
	}
	return c
}

// SetPersistPath overrides the default path for the token summary snapshot.
// Safe to call after construction and before first use: the persisted
// snapshot is loaded lazily on first Summary/scan, so the override wins even
// when set later than NewCollector (#4585).
func (c *Collector) SetPersistPath(path string) {
	c.persistPath = path
}

func (c *Collector) SetClaudeSessionsDir(dir string) {
	c.claudeSessionsDir = dir
}

func (c *Collector) SetCopilotSessionsDir(dir string) {
	c.copilotSessionsDir = dir
}

func (c *Collector) SetBobSessionsDir(dir string) {
	c.bobSessionsDir = dir
}

// SetCopilotLiveCapture tells the collector that the MITM proxy began recording
// Copilot token usage live (per completion) into the inference sink at
// sinceUnixMs. The copilot session-file scanner then defers token accrual to the
// sink ONLY for sessions that were still active at/after that moment (i.e. ones
// the proxy could actually have sniffed). Sessions that ended BEFORE live
// capture started were never seen by the sink, so the scanner must still count
// their session.shutdown ModelMetrics — otherwise all historical Copilot spend
// vanishes (the $18k→$394 regression). A sinceUnixMs of 0 disables the deferral
// entirely (always count shutdown tokens).
//
// Mutex-guarded so it can be called after Start (the scan goroutine reads it
// under the same lock).
func (c *Collector) SetCopilotLiveCapture(sinceUnixMs int64) {
	c.mu.Lock()
	c.copilotLiveCaptureSinceMs = sinceUnixMs
	c.diagnostics.LiveCaptureEnabled = sinceUnixMs > 0
	c.mu.Unlock()
}

// copilotLiveCaptureSince reads the live-capture start timestamp (ms), 0 if off.
func (c *Collector) copilotLiveCaptureSince() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.copilotLiveCaptureSinceMs
}

func (c *Collector) Start(stop <-chan struct{}) {
	c.scan()
	ticker := time.NewTicker(c.scanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			c.scan()
		}
	}
}

func (c *Collector) scan() {
	// Restore the persisted snapshot once, BEFORE the first scan overwrites
	// c.latest, preserving the original constructor-time load order (#4585).
	c.mu.Lock()
	c.loadOnce.Do(c.loadSnapshot)
	c.mu.Unlock()

	agg, err := CollectFromDir(c.sessionsDir, c.detector)
	if err != nil {
		c.logger.Warn("token scan failed", "error", err)
		c.mu.Lock()
		c.diagnostics.LastScanError = err.Error()
		c.mu.Unlock()
		return
	}
	diag := Diagnostics{LiveCaptureEnabled: c.copilotLiveCaptureSince() > 0}

	if c.claudeSessionsDir != "" {
		claudeAgg, err := ScanClaudeSessionsWithPathDetection(c.claudeSessionsDir)
		if err != nil {
			c.logger.Warn("claude session scan failed", "error", err)
			diag.LastClaudeScanError = err.Error()
		} else if claudeAgg != nil && claudeAgg.SessionCount > 0 {
			MergeAggregates(agg, claudeAgg)
		}
	}

	if c.copilotSessionsDir != "" {
		copilotAgg, err := ScanCopilotSessions(c.copilotSessionsDir, c.copilotLiveCaptureSince())
		if err != nil {
			c.logger.Warn("copilot session scan failed", "error", err)
			diag.LastCopilotScanError = err.Error()
		} else if copilotAgg != nil && copilotAgg.SessionCount > 0 {
			MergeAggregates(agg, copilotAgg)
		}
	}

	if c.bobSessionsDir != "" {
		bobAgg, err := ScanBobSessionsWithLogger(c.bobSessionsDir, c.logger)
		if err != nil {
			c.logger.Warn("bob session scan failed", "error", err)
			diag.LastBobScanError = err.Error()
		} else if bobAgg != nil && bobAgg.SessionCount > 0 {
			MergeAggregates(agg, bobAgg)
		}
		// Per-agent consecutive model-call failure streaks (#5577/#5338),
		// derived from the same recordings this scan just walked. Always
		// stored non-nil after a scan, so AgentErrorStreaks can distinguish
		// "measured, none failing" (empty) from "never scanned" (nil).
		streaks := BobAgentErrorStreaks(c.bobSessionsDir, time.Now())
		c.mu.Lock()
		c.errorStreaks = streaks
		c.mu.Unlock()
	}

	sessionDelta := agg.SessionCount - c.prevSessionCount
	tokenDelta := agg.TotalTokens - c.prevTotalTokens

	if sessionDelta != 0 || tokenDelta != 0 {
		attrs := []any{
			"sessions", agg.SessionCount,
			"session_delta", sessionDelta,
			"total_tokens", agg.TotalTokens,
			"token_delta", tokenDelta,
		}

		// Log per-agent token deltas for any agent whose count changed
		for agent, tokens := range agg.ByAgent {
			prev := c.prevByAgent[agent]
			if tokens != prev {
				attrs = append(attrs, "delta_"+agent, tokens-prev)
			}
		}

		c.logger.Info("token summary updated", attrs...)

		c.prevSessionCount = agg.SessionCount
		c.prevTotalTokens = agg.TotalTokens
		c.prevByAgent = make(map[string]int64, len(agg.ByAgent))
		for k, v := range agg.ByAgent {
			c.prevByAgent[k] = v
		}
	}

	c.mu.Lock()
	c.latest = agg
	c.diagnostics = diag
	c.mu.Unlock()

	c.saveSnapshot(agg)
}

func (c *Collector) Diagnostics() Diagnostics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.diagnostics
}

func (c *Collector) Summary() *AggregateSummary {
	// Restore the persisted snapshot once, on first read, under the write
	// lock so a concurrent reader never observes the load mid-write.
	c.mu.Lock()
	c.loadOnce.Do(c.loadSnapshot)
	c.mu.Unlock()

	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latest
}

// AgentErrorStreaks returns the per-agent consecutive model-call failure
// streaks computed on the last scan (see BobAgentErrorStreaks). nil until a
// scan of a configured bob sessions dir has completed — callers (the
// heartbeat) forward that nil as "not measured", never as "no failures".
// The returned map is a copy.
func (c *Collector) AgentErrorStreaks() map[string]int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.errorStreaks == nil {
		return nil
	}
	out := make(map[string]int, len(c.errorStreaks))
	for k, v := range c.errorStreaks {
		out[k] = v
	}
	return out
}

func CollectFromDir(sessionsDir string, agentDetector func(firstMsg string) string) (*AggregateSummary, error) {
	pattern := filepath.Join(sessionsDir, "*.jsonl")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("globbing session files: %w", err)
	}

	agg := &AggregateSummary{
		ByAgent:       make(map[string]int64),
		ByModel:       make(map[string]int64),
		ByAgentDetail: make(map[string]*AgentModelBucket),
		ByModelDetail: make(map[string]*AgentModelBucket),
	}

	for _, file := range files {
		summary, err := parseSessionFile(file, agentDetector)
		if err != nil {
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

func parseSessionFile(path string, agentDetector func(string) string) (*SessionSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only fd; nothing to lose on close error

	sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	summary := &SessionSummary{
		SessionID: sessionID,
		Agent:     "unknown",
	}

	scanner := bufio.NewScanner(f)
	const maxScanBufSize = 10 * 1024 * 1024
	scanner.Buffer(make([]byte, 0, maxScanBufSize), maxScanBufSize)

	firstUserMsg := ""
	explicitAgent := ""
	explicitBackend := ""
	var timeline usageTimeline
	// FirstActive/LastActive are the min/max parseable entry timestamps, not
	// the first/last line seen: flat-format files are append-mostly but not
	// guaranteed ordered (atomic rewrites and merged records can interleave),
	// so line position is not a reliable recency signal. Unparseable or absent
	// timestamps contribute nothing, leaving 0 when no entry carries one.
	var firstTimestamp, lastTimestamp int64
	for scanner.Scan() {
		var entry SessionEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}

		if ts := parseTimestampToUnixMilli(entry.Timestamp); ts > 0 {
			if ts > lastTimestamp {
				lastTimestamp = ts
			}
			if firstTimestamp == 0 || ts < firstTimestamp {
				firstTimestamp = ts
			}
		}

		if entry.Agent != "" && explicitAgent == "" {
			explicitAgent = entry.Agent
		}
		if entry.Backend != "" && explicitBackend == "" {
			explicitBackend = entry.Backend
		}

		if entry.Role == "user" && firstUserMsg == "" {
			firstUserMsg = entry.Message
		}

		if entry.Model != "" && summary.Model == "" {
			summary.Model = entry.Model
		}

		summary.InputTokens += entry.InputTokens
		summary.OutputTokens += entry.OutputTokens
		summary.CacheRead += entry.CacheRead
		summary.CacheCreate += entry.CacheCreation
		if entry.InputTokens > 0 || entry.OutputTokens > 0 || entry.CacheRead > 0 || entry.CacheCreation > 0 {
			timeline.add(UsageEvent{
				TimestampMs: parseTimestampToUnixMilli(entry.Timestamp),
				Model:       entry.Model,
				Coalesced:   entry.Coalesced,
				Input:       entry.InputTokens,
				Output:      entry.OutputTokens,
				CacheRead:   entry.CacheRead,
				CacheCreate: entry.CacheCreation,
			})
		}

		if entry.Role == "user" || entry.Role == "assistant" {
			summary.Messages++
		}
	}

	summary.TotalTokens = summary.InputTokens + summary.OutputTokens + summary.CacheRead + summary.CacheCreate
	summary.FirstActive = firstTimestamp
	summary.LastActive = lastTimestamp
	summary.Usage, summary.UsageCoalesced = timeline.finish()
	if explicitBackend != "" {
		summary.Backend = explicitBackend
	} else {
		base := filepath.Base(path)
		switch {
		case strings.HasPrefix(base, inferenceSessionFilePrefix):
			summary.Backend = BackendInference
		case strings.HasPrefix(base, copilotLiveSessionFilePrefix):
			summary.Backend = BackendCopilot
		}
	}

	if explicitAgent != "" {
		summary.Agent = explicitAgent
	} else if agentDetector != nil && firstUserMsg != "" {
		summary.Agent = agentDetector(firstUserMsg)
	}

	return summary, nil
}

func (c *Collector) loadSnapshot() {
	if c.persistPath == "" {
		return
	}
	data, err := os.ReadFile(c.persistPath)
	if err != nil {
		return
	}
	var agg AggregateSummary
	if err := json.Unmarshal(data, &agg); err != nil {
		c.logger.Warn("failed to parse token snapshot", "path", c.persistPath, "error", err)
		return
	}
	if agg.ByAgent == nil {
		agg.ByAgent = make(map[string]int64)
	}
	if agg.ByModel == nil {
		agg.ByModel = make(map[string]int64)
	}
	if agg.ByAgentDetail == nil {
		agg.ByAgentDetail = make(map[string]*AgentModelBucket)
	}
	if agg.ByModelDetail == nil {
		agg.ByModelDetail = make(map[string]*AgentModelBucket)
	}
	c.latest = &agg
	c.logger.Info("loaded token snapshot", "path", c.persistPath, "sessions", agg.SessionCount, "total_tokens", agg.TotalTokens)
}

func (c *Collector) saveSnapshot(agg *AggregateSummary) {
	if c.persistPath == "" || agg == nil {
		return
	}
	data, err := json.Marshal(stripUsageTimelines(agg))
	if err != nil {
		c.logger.Warn("failed to marshal token snapshot", "error", err)
		return
	}
	tmpPath := c.persistPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		c.logger.Warn("failed to write token snapshot", "path", tmpPath, "error", err)
		return
	}
	if err := os.Rename(tmpPath, c.persistPath); err != nil {
		c.logger.Warn("failed to rename token snapshot", "error", err)
	}
}

var detectMu sync.RWMutex
var configuredDetectKeywords map[string][]string
var configuredAgentNames []string

// SetDetectKeywords sets the agent detection keyword map from config.
func SetDetectKeywords(keywords map[string][]string) {
	detectMu.Lock()
	defer detectMu.Unlock()
	configuredDetectKeywords = keywords
}

// SetAgentNames sets the list of known agent names from config.
func SetAgentNames(names []string) {
	detectMu.Lock()
	defer detectMu.Unlock()
	configuredAgentNames = names
}

// ConfiguredAgentNames returns the list of configured agent names.
func ConfiguredAgentNames() []string {
	detectMu.RLock()
	defer detectMu.RUnlock()
	if len(configuredAgentNames) > 0 {
		return configuredAgentNames
	}
	return []string{"scanner", "ci-maintainer", "architect", "outreach", "supervisor", "sec-check", "quality", "analyst"}
}

var defaultDetectKeywords = map[string][]string{
	"scanner":       {"scanner", "triage", "issue", "bug"},
	"ci-maintainer": {"ci-maintainer", "review", "ci", "coverage", "ga4"},
	"architect":     {"architect", "rfc", "refactor"},
	"outreach":      {"outreach", "adopters", "community"},
	"supervisor":    {"supervisor", "sweep", "monitor"},
	"sec-check":     {"security", "sec-check", "vulnerability"},
}

func DefaultAgentDetector(firstMsg string) string {
	lower := strings.ToLower(firstMsg)

	detectMu.RLock()
	agents := configuredDetectKeywords
	detectMu.RUnlock()
	if len(agents) == 0 {
		agents = defaultDetectKeywords
	}

	for agent, keywords := range agents {
		for _, kw := range keywords {
			if strings.Contains(lower, kw) {
				return agent
			}
		}
	}
	return "unknown"
}
