package tokens

import (
	"testing"
	"time"
)

// TestInferenceSinkCopilotHelperModelDoesNotRelabelAgent covers the Copilot
// CLI's gpt-4o-mini helper calls interleaved with the agent's Claude calls:
// each call keeps its own model and cache columns, and the agent's pinned model
// stays the one carrying its spend, across a restart too.
func TestInferenceSinkCopilotHelperModelDoesNotRelabelAgent(t *testing.T) {
	dir := t.TempDir()
	sink := NewInferenceSink(dir, nil)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	current := t0
	sink.now = func() time.Time { return current }

	sink.RecordCopilotWithCache("scanner", "claude-sonnet-4.5", 12, 57, 4000, 300)
	current = t0.Add(time.Minute)
	sink.RecordCopilot("scanner", "gpt-4o-mini", 30, 2)

	check := func(s *InferenceSink, label string) {
		t.Helper()
		s.mu.Lock()
		tot := s.totals[sinkTotalsKey(BackendCopilot, "scanner")]
		s.mu.Unlock()
		if tot == nil {
			t.Fatalf("%s: no totals", label)
		}
		if tot.Model != "claude-sonnet-4.5" {
			t.Errorf("%s: pinned model = %q, want claude-sonnet-4.5", label, tot.Model)
		}
		byModel := map[string]UsageEvent{}
		for _, u := range tot.Usage {
			e := byModel[u.Model]
			e.Input += u.Input
			e.Output += u.Output
			e.CacheRead += u.CacheRead
			e.CacheCreate += u.CacheCreate
			byModel[u.Model] = e
		}
		if got := byModel["claude-sonnet-4.5"]; got.Input != 12 || got.Output != 57 || got.CacheRead != 4000 || got.CacheCreate != 300 {
			t.Errorf("%s: claude usage = %+v, want 12/57/4000/300", label, got)
		}
		if got := byModel["gpt-4o-mini"]; got.Input != 30 || got.Output != 2 || got.CacheRead != 0 {
			t.Errorf("%s: gpt-4o-mini usage = %+v, want 30/2/0", label, got)
		}
	}
	check(sink, "live")
	check(NewInferenceSink(dir, nil), "restored")

	agg, err := CollectFromDir(dir, nil)
	if err != nil {
		t.Fatalf("CollectFromDir: %v", err)
	}
	if len(agg.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(agg.Sessions))
	}
	if want := int64(12 + 57 + 4000 + 300 + 30 + 2); agg.Sessions[0].TotalTokens != want {
		t.Errorf("TotalTokens = %d, want %d", agg.Sessions[0].TotalTokens, want)
	}
}

func TestInferenceSinkRecordCacheOnly(t *testing.T) {
	dir := t.TempDir()
	sink := NewInferenceSink(dir, nil)
	sink.RecordCopilotWithCache("scanner", "claude-sonnet-4.5", 0, 0, 500, 0)
	sink.mu.Lock()
	tot := sink.totals[sinkTotalsKey(BackendCopilot, "scanner")]
	sink.mu.Unlock()
	if tot == nil || len(tot.Usage) != 1 || tot.Usage[0].CacheRead != 500 {
		t.Fatalf("cache-only usage not recorded: %+v", tot)
	}
}
