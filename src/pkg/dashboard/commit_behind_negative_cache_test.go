package dashboard

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// #7430: while the quota was exhausted, the status builder retried the
// stable-tip compare on every build (~1/s), each attempt a doomed request
// and a warning line against the very limit the hive was waiting out. A
// failed compare is now remembered and not retried for commitBehindRetryAfter.
func TestCommitsBehindStableTip_FailureIsNotRetriedEveryBuild(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Any failure will do (the live one was a rate-limit 403); a 502 is
		// used here so the shared post-rate-limit pacing is not engaged for
		// the rest of this package's tests.
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"message":"bad gateway"}`))
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	s.deps = &Dependencies{
		GHClient: github.NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, logger),
		Ctx:      context.Background(),
		Logger:   logger,
	}

	for i := 0; i < 5; i++ {
		if n, ok := s.commitsBehindStableTip("5e7d2a9aaaaaaa", "1620526bbbbbbb"); ok || n != 0 {
			t.Fatalf("build %d: compare reported (%d, %v) against a failing API", i, n, ok)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("GitHub was asked %d times in 5 status builds, want 1: a failed compare must wait out commitBehindRetryAfter", got)
	}

	// Once the retry window has passed, it is asked again.
	s.versionMu.Lock()
	for k := range s.commitBehindFailedAt {
		s.commitBehindFailedAt[k] = time.Now().Add(-commitBehindRetryAfter - time.Second)
	}
	s.versionMu.Unlock()
	s.commitsBehindStableTip("5e7d2a9aaaaaaa", "1620526bbbbbbb")
	if got := calls.Load(); got != 2 {
		t.Fatalf("after the retry window GitHub was asked %d times total, want 2", got)
	}
}
