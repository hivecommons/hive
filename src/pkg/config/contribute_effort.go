package config

import (
	"fmt"
	"strings"
)

// ReasoningEffortLadder is the canonical, ordered reasoning-effort scale used
// to compare efforts across backends for the contributor effort floor
// (hub.contribute_min_reasoning_effort, hivecommons/hive#9197). Every level in
// ReasoningEffortsByBackend is a point on this ladder, so a relay's effort can
// be ranked without a raw string compare: codex's "minimal" sits below
// claude's "low", and claude's "max" sits above codex's "xhigh".
//
// An ordered floor (rather than an explicit allow-list) was chosen because
// effort vocabularies differ per backend: an allow-list would force operators
// to enumerate every backend's spelling, while a single floor on a shared
// ladder expresses "at least this much reasoning" once for all backends.
var ReasoningEffortLadder = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

// ReasoningEffortRank returns effort's position on ReasoningEffortLadder
// (case-insensitive, whitespace-trimmed), or -1 when effort is empty or not a
// recognised level.
func ReasoningEffortRank(effort string) int {
	effort = strings.ToLower(strings.TrimSpace(effort))
	for i, v := range ReasoningEffortLadder {
		if v == effort {
			return i
		}
	}
	return -1
}

// NormalizeContributeMinReasoningEffort canonicalises a configured effort
// floor: empty means "no floor"; anything else must be a level on
// ReasoningEffortLadder.
func NormalizeContributeMinReasoningEffort(v string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return "", nil
	}
	if ReasoningEffortRank(v) < 0 {
		return "", fmt.Errorf("invalid contribute_min_reasoning_effort %q (accepted: %s, or empty for no floor)",
			v, strings.Join(ReasoningEffortLadder, ", "))
	}
	return v, nil
}

// ReasoningEffortMeetsFloor reports whether a relay running backend at effort
// satisfies floor. known is false when the effort cannot be ranked — it is
// empty, not on the ladder, or not a level backend accepts — so the caller can
// apply its reject-unknown policy; ok is meaningful only when known is true.
//
// The floor is normalised per backend: when backend has a closed effort
// vocabulary (ReasoningEffortsByBackend) whose top level is below the floor,
// the floor is clamped to that top level, so e.g. a "max" floor is satisfied
// by agy at "high" (its highest setting) rather than locking agy out entirely.
// An empty or unrecognised floor imposes nothing.
func ReasoningEffortMeetsFloor(backend, effort, floor string) (ok, known bool) {
	floorRank := ReasoningEffortRank(floor)
	if floorRank < 0 {
		return true, true
	}
	effort = strings.ToLower(strings.TrimSpace(effort))
	rank := ReasoningEffortRank(effort)
	if rank < 0 {
		return false, false
	}
	backend = strings.ToLower(strings.TrimSpace(backend))
	if levels, closed := ReasoningEffortsByBackend[backend]; closed {
		if !ValidEffort(backend, effort) {
			return false, false
		}
		top := -1
		for _, l := range levels {
			if r := ReasoningEffortRank(l); r > top {
				top = r
			}
		}
		if top >= 0 && floorRank > top {
			floorRank = top
		}
	}
	return rank >= floorRank, true
}
