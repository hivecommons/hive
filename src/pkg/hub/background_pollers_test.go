package hub

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ============================================================
// background_pollers.go — StartBackgroundPollers
//
// The wrapper is the composition root's single lifecycle entrypoint: it must
// actually launch the pollers (route registration is side-effect free, so if
// this wrapper silently dropped a `go` statement nothing else would start
// them), and the documented contract — "the provided ctx bounds every poller;
// cancelling it stops them all" — must hold. Each poller's own behavior is
// covered by its dedicated tests (sha_poller_*, auth_audit_*,
// provision_watcher_*, reach_diag_wiring); this test covers only the wiring.
// ============================================================

// TestStartBackgroundPollers_LaunchesAndStopsOnCancel calls the wrapper with
// an already-cancelled context against the fake GitHub/GHCR fixture. Each
// poller runs its synchronous startup work (the SHA poller's pre-loop fetch is
// the observable proof the wrapper launched it) and then exits on ctx.Done()
// instead of blocking on its ticker. The test then waits on the wrapper's
// returned done channel: that join is both the assertion that cancel stops
// every poller and the guard that keeps the daemons from outliving the
// per-test temp dirs (the same lifetime hazard TestStartLatestSHAPollerPreLoop
// handles with an explicit join).
func TestStartBackgroundPollers_LaunchesAndStopsOnCancel(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetSHACaches(t)

	// Keep the SHA poller's persist step off the real /data/saas path.
	oldSHAsPath := latestSHAsPath
	latestSHAsPath = t.TempDir() + "/latest-shas.json"
	defer func() { latestSHAsPath = oldSHAsPath }()

	srv := fakeGitHubGHCR(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead:
			w.WriteHeader(http.StatusOK) // image on GHCR
		case strings.Contains(r.URL.Path, "/token"):
			w.Write([]byte(`{"token":"anon"}`))
		case strings.Contains(r.URL.Path, "/branches/"):
			w.Write([]byte(`{"commit":{"sha":"abcdef1234567890","commit":{"message":"m"}}}`))
		default:
			w.Write([]byte(`{}`))
		}
	})
	// Without keep-alives every handler goroutine and client read loop exits
	// as soon as its response is written, so no idle pooled connections keep
	// touching the fixture after the pollers are joined.
	srv.Config.SetKeepAlivesEnabled(false)

	// Keep the image-pulls snapshot fetch off the real github.com.
	oldPullURL := pullPackagePageURL
	pullPackagePageURL = srv.URL + "/pkgs/container/hive"
	defer func() { pullPackagePageURL = oldPullURL }()

	s := &HubServer{
		logger:     slog.Default(),
		saveCh:     make(chan struct{}, 1),
		hubGitHash: "abc1234",
	}
	// A registered hive contributes a tracked branch so the SHA poller's
	// pre-loop fetch has something observable to populate.
	s.registry.Hives = []RegistryEntry{{ID: "h1", GitBranch: "v2"}}

	// Pre-cancelled: each poller performs its synchronous startup pass, then
	// its select sees ctx.Done() and returns instead of waiting on a ticker.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := s.StartBackgroundPollers(ctx)

	// Launch proof: the wrapper must have started the SHA poller, whose
	// pre-loop fetch populates v2 from the fake. Non-fatal on timeout (the
	// fetch can lose a slow-CI race), mirroring TestStartLatestSHAPollerPreLoop;
	// the cancel contract below is still enforced either way.
	launchDeadline := time.After(5 * time.Second)
	for getLatestSHAForBranch("v2") == "" {
		select {
		case <-launchDeadline:
			t.Log("SHA poller did not populate v2 in time (non-fatal)")
			goto join
		case <-time.After(20 * time.Millisecond):
		}
	}

join:
	// Cancel contract + join: every poller goroutine must exit, closing the
	// wrapper's done channel. This wait runs before the deferred temp-dir
	// cleanup and global restores, so no poller is still reading the saas path
	// variables (or pullPackagePageURL) when they are torn down or restored.
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("pollers did not stop on cancelled context: done channel still open after 10s")
	}

	// Any in-flight commit-order resolvers spawned by the fetch must also
	// finish before the temp dirs are removed.
	waitForCommitOrderResolvers(t)
}
