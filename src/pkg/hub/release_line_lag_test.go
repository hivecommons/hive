package hub

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
)

// stubReleaseLineTips primes the SHA poller cache the way the reconcile tick
// would, so ReleaseLineLagStatus resolves real v5/v4 tips. Restored on cleanup.
// testEdgeLine is the edge line the lag tests pin (stable is fallbackReleaseLine).
const testEdgeLine = "v6"

func stubReleaseLineTips(t *testing.T, edgeSHA, stableSHA string) {
	origEdgeFn := edgeReleaseLine
	edgeReleaseLine = func(*slog.Logger) string { return testEdgeLine }
	t.Cleanup(func() { edgeReleaseLine = origEdgeFn })
	t.Helper()
	latestSHAMu.Lock()
	origEdge, hadEdge := latestSHAByBranch[testEdgeLine]
	origStable, hadStable := latestSHAByBranch[fallbackReleaseLine]
	latestSHAByBranch[testEdgeLine] = branchSHAInfo{SHA: edgeSHA}
	latestSHAByBranch[fallbackReleaseLine] = branchSHAInfo{SHA: stableSHA}
	latestSHAMu.Unlock()
	t.Cleanup(func() {
		latestSHAMu.Lock()
		if hadEdge {
			latestSHAByBranch[testEdgeLine] = origEdge
		} else {
			delete(latestSHAByBranch, testEdgeLine)
		}
		if hadStable {
			latestSHAByBranch[fallbackReleaseLine] = origStable
		} else {
			delete(latestSHAByBranch, fallbackReleaseLine)
		}
		latestSHAMu.Unlock()
	})
}

// The compare MUST be dispatched base=v5 tip, head=v4 tip so its ahead_by is
// "commits on v4 not yet on v5" = how far v5 is behind v4. Swapping the two
// (base=v4, head=v5) would count v4-behind-v5 instead and flag the wrong line.
// This pins the direction the whole feature rests on.
func TestReleaseLineLagDirectionV5BehindV4(t *testing.T) {
	resetCommitBehindState(t)
	stubReleaseLineTips(t, "v5tip00", "v4tip00")

	commitBehindMu.Lock()
	fetchCommitBehindCount = func(base, head string, _ *slog.Logger) (int, bool, error) {
		if base != "v5tip00" || head != "v4tip00" {
			t.Errorf("compare dispatched %s...%s; want v5tip00...v4tip00 (base=v5, head=v4)", base, head)
		}
		return 7, true, nil
	}
	commitBehindMu.Unlock()

	// First call dispatches the background compare and reports unknown.
	if first := ReleaseLineLagStatus(nil); first.Known {
		t.Fatal("first call must report unknown while the compare is in flight")
	}
	got := testutil.EventuallyValue(t, 2*time.Second, func() (ReleaseLineLag, bool) {
		lag := ReleaseLineLagStatus(nil)
		return lag, lag.Known
	}, "release-line compare never landed in the cache")

	if got.BehindBy != 7 {
		t.Errorf("BehindBy = %d, want 7", got.BehindBy)
	}
	if got.EdgeBranch != testEdgeLine || got.StableBranch != fallbackReleaseLine {
		t.Errorf("branches = %s behind %s, want edge behind stable", got.EdgeBranch, got.StableBranch)
	}
	if got.EdgeSHA != "v5tip00" || got.StableSHA != "v4tip00" {
		t.Errorf("tips = %s/%s, want v5tip00/v4tip00", got.EdgeSHA, got.StableSHA)
	}
}

// A known lag above the threshold is flagged Exceeded; at or below it is not.
func TestReleaseLineLagExceedsThreshold(t *testing.T) {
	resetCommitBehindState(t)
	stubReleaseLineTips(t, "v5tipAA", "v4tipAA")
	t.Setenv(ReleaseLineLagMaxEnvVar, "5")

	commitBehindMu.Lock()
	fetchCommitBehindCount = func(_, _ string, _ *slog.Logger) (int, bool, error) {
		return 27, true, nil
	}
	commitBehindMu.Unlock()

	_ = ReleaseLineLagStatus(nil)
	got := testutil.EventuallyValue(t, 2*time.Second, func() (ReleaseLineLag, bool) {
		lag := ReleaseLineLagStatus(nil)
		return lag, lag.Known
	}, "release-line compare never landed in the cache")

	if got.Threshold != 5 {
		t.Errorf("Threshold = %d, want 5 from %s", got.Threshold, ReleaseLineLagMaxEnvVar)
	}
	if got.BehindBy != 27 || !got.Exceeded {
		t.Errorf("27 behind with threshold 5 must be Exceeded; got BehindBy=%d Exceeded=%v", got.BehindBy, got.Exceeded)
	}
}

// A lag at or below the threshold is known-healthy, not exceeded.
func TestReleaseLineLagWithinThresholdNotExceeded(t *testing.T) {
	resetCommitBehindState(t)
	stubReleaseLineTips(t, "v5tipBB", "v4tipBB")
	t.Setenv(ReleaseLineLagMaxEnvVar, "5")

	commitBehindMu.Lock()
	fetchCommitBehindCount = func(_, _ string, _ *slog.Logger) (int, bool, error) {
		return 5, true, nil
	}
	commitBehindMu.Unlock()

	_ = ReleaseLineLagStatus(nil)
	got := testutil.EventuallyValue(t, 2*time.Second, func() (ReleaseLineLag, bool) {
		lag := ReleaseLineLagStatus(nil)
		return lag, lag.Known
	}, "release-line compare never landed in the cache")

	if got.BehindBy != 5 || got.Exceeded {
		t.Errorf("5 behind with threshold 5 must NOT be Exceeded; got BehindBy=%d Exceeded=%v", got.BehindBy, got.Exceeded)
	}
}

// The critical fail-open guard (#6960/#6909/#6833/#6951): a compare that ERRORS
// must report Known=false — "unknown" — and NEVER a healthy zero. Reporting
// healthy on error is the exact silent-failure mode this feature exists to end.
func TestReleaseLineLagUnknownOnCompareError(t *testing.T) {
	resetCommitBehindState(t)
	stubReleaseLineTips(t, "v5tipEE", "v4tipEE")

	commitBehindMu.Lock()
	fetchCommitBehindCount = func(_, _ string, _ *slog.Logger) (int, bool, error) {
		return 0, false, errors.New("compare API unreachable")
	}
	commitBehindMu.Unlock()

	// Dispatch the compare, then let the resolver drain (it caches nothing on
	// error). Every call must stay Known=false — never flip to a healthy 0.
	_ = ReleaseLineLagStatus(nil)
	waitForCommitBehindResolvers(t)
	for i := 0; i < 3; i++ {
		lag := ReleaseLineLagStatus(nil)
		if lag.Known {
			t.Fatalf("compare error must report unknown, got Known=true BehindBy=%d Exceeded=%v", lag.BehindBy, lag.Exceeded)
		}
		if lag.Exceeded {
			t.Fatal("an unknown lag must never be Exceeded")
		}
	}
}

// Tips the SHA poller has not resolved yet also yield unknown, never a zero.
func TestReleaseLineLagUnknownWhenTipsUnresolved(t *testing.T) {
	resetCommitBehindState(t)
	stubReleaseLineTips(t, "", "")

	lag := ReleaseLineLagStatus(nil)
	if lag.Known {
		t.Errorf("unresolved tips must report unknown, got Known=true BehindBy=%d", lag.BehindBy)
	}
	commitBehindMu.Lock()
	inFlight := len(commitBehindInFlight)
	commitBehindMu.Unlock()
	if inFlight != 0 {
		t.Error("unresolved tips must not dispatch a compare")
	}
}

// The threshold env var overrides the default; junk/negative values fall back
// to the built-in default rather than silencing the alarm.
func TestReleaseLineLagMaxEnvVarParsing(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", defaultReleaseLineLagMax},
		{"0", 0},
		{"12", 12},
		{"notanumber", defaultReleaseLineLagMax},
		{"-3", defaultReleaseLineLagMax},
	}
	for _, tc := range cases {
		t.Setenv(ReleaseLineLagMaxEnvVar, tc.raw)
		if got := releaseLineLagMax(); got != tc.want {
			t.Errorf("releaseLineLagMax(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}
