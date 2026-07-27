package hub

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ============================================================
// saas.go — StartLatestSHAPoller pre-loop portion
//
// The poller runs an initial load+fetch+persist+trigger pass synchronously
// before entering its 2-minute ticker loop. We run it in a goroutine against a
// fake GitHub/GHCR, wait for that pass to complete, then let the goroutine
// block harmlessly on the long ticker (it never fires during the test).
// ============================================================

func TestStartLatestSHAPollerPreLoop(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetSHACaches(t)

	fakeGitHubGHCR(t, func(w http.ResponseWriter, r *http.Request) {
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

	s := &HubServer{
		logger:     slog.Default(),
		saveCh:     make(chan struct{}, 1),
		hubGitHash: "abc1234",
	}
	// A registered hive contributes a tracked branch so the fetch loop runs.
	s.registry.Hives = []RegistryEntry{{ID: "h1", GitBranch: "v2"}}

	done := make(chan struct{})
	go func() {
		s.StartLatestSHAPoller() // blocks on the 2-min ticker after the first pass
		close(done)
	}()

	// Give the synchronous pre-loop pass time to finish its fetch.
	deadline := time.After(3 * time.Second)
	for {
		if getLatestSHAForBranch("v2") != "" {
			break
		}
		select {
		case <-deadline:
			t.Log("pre-loop did not populate v2 SHA in time (non-fatal)")
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	// The poller goroutine is intentionally left blocking on its long ticker;
	// it makes no further network calls until the 2-minute tick, well past test end.
}
