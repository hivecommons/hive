package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/linearagent"
	"github.com/hivecommons/hive/pkg/tokens"
)

// Covers previously 0%-covered exported funcs in pkg/dashboard:
// SetReleaseChannel (api.go), LinearAgentPROpened (api_linear_agent.go),
// Server.sampleMetricsInputs + StartContributeMetrics (contribute_metrics.go),
// and RepoCostCollector.Start + CollectedAt (repo_cost_collector.go).

// ---- SetReleaseChannel ----

// TestSetReleaseChannel_VersionResponse asserts the channel set via
// SetReleaseChannel shows up as /api/version's "channel" field, and that an
// empty channel omits the field entirely (the documented "" contract for
// branch-tag / SHA-pinned deployments).
func TestSetReleaseChannel_VersionResponse(t *testing.T) {
	origChannel := versionChannel
	t.Cleanup(func() { versionChannel = origChannel })

	s, _ := apiServer(t)

	SetReleaseChannel("stable")
	result := decodeJSON(t, doGet(s, "/api/version"))
	if result["channel"] != "stable" {
		t.Errorf("channel = %v, want %q", result["channel"], "stable")
	}

	SetReleaseChannel("")
	result = decodeJSON(t, doGet(s, "/api/version"))
	if _, present := result["channel"]; present {
		t.Errorf("channel must be omitted when SetReleaseChannel(%q) was called, got %v", "", result["channel"])
	}
}

// TestSetReleaseChannel_DoesNotAffectUpstreamBranch pins the display-only
// contract: the channel badge must not change which branch the self-version
// check compares against.
func TestSetReleaseChannel_DoesNotAffectUpstreamBranch(t *testing.T) {
	origChannel := versionChannel
	origBranch := versionBranch
	t.Cleanup(func() {
		versionChannel = origChannel
		versionBranch = origBranch
	})

	SetGitBranch("v4")
	before := upstreamBranch()
	SetReleaseChannel("edge")
	if got := upstreamBranch(); got != before {
		t.Errorf("upstreamBranch changed from %q to %q after SetReleaseChannel — channel must be display-only", before, got)
	}
}

// ---- LinearAgentPROpened ----

// prOpenedServer builds a Server whose lazy linearAgent() returns the given
// pre-built service, so the test never touches linearagent.DefaultStorePath()
// on disk (hermetic: no /data reads).
func prOpenedServer(t *testing.T, svc *linearAgentService) *Server {
	t.Helper()
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.linearAgentSvc = svc
	return s
}

// TestLinearAgentPROpened_NilResponder asserts the hook is a safe no-op when
// the Linear agent service exists but has no responder (store-open failure
// path) — the pr-request watcher calls this unconditionally on every PR.
func TestLinearAgentPROpened_NilResponder(t *testing.T) {
	s := prOpenedServer(t, &linearAgentService{})
	// Must not panic.
	s.LinearAgentPROpened("quality", "org/repo", 42, "https://example.test/pr/42")
}

// TestLinearAgentPROpened_NoActiveSession asserts the hook delegates to the
// responder and returns cleanly when the agent has no active Linear session
// (the overwhelmingly common case: most PRs are not Linear-delegated work).
func TestLinearAgentPROpened_NoActiveSession(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	responder := linearagent.NewResponder(nil, nil, nil, linearagent.NewTracker(), logger)
	s := prOpenedServer(t, &linearAgentService{responder: responder})
	// Empty tracker -> HandlePROpened's ActiveSessionForAgent miss -> no-op.
	s.LinearAgentPROpened("quality", "org/repo", 7, "https://example.test/pr/7")
}

// ---- Server.sampleMetricsInputs ----

// TestSampleMetricsInputs_NoHub asserts the sampler is safe with no
// contribute hub wired (queue/fleet stay zero) and reads per-contributor
// cumulative totals from the contributors dir, skipping profiles with no
// GitHub username.
func TestSampleMetricsInputs_NoHub(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)

	writeProfile := func(name string, p ContributorProfile) {
		t.Helper()
		data, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal profile: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatalf("write profile: %v", err)
		}
	}
	writeProfile("alice.json", ContributorProfile{
		GitHubUsername: "alice",
		ContributorID:  "c-alice",
		TasksCompleted: 5,
	})
	// No GitHubUsername -> must be skipped by the sampler.
	writeProfile("ghost.json", ContributorProfile{
		ContributorID:  "c-ghost",
		TasksCompleted: 99,
	})

	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	before := time.Now()
	sample := s.sampleMetricsInputs()

	if sample.queueDepth != 0 || sample.fleetSize != 0 {
		t.Errorf("queueDepth/fleetSize = %d/%d, want 0/0 with no contribute hub", sample.queueDepth, sample.fleetSize)
	}
	if got := sample.userTotals["alice"]; got != 5 {
		t.Errorf("userTotals[alice] = %d, want 5", got)
	}
	if len(sample.userTotals) != 1 {
		t.Errorf("userTotals = %v, want only alice (username-less profiles skipped)", sample.userTotals)
	}
	if sample.now.Before(before) {
		t.Errorf("sample.now = %v predates the call", sample.now)
	}
}

// ---- Server.StartContributeMetrics ----

// TestStartContributeMetrics_RollsUpAndStopsOnCancel asserts the startup
// wiring actually drives the hourly rollup loop with the Server as sampler
// (buckets appear) and that cancelling ctx shuts the goroutine down (no leak:
// bucket count stops growing).
func TestStartContributeMetrics_RollsUpAndStopsOnCancel(t *testing.T) {
	t.Setenv("HIVE_METRICS_FILE", filepath.Join(t.TempDir(), "metrics.json"))
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())

	origInterval := metricsRollupInterval
	metricsRollupInterval = 5 * time.Millisecond
	t.Cleanup(func() { metricsRollupInterval = origInterval })

	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.StartContributeMetrics(ctx)

	store := s.contributeMetricsStore()
	deadline := time.Now().Add(5 * time.Second)
	for {
		store.mu.Lock()
		n := len(store.queueDepth)
		store.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("StartContributeMetrics never rolled up a bucket")
		}
		time.Sleep(2 * time.Millisecond)
	}

	cancel()
	// Give the loop time to observe cancellation, then verify it stopped.
	time.Sleep(20 * time.Millisecond)
	store.mu.Lock()
	after := len(store.queueDepth)
	store.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	store.mu.Lock()
	final := len(store.queueDepth)
	store.mu.Unlock()
	if final != after {
		t.Errorf("rollup kept running after ctx cancel: buckets %d -> %d", after, final)
	}
}

// ---- RepoCostCollector.Start / CollectedAt ----

// TestRepoCostCollector_StartCollectsAndStops asserts Start performs the
// up-front collect (Snapshot ready, CollectedAt set), keeps collecting on the
// ticker, and exits on ctx cancel.
func TestRepoCostCollector_StartCollectsAndStops(t *testing.T) {
	origInterval := repoCostCollectInterval
	repoCostCollectInterval = 5 * time.Millisecond
	t.Cleanup(func() { repoCostCollectInterval = origInterval })

	audit := &fakeFixedAudit{}
	rc := NewRepoCostCollector(audit, &fakeTokensSummary{summary: &tokens.AggregateSummary{}}, "", nil)

	if !rc.CollectedAt().IsZero() {
		t.Fatalf("CollectedAt = %v before any collect, want zero", rc.CollectedAt())
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		rc.Start(ctx)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ready := rc.Snapshot(); ready {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("Start never produced a ready snapshot")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if rc.CollectedAt().IsZero() {
		t.Error("CollectedAt still zero after a successful collect")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after ctx cancel — goroutine leak")
	}
}

// TestRepoCostCollector_StartInertWithoutInputs asserts the documented inert
// contract: Start returns immediately (rather than spinning a ticker) when
// either side of the audit/tokens join is missing, and on a nil receiver.
func TestRepoCostCollector_StartInertWithoutInputs(t *testing.T) {
	cases := map[string]*RepoCostCollector{
		"nil collector": nil,
		"nil audit":     NewRepoCostCollector(nil, &fakeTokensSummary{}, "", nil),
		"nil tokens":    NewRepoCostCollector(&fakeFixedAudit{}, nil, "", nil),
	}
	for name, rc := range cases {
		done := make(chan struct{})
		go func() {
			rc.Start(context.Background())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: Start must return immediately when inert", name)
		}
		if !rc.CollectedAt().IsZero() {
			t.Errorf("%s: CollectedAt = %v, want zero (no collect must have run)", name, rc.CollectedAt())
		}
	}
}
