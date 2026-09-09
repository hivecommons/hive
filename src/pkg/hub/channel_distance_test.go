package hub

import (
	"fmt"
	"log/slog"
	"testing"
)

// The distance column answers "how much is queued to be promoted into this
// channel". These tests pin the two properties that make that answer
// trustworthy: it is measured against the ADJACENT stage, and it never invents
// a number when the compare did not resolve.

// Each channel must be measured against the stage immediately upstream of it,
// not against the end of the pipeline. Pinned because the tempting shortcut —
// compare everything to edge — produces a stable row that silently restates
// candidate's backlog and cannot say which hop is stalled.
func TestChannelUpstreamIsTheAdjacentStage(t *testing.T) {
	if got := channelUpstream(ReleaseChannelStable); got != ReleaseChannelCandidate {
		t.Errorf("stable must be measured against candidate, got %q", got)
	}
	if got := channelUpstream(ReleaseChannelCandidate); got != ReleaseChannelEdge {
		t.Errorf("candidate must be measured against edge, got %q", got)
	}
	// Edge is where builds enter the pipeline; it has nothing upstream, and a
	// distance for it would have to be invented.
	if got := channelUpstream(ReleaseChannelEdge); got != "" {
		t.Errorf("edge has no upstream stage, got %q", got)
	}
	if got := channelUpstream("not-a-channel"); got != "" {
		t.Errorf("unknown channel must have no upstream, got %q", got)
	}
}

// The end-to-end shape: three channels on three commits, and each row carries
// the distance to its own upstream.
func TestChannelTargetsCarryDistanceToUpstream(t *testing.T) {
	stubChannelDigests(t, map[string]string{
		"v4-latest": "sha256:v4",
		"v5-latest": "sha256:v5",
		"stable":    "sha256:old",
		"candidate": "sha256:v4",
		"edge":      "sha256:v5",
	})
	stubChannelRevisions(t, map[string]string{"stable": "55bd2bc"})
	stubChannelDistances(t, map[channelDistanceKey]channelDistance{
		// stable is a plain ancestor of candidate: 12 commits waiting.
		{base: "9eefa3a", head: "55bd2bc"}: {Status: "behind", Behind: 12},
		// candidate (v4) vs edge (v5): genuinely diverged branches.
		{base: "bc41b53", head: "9eefa3a"}: {Status: "diverged", Behind: 7, Ahead: 3},
	})

	targets := resolveChannelTargets(map[string]string{"v4": "9eefa3a", "v5": "bc41b53"}, testChannelLogger())

	stable := targetFor(targets, ReleaseChannelStable)
	if stable.CompareTo != ReleaseChannelCandidate {
		t.Errorf("stable compares against %q, want candidate", stable.CompareTo)
	}
	if stable.Behind != 12 || stable.Ahead != 0 {
		t.Errorf("stable distance = behind %d ahead %d, want behind 12 ahead 0", stable.Behind, stable.Ahead)
	}
	if stable.CompareStatus != "behind" {
		t.Errorf("stable compare status = %q, want behind", stable.CompareStatus)
	}

	// A diverged pair must keep BOTH counts. Collapsing them into one number
	// would misreport the branch pair these two channels actually track.
	candidate := targetFor(targets, ReleaseChannelCandidate)
	if candidate.CompareTo != ReleaseChannelEdge {
		t.Errorf("candidate compares against %q, want edge", candidate.CompareTo)
	}
	if candidate.CompareStatus != "diverged" || candidate.Behind != 7 || candidate.Ahead != 3 {
		t.Errorf("candidate distance = %q behind %d ahead %d, want diverged behind 7 ahead 3",
			candidate.CompareStatus, candidate.Behind, candidate.Ahead)
	}

	edge := targetFor(targets, ReleaseChannelEdge)
	if edge.CompareTo != "" || edge.Behind != 0 || edge.Ahead != 0 {
		t.Errorf("edge must carry no distance, got compare_to=%q behind %d ahead %d",
			edge.CompareTo, edge.Behind, edge.Ahead)
	}
}

// A failed compare must leave the row with NO distance. The alternative — the
// zero value rendering as "0 behind" — asserts the tracks are level, which is
// the one answer that must never be guessed: an operator would read a stalled
// promotion as a healthy one.
func TestUnresolvedDistanceRendersAsNoDistanceNotZero(t *testing.T) {
	stubChannelDigests(t, map[string]string{
		"v4-latest": "sha256:v4",
		"stable":    "sha256:old",
		"candidate": "sha256:v4",
	})
	stubChannelRevisions(t, map[string]string{"stable": "55bd2bc"})
	// Deliberately NOT stubbing the compare: every lookup fails.
	stubChannelDistances(t, map[channelDistanceKey]channelDistance{})

	targets := resolveChannelTargets(map[string]string{"v4": "9eefa3a"}, testChannelLogger())

	stable := targetFor(targets, ReleaseChannelStable)
	if stable.CompareTo != "" || stable.CompareStatus != "" {
		t.Errorf("an unresolved compare must leave the row without a distance, got compare_to=%q status=%q",
			stable.CompareTo, stable.CompareStatus)
	}
	// The row must still be useful — losing the distance must not lose the SHA.
	if stable.SHA != "55bd2bc" {
		t.Errorf("stable SHA = %q, want 55bd2bc: a failed compare must not blank the row", stable.SHA)
	}
}

// "identical" carries no counts, so it is indistinguishable from an
// unresolved compare on the numbers alone. The status is what separates them,
// and the UI branches on it to say "in sync" rather than rendering nothing.
func TestIdenticalChannelsAreDistinguishableFromUnresolved(t *testing.T) {
	stubChannelDigests(t, map[string]string{
		"v4-latest": "sha256:v4",
		"stable":    "sha256:v4",
		"candidate": "sha256:v4",
	})
	stubChannelDistances(t, map[channelDistanceKey]channelDistance{})

	targets := resolveChannelTargets(map[string]string{"v4": "9eefa3a"}, testChannelLogger())

	stable := targetFor(targets, ReleaseChannelStable)
	if stable.CompareStatus != "identical" {
		t.Errorf("two channels on the same commit must compare identical, got %q", stable.CompareStatus)
	}
	if stable.Behind != 0 || stable.Ahead != 0 {
		t.Errorf("identical channels must have no commits between them, got behind %d ahead %d",
			stable.Behind, stable.Ahead)
	}
	if stable.CompareTo != ReleaseChannelCandidate {
		t.Errorf("identical row must still name what it was compared against, got %q", stable.CompareTo)
	}
}

// Same commits, one API call. The pair is immutable, so re-resolving it on
// every dashboard refresh would spend rate limit to re-learn a fixed answer.
func TestChannelDistanceIsCachedPerCommitPair(t *testing.T) {
	resetChannelDistanceCache()
	calls := 0
	orig := fetchCommitCompareCounts
	fetchCommitCompareCounts = func(base, head string, _ *slog.Logger) (channelDistance, error) {
		calls++
		return channelDistance{Status: "behind", Behind: 4}, nil
	}
	t.Cleanup(func() {
		fetchCommitCompareCounts = orig
		resetChannelDistanceCache()
	})

	for i := 0; i < 5; i++ {
		d := resolveChannelDistance("9eefa3a", "55bd2bc", testChannelLogger())
		if d.Behind != 4 {
			t.Fatalf("call %d returned behind %d, want 4", i, d.Behind)
		}
	}
	if calls != 1 {
		t.Errorf("compare API called %d times for one immutable pair, want 1", calls)
	}

	// A moved channel is a NEW pair and must be resolved, not served the
	// previous channel's answer.
	if d := resolveChannelDistance("9eefa3a", "aaaaaaa", testChannelLogger()); d.Behind != 4 {
		t.Fatalf("unexpected distance for the new pair: %+v", d)
	}
	if calls != 2 {
		t.Errorf("a new commit pair must trigger a resolve, calls = %d want 2", calls)
	}
}

// The same commit is zero distance from itself without asking GitHub. Cheap,
// but it also keeps the common "all three channels on one build" state off the
// network entirely.
func TestSameCommitNeedsNoCompareCall(t *testing.T) {
	resetChannelDistanceCache()
	orig := fetchCommitCompareCounts
	fetchCommitCompareCounts = func(string, string, *slog.Logger) (channelDistance, error) {
		t.Error("comparing a commit against itself must not call the API")
		return channelDistance{}, fmt.Errorf("unreachable")
	}
	t.Cleanup(func() {
		fetchCommitCompareCounts = orig
		resetChannelDistanceCache()
	})

	if d := resolveChannelDistance("9eefa3a", "9eefa3a", testChannelLogger()); d.Status != "identical" {
		t.Errorf("a commit against itself is identical, got %q", d.Status)
	}
}

// An empty SHA must never be compared: it would ask GitHub about a ref that
// does not exist, and any answer attributed to it would be fiction.
func TestEmptySHAIsNeverCompared(t *testing.T) {
	resetChannelDistanceCache()
	orig := fetchCommitCompareCounts
	fetchCommitCompareCounts = func(string, string, *slog.Logger) (channelDistance, error) {
		t.Error("an unresolved channel must not be compared")
		return channelDistance{}, fmt.Errorf("unreachable")
	}
	t.Cleanup(func() {
		fetchCommitCompareCounts = orig
		resetChannelDistanceCache()
	})

	if d := resolveChannelDistance("", "55bd2bc", testChannelLogger()); d.Status != "" {
		t.Errorf("empty base must yield no distance, got %+v", d)
	}
	if d := resolveChannelDistance("9eefa3a", "", testChannelLogger()); d.Status != "" {
		t.Errorf("empty head must yield no distance, got %+v", d)
	}
}

// A channel whose upstream did not resolve must not be measured against the
// stage beyond it. Silently falling through to edge would label the number
// "vs candidate" while measuring something else.
func TestMissingUpstreamSkipsRatherThanSubstitutes(t *testing.T) {
	targets := []ChannelTarget{
		{Channel: ReleaseChannelStable, SHA: "55bd2bc"},
		{Channel: ReleaseChannelCandidate}, // did not resolve: no SHA
		{Channel: ReleaseChannelEdge, SHA: "bc41b53"},
	}
	resetChannelDistanceCache()
	orig := fetchCommitCompareCounts
	fetchCommitCompareCounts = func(base, head string, _ *slog.Logger) (channelDistance, error) {
		if base == "bc41b53" && head == "55bd2bc" {
			t.Error("stable must not be measured against edge when candidate is missing")
		}
		return channelDistance{Status: "behind", Behind: 1}, nil
	}
	t.Cleanup(func() {
		fetchCommitCompareCounts = orig
		resetChannelDistanceCache()
	})

	annotateChannelDistances(targets, testChannelLogger())

	if targets[0].CompareTo != "" {
		t.Errorf("stable must carry no distance when candidate did not resolve, got %q", targets[0].CompareTo)
	}
}
