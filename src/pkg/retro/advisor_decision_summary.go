package retro

import (
	"time"
)

// AdvisorDecisionSummary is the aggregate of the knowledge-advisor decision
// log: counts and means only. It never carries a slug, ref, agent or kick id
// (hivecommons/hive#18897).
type AdvisorDecisionSummary struct {
	// Total is the number of decisions in the window.
	Total int
	// FirstDecision and LastDecision are the earliest and latest decision
	// timestamps in the window (RFC 3339), empty when there are none.
	FirstDecision string
	LastDecision  string
	// RankModeStart is the earliest decision made in rank mode, empty when none.
	RankModeStart string
	ByMode        map[string]int
	ByArm         map[string]int
	ByOutcome     map[string]int
	// LatencySamples counts the decisions that recorded a model-call latency;
	// MeanLatencyMS is their mean.
	LatencySamples int
	MeanLatencyMS  float64
	// TokenSamples counts the decisions that recorded token usage.
	TokenSamples         int
	MeanPromptTokens     float64
	MeanCompletionTokens float64
}

// Summarize aggregates the stored decisions at or after since (a zero since
// takes them all). Records whose timestamp does not parse are counted only
// when since is zero.
func (s *AdvisorDecisionStore) Summarize(since time.Time) AdvisorDecisionSummary {
	out := AdvisorDecisionSummary{ByMode: map[string]int{}, ByArm: map[string]int{}, ByOutcome: map[string]int{}}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var latSum int64
	var promptSum, completionSum int
	var first, last, rank time.Time
	for _, rec := range s.ring {
		ts, err := time.Parse(time.RFC3339, rec.Timestamp)
		if err != nil {
			if !since.IsZero() {
				continue
			}
		} else if ts.Before(since) {
			continue
		}
		out.Total++
		out.ByMode[rec.Mode]++
		out.ByArm[rec.Arm]++
		out.ByOutcome[rec.Outcome]++
		if rec.LatencyMS > 0 {
			out.LatencySamples++
			latSum += rec.LatencyMS
		}
		if rec.PromptTokens > 0 || rec.CompletionTokens > 0 {
			out.TokenSamples++
			promptSum += rec.PromptTokens
			completionSum += rec.CompletionTokens
		}
		if err != nil {
			continue
		}
		if first.IsZero() || ts.Before(first) {
			first = ts
		}
		if ts.After(last) {
			last = ts
		}
		if rec.Mode == "rank" && (rank.IsZero() || ts.Before(rank)) {
			rank = ts
		}
	}
	if out.LatencySamples > 0 {
		out.MeanLatencyMS = float64(latSum) / float64(out.LatencySamples)
	}
	if out.TokenSamples > 0 {
		out.MeanPromptTokens = float64(promptSum) / float64(out.TokenSamples)
		out.MeanCompletionTokens = float64(completionSum) / float64(out.TokenSamples)
	}
	out.FirstDecision = formatAdvisorTime(first)
	out.LastDecision = formatAdvisorTime(last)
	out.RankModeStart = formatAdvisorTime(rank)
	return out
}

func formatAdvisorTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
