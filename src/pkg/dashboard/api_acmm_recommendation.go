package dashboard

import (
	"net/http"

	"github.com/hivecommons/hive/pkg/acmmadvisor"
	"github.com/hivecommons/hive/pkg/config"
)

// qualityAgentName is the config-level name of the quality/coverage agent that
// owns the coverage loop. The ACMM advisor treats its presence in the active
// config as the HasQualityAgent prerequisite (see pkg/config default agents and
// pkg/acmmadvisor criteria).
const qualityAgentName = "quality"

// agentMetricsCIMaintainerKey is the AgentMetrics sub-map that carries the
// coverage reading collected from the coverage badge (see
// MetricsCollector.collect / collectCoverage). The coverage percentage lives at
// AgentMetrics["ci-maintainer"]["coverage"].
const agentMetricsCIMaintainerKey = "ci-maintainer"

// agentMetricsCoverageKey is the field within the ci-maintainer metrics map
// holding the integer coverage percentage.
const agentMetricsCoverageKey = "coverage"

// handleACMMRecommendation serves an advisory ACMM level-up recommendation
// derived from the live status. It is additive, read-only, and ADVISORY ONLY:
// it never changes the applied level (a human approves that via
// handlePackSetLevel). Missing data degrades to conservative zero signals and
// never panics, so the endpoint is safe on a freshly-booted hive with no
// status yet.
//
// The response body is an acmmadvisor.Recommendation: the advise verb,
// current/target level, readiness, and the Met/Unmet criteria checklist the UI
// renders directly.
func (s *Server) handleACMMRecommendation(w http.ResponseWriter, r *http.Request) {
	rec := acmmadvisor.RecommendFromStatus(s.buildACMMStatusInputs())
	jsonResponse(w, rec)
}

// buildACMMStatusInputs assembles the forge-neutral advisor inputs from the
// signals the dashboard already gathers. It is nil-safe at every step: a nil
// Server, nil deps, nil config, or nil cached status all collapse to safe zero
// values rather than panicking. It fabricates nothing — a signal the hive does
// not yet track (green-CI streak) is left at zero, which the advisor treats as
// "not yet earned" rather than inventing a number.
func (s *Server) buildACMMStatusInputs() acmmadvisor.StatusInputs {
	in := acmmadvisor.StatusInputs{
		// A hive with no explicit level detects as MinLevel (L1); see
		// detectACMMLevel. Zero-value fallbacks keep this at a safe default.
		CurrentLevel: acmmadvisor.MinLevel,
		// GreenStreak IS real as of #5226: it is populated below from the
		// green-CI streak the status-build path measures against the primary
		// repo's default-branch Actions history. Until a measurement has
		// actually succeeded it STAYS at zero — "unknown / not yet earned"
		// rather than a fabricated number — which only ever makes the advisor
		// MORE conservative. A repo with no CI at all reads as unknown, never
		// as green. See Client.GreenCIStreak for what it measures and its
		// known weaknesses (flake-sensitive, stale on quiet repos, capped).
		//
		// MergeSuccessRate IS real as of #3972: it is populated below from the
		// fleet-stats collector's cached 90-day merged/rejected counts. See
		// mergeSuccessRateFromFleetStats for what it measures and its known
		// weaknesses (size-gameable, single-judgment, censored); the
		// survival-adjusted term ("without being reverted") remains follow-up
		// work blocked by #3971.
	}

	var cfg *config.Config
	if s != nil && s.deps != nil {
		cfg = s.deps.Config
	}
	if cfg != nil {
		in.CurrentLevel = detectACMMLevel(cfg)
		in.HasQualityAgent = hasQualityAgent(cfg)
	}

	// Baseline merge-success rate (#3972), read from the fleet-stats collector
	// cache the heartbeat already maintains — no fresh GitHub call. When the
	// collector has never completed a collect, or the 90-day window holds no
	// resolved PRs, the signal STAYS at zero ("unknown / not yet earned")
	// rather than being reported as a measured 0.0 or a vacuous 1.0 — the
	// advisor gates autonomy on this value, and the never-fabricate contract
	// above applies to absence-of-data exactly as it does to GreenStreak.
	if s != nil && s.deps != nil {
		if rate, measured := mergeSuccessRateFromFleetStats(s.deps.FleetStats); measured {
			in.MergeSuccessRate = rate
		}
	}

	// Real green-CI streak (#5226), read from the cache the status-build path
	// refreshes on the same pass it already fetches workflow health — no fresh
	// GitHub call on this advisory request. When no measurement has ever
	// succeeded (no GitHub client, API failure, or a repo with no CI history
	// on its default branch) the signal STAYS at zero rather than reporting an
	// unmeasured zero as a measured one.
	if streak, measured := greenCIStreakSnapshot(); measured {
		in.GreenStreak = streak
	}

	// Live queue/coverage signals come from the most recent status snapshot the
	// eval loop published. Read it under the status lock and tolerate a nil
	// snapshot (no eval has run yet).
	if s != nil {
		s.statusMu.RLock()
		status := s.status
		s.statusMu.RUnlock()
		if status != nil {
			in.ActionableIssues = status.Governor.Issues
			in.HoldCount = status.Hold.Total
			in.CoveragePct = coverageFromAgentMetrics(status.AgentMetrics)
		}
	}

	return in
}

// hasQualityAgent reports whether an active quality/coverage agent is present
// in the config. Nil-safe.
func hasQualityAgent(cfg *config.Config) bool {
	if cfg == nil || cfg.Agents == nil {
		return false
	}
	_, ok := cfg.Agents[qualityAgentName]
	return ok
}

// mergeSuccessRateFromFleetStats derives the advisor's baseline merge-success
// rate from the fleet-stats collector's cached counts: merged / (merged +
// closed-unmerged) over the trailing 90-day window (#3972). Nil-safe: a nil
// collector, a collector that has never completed a collect, or a window with
// no resolved PRs all return measured=false, and the caller leaves the signal
// at its conservative zero instead of fabricating a rate.
//
// Caveats (condensed; full discussion on the counts method): the ratio is
// gameable by PR size — the console's self-improvement loop literally learned
// "keep changes under 50 lines for best acceptance rate" (Goodhart observed
// in-project); it measures one maintainer's judgment; and it is censored,
// counting only proposed work — it was green throughout the 2026-08-14 fleet
// outage. These weaknesses are WHY the survival term ("merged AND not
// reverted") exists as the #3972 follow-up, blocked today by #3971 (beads
// archival destroys the post-merge history it needs). The denominator
// deliberately counts superseded/duplicate PRs as non-successes: stricter,
// and harder to game by re-proposing the same change.
func mergeSuccessRateFromFleetStats(fc *FleetStatsCollector) (rate float64, measured bool) {
	counts, ready := fc.Snapshot()
	if !ready {
		return 0, false
	}
	return counts.BaselineMergeSuccessRate()
}

// coverageFromAgentMetrics extracts the integer coverage percentage from the
// AgentMetrics map, returning 0 when absent or malformed. The value is stored
// under AgentMetrics["ci-maintainer"]["coverage"] by collectCoverage; JSON
// round-trips can render it as int or float64, so both numeric forms are
// handled.
func coverageFromAgentMetrics(metrics map[string]any) float64 {
	if metrics == nil {
		return 0
	}
	ci, ok := metrics[agentMetricsCIMaintainerKey].(map[string]any)
	if !ok {
		return 0
	}
	switch v := ci[agentMetricsCoverageKey].(type) {
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case float64:
		return v
	default:
		return 0
	}
}
