package advisor

import (
	"sort"
	"time"
)

// AgentSpend is one agent's aggregated advisor spend over a window
// (hivecommons/hive#9725, phase 4 of #9638): what the advisor lane cost to
// review that agent's turns, reported beside the agent's own spend. CostUSD
// is the plain sum of the CostUSD figures on the agent's records in the
// window, so the aggregate always agrees with the record listing.
type AgentSpend struct {
	Agent        string  `json:"agent"`
	Reviews      int     `json:"reviews"`
	Skipped      int     `json:"skipped"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// inWindow reports whether a record's timestamp falls in [since, until]. A
// zero bound is open. A record whose timestamp does not parse is only kept
// when both bounds are open, matching List's since-filter posture.
func inWindow(rec Record, since, until time.Time) bool {
	if since.IsZero() && until.IsZero() {
		return true
	}
	ts, err := time.Parse(time.RFC3339, rec.Timestamp)
	if err != nil {
		return false
	}
	if !since.IsZero() && ts.Before(since) {
		return false
	}
	if !until.IsZero() && ts.After(until) {
		return false
	}
	return true
}

// ListWindow returns records newest first, filtered by agent (empty = all)
// and by the closed window [since, until] (a zero bound is open), capped at
// limit (<=0 = all retained).
func (s *Store) ListWindow(agent string, since, until time.Time, limit int) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.ring))
	for i := len(s.ring) - 1; i >= 0; i-- {
		rec := s.ring[i]
		if agent != "" && rec.Agent != agent {
			continue
		}
		if !inWindow(rec, since, until) {
			continue
		}
		out = append(out, rec)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// SpendByAgent aggregates the retained records in [since, until] (a zero
// bound is open) per agent, sorted by agent name. Skipped reviews count
// toward Reviews and Skipped and contribute whatever cost they recorded
// (zero for a review that never reached the model).
func (s *Store) SpendByAgent(since, until time.Time) []AgentSpend {
	s.mu.Lock()
	byAgent := map[string]*AgentSpend{}
	for _, rec := range s.ring {
		if !inWindow(rec, since, until) {
			continue
		}
		sp, ok := byAgent[rec.Agent]
		if !ok {
			sp = &AgentSpend{Agent: rec.Agent}
			byAgent[rec.Agent] = sp
		}
		sp.Reviews++
		if rec.Skipped != "" {
			sp.Skipped++
		}
		sp.InputTokens += rec.InputTokens
		sp.OutputTokens += rec.OutputTokens
		sp.CostUSD += rec.CostUSD
	}
	s.mu.Unlock()
	out := make([]AgentSpend, 0, len(byAgent))
	for _, sp := range byAgent {
		out = append(out, *sp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out
}
