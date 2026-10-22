package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/tokens"
)

// tokenCollectorWithSummary returns a Collector whose first Summary() is the
// given aggregate, restored from a persisted snapshot (no sessions dir scan).
func tokenCollectorWithSummary(t *testing.T, agg tokens.AggregateSummary) *tokens.Collector {
	t.Helper()
	data, err := json.Marshal(agg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "token-summary.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	c := tokens.NewCollector(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetPersistPath(path)
	return c
}

// #13483: the cost history must be sampled by the server itself, not only
// when a browser polls /api/knowledge/stats.
func TestRunCostSamplerRecordsWithoutViewers(t *testing.T) {
	s := newTestServer()
	s.deps = &Dependencies{Tokens: tokenCollectorWithSummary(t, tokens.AggregateSummary{
		ByModelDetail: map[string]*tokens.AgentModelBucket{
			"claude-sonnet-4-5": {Input: 1_000_000, Output: 100_000},
		},
		ByAgentDetail: map[string]*tokens.AgentModelBucket{
			"scanner": {Input: 1_000_000, Output: 100_000},
		},
		ByAgent: map[string]int64{"scanner": 1_100_000},
		ByModel: map[string]int64{"claude-sonnet-4-5": 1_100_000},
		Sessions: []tokens.SessionSummary{{
			SessionID: "s1", Agent: "scanner", Model: "claude-sonnet-4-5",
			InputTokens: 1_000_000, OutputTokens: 100_000, TotalTokens: 1_100_000,
		}},
		SessionCount: 1,
	})}

	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		s.runCostSampler(ticks)
		close(done)
	}()
	ticks <- time.Now()
	ticks <- time.Now() // inside the 5-min throttle: must be a no-op
	close(ticks)
	<-done

	hist := s.CostHistory()
	if len(hist) != 1 {
		t.Fatalf("want exactly one snapshot per throttle window, got %d", len(hist))
	}
	// 1M input × $3 + 100K output × $15 at the sonnet-4-5 list price.
	if want := 4.5; hist[0].USD < want-0.01 || hist[0].USD > want+0.01 {
		t.Errorf("usd = %v, want ≈ %v", hist[0].USD, want)
	}
	if hist[0].Models["claude-sonnet-4-5"].Input != 1_000_000 {
		t.Errorf("per-model snapshot missing: %+v", hist[0].Models)
	}
	if hist[0].Agents["scanner"] <= 0 {
		t.Errorf("per-agent snapshot missing: %+v", hist[0].Agents)
	}
}

// Before the collector has anything to report the sampler must not write a
// $0 point: the headline's lineage detector would read it as a reset to zero.
func TestSampleCostHistorySkipsWhenNoSummary(t *testing.T) {
	s := newTestServer()
	if s.sampleCostHistory() {
		t.Fatal("nil deps: sampled")
	}
	s.deps = &Dependencies{Tokens: tokens.NewCollector(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))}
	if s.sampleCostHistory() {
		t.Fatal("collector without a summary: sampled")
	}
	if n := len(s.CostHistory()); n != 0 {
		t.Fatalf("history has %d entries, want 0", n)
	}
}
