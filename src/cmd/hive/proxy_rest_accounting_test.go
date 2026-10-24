package main

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/crux/proxy"
)

// The adapter is pkg/github's package-level REST accounting behind the
// proxy's seam: an injected (daemon-credential) record on the hive's own core
// bucket moves the shared snapshot, and the adapter reads back exactly what
// pkg/github reports. The store is process-global, so this asserts the
// injected observation is visible rather than resetting it.
func TestGitHubRESTAccountingAdapterTracksSharedCoreBucket(t *testing.T) {
	var acct proxy.GitHubRESTAccounting = githubRESTAccounting{}
	reset := time.Now().Add(time.Hour).Unix()
	h := http.Header{}
	h.Set("X-RateLimit-Resource", "core")
	h.Set("X-RateLimit-Remaining", "1234")
	h.Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))

	acct.RecordRESTRequest("agent:scanner", http.MethodGet, "/repos/o/r/pulls", http.StatusOK, h)
	acct.RecordInjectedRESTRequest("agent:scanner", http.MethodGet, "/repos/o/r/pulls", http.StatusOK, h)
	remaining, resetAt, ok := acct.CoreRateLimitSnapshot()
	if !ok || remaining != 1234 || resetAt.Unix() != reset {
		t.Fatalf("snapshot = %d, %v, %v; want 1234 at %d from the injected record", remaining, resetAt, ok, reset)
	}
	if want, _, _ := github.CoreRateLimitSnapshot(); want != remaining {
		t.Fatalf("adapter snapshot %d differs from pkg/github's %d", remaining, want)
	}
}
