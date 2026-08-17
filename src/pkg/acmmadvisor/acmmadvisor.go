// Package acmmadvisor computes coverage-driven, guided level-up
// recommendations for a hive's ACMM (Agentic Contribution Maturity Model)
// level.
//
// The core product thesis is "you earn automation with test coverage": a hive
// should only be advanced to a higher-trust ACMM level once it has demonstrated
// the operational signals that justify that trust — sustained test coverage, a
// green-CI / merge-success track record, a controlled actionable-issue backlog,
// and few outstanding holds. This package turns that thesis into a concrete,
// auditable computation.
//
// This package is ADVISORY ONLY. It never changes the applied ACMM level; it
// only computes a Recommendation describing whether the caller should keep the
// current level or propose raising to the next one, together with the exact
// criteria that are met and unmet. A human always approves the actual level
// change. The recommendation is a pure, deterministic function of its inputs
// (stdlib only, no I/O, no clocks, no randomness).
package acmmadvisor

import (
	"fmt"
	"sort"
	"strings"
)

// ── Level bounds ────────────────────────────────────────────────────────────
//
// ACMM levels run L1 (Assisted) through L6 (Fully Autonomous), matching the
// level-*.yaml packs and the dashboard's 1..6 bound (pkg/dashboard/api_packs.go
// handlePackSetLevel). The advisor never proposes a target below MinLevel,
// above MaxLevel, or more than one level above the current level.
const (
	// MinLevel is the lowest ACMM level a hive can run at.
	MinLevel = 1
	// MaxLevel is the highest ACMM level (Fully Autonomous). No recommendation
	// ever proposes a target above this.
	MaxLevel = 6
	// levelStep is the number of levels a single recommendation may raise.
	// Advancement is always one level at a time — the advisor never proposes
	// skipping a level, because each level unlocks trust that must be earned
	// incrementally.
	levelStep = 1
)

// ── Coverage thresholds ─────────────────────────────────────────────────────
//
// The coverage bar is anchored on the real 90% gate the hive codebase enforces
// (see pkg/dashboard/metrics_collector.go collectCoverage, coverageTarget=91).
// Lower levels use gentler coverage bars so that a young project can still make
// early progress; the bar tightens as the level (and the trust it grants)
// rises, culminating at the real 90% gate for the fully-automated levels.
const (
	// coverageGate is the project's real, enforced coverage target. Proposing
	// the highest-trust levels requires meeting it in full.
	coverageGate = 90.0

	// Per-level coverage floors required to PROPOSE raising *to* the target
	// level. They increase monotonically with level.
	coverageFloorL2 = 0.0          // L1→L2: instruction files, not yet coverage-gated.
	coverageFloorL3 = 40.0         // L2→L3: measurement infra appearing; baseline coverage.
	coverageFloorL4 = 60.0         // L3→L4: closed-loop feedback needs a real safety net.
	coverageFloorL5 = 75.0         // L4→L5: semi-automated PRs demand strong coverage.
	coverageFloorL6 = coverageGate // L5→L6: full autonomy requires the full gate.
)

// ── Green-CI streak thresholds ──────────────────────────────────────────────
//
// GreenStreak is the count of consecutive green CI runs (or merges) with no red.
// A streak is the clearest evidence that the pipeline gates work in practice.
// Higher levels require a longer proven streak before granting more autonomy.
const (
	greenStreakL3 = 3  // L2→L3: a short proven streak.
	greenStreakL4 = 5  // L3→L4.
	greenStreakL5 = 8  // L4→L5.
	greenStreakL6 = 12 // L5→L6: full autonomy requires a long, stable streak.
)

// ── Merge-success-rate thresholds ───────────────────────────────────────────
//
// MergeSuccessRate is the fraction (0.0–1.0) of recently opened PRs that merged
// cleanly without being reverted or abandoned. It measures whether the hive's
// proposals are actually good. Autonomy is only warranted when most proposals
// land.
const (
	mergeRateL4 = 0.70 // L3→L4.
	mergeRateL5 = 0.85 // L4→L5.
	mergeRateL6 = 0.95 // L5→L6: near-flawless track record for full autonomy.
)

// ── PR reach rate thresholds ───────────────────────────────────────────────
//
// PRReachRate is the fraction (0.0–1.0) of recently deployed attributable PRs
// that achieved non-zero production reach across the fleet (#3995, phase 2c of
// #3973). It complements MergeSuccessRate (#3972) and GreenStreak: reach
// answers "did anyone use it", merge-success answers "did it last", and
// acceptance answers "did I approve it" — complements, never collapsed into
// one number.
const (
	reachRateL5 = 0.50 // L4→L5: at least half of deployed attributable PRs executed.
	reachRateL6 = 0.80 // L5→L6: strong majority of deployed PRs executed.
)

// ── Backlog / hold thresholds ───────────────────────────────────────────────
//
// ActionableIssues is the count of open, actionable issues the hive itself has
// surfaced but not yet resolved — a large backlog means the loop is not keeping
// up and should not be trusted with more autonomy. HoldCount is the number of
// open PRs still carrying a `hold` label awaiting human review; a pile of
// unreviewed holds means humans are already the bottleneck, so adding autonomy
// is premature. Both are ceilings (Actual must be at or below the ceiling).
const (
	maxActionableL5 = 25 // L4→L5: backlog must be under control.
	maxActionableL6 = 10 // L5→L6: near-empty backlog for full autonomy.

	maxHoldsL5 = 15 // L4→L5: holds must be drained regularly.
	maxHoldsL6 = 5  // L5→L6: almost no unreviewed holds for full autonomy.
)

// AdviseAction is the top-level recommendation verb.
type AdviseAction string

const (
	// AdviseStay means the hive should remain at its current level, either
	// because it is already at the maximum or because it has not yet earned the
	// next level.
	AdviseStay AdviseAction = "stay"
	// AdviseRaise means the advisor proposes raising to the next level. It is
	// only ever emitted when every criterion for that next level is met; a
	// human still approves the actual change.
	AdviseRaise AdviseAction = "raise"
)

// Signals is the forge-neutral set of operational inputs the advisor evaluates.
// "Forge-neutral" means these are abstract measurements (coverage %, streaks,
// rates, counts) rather than GitHub- or GitLab-specific objects, so the advisor
// works regardless of the underlying code-forge.
type Signals struct {
	// CurrentLevel is the ACMM level the hive is applying right now (1–6).
	// Values outside [MinLevel, MaxLevel] are clamped defensively.
	CurrentLevel int
	// CoveragePct is the current test-coverage percentage (0–100).
	CoveragePct float64
	// GreenStreak is the number of consecutive green CI runs with no red.
	GreenStreak int
	// MergeSuccessRate is the fraction (0.0–1.0) of recent PRs that merged
	// cleanly. Values outside [0,1] are clamped defensively.
	MergeSuccessRate float64
	// PRReachRate is the fraction (0.0–1.0) of recently deployed attributable
	// PRs that achieved non-zero production reach (#3995, phase 2c of #3973).
	// Values outside [0,1] are clamped defensively.
	PRReachRate float64
	// ActionableIssues is the count of open actionable issues the hive has
	// surfaced but not yet resolved (a backlog ceiling, lower is better).
	ActionableIssues int
	// HoldCount is the number of open PRs still carrying a hold label awaiting
	// human review (a ceiling, lower is better).
	HoldCount int
	// HasQualityAgent reports whether a quality/coverage agent is present and
	// active. Coverage-gated advancement is meaningless without one, so it is a
	// prerequisite for the coverage-driven levels.
	HasQualityAgent bool
}

// Comparator describes how a Criterion's Actual is compared to its Required.
type Comparator string

const (
	// AtLeast means the criterion passes when Actual >= Required (floors).
	AtLeast Comparator = ">="
	// AtMost means the criterion passes when Actual <= Required (ceilings).
	AtMost Comparator = "<="
	// IsTrue means the criterion passes when a boolean signal is true.
	IsTrue Comparator = "is"
)

// Criterion is a single, renderable requirement for advancing to a target
// level. The dashboard can render a checklist directly from a slice of these.
type Criterion struct {
	// Name is a short human-readable label, e.g. "Test coverage".
	Name string
	// Comparator describes the pass relationship (>=, <=, is).
	Comparator Comparator
	// Required is the threshold, formatted for display (e.g. "90%", "12").
	Required string
	// Actual is the observed value, formatted for display.
	Actual string
	// Met reports whether this criterion is currently satisfied.
	Met bool
	// Detail is an optional one-line rationale for why this criterion exists.
	Detail string
}

// Recommendation is the advisor's output. It is advisory only: the caller (and
// ultimately a human) decides whether to act on it.
type Recommendation struct {
	// Advise is stay or raise.
	Advise AdviseAction
	// CurrentLevel echoes the (clamped) level that was evaluated.
	CurrentLevel int
	// TargetLevel is the level being proposed. When Advise is stay, TargetLevel
	// equals CurrentLevel.
	TargetLevel int
	// Ready reports whether every criterion for TargetLevel is met. When Advise
	// is raise, Ready is always true. When Advise is stay because criteria are
	// unmet, Ready is false; when Advise is stay because the hive is already at
	// MaxLevel, Ready is true (nothing left to earn).
	Ready bool
	// Met lists the satisfied criteria for the evaluated next level.
	Met []Criterion
	// Unmet lists the unsatisfied criteria, i.e. exactly what is blocking the
	// level-up. Empty when Ready is true.
	Unmet []Criterion
	// Rationale is a human-readable summary suitable for a dashboard tooltip.
	Rationale string
}

// Recommend computes a level-up recommendation from the given Signals. It is a
// pure, deterministic function: identical Signals always yield an identical
// Recommendation. It never changes any applied level.
func Recommend(s Signals) Recommendation {
	level := clampLevel(s.CurrentLevel)

	// Already at the ceiling: nothing higher to propose.
	if level >= MaxLevel {
		return Recommendation{
			Advise:       AdviseStay,
			CurrentLevel: level,
			TargetLevel:  level,
			Ready:        true,
			Rationale: fmt.Sprintf(
				"At the maximum ACMM level (L%d, Fully Autonomous). No higher level to propose.",
				MaxLevel,
			),
		}
	}

	target := level + levelStep
	criteria := criteriaForTarget(target, s)

	met := make([]Criterion, 0, len(criteria))
	unmet := make([]Criterion, 0, len(criteria))
	for _, c := range criteria {
		if c.Met {
			met = append(met, c)
		} else {
			unmet = append(unmet, c)
		}
	}

	rec := Recommendation{
		CurrentLevel: level,
		TargetLevel:  target,
		Met:          met,
		Unmet:        unmet,
	}

	if len(unmet) == 0 {
		rec.Advise = AdviseRaise
		rec.Ready = true
		rec.Rationale = fmt.Sprintf(
			"Ready to propose raising L%d→L%d: all %d criteria met. Human approval required.",
			level, target, len(met),
		)
		return rec
	}

	rec.Advise = AdviseStay
	rec.Ready = false
	rec.Rationale = fmt.Sprintf(
		"Stay at L%d. %d of %d criteria for L%d not yet met: %s.",
		level, len(unmet), len(criteria), target, summarizeUnmet(unmet),
	)
	return rec
}

// criteriaForTarget returns the ordered checklist required to propose raising
// *to* the given target level, evaluated against the supplied signals. The bar
// is gentler for the low levels (L1→L2) and strictest for full autonomy
// (L5→L6). target is assumed to be in [MinLevel+1, MaxLevel].
func criteriaForTarget(target int, s Signals) []Criterion {
	switch target {
	case 2:
		// L1→L2 (Assisted → Instructed): the gentlest gate. Advancing here only
		// asks that a quality agent be present so future coverage gating has an
		// owner; no numeric coverage/streak bars yet.
		return []Criterion{
			boolCriterion(
				"Quality agent present",
				s.HasQualityAgent,
				"A quality/coverage agent must own the coverage loop before higher levels.",
			),
		}
	case 3:
		// L2→L3 (Instructed → Measured): baseline measurement. A quality agent
		// plus a modest coverage floor and a short green streak.
		return []Criterion{
			boolCriterion("Quality agent present", s.HasQualityAgent,
				"Measured level requires an active quality agent."),
			floorPctCriterion("Test coverage", s.CoveragePct, coverageFloorL3,
				"Baseline coverage so measurements are meaningful."),
			floorIntCriterion("Green-CI streak", s.GreenStreak, greenStreakL3,
				"A short proven streak of green CI runs."),
		}
	case 4:
		// L3→L4 (Measured → Adaptive): closed-loop feedback needs a real safety
		// net — stronger coverage, a longer streak, and a merge-success record.
		return []Criterion{
			boolCriterion("Quality agent present", s.HasQualityAgent,
				"Adaptive feedback loops require an active quality agent."),
			floorPctCriterion("Test coverage", s.CoveragePct, coverageFloorL4,
				"Closed-loop changes need a real coverage safety net."),
			floorIntCriterion("Green-CI streak", s.GreenStreak, greenStreakL4,
				"A sustained streak proving CI gates hold."),
			floorRateCriterion("Merge success rate", s.MergeSuccessRate, mergeRateL4,
				"Most proposed PRs must land cleanly."),
		}
	case 5:
		// L4→L5 (Adaptive → Semi-Automated): the system now proposes PRs at
		// scale. Strong coverage, a long streak, a high merge rate, production
		// PR reach, and a controlled backlog / hold queue.
		return []Criterion{
			boolCriterion("Quality agent present", s.HasQualityAgent,
				"Semi-automated PRs require an active quality agent."),
			floorPctCriterion("Test coverage", s.CoveragePct, coverageFloorL5,
				"Semi-automated PRs demand strong coverage."),
			floorIntCriterion("Green-CI streak", s.GreenStreak, greenStreakL5,
				"A long streak proving stability under load."),
			floorRateCriterion("Merge success rate", s.MergeSuccessRate, mergeRateL5,
				"A high fraction of proposals must merge cleanly."),
			floorRateCriterion("PR reach rate", s.PRReachRate, reachRateL5,
				"Deployed PRs must demonstrate production reach across the fleet."),
			ceilIntCriterion("Actionable-issue backlog", s.ActionableIssues, maxActionableL5,
				"The backlog must be under control before scaling PRs."),
			ceilIntCriterion("Open holds", s.HoldCount, maxHoldsL5,
				"Humans must be draining the hold queue, not drowning in it."),
		}
	default: // target == 6
		// L5→L6 (Semi-Automated → Fully Autonomous): the strictest gate. Full
		// autonomy (auto-merge on green) requires meeting the real 90% coverage
		// gate, a long streak, a near-flawless merge rate, verified production
		// reach, a near-empty backlog, and almost no outstanding holds.
		return []Criterion{
			boolCriterion("Quality agent present", s.HasQualityAgent,
				"Full autonomy requires an active quality agent."),
			floorPctCriterion("Test coverage", s.CoveragePct, coverageFloorL6,
				"Full autonomy requires meeting the enforced coverage gate."),
			floorIntCriterion("Green-CI streak", s.GreenStreak, greenStreakL6,
				"A long, stable streak before auto-merge is granted."),
			floorRateCriterion("Merge success rate", s.MergeSuccessRate, mergeRateL6,
				"A near-flawless merge track record for auto-merge."),
			floorRateCriterion("PR reach rate", s.PRReachRate, reachRateL6,
				"Full autonomy requires verified production execution of deployed PRs."),
			ceilIntCriterion("Actionable-issue backlog", s.ActionableIssues, maxActionableL6,
				"A near-empty backlog before granting full autonomy."),
			ceilIntCriterion("Open holds", s.HoldCount, maxHoldsL6,
				"Almost no unreviewed holds before removing the hold gate."),
		}
	}
}

// ── Criterion builders ──────────────────────────────────────────────────────

// floorPctCriterion builds a >= criterion over a percentage signal.
func floorPctCriterion(name string, actual, required float64, detail string) Criterion {
	return Criterion{
		Name:       name,
		Comparator: AtLeast,
		Required:   formatPct(required),
		Actual:     formatPct(clampPct(actual)),
		Met:        clampPct(actual) >= required,
		Detail:     detail,
	}
}

// floorRateCriterion builds a >= criterion over a 0–1 rate signal, rendered as
// a percentage for readability.
func floorRateCriterion(name string, actual, required float64, detail string) Criterion {
	return Criterion{
		Name:       name,
		Comparator: AtLeast,
		Required:   formatPct(required * pctScale),
		Actual:     formatPct(clampRate(actual) * pctScale),
		Met:        clampRate(actual) >= required,
		Detail:     detail,
	}
}

// floorIntCriterion builds a >= criterion over an integer signal.
func floorIntCriterion(name string, actual, required int, detail string) Criterion {
	return Criterion{
		Name:       name,
		Comparator: AtLeast,
		Required:   fmt.Sprintf("%d", required),
		Actual:     fmt.Sprintf("%d", actual),
		Met:        actual >= required,
		Detail:     detail,
	}
}

// ceilIntCriterion builds a <= criterion over an integer signal (a ceiling).
func ceilIntCriterion(name string, actual, required int, detail string) Criterion {
	return Criterion{
		Name:       name,
		Comparator: AtMost,
		Required:   fmt.Sprintf("%d", required),
		Actual:     fmt.Sprintf("%d", actual),
		Met:        actual <= required,
		Detail:     detail,
	}
}

// boolCriterion builds an "is true" criterion over a boolean signal.
func boolCriterion(name string, actual bool, detail string) Criterion {
	return Criterion{
		Name:       name,
		Comparator: IsTrue,
		Required:   "yes",
		Actual:     boolWord(actual),
		Met:        actual,
		Detail:     detail,
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// pctScale converts a 0–1 fraction to a 0–100 percentage.
const pctScale = 100.0

// clampLevel constrains an arbitrary level to [MinLevel, MaxLevel] so degenerate
// inputs (0, negative, or >MaxLevel) are handled safely.
func clampLevel(level int) int {
	if level < MinLevel {
		return MinLevel
	}
	if level > MaxLevel {
		return MaxLevel
	}
	return level
}

// clampPct constrains a percentage to [0, 100].
func clampPct(pct float64) float64 {
	if pct < 0 {
		return 0
	}
	if pct > pctScale {
		return pctScale
	}
	return pct
}

// clampRate constrains a fraction to [0, 1].
func clampRate(rate float64) float64 {
	if rate < 0 {
		return 0
	}
	if rate > 1 {
		return 1
	}
	return rate
}

// formatPct renders a percentage without trailing zeros (e.g. "90%", "62.5%").
func formatPct(pct float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.1f", pct), "0"), ".") + "%"
}

// boolWord renders a boolean as a human word.
func boolWord(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// summarizeUnmet joins unmet criterion names into a readable clause.
func summarizeUnmet(unmet []Criterion) string {
	names := make([]string, 0, len(unmet))
	for _, c := range unmet {
		names = append(names, c.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
