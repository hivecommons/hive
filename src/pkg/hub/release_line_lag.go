package hub

import (
	"log/slog"
	"os"
	"strconv"
)

// Release-line lag (#6960). Hosted spokes run the edge release line (v5) while
// fixes are landed on the stable default branch (v4). When the routine v4→v5
// sync stalls, v5 silently falls behind and every v4 fix becomes a no-op in
// the field until a sync lands — exactly the trap #6960 documents (v5 was 27
// commits behind v4 with nothing warning about it).
//
// This surfaces "v5 is N commits behind v4" so the drift can never go
// unnoticed again. It deliberately REUSES the commit_behind.go compare/cache
// path (commitsBehindTarget → fetchCommitBehindCount → the GitHub compare API)
// rather than opening a second HTTP client: the same background resolver, the
// same (base, head) cache that naturally invalidates as either branch tip
// moves, and the same honest unknown-on-first-call / unknown-on-error
// semantics.
const (
	// edgeReleaseBranch is the release line hosted spokes run and the one that
	// falls behind. stableReleaseBranch ("v4", commit_behind.go) is where fixes
	// land and the line edge is measured against.
	edgeReleaseBranch = "v5"

	// ReleaseLineLagMaxEnvVar overrides the alarm threshold: v5 may sit up to
	// this many commits behind v4 before the dashboard flags the line as
	// drifting. An unset, empty, non-numeric, or negative value falls back to
	// defaultReleaseLineLagMax — the fail-safe direction is the built-in
	// default, never an accidental "unlimited".
	ReleaseLineLagMaxEnvVar = "HIVE_RELEASE_LINE_LAG_MAX"

	// defaultReleaseLineLagMax is the built-in alarm threshold. Small on
	// purpose: the failure #6960 describes grew to 27 commits precisely because
	// nothing complained, so the default leaves only a little headroom for
	// normal churn between syncs before the line is flagged as drifting.
	defaultReleaseLineLagMax = 5
)

// ReleaseLineLag reports how far the edge release line (v5) sits behind the
// stable default branch (v4), and whether that lag has crossed the alarm
// threshold.
type ReleaseLineLag struct {
	// EdgeBranch and StableBranch name the two lines compared (v5 behind v4).
	EdgeBranch   string `json:"edgeBranch"`
	StableBranch string `json:"stableBranch"`
	// EdgeSHA and StableSHA are the short tips the count was measured between,
	// empty until the SHA poller has resolved them.
	EdgeSHA   string `json:"edgeSHA,omitempty"`
	StableSHA string `json:"stableSHA,omitempty"`
	// BehindBy is the number of commits on v4 that are not yet on v5. Only
	// meaningful when Known is true.
	BehindBy int `json:"behindBy"`
	// Known is false when the tips are not yet resolved or the compare has not
	// landed / failed. A false Known must render as "unknown", NEVER as a
	// healthy zero — reporting healthy on an error is the exact silent-failure
	// mode #6960 (and #6909/#6833/#6951) is about.
	Known bool `json:"known"`
	// Threshold is the effective alarm ceiling (defaultReleaseLineLagMax or the
	// HIVE_RELEASE_LINE_LAG_MAX override).
	Threshold int `json:"threshold"`
	// Exceeded is true only when the lag is BOTH known AND above Threshold. An
	// unknown lag is never "exceeded" and never "healthy" — it is unknown.
	Exceeded bool `json:"exceeded"`
}

// releaseLineLagMax resolves the effective alarm threshold from the
// environment, falling back to defaultReleaseLineLagMax for any unset, empty,
// non-numeric, or negative value.
func releaseLineLagMax() int {
	raw := os.Getenv(ReleaseLineLagMaxEnvVar)
	if raw == "" {
		return defaultReleaseLineLagMax
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return defaultReleaseLineLagMax
	}
	return n
}

// ReleaseLineLagStatus measures how far the edge line (v5) is behind the stable
// line (v4), reusing the commit_behind.go compare/cache path. The first call
// for a given (v5 tip, v4 tip) pair reports Known=false and dispatches the
// background compare; a later call returns the cached count. Tips it cannot yet
// resolve (SHA poller not populated) also yield Known=false. It never blocks a
// dashboard render on the network and never reports a healthy zero for an
// unresolved or failed compare.
func ReleaseLineLagStatus(logger *slog.Logger) ReleaseLineLag {
	threshold := releaseLineLagMax()
	edgeSHA := shortSHA(getLatestSHAForBranch(edgeReleaseBranch))
	stableSHA := shortSHA(getLatestSHAForBranch(stableReleaseBranch))

	lag := ReleaseLineLag{
		EdgeBranch:   edgeReleaseBranch,
		StableBranch: stableReleaseBranch,
		EdgeSHA:      edgeSHA,
		StableSHA:    stableSHA,
		Threshold:    threshold,
	}
	if edgeSHA == "" || stableSHA == "" {
		return lag
	}

	// base = edge (v5), head = stable (v4): commitsBehindTarget returns the
	// compare API's ahead_by for v5...v4 — the commits reachable from v4 but
	// not v5, i.e. exactly how far v5 is behind v4. Swapping the arguments
	// would instead count how far v4 is behind v5 and report the wrong line as
	// drifting; the direction is pinned by test.
	count, known := commitsBehindTarget(edgeSHA, stableSHA, logger)
	if !known {
		return lag
	}
	lag.BehindBy = count
	lag.Known = true
	lag.Exceeded = count > threshold
	return lag
}
