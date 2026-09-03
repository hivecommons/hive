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

// inferenceSessionFilePrefix is prepended to per-agent inference usage files
// written under the metrics dir. The token Collector globs "*.jsonl" in that
// dir, so these files are picked up by the normal scan without any extra
// wiring. The prefix keeps them visually distinct from CLI session files.
const inferenceSessionFilePrefix = "inference-"

// copilotLiveSessionFilePrefix is used for Copilot completions captured by the
// MITM proxy. Keeping them in their own collectable session file prevents them
// from being merged with bare-mode inference totals for the same agent.
const copilotLiveSessionFilePrefix = "copilot-live-"

// inferenceFilePerm is the mode for the per-agent inference usage files.
const inferenceFilePerm = 0o644

// InferenceSink records token usage for bare-mode inference agents (litellm /
// vllm / llm-d) that run `claude --bare` against the translator and therefore
// never write a Claude session transcript for the collector to scan.
//
// The translator calls Record after each upstream response with the OpenAI
// usage block. The sink keeps a cumulative per-agent counter in memory and
// mirrors it to one atomically-rewritten "*.jsonl" file per agent under the
// metrics dir. The existing token Collector globs that dir every scan, so
// inference usage flows into the same AggregateSummary the dashboard, the
// governor budget, and the hub heartbeat all read — no parallel counter.
type InferenceSink struct {
	dir    string
	logger *slog.Logger
	// now is a clock seam for tests; production uses time.Now.
	now func() time.Time

	mu     sync.Mutex
	totals map[string]*inferenceAgentTotals
}

type inferenceAgentTotals struct {
	Model        string
	InputTokens  int64
	OutputTokens int64
	// FirstSeen/LastSeen bracket the agent's recorded activity as RFC 3339
	// strings, written into the usage file's entry timestamps so the
	// Collector's FirstActive/LastActive come out nonzero for inference
	// sessions. FirstSeen survives restarts via restoreFromDisk.
	FirstSeen string
	LastSeen  string
	Backend   string
	Usage     []UsageEvent
}

// NewInferenceSink returns a sink that writes per-agent usage files into dir.
// dir is created if missing. A nil sink (dir == "") is a safe no-op.
//
// On construction the sink reloads any existing per-agent totals from the
// inference-<agent>.jsonl files already on disk, so cumulative counts survive a
// hive restart. Without this, an in-memory-only sink would restart every agent's
// counter at zero and overwrite the persisted file with a lower value on the
// next Record — silently losing all pre-restart tokens (this affected inference
// agents and would double-hurt the Copilot live path, whose only durable record
// is this file once the session-shutdown scanner defers to the sink).
func NewInferenceSink(dir string, logger *slog.Logger) *InferenceSink {
	s := &InferenceSink{
		dir:    dir,
		logger: logger,
		now:    time.Now,
		totals: make(map[string]*inferenceAgentTotals),
	}
	s.restoreFromDisk()
	return s
}

// restoreFromDisk rebuilds the in-memory per-agent totals from the persisted
// inference-<agent>.jsonl files. Best-effort: unreadable or malformed files are
// skipped. Safe to call before any goroutines run (only invoked from the
// constructor).
func (s *InferenceSink) restoreFromDisk() {
	if s == nil || s.dir == "" {
		return
	}
	for _, prefix := range []string{inferenceSessionFilePrefix, copilotLiveSessionFilePrefix} {
		matches, err := filepath.Glob(filepath.Join(s.dir, prefix+"*.jsonl"))
		if err != nil {
			continue
		}
		for _, path := range matches {
			base := filepath.Base(path)
			agent := strings.TrimSuffix(strings.TrimPrefix(base, prefix), ".jsonl")
			if agent == "" {
				continue
			}
			t := s.parseAgentFile(path)
			if t != nil {
				if t.Backend == "" {
					t.Backend = sinkBackendFromPrefix(prefix)
				}
				s.totals[sinkTotalsKey(t.Backend, agent)] = t
			}
		}
	}
}

// parseAgentFile reads a persisted inference usage file back into totals. The
// file holds a "user" pin line and an "assistant" line carrying the cumulative
// InputTokens/OutputTokens/Model. Returns nil on any read/parse failure.
func (s *InferenceSink) parseAgentFile(path string) *inferenceAgentTotals {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }() // read-only fd; nothing to lose on close error

	t := &inferenceAgentTotals{}
	found := false
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var e SessionEntry
		if json.Unmarshal(scanner.Bytes(), &e) != nil {
			continue
		}
		if e.Model != "" {
			t.Model = e.Model
		}
		if e.Backend != "" {
			t.Backend = e.Backend
		}
		if ms := parseTimestampToUnixMilli(e.Timestamp); ms > 0 {
			if t.FirstSeen == "" || ms < parseTimestampToUnixMilli(t.FirstSeen) {
				t.FirstSeen = e.Timestamp
			}
			if t.LastSeen == "" || ms > parseTimestampToUnixMilli(t.LastSeen) {
				t.LastSeen = e.Timestamp
			}
		}
		if e.InputTokens > 0 || e.OutputTokens > 0 || e.CacheRead > 0 || e.CacheCreation > 0 {
			t.InputTokens += e.InputTokens
			t.OutputTokens += e.OutputTokens
			t.Usage = append(t.Usage, UsageEvent{
				TimestampMs: parseTimestampToUnixMilli(e.Timestamp),
				Model:       e.Model,
				Coalesced:   e.Coalesced,
				Input:       e.InputTokens,
				Output:      e.OutputTokens,
				CacheRead:   e.CacheRead,
				CacheCreate: e.CacheCreation,
			})
			found = true
		}
	}
	if !found {
		return nil
	}
	return t
}

// Record adds one response's token usage for agent to the running total and
// rewrites that agent's usage file. inputTokens/outputTokens come from the
// gateway's OpenAI "usage" block (prompt_tokens / completion_tokens). It is a
// no-op when the sink is disabled, the agent name is empty, or the usage is
// non-positive, so callers can invoke it unconditionally.
func (s *InferenceSink) Record(agent, model string, inputTokens, outputTokens int64) {
	s.record(agent, model, BackendInference, inputTokens, outputTokens)
}

// RecordCopilot adds one Copilot completion captured live by the MITM proxy.
// Unlike Copilot session.shutdown metrics, each call has a real wall-clock
// timestamp, so the collector persists a Usage timeline that /api/repo-cost can
// join against audited repo activity.
func (s *InferenceSink) RecordCopilot(agent, model string, inputTokens, outputTokens int64) {
	s.record(agent, model, BackendCopilot, inputTokens, outputTokens)
}

func (s *InferenceSink) record(agent, model, backend string, inputTokens, outputTokens int64) {
	if s == nil || s.dir == "" || agent == "" {
		return
	}
	if inputTokens <= 0 && outputTokens <= 0 {
		return
	}

	s.mu.Lock()
	key := sinkTotalsKey(backend, agent)
	t, ok := s.totals[key]
	if !ok {
		t = &inferenceAgentTotals{Backend: backend}
		s.totals[key] = t
	}
	if model != "" {
		t.Model = model
	}
	nowFn := s.now
	if nowFn == nil {
		nowFn = time.Now
	}
	stamp := nowFn().UTC().Format(time.RFC3339)
	if t.FirstSeen == "" {
		t.FirstSeen = stamp
	}
	t.LastSeen = stamp
	if inputTokens > 0 {
		t.InputTokens += inputTokens
	}
	if outputTokens > 0 {
		t.OutputTokens += outputTokens
	}
	timeline := usageTimeline{events: append([]UsageEvent(nil), t.Usage...)}
	timeline.add(UsageEvent{
		TimestampMs: parseTimestampToUnixMilli(stamp),
		Model:       model,
		Input:       inputTokens,
		Output:      outputTokens,
	})
	t.Usage, _ = timeline.finish()
	snapshot := *t
	snapshot.Usage = append([]UsageEvent(nil), t.Usage...)
	s.mu.Unlock()

	if err := s.writeAgentFile(agent, &snapshot); err != nil && s.logger != nil {
		// Never log token values as sensitive; count magnitudes are fine.
		s.logger.Warn("inference token sink write failed", "agent", agent, "error", err)
	}
}

// writeAgentFile atomically rewrites the cumulative usage file for one agent.
// The file is a single-session ".jsonl" whose session ID is stable (the agent
// name), so the Collector's dedup-by-session-ID replaces rather than sums
// across scans. The first line pins the agent explicitly via SessionEntry.Agent
// so attribution does not depend on keyword detection.
func (s *InferenceSink) writeAgentFile(agent string, t *inferenceAgentTotals) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("creating metrics dir: %w", err)
	}

	backend := t.Backend
	if backend == "" {
		backend = BackendInference
	}
	entries := []SessionEntry{
		{Role: "user", Agent: agent, Message: agent, Model: t.Model, Timestamp: t.FirstSeen, Backend: backend},
	}
	if len(t.Usage) == 0 {
		entries = append(entries, SessionEntry{
			Role:         "assistant",
			Agent:        agent,
			Model:        t.Model,
			InputTokens:  t.InputTokens,
			OutputTokens: t.OutputTokens,
			Timestamp:    t.LastSeen,
			Backend:      backend,
		})
	} else {
		for _, u := range t.Usage {
			ts := t.LastSeen
			if u.TimestampMs > 0 {
				ts = time.UnixMilli(u.TimestampMs).UTC().Format(time.RFC3339)
			}
			model := u.Model
			if model == "" {
				model = t.Model
			}
			entries = append(entries, SessionEntry{
				Role:          "assistant",
				Agent:         agent,
				Model:         model,
				InputTokens:   u.Input,
				OutputTokens:  u.Output,
				CacheRead:     u.CacheRead,
				CacheCreation: u.CacheCreate,
				Timestamp:     ts,
				Backend:       backend,
				Coalesced:     u.Coalesced,
			})
		}
	}

	var buf []byte
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("marshaling usage entry: %w", err)
		}
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}

	path := filepath.Join(s.dir, sinkFilePrefix(backend)+agent+".jsonl")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, inferenceFilePerm); err != nil {
		return fmt.Errorf("writing usage file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("renaming usage file: %w", err)
	}
	return nil
}

func sinkTotalsKey(backend, agent string) string {
	if backend == "" {
		backend = BackendInference
	}
	return backend + "\x00" + agent
}

func sinkFilePrefix(backend string) string {
	if backend == BackendCopilot {
		return copilotLiveSessionFilePrefix
	}
	return inferenceSessionFilePrefix
}

func sinkBackendFromPrefix(prefix string) string {
	if prefix == copilotLiveSessionFilePrefix {
		return BackendCopilot
	}
	return BackendInference
}
