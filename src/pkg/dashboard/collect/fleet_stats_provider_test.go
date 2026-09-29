package collect

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// #9621: a collector built before the hive had a usable App (hosted spokes get
// theirs over the heartbeat) must pick up the client once it arrives, and a
// rebuilt client must replace the old one, without a restart.
func TestFleetStatsCollector_ProviderFollowsClientSwap(t *testing.T) {
	var cur atomic.Pointer[ghpkg.Client]
	fc := NewFleetStatsCollector(nil, "bot", "org", slog.Default())
	fc.SetGitHubClientProvider(cur.Load)

	if !fc.hasClientSource() {
		t.Fatal("a provider must count as a client source, or Start refuses to run on an App-less boot")
	}
	// No client yet: the collect is skipped, not recorded as zeros.
	fc.collect(context.Background())
	if _, ready := fc.Snapshot(); ready {
		t.Fatal("collect with no client yet marked the snapshot ready")
	}

	cur.Store(newFleetTestClient(t, map[string]int{"is:merged merged:>=": 7}))
	fc.collect(context.Background())
	snap, ready := fc.Snapshot()
	if !ready || snap.PRsMerged != 7 {
		t.Fatalf("after first client: snapshot = %+v ready=%v, want PRsMerged 7", snap, ready)
	}

	// A rebuilt client answers differently; the collector must use it.
	cur.Store(newFleetTestClient(t, map[string]int{"is:merged merged:>=": 11}))
	fc.collect(context.Background())
	if snap, _ := fc.Snapshot(); snap.PRsMerged != 11 {
		t.Fatalf("after rebuild: PRsMerged = %d, want 11 (collector kept the old client)", snap.PRsMerged)
	}
}

func TestFleetStatsCollector_ProviderOverridesConstructorClient(t *testing.T) {
	boot := newFleetTestClient(t, map[string]int{"is:merged merged:>=": 1})
	rebuilt := newFleetTestClient(t, map[string]int{"is:merged merged:>=": 2})
	fc := NewFleetStatsCollector(boot, "bot", "org", slog.Default())
	if fc.client() != boot {
		t.Fatal("without a provider the constructor client must be used")
	}
	fc.SetGitHubClientProvider(func() *ghpkg.Client { return rebuilt })
	if fc.client() != rebuilt {
		t.Fatal("provider did not override the captured constructor client")
	}
	var nilFc *FleetStatsCollector
	nilFc.SetGitHubClientProvider(func() *ghpkg.Client { return rebuilt }) // must not panic
}

func TestFleetStatsCollector_StartWithProviderRunsUntilCancel(t *testing.T) {
	origJitter := fleetStatsStartupJitterMax
	fleetStatsStartupJitterMax = 0
	t.Cleanup(func() { fleetStatsStartupJitterMax = origJitter })

	var calls atomic.Int32
	fc := NewFleetStatsCollector(nil, "bot", "org", slog.Default())
	fc.SetGitHubClientProvider(func() *ghpkg.Client { calls.Add(1); return nil })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { fc.Start(ctx); close(done) }()
	cancel()
	<-done
	// Start must have entered the loop (one up-front collect consulting the
	// provider) instead of returning early for lack of a constructor client.
	if calls.Load() == 0 {
		t.Fatal("Start with a provider returned before consulting it; an App-less boot would never collect")
	}
}
