package acmmadvisor

// This file holds the thin, forge-neutral bridge the dashboard/status builder
// would call to attach a level-up recommendation to the status payload. It is
// deliberately kept as a PURE computation here: it takes already-collected
// signals and returns a recommendation. It does NOT read the config, apply a
// level, or perform any I/O.
//
// Wiring status: RecommendFromStatus is wired into pkg/dashboard on two
// surfaces that share ONE signal-collection path (buildACMMStatusInputs) so
// they cannot drift — the GET /api/acmm-recommendation endpoint (#5225) and
// the StatusPayload.ACMMAdvice field attached on the status-build path.
// Every input is now sourced from real data: CurrentLevel from
// detectACMMLevel, CoveragePct from the ci-maintainer coverage metric,
// ActionableIssues/HoldCount from the live status snapshot, HasQualityAgent
// from the active pack, MergeSuccessRate from the fleet-stats collector
// (#3972), and GreenStreak from default-branch Actions history (#5226).
// Signals that cannot be measured stay at zero rather than being fabricated.
//
// The dashboard renders Recommendation.Met/Unmet as a checklist and must
// NEVER auto-apply the target level — a human approves via handlePackSetLevel.

// StatusInputs is the forge-neutral bundle a status builder assembles from
// already-collected metrics. It mirrors Signals but exists as a distinct
// boundary type so the dashboard can populate it without importing internal
// advisor thresholds.
type StatusInputs struct {
	CurrentLevel     int
	CoveragePct      float64
	CoverageTarget   float64
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
