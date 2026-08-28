package acmmadvisor

// This file holds the thin, forge-neutral bridge the dashboard/status builder
// would call to attach a level-up recommendation to the status payload. It is
// deliberately kept as a PURE computation here: it takes already-collected
// signals and returns a recommendation. It does NOT read the config, apply a
// level, or perform any I/O.
//
// TODO(acmm2-wiring): wire RecommendFromStatus into pkg/dashboard's
// status_builder.go — populate a StatusPayload.ACMMAdvice field from the same
// inputs the dashboard already gathers:
//   - CurrentLevel: dashboard.detectACMMLevel(cfg)
//   - CoveragePct:  MetricsCollector coverage ("coverage" key, collectCoverage)
//   - GreenStreak / MergeSuccessRate: from issue-to-merge / CI metrics
//   - ActionableIssues: len(actionable) already passed to buildRepos/buildHold
//   - HoldCount: buildHold(actionable) count
//   - HasQualityAgent: presence of an active "quality" agent in the pack
// The dashboard must render Recommendation.Met/Unmet as a checklist and must
// NEVER auto-apply the target level — a human approves via handlePackSetLevel.

// StatusInputs is the forge-neutral bundle a status builder assembles from
// already-collected metrics. It mirrors Signals but exists as a distinct
// boundary type so the dashboard can populate it without importing internal
// advisor thresholds.
type StatusInputs struct {
	CurrentLevel     int
	CoveragePct      float64
	GreenStreak      int
	MergeSuccessRate float64
	ActionableIssues int
	HoldCount        int
	HasQualityAgent  bool
}

// RecommendFromStatus adapts already-collected status inputs into a
// Recommendation. It is a pure pass-through to Recommend and performs no I/O,
// so it is safe to call from a hot status-build path. Callers attach the result
// to their status payload; they must not act on it automatically.
func RecommendFromStatus(in StatusInputs) Recommendation {
	return Recommend(Signals(in))
}
