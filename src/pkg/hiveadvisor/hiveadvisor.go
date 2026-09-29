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
	// Links point at the Overview export lists (relative to the dashboard
	// origin) and Items name the first few issues/PRs the advice is about.
	Links []Link `json:"links,omitempty"`
	Items []Item `json:"items,omitempty"`
	// Bands lists the Overview band keys ("pr/blocked", "issue/waiting") the
	// advice names, so a renderer can print each band's rule beside it.
	Bands []string `json:"bands,omitempty"`
	// Cleared marks a recommendation that the current epoch froze but whose
	// trigger no longer holds. The slot stays (the owner sees what was asked
	// and that it was done) while its stale numbers are dropped.
	Cleared bool `json:"cleared,omitempty"`
	Score   int  `json:"score"`
}

type Signals struct {
	Mode string
	// QueueIssues and QueuePRs are the governor's ACTIONABLE queue — what
	// mode and cadence key on. They exclude held, draft, in-review and
	// human-gated items, so they are usually smaller than Queue's totals.
	QueueIssues         int
	QueuePRs            int
	HoldCount           int
	RepoCount           int
	DisabledAgentCount  int
	NoCadenceAgentCount int
	BudgetUsedPct       float64
	BudgetExhausted     bool
	ACMM                acmmadvisor.Recommendation
	// Queue is the Overview chart's per-item band breakdown. The queue-health
	// rules read only this, never the governor counters above.
	Queue Queue
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
	// Bands carries the rule text for every band the recommendations name.
	Bands []BandRule `json:"bands,omitempty"`
	// Counts states which totals the advice quotes; recomputed every call.
	Counts Counts `json:"counts"`
}

type Request struct {
	Now      time.Time
	TopN     int
	Signals  Signals
	Previous *Epoch
	// Thresholds tune the queue-health rules; zero fields use DefaultThresholds.
	Thresholds Thresholds
}

// Recommend computes the owner advice for one evaluation.
//
// The 7-day epoch freezes WHICH recommendations show, not their numbers: a
// frozen epoch keeps its recommendation IDs and order, but every rationale,
// signal, item list and link is recomputed from the signals passed in now.
// A frozen recommendation whose trigger no longer holds is kept, marked
// Cleared, with its stale numbers dropped — "70 blocked" a week after the CI
// fix would be worse than no advice. Bands and Counts are never frozen.
func Recommend(req Request) Result {
	now := req.Now
	if now.IsZero() {
		now = time.Unix(0, 0).UTC()
	}
	mode := normalizeMode(req.Signals.Mode)
	if req.TopN <= 0 {
		req.TopN = DefaultTopN
	}
	fresh := candidates(mode, req.Signals, req.Thresholds)
	counts := Counts{
		GovernorIssues: max(req.Signals.QueueIssues, 0),
		GovernorPRs:    max(req.Signals.QueuePRs, 0),
		OverviewIssues: len(req.Signals.Queue.Issues),
		OverviewPRs:    len(req.Signals.Queue.PRs),
	}

	if req.Previous != nil && !req.Previous.Start.IsZero() && normalizeMode(req.Previous.Mode) == mode && now.Before(req.Previous.Start.Add(EpochLength)) && !placeholderOnly(req.Previous.Recommendations, fresh) {
		epoch := copyEpoch(*req.Previous)
		epoch.Mode = mode
		epoch.End = epoch.Start.Add(EpochLength)
		epoch.Recommendations = refreshFrozen(epoch.Recommendations, fresh)
		return Result{Epoch: epoch, Recommendations: copyRecommendations(epoch.Recommendations), NextReviewInDays: daysUntil(now, epoch.End), Frozen: true, Bands: bandRules(epoch.Recommendations, req.Signals.Queue), Counts: counts}
	}

	recs := rank(fresh, req.TopN)
	epoch := Epoch{Start: now.UTC(), End: now.UTC().Add(EpochLength), Mode: mode, Recommendations: recs}
	return Result{Epoch: epoch, Recommendations: copyRecommendations(recs), NextReviewInDays: daysUntil(now, epoch.End), Bands: bandRules(recs, req.Signals.Queue), Counts: counts}
}

// refreshFrozen keeps the frozen IDs and order, substituting each one's
// freshly computed body. An ID with no fresh candidate is marked Cleared.
func refreshFrozen(frozen, fresh []Recommendation) []Recommendation {
	byID := make(map[string]Recommendation, len(fresh))
	for _, r := range fresh {
		byID[r.ID] = r
	}
	out := make([]Recommendation, 0, len(frozen))
	for _, prev := range frozen {
		if cur, ok := byID[prev.ID]; ok {
			out = append(out, cur)
			continue
		}
		out = append(out, Recommendation{ID: prev.ID, Title: prev.Title, Rationale: "No longer applies: the signal that triggered this advice has cleared since the epoch began.", Cleared: true, Score: prev.Score})
	}
	return copyRecommendations(out)
}

// placeholderOnly reports a frozen epoch that holds nothing but the
// "keep observing" placeholder while real advice is now available. Freezing
// on a placeholder would hide a week of actionable advice behind "nothing to
// say", so such an epoch is not worth keeping.
func placeholderOnly(frozen, fresh []Recommendation) bool {
	if len(frozen) != 1 || frozen[0].ID != placeholderID {
		return false
	}
	for _, r := range fresh {
		if r.ID != placeholderID {
			return true
		}
	}
	return false
}

const placeholderID = "observe-more-signals"

// candidates is the rule table. Mode-gated rules come first; the queue-health
// rules (queueCandidates) run in every mode.
func candidates(mode string, s Signals, th Thresholds) []Recommendation {
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
			out = append(out, rec("widen-hive-repos", 70, "Widen HIVE_REPOS", "The governor's actionable backlog is shallow in a low-pressure mode; adding repositories can feed more work to the hive.", sig("mode", mode), sig("repo_count", s.RepoCount), sig("governor_queue_total", s.QueueIssues+s.QueuePRs)))
		}
		if s.ACMM.Advise == acmmadvisor.AdviseRaise {
			out = append(out, rec("raise-acmm-level", 85, "Raise the ACMM level", fmt.Sprintf("ACMM signals support raising L%d→L%d; higher trust can unlock more throughput after human approval.", s.ACMM.CurrentLevel, s.ACMM.TargetLevel), sig("current_level", s.ACMM.CurrentLevel), sig("target_level", s.ACMM.TargetLevel)))
		}
	case "BUSY":
		if s.HoldCount > 0 || s.QueuePRs >= s.QueueIssues {
			out = append(out, rec("rebalance-merge-side", 75, "Rebalance toward merge-side agents", "The hive is busy; prioritize review, hold draining, and merge-side lanes before increasing new issue intake.", sig("mode", mode), sig("open_holds", s.HoldCount), sig("governor_prs", s.QueuePRs), sig("governor_issues", s.QueueIssues)))
		}
		if s.BudgetUsedPct >= 90 || s.BudgetExhausted {
			out = append(out, rec("watch-budget-before-cadence", 65, "Hold cadence while budget is tight", "Busy-mode throughput is already consuming the budget window; avoid cadence increases until spend stabilizes.", sig("budget_used_pct", fmt.Sprintf("%.1f", s.BudgetUsedPct)), sig("budget_exhausted", s.BudgetExhausted)))
		}
	case "SURGE":
		if s.HoldCount > 0 {
			out = append(out, rec("fix-merge-blockers", 100, "Fix merge blockers first", "Surge mode means pressure is high; drain held PRs before changing cadence or adding more inflow.", sig("mode", mode), sig("open_holds", s.HoldCount)))
		}
		if s.QueueIssues+s.QueuePRs > 0 {
			out = append(out, rec("throttle-pr-producing-lanes", 90, "Throttle PR-producing lanes", "The hive is in surge; reduce new PR creation until the queue falls back to a steadier mode.", sig("mode", mode), sig("governor_issues", s.QueueIssues), sig("governor_prs", s.QueuePRs)))
		}
		if s.BudgetExhausted || s.BudgetUsedPct >= 90 {
			out = append(out, rec("raise-budget-if-starving", 85, "Raise budget if agents are starving", "Surge pressure plus an exhausted budget can strand agents before they clear the backlog.", sig("budget_used_pct", fmt.Sprintf("%.1f", s.BudgetUsedPct)), sig("budget_exhausted", s.BudgetExhausted)))
		}
		out = append(out, rec("document-agents-conventions", 40, "Document conventions in AGENTS.md", "When pressure is high, clear local conventions reduce review churn and repeated agent mistakes.", sig("mode", mode)))
	}
	out = append(out, queueCandidates(s.Queue, th)...)
	if len(out) == 0 {
		out = append(out, rec(placeholderID, 10, "Keep observing", "There are not enough actionable signals for a stronger owner recommendation yet.", sig("mode", mode)))
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
		out[i].Links = append([]Link(nil), out[i].Links...)
		out[i].Items = append([]Item(nil), out[i].Items...)
		out[i].Bands = append([]string(nil), out[i].Bands...)
	}
	return out
}
