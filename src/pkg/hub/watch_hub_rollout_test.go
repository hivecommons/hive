package hub

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// hivecommons/hive#7220 — watchHubRollout's SUCCESS path.
//
// watchHubRollout is the detect-and-report half of hub self-upgrade safety. Its
// failure half was already covered via hubRolloutFailureReason /
// setHubUpgradeFault, but the success path — rollout becomes Ready, the fault
// is cleared, the loop exits — was not, because the loop hardcoded a 15s sleep
// and a kubectl exec. A regression there is silent: a stale fault stays on the
// dashboard, or the watcher exits early and a genuinely stuck upgrade is never
// reported.
//
// watchHubRolloutWithInterval takes the timeout, poll interval and readiness
// check as parameters, so these drive both branches deterministically in
// milliseconds.

// A rollout that becomes Ready must clear any fault and return promptly —
// without waiting out the timeout.
func TestWatchHubRolloutSuccessClearsFault(t *testing.T) {
	s := bulkTestHub(t)
	s.setHubUpgradeFault("previous attempt was stuck")

	var calls int32
	ready := func() bool {
		atomic.AddInt32(&calls, 1)
		return true
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.watchHubRolloutWithInterval("sha1", "img:sha1", 5*time.Second, time.Millisecond, ready)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("watchHubRolloutWithInterval must return as soon as the rollout is Ready, not wait out the timeout")
	}

	if got := s.HubUpgradeFault(); got != "" {
		t.Fatalf("a successful rollout must clear the upgrade fault, got %q", got)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("a rollout that is Ready on the first poll must be checked exactly once, got %d", n)
	}
}

// The timeout branch must name the pod-level reason, not just "timed out" —
// that string is what an operator sees on the dashboard.
func TestWatchHubRolloutTimeoutSetsFaultWithReason(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"echo 'ImagePullBackOff Back-off pulling image \"ghcr.io/hivecommons/hive-hub:sha2\"'\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	s := bulkTestHub(t)
	never := func() bool { return false }

	s.watchHubRolloutWithInterval("sha2", "img:sha2", 20*time.Millisecond, time.Millisecond, never)

	got := s.HubUpgradeFault()
	if got == "" {
		t.Fatal("a rollout that never becomes Ready must record a user-visible fault")
	}
	for _, want := range []string{"img:sha2", "ImagePullBackOff", "still serving the previous image"} {
		if !strings.Contains(got, want) {
			t.Fatalf("fault must contain %q so the operator sees the real cause, got %q", want, got)
		}
	}
	// The wrapper's 5m constant must not leak into a message describing a 20ms
	// wait — the reported duration has to be the one actually waited.
	if strings.Contains(got, hubRolloutWatchTimeout.String()) {
		t.Fatalf("fault must report the timeout actually used, not the package constant, got %q", got)
	}
}

// A rollout that is slow but succeeds inside the window still clears a fault
// left by an earlier attempt. This is the case that distinguishes "polls until
// Ready" from "checks once and gives up".
func TestWatchHubRolloutLateSuccessClearsEarlierFault(t *testing.T) {
	s := bulkTestHub(t)
	s.setHubUpgradeFault("earlier attempt stuck")

	const readyAfter = 3
	var calls int32
	ready := func() bool { return atomic.AddInt32(&calls, 1) >= readyAfter }

	s.watchHubRolloutWithInterval("sha3", "img:sha3", 5*time.Second, time.Millisecond, ready)

	if got := s.HubUpgradeFault(); got != "" {
		t.Fatalf("a late success must still clear the earlier fault, got %q", got)
	}
	if n := atomic.LoadInt32(&calls); n != readyAfter {
		t.Fatalf("watcher must keep polling until Ready and stop immediately after: want %d checks, got %d", readyAfter, n)
	}
}

// The production wrapper must keep passing the real constants. Without this, a
// seam introduced for testability could silently ship a test-only timeout.
func TestWatchHubRolloutConstantsArePlausible(t *testing.T) {
	if hubRolloutPollInterval <= 0 || hubRolloutWatchTimeout <= 0 {
		t.Fatal("rollout poll interval and watch timeout must both be positive")
	}
	if hubRolloutPollInterval >= hubRolloutWatchTimeout {
		t.Fatalf("poll interval (%s) must be shorter than the watch timeout (%s), or the loop can only ever check once",
			hubRolloutPollInterval, hubRolloutWatchTimeout)
	}
}
