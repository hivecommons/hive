package tokens

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// inferenceSessionFilePrefix is prepended to per-agent inference usage files
// written under the metrics dir. The token Collector globs "*.jsonl" in that
// dir, so these files are picked up by the normal scan without any extra
// wiring. The prefix keeps them visually distinct from CLI session files.
const inferenceSessionFilePrefix = "inference-"

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

	mu     sync.Mutex
	totals map[string]*inferenceAgentTotals
}

type inferenceAgentTotals struct {
	Model        string
	InputTokens  int64
	OutputTokens int64
}

// NewInferenceSink returns a sink that writes per-agent usage files into dir.
// dir is created if missing. A nil sink (dir == "") is a safe no-op.
func NewInferenceSink(dir string, logger *slog.Logger) *InferenceSink {
	return &InferenceSink{
		dir:    dir,
		logger: logger,
		totals: make(map[string]*inferenceAgentTotals),
	}
}

// Record adds one response's token usage for agent to the running total and
// rewrites that agent's usage file. inputTokens/outputTokens come from the
// gateway's OpenAI "usage" block (prompt_tokens / completion_tokens). It is a
// no-op when the sink is disabled, the agent name is empty, or the usage is
// non-positive, so callers can invoke it unconditionally.
func (s *InferenceSink) Record(agent, model string, inputTokens, outputTokens int64) {
	if s == nil || s.dir == "" || agent == "" {
		return
	}
	if inputTokens <= 0 && outputTokens <= 0 {
		return
	}

	s.mu.Lock()
	t, ok := s.totals[agent]
	if !ok {
		t = &inferenceAgentTotals{}
		s.totals[agent] = t
	}
	if model != "" {
		t.Model = model
	}
	if inputTokens > 0 {
		t.InputTokens += inputTokens
	}
	if outputTokens > 0 {
		t.OutputTokens += outputTokens
	}
	snapshot := *t
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

	entries := []SessionEntry{
		{Role: "user", Agent: agent, Message: agent, Model: t.Model},
		{
			Role:         "assistant",
			Agent:        agent,
			Model:        t.Model,
			InputTokens:  t.InputTokens,
			OutputTokens: t.OutputTokens,
		},
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

	path := filepath.Join(s.dir, inferenceSessionFilePrefix+agent+".jsonl")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, inferenceFilePerm); err != nil {
		return fmt.Errorf("writing usage file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("renaming usage file: %w", err)
	}
	return nil
}
