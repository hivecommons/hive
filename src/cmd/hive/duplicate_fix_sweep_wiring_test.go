package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// Wiring tests for runDuplicateFixSweepIfDue (hivecommons/hive#14799): the
// throttle, the scanner.skip_issues_with_open_pr switch, lastRun stamping and
// the error path. The resolver's decisions live in pkg/github.

func newDuplicateFixAPI(t *testing.T, status int, requests *atomic.Int64) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/testorg/widget/pulls", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestRunDuplicateFixSweepIfDue_NilClientIsNoop(t *testing.T) {
	var lastRun time.Time
	runDuplicateFixSweepIfDue(context.Background(), nil, nil, nil, &lastRun, sweepTestLogger(nil))
	if !lastRun.IsZero() {
		t.Fatalf("nil client must not stamp lastRun, got %v", lastRun)
	}
}

func TestRunDuplicateFixSweepIfDue_SwitchOffIsNoop(t *testing.T) {
	var requests atomic.Int64
	srv := newDuplicateFixAPI(t, http.StatusOK, &requests)
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget"}, sweepTestLogger(nil))
	off := false
	cfg := &config.Config{}
	cfg.Scanner.SkipIssuesWithOpenPR = &off

	var lastRun time.Time
	runDuplicateFixSweepIfDue(context.Background(), ghClient, cfg, nil, &lastRun, sweepTestLogger(nil))
	if requests.Load() != 0 || !lastRun.IsZero() {
		t.Fatalf("sweep must be off with scanner.skip_issues_with_open_pr=false (requests=%d lastRun=%v)", requests.Load(), lastRun)
	}
}

func TestRunDuplicateFixSweepIfDue_ThrottledWithinInterval(t *testing.T) {
	var requests atomic.Int64
	srv := newDuplicateFixAPI(t, http.StatusOK, &requests)
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget"}, sweepTestLogger(nil))

	lastRun := time.Now()
	before := lastRun
	runDuplicateFixSweepIfDue(context.Background(), ghClient, &config.Config{}, nil, &lastRun, sweepTestLogger(nil))
	if requests.Load() != 0 || !lastRun.Equal(before) {
		t.Fatalf("throttled sweep must make no API calls and keep lastRun (requests=%d)", requests.Load())
	}
}

func TestRunDuplicateFixSweepIfDue_DueSweepStampsLastRun(t *testing.T) {
	var requests atomic.Int64
	srv := newDuplicateFixAPI(t, http.StatusOK, &requests)
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget"}, sweepTestLogger(nil))

	var lastRun time.Time
	start := time.Now()
	runDuplicateFixSweepIfDue(context.Background(), ghClient, nil, nil, &lastRun, sweepTestLogger(nil))
	if requests.Load() == 0 || lastRun.Before(start) {
		t.Fatalf("due sweep must list PRs and stamp lastRun (requests=%d lastRun=%v)", requests.Load(), lastRun)
	}
	runDuplicateFixSweepIfDue(context.Background(), ghClient, nil, nil, nil, sweepTestLogger(nil))
}

func TestRunDuplicateFixSweepIfDue_ErrorLogged(t *testing.T) {
	var requests atomic.Int64
	srv := newDuplicateFixAPI(t, http.StatusInternalServerError, &requests)
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget"}, sweepTestLogger(nil))

	var buf strings.Builder
	var lastRun time.Time
	runDuplicateFixSweepIfDue(context.Background(), ghClient, nil, nil, &lastRun, sweepTestLogger(&buf))
	if !strings.Contains(buf.String(), "duplicate-fix sweep failed") {
		t.Fatalf("sweep failure must be logged, log:\n%s", buf.String())
	}
	if lastRun.IsZero() {
		t.Fatal("failed sweep must still stamp lastRun so it is not retried every cycle")
	}
}
