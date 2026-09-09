package hub

import (
	"github.com/hivecommons/hive/internal/testutil"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// A spoke on a branch tag is measured against that branch's tip — the same
// answer the badge always gave, and the only one that is right for v4-latest.
func TestBehindTargetForBranchSpokeUsesBranchTip(t *testing.T) {
	stubBranchHead(t, "v4", "526ef71")
	calls := stubChannelRevisions(t, map[string]string{"stable": "77ba848"})
	s := &HubServer{logger: targetingLogger()}

	got := s.behindTargetFor(&RegistryEntry{GitBranch: "v4", ImageRef: "ghcr.io/hivecommons/hive:v4-latest"}, "")
	if got.SHA != "526ef71" || got.Channel {
		t.Errorf("behindTargetFor = %+v, want branch tip 526ef71, not a channel", got)
	}
	if got.Ref != "v4 tip" {
		t.Errorf("Ref = %q, want %q", got.Ref, "v4 tip")
	}
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Errorf("a branch spoke must not cost a registry round-trip, got %d", n)
	}
}

// The operator's case: 50 spokes on :stable at exactly the commit :stable
// carries, badged "128 behind · Queued for auto-upgrade" against v4 HEAD while
// the upgrade engine (correctly) refused to move them. Against their own
// channel they are current, so the target must be the channel's commit.
func TestBehindTargetForStableSpokeUsesChannelCommit(t *testing.T) {
	stubBranchHead(t, "v4", "87b2b02")
	stubChannelRevisions(t, map[string]string{"stable": "df9b867"})
	s := &HubServer{logger: targetingLogger()}
	// Prime the cache the way the reconcile tick does.
	if sha := channelRevisionSHA("stable", s.logger); sha != "df9b867" {
		t.Fatalf("channel prime = %q", sha)
	}

	got := s.behindTargetFor(&RegistryEntry{GitBranch: "v4", ImageRef: "ghcr.io/hivecommons/hive:stable"}, "stable")
	if got.SHA != "df9b867" || !got.Channel {
		t.Errorf("behindTargetFor = %+v, want the channel's commit df9b867 (branch tip is 87b2b02)", got)
	}
	if got.Ref != ":stable" {
		t.Errorf("Ref = %q, want %q so the tooltip names the channel", got.Ref, ":stable")
	}
}

// Read paths must never block a dashboard render on GHCR: with nothing cached
// the target is simply unknown (no count rendered), and no resolver runs.
func TestBehindTargetForStableSpokeIsCacheOnly(t *testing.T) {
	stubBranchHead(t, "v4", "87b2b02")
	calls := stubChannelRevisions(t, map[string]string{"stable": "df9b867"})
	s := &HubServer{logger: targetingLogger()}

	got := s.behindTargetFor(&RegistryEntry{GitBranch: "v4", ImageRef: "ghcr.io/hivecommons/hive:stable"}, "")
	if got.SHA != "" {
		t.Errorf("SHA = %q, want empty — branch tip must never stand in for an unresolved channel", got.SHA)
	}
	if !got.Channel || got.Ref != ":stable" {
		t.Errorf("behindTargetFor = %+v, want an unresolved :stable target", got)
	}
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Errorf("behindTargetFor must be cache-only, but the resolver ran %d time(s)", n)
	}
}

func TestBehindTargetForNilEntry(t *testing.T) {
	s := &HubServer{logger: targetingLogger()}
	if got := s.behindTargetFor(nil, ""); got.SHA != "" || got.Ref != "" {
		t.Errorf("nil entry must resolve to nothing, got %+v", got)
	}
}

// commitsBehindTarget is the generalized counter behind commitsBehindStableV4:
// same cache, same dispatch, explicit head. Same commit short-circuits with no
// dispatch, whatever the branch tip is.
func TestCommitsBehindTargetSameCommitShortCircuits(t *testing.T) {
	resetCommitBehindState(t)
	if got, known := commitsBehindTarget("df9b867", "df9b867", nil); !known || got != 0 {
		t.Fatalf("same-commit = %d,%v; want 0,true", got, known)
	}
	commitBehindMu.Lock()
	inFlight := len(commitBehindInFlight)
	commitBehindMu.Unlock()
	if inFlight != 0 {
		t.Fatal("same-commit path must not dispatch a compare")
	}
}

// The compare is dispatched against the GIVEN head — the channel's commit —
// not the branch tip that commitsBehindStableV4 would have used.
func TestCommitsBehindTargetDispatchesAgainstGivenHead(t *testing.T) {
	resetCommitBehindState(t)
	commitBehindMu.Lock()
	fetchCommitBehindCount = func(base, head string, _ *slog.Logger) (int, bool, error) {
		if base != "df9b867" || head != "21b30e8" {
			t.Errorf("compare dispatched with %s...%s; want df9b867...21b30e8", base, head)
		}
		return 3, true, nil
	}
	commitBehindMu.Unlock()

	if _, known := commitsBehindTarget("df9b867", "21b30e8", nil); known {
		t.Fatal("first call must report unknown while the compare is in flight")
	}
	got := testutil.EventuallyValue(t, 2*time.Second, func() (int, bool) {
		return commitsBehindTarget("df9b867", "21b30e8", nil)
	}, "compare result never landed in the cache")
	if got != 3 {
		t.Fatalf("cached count = %d, want 3", got)
	}
}
