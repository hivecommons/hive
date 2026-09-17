// Package hiveadvisor computes ranked, mode-aware operational advice for a
// hive owner.
//
// The package is advisory-only and pure: it performs no I/O, reads no clocks,
// and uses no randomness. Callers pass every signal in, including the current
// time and any previously-published epoch, so dashboard and digest surfaces can
// share one frozen weekly recommendation set.
package hiveadvisor

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/acmmadvisor"
)

const (
	DefaultTopN = 3
	EpochLength = 7 * 24 * time.Hour
)

type Signal struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Recommendation struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Rationale string   `json:"rationale"`
	Signals   []Signal `json:"signals,omitempty"`
	Score     int      `json:"score"`
}

type Signals struct {
	Mode                string
	QueueIssues         int
	QueuePRs            int
	HoldCount           int
	RepoCount           int
	DisabledAgentCount  int
	NoCadenceAgentCount int
	BudgetUsedPct       float64
	BudgetExhausted     bool
	ACMM                acmmadvisor.Recommendation
}

type Epoch struct {
	Start           time.Time        `json:"start"`
	End             time.Time        `json:"end"`
	Mode            string           `json:"mode"`
	Recommendations []Recommendation `json:"recommendations"`
}

type Result struct {
	Epoch            Epoch            `json:"epoch"`
	Recommendations  []Recommendation `json:"recommendations"`
	NextReviewInDays int              `json:"nextReviewInDays"`
	Frozen           bool             `json:"frozen"`
}

type Request struct {
	Now      time.Time
	TopN     int
	Signals  Signals
	Previous *Epoch
}

func Recommend(req Request) Result {
	now := req.Now
	if now.IsZero() {
		now = time.Unix(0, 0).UTC()
	}
	mode := normalizeMode(req.Signals.Mode)
	if req.TopN <= 0 {
		req.TopN = DefaultTopN
	}

	if req.Previous != nil && !req.Previous.Start.IsZero() && normalizeMode(req.Previous.Mode) == mode && now.Before(req.Previous.Start.Add(EpochLength)) {
		epoch := copyEpoch(*req.Previous)
		epoch.Mode = mode
		epoch.End = epoch.Start.Add(EpochLength)
		return Result{Epoch: epoch, Recommendations: copyRecommendations(epoch.Recommendations), NextReviewInDays: daysUntil(now, epoch.End), Frozen: true}
	}

	recs := rank(candidates(mode, req.Signals), req.TopN)
	epoch := Epoch{Start: now.UTC(), End: now.UTC().Add(EpochLength), Mode: mode, Recommendations: recs}
	return Result{Epoch: epoch, Recommendations: copyRecommendations(recs), NextReviewInDays: daysUntil(now, epoch.End)}
}

func candidates(mode string, s Signals) []Recommendation {
	s = normalizeSignals(s)
	var out []Recommendation
	switch mode {
	case "IDLE", "QUIET":
		if s.DisabledAgentCount > 0 {
			out = append(out, rec("enable-disabled-agent", 90, "Enable a disabled agent", "The hive has spare capacity; enabling one disabled lane can increase useful throughput.", sig("mode", mode), sig("disabled_agents", s.DisabledAgentCount)))
		}
		if s.NoCadenceAgentCount > 0 {
			out = append(out, rec("add-agent-cadence", 80, "Give idle agents a cadence", "At least one enabled agent has no governor cadence, so it cannot be kicked automatically while the hive is quiet.", sig("mode", mode), sig("no_cadence_agents", s.NoCadenceAgentCount)))
		}
		if s.QueueIssues+s.QueuePRs <= 2 && s.RepoCount <= 1 {
			out = append(out, rec("widen-hive-repos", 70, "Widen HIVE_REPOS", "The watched backlog is shallow in a low-pressure mode; adding repositories can feed more work to the hive.", sig("mode", mode), sig("repo_count", s.RepoCount), sig("queue_total", s.QueueIssues+s.QueuePRs)))
		}
		if s.ACMM.Advise == acmmadvisor.AdviseRaise {
			out = append(out, rec("raise-acmm-level", 85, "Raise the ACMM level", fmt.Sprintf("ACMM signals support raising L%d→L%d; higher trust can unlock more throughput after human approval.", s.ACMM.CurrentLevel, s.ACMM.TargetLevel), sig("current_level", s.ACMM.CurrentLevel), sig("target_level", s.ACMM.TargetLevel)))
		}
	case "BUSY":
		if s.HoldCount > 0 || s.QueuePRs >= s.QueueIssues {
			out = append(out, rec("rebalance-merge-side", 75, "Rebalance toward merge-side agents", "The hive is busy; prioritize review, hold draining, and merge-side lanes before increasing new issue intake.", sig("mode", mode), sig("open_holds", s.HoldCount), sig("prs", s.QueuePRs), sig("issues", s.QueueIssues)))
		}
		if s.BudgetUsedPct >= 90 || s.BudgetExhausted {
			out = append(out, rec("watch-budget-before-cadence", 65, "Hold cadence while budget is tight", "Busy-mode throughput is already consuming the budget window; avoid cadence increases until spend stabilizes.", sig("budget_used_pct", fmt.Sprintf("%.1f", s.BudgetUsedPct)), sig("budget_exhausted", s.BudgetExhausted)))
		}
	case "SURGE":
		if s.HoldCount > 0 {
			out = append(out, rec("fix-merge-blockers", 100, "Fix merge blockers first", "Surge mode means pressure is high; drain held PRs before changing cadence or adding more inflow.", sig("mode", mode), sig("open_holds", s.HoldCount)))
		}
		if s.QueueIssues+s.QueuePRs > 0 {
			out = append(out, rec("throttle-pr-producing-lanes", 90, "Throttle PR-producing lanes", "The hive is in surge; reduce new PR creation until the queue falls back to a steadier mode.", sig("mode", mode), sig("issues", s.QueueIssues), sig("prs", s.QueuePRs)))
		}
		if s.BudgetExhausted || s.BudgetUsedPct >= 90 {
			out = append(out, rec("raise-budget-if-starving", 85, "Raise budget if agents are starving", "Surge pressure plus an exhausted budget can strand agents before they clear the backlog.", sig("budget_used_pct", fmt.Sprintf("%.1f", s.BudgetUsedPct)), sig("budget_exhausted", s.BudgetExhausted)))
		}
		out = append(out, rec("document-agents-conventions", 40, "Document conventions in AGENTS.md", "When pressure is high, clear local conventions reduce review churn and repeated agent mistakes.", sig("mode", mode)))
	}
	if len(out) == 0 {
		out = append(out, rec("observe-more-signals", 10, "Keep observing", "There are not enough actionable signals for a stronger owner recommendation yet.", sig("mode", mode)))
	}
	return out
}

func rank(in []Recommendation, topN int) []Recommendation {
	out := copyRecommendations(in)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	if topN > 0 && len(out) > topN {
		out = out[:topN]
	}
	return out
}

func normalizeSignals(s Signals) Signals {
	if s.QueueIssues < 0 {
		s.QueueIssues = 0
	}
	if s.QueuePRs < 0 {
		s.QueuePRs = 0
	}
	if s.HoldCount < 0 {
		s.HoldCount = 0
	}
	if s.RepoCount < 0 {
		s.RepoCount = 0
	}
	if s.DisabledAgentCount < 0 {
		s.DisabledAgentCount = 0
	}
	if s.NoCadenceAgentCount < 0 {
		s.NoCadenceAgentCount = 0
	}
	if s.BudgetUsedPct < 0 {
		s.BudgetUsedPct = 0
	}
	if s.BudgetUsedPct > 100 {
		s.BudgetUsedPct = 100
	}
	return s
}

func normalizeMode(mode string) string {
	switch strings.ToUpper(strings.TrimSpace(mode)) {
	case "SURGE":
		return "SURGE"
	case "BUSY":
		return "BUSY"
	case "QUIET":
		return "QUIET"
	default:
		return "IDLE"
	}
}

func rec(id string, score int, title, rationale string, signals ...Signal) Recommendation {
	return Recommendation{ID: id, Score: score, Title: title, Rationale: rationale, Signals: signals}
}

func sig(name string, value any) Signal { return Signal{Name: name, Value: fmt.Sprint(value)} }

func daysUntil(now, end time.Time) int {
	if !now.Before(end) {
		return 0
	}
	remaining := end.Sub(now)
	return int((remaining + 24*time.Hour - time.Nanosecond) / (24 * time.Hour))
}

func copyEpoch(e Epoch) Epoch {
	e.Recommendations = copyRecommendations(e.Recommendations)
	return e
}

func copyRecommendations(in []Recommendation) []Recommendation {
	out := make([]Recommendation, len(in))
	copy(out, in)
	for i := range out {
		out[i].Signals = append([]Signal(nil), out[i].Signals...)
	}
	return out
}
