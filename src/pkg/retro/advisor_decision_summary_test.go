package retro

import (
	"reflect"
	"testing"
	"time"
)

func TestAdvisorDecisionStoreSummarize(t *testing.T) {
	var nilStore *AdvisorDecisionStore
	if got := nilStore.Summarize(time.Time{}); got.Total != 0 || got.ByArm == nil {
		t.Fatalf("nil store = %+v", got)
	}
	s := NewAdvisorDecisionStore("")
	s.Record(AdvisorDecisionRecord{Timestamp: "2026-11-01T00:00:00Z", Agent: "a", Kick: "k1", Mode: "shadow", Outcome: "gated"})
	s.Record(AdvisorDecisionRecord{Timestamp: "2026-11-02T00:00:00Z", Agent: "a", Kick: "k2", Mode: "rank", Outcome: "held-out", LatencyMS: 100, PromptTokens: 10, CompletionTokens: 2})
	s.Record(AdvisorDecisionRecord{Timestamp: "2026-11-03T00:00:00Z", Agent: "a", Kick: "k3", Mode: "rank", Outcome: "delivered", LatencyMS: 300, PromptTokens: 30, CompletionTokens: 4})
	s.Record(AdvisorDecisionRecord{Timestamp: "not-a-time", Agent: "a", Kick: "k4", Mode: "rank", Outcome: "delivered"})

	all := s.Summarize(time.Time{})
	if all.Total != 4 || all.FirstDecision != "2026-11-01T00:00:00Z" || all.LastDecision != "2026-11-03T00:00:00Z" || all.RankModeStart != "2026-11-02T00:00:00Z" {
		t.Fatalf("all = %+v", all)
	}
	if all.LatencySamples != 2 || all.MeanLatencyMS != 200 || all.TokenSamples != 2 || all.MeanPromptTokens != 20 || all.MeanCompletionTokens != 3 {
		t.Fatalf("cost = %+v", all)
	}
	if !reflect.DeepEqual(all.ByOutcome, map[string]int{"gated": 1, "held-out": 1, "delivered": 2}) {
		t.Fatalf("by outcome = %v", all.ByOutcome)
	}

	since := s.Summarize(time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC))
	if since.Total != 1 || since.ByMode["rank"] != 1 || since.FirstDecision != "2026-11-03T00:00:00Z" {
		t.Fatalf("since = %+v", since)
	}
}
