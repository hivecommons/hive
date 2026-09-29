package config

import (
	"math"
)

// Default governor mode thresholds, in queue items (actionable issues + open
// PRs). These are the per-repo BASE values: a hive watching one repo surges at
// 20 items, and EffectiveThreshold scales them for hives watching more.
//
// They live here rather than in pkg/governor because two callers need the same
// answer — the governor, which decides the mode, and the dashboard gauge, which
// tells the operator which numbers produced it. Those were independently
// duplicated constants before #3498; scaling one and not the other would have
// made the gauge disagree with the governor it describes.
const (
	DefaultThresholdSurge = 20
	DefaultThresholdBusy  = 10
	DefaultThresholdQuiet = 2
)

// Threshold scaling curves (GovernorConfig.ThresholdScaling).
const (
	// CadenceScopeAggregate keeps the historical governor behavior: one hive
	// mode is computed from the aggregate actionable issue+PR queue.
	CadenceScopeAggregate = "aggregate"
	// CadenceScopePerRepo computes a separate mode for each successfully
	// scanned repo from that repo's own actionable issue+PR queue.
	CadenceScopePerRepo = "per_repo"
)

// ValidCadenceScopes are the accepted cadence_scope values. "" means "unset",
// which resolves to aggregate.
var ValidCadenceScopes = map[string]bool{
	"":                    true,
	CadenceScopeAggregate: true,
	CadenceScopePerRepo:   true,
}

// ValidateCadenceScope reports whether v is an accepted cadence mode scope.
func ValidateCadenceScope(v string) bool { return ValidCadenceScopes[v] }

// CadenceScopeMode returns the configured cadence mode scope with its default
// applied. Unset means aggregate to preserve the historical one-mode cadence
// behavior for existing hives.
func (g GovernorConfig) CadenceScopeMode() string {
	switch g.CadenceScope {
	case CadenceScopePerRepo:
		return CadenceScopePerRepo
	default:
		return CadenceScopeAggregate
	}
}

const (
	// ThresholdScalingLinear multiplies the base threshold by the repo count.
	// This is the default, and it is exactly equivalent to comparing PER-REPO
	// queue pressure against the base thresholds — the mode ladder then means
	// the same thing on a 3-repo hive as on a 39-repo one. The issue considered
	// normalizing the queue instead (queue / repos) and rejected it because it
	// changes the number the dashboard displays; scaling the thresholds reaches
	// the same outcome while leaving the displayed queue depth alone.
	ThresholdScalingLinear = "linear"
	// ThresholdScalingSqrt multiplies by ceil(sqrt(repos)) instead — a gentler
	// curve for hives whose queue depth does not grow linearly with repo count
	// (many small, quiet repos alongside a few busy ones). It reaches SURGE
	// sooner than linear does.
	ThresholdScalingSqrt = "sqrt"
	// ThresholdScalingNone disables scaling: the base thresholds are used as
	// absolute queue depths, which is the behavior from before #3498.
	ThresholdScalingNone = "none"
)

// ThresholdSourcePack marks GovernorConfig.Modes thresholds as seeded by an
// ACMM pack rather than typed by an operator (#4037). It is written only by the
// pack-apply paths and cleared by the operator threshold-write path.
const ThresholdSourcePack = "pack"

// ThresholdsArePackSeeded reports whether the explicit thresholds in Modes were
// written by a pack apply. Anything other than ThresholdSourcePack — including
// the empty value every pre-#4037 hive has — reads as operator-owned.
func (g GovernorConfig) ThresholdsArePackSeeded() bool {
	return g.ThresholdsSource == ThresholdSourcePack
}

// ValidThresholdScalings are the accepted threshold_scaling values. "" means
// "unset", which resolves to linear.
var ValidThresholdScalings = map[string]bool{
	"":                     true,
	ThresholdScalingLinear: true,
	ThresholdScalingSqrt:   true,
	ThresholdScalingNone:   true,
}

// ValidateThresholdScaling reports whether v is an accepted scaling curve.
func ValidateThresholdScaling(v string) bool { return ValidThresholdScalings[v] }

// ThresholdScalingMode returns the configured scaling curve with its default
// applied. Unset means linear: the whole point of #3498 is that a hive which
// says nothing gets thresholds matched to its size.
//
// An unrecognized value also resolves to linear rather than silently disabling
// scaling — config load rejects bad values outright, so reaching this with one
// means the value came from a path that skipped validation, and defaulting to
// the documented behavior beats defaulting to "off" for reasons nobody can see.
func (g GovernorConfig) ThresholdScalingMode() string {
	switch g.ThresholdScaling {
	case ThresholdScalingSqrt:
		return ThresholdScalingSqrt
	case ThresholdScalingNone:
		return ThresholdScalingNone
	default:
		return ThresholdScalingLinear
	}
}

// ScaleThreshold applies a scaling curve to a base threshold for a hive
// watching repoCount repos.
//
// repoCount is clamped to at least 1, so a hive with no repos: list — or one
// that watches a single repo via primary_repo — gets the unscaled base rather
// than a threshold of zero, which would put every non-empty queue in SURGE.
func ScaleThreshold(base, repoCount int, scaling string) int {
	if base <= 0 {
		return base
	}
	if repoCount < 1 {
		repoCount = 1
	}
	switch scaling {
	case ThresholdScalingNone:
		return base
	case ThresholdScalingSqrt:
		return base * int(math.Ceil(math.Sqrt(float64(repoCount))))
	default:
		return base * repoCount
	}
}

// EffectiveThreshold resolves the queue depth at which modeName engages, for a
// hive watching repoCount repos.
//
// Resolution, in order:
//
//  1. An OPERATOR-SET explicit governor.modes.<mode>.threshold greater than
//     zero wins and is returned UNSCALED. An operator who hand-tuned a number
//     meant that number, and #3498 is explicit that hand-tuned hives see no
//     behavior change: scaling a hand-tuner's `surge: 300` on a 39-repo hive
//     would produce 11700.
//  2. A PACK-SEEDED explicit threshold (governor.thresholds_source: pack) is
//     treated as the per-repo BASE and scaled, exactly like the built-in
//     defaults (#4037). This is what makes scaling reach a pack-applied hive —
//     the normal path — while keeping each level's own tuning, since L3's
//     15/10/3 and L4-L6's 10/5/2 stay distinct bases rather than collapsing
//     onto one default.
//  3. Otherwise the base default for surge/busy/quiet, scaled by repo count.
//  4. Any other mode name has no threshold (0). computeMode only ladders over
//     surge/busy/quiet — idle is the fallthrough — so a threshold on `idle` or
//     on a custom mode has never been consulted.
//
// A threshold of exactly zero in config counts as UNSET, matching the existing
// thresholdFor behavior: mode entries frequently exist only to carry cadences,
// and a literal zero would put every non-empty queue in that mode.
//
// WHY AN ABSENT MARKER MEANS "OPERATOR-SET". Every hive that applied a pack
// before #4037 has seeded thresholds and no marker, and reading those as
// pack-seeded would multiply them by the repo count the first time the new code
// ran — a silent, potentially large mode-ladder change on upgrade. Reading them
// as operator-owned instead keeps those hives exactly as they are; re-applying
// the level stamps the marker and turns scaling on as an explicit, operator-
// initiated act. Fail-quiet on upgrade, opt-in to the new behavior.
func (g GovernorConfig) EffectiveThreshold(modeName string, repoCount int) int {
	packSeeded := g.ThresholdsArePackSeeded()
	if mode, ok := g.Modes[modeName]; ok && mode.Threshold > 0 && !packSeeded {
		return mode.Threshold
	}

	var base int
	switch modeName {
	case "surge":
		base = DefaultThresholdSurge
	case "busy":
		base = DefaultThresholdBusy
	case "quiet":
		base = DefaultThresholdQuiet
	default:
		// Unknown mode: no threshold, and a pack-seeded value on it is still
		// not a threshold the ladder consults.
		return 0
	}

	// A pack-seeded value replaces the built-in base for this mode, then scales
	// the same way. A pack that seeds only some modes leaves the rest on the
	// built-in bases, which is the behavior the packs already rely on.
	if packSeeded {
		if mode, ok := g.Modes[modeName]; ok && mode.Threshold > 0 {
			base = mode.Threshold
		}
	}

	return ScaleThreshold(base, repoCount, g.ThresholdScalingMode())
}

// RepoCount returns the number of repos this hive watches, for threshold
// scaling. A hive with an empty repos: list still watches at least its primary
// repo, so the floor is 1.
func (p ProjectConfig) RepoCount() int {
	if n := len(p.Repos); n > 0 {
		return n
	}
	return 1
}

// AttributionTrailerEnabled reports whether the visible attribution trailer on
// hive-created PRs/issues is on for this hive. Default ON: a hive that says
// nothing gets the trailer; set `governor.attribution_trailer: false` to hide
// it. The audit-log entry for each creation is unconditional and is NOT gated
// by this.
func (g *GovernorConfig) AttributionTrailerEnabled() bool {
	return g.AttributionTrailer == nil || *g.AttributionTrailer
}
