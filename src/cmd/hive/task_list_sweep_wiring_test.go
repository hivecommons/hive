package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// Tests for the eval-loop task-list sweep wiring in cmd/hive/main.go:
// runTaskListSweepIfDue (throttle, lastRun stamping, nil-client no-op, nil
// lastRun pointer, error handling, completion logging). The sweep DECISIONS —
// which issues qualify as hive-filed, box counting, exempt labels — are covered
// in pkg/github/task_list_sweep_test.go; these tests cover only the main.go
// wiring around SweepCompletedTaskListIssues, mirroring the
// runAutoMergeSweepIfDue wiring tests in automerge_sweep_wiring_test.go.

// hiveFiledTickedIssueJSON is one open, hive-filed (attribution trailer in the
// body) issue whose task list is fully ticked — the exact shape the sweep
// closes: list → comment → close.
const hiveFiledTickedIssueJSON = `[{
	"number": 7,
	"state": "open",
	"user": {"login": "hive-bot", "type": "Bot"},
	"body": "- [x] one\n- [x] two\n\n— hive: scanner via test"
}]`

// newTaskListSweepAPI serves the three endpoints a successful sweep of repo
// testorg/widget touches: the open-issue listing, the audit comment, and the
// close edit. Every request is counted so throttle tests can assert "no API
// call"; comment/close hits are counted separately so the due-sweep test can
// assert the wiring drove a real closure.
func newTaskListSweepAPI(t *testing.T, issuesJSON string, issuesStatus int, requests, comments, closes *atomic.Int64) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/testorg/widget/issues", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if issuesStatus != http.StatusOK {
			w.WriteHeader(issuesStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(issuesJSON))
	})
	mux.HandleFunc("/repos/testorg/widget/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// The sweep LISTS existing comments (marker scan) before it creates
		// one; only the POST is the audit comment the tests count.
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		comments.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id": 1}`))
	})
	mux.HandleFunc("/repos/testorg/widget/issues/7", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		closes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number": 7, "state": "closed"}`))
	})
	mux.HandleFunc("/repos/testorg/widget/pulls", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		// One recently merged PR whose body references issue #7 — the
		// merged-PR gate (#7071) requires at least one such PR before the
		// sweep may close a fully ticked issue.
		now := time.Now().UTC().Format(time.RFC3339)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{
			"number": 8,
			"state": "closed",
			"title": "land the widget work",
			"body": "Fixes #7",
			"merged_at": "` + now + `",
			"updated_at": "` + now + `"
		}]`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestRunTaskListSweepIfDue_NilClientIsNoop(t *testing.T) {
	var lastRun time.Time
	runTaskListSweepIfDue(context.Background(), nil, nil, &lastRun, sweepTestLogger(nil))
	if !lastRun.IsZero() {
		t.Fatalf("nil client must not stamp lastRun, got %v", lastRun)
	}
}

func TestRunTaskListSweepIfDue_ThrottledWithinInterval(t *testing.T) {
	var requests, comments, closes atomic.Int64
	srv := newTaskListSweepAPI(t, `[]`, http.StatusOK, &requests, &comments, &closes)
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget"}, sweepTestLogger(nil))

	lastRun := time.Now()
	before := lastRun
	runTaskListSweepIfDue(context.Background(), ghClient, nil, &lastRun, sweepTestLogger(nil))

	if got := requests.Load(); got != 0 {
		t.Fatalf("throttled sweep must make no API calls, got %d", got)
	}
	if !lastRun.Equal(before) {
		t.Fatalf("throttled sweep must not advance lastRun: before=%v after=%v", before, lastRun)
	}
}

func TestRunTaskListSweepIfDue_DueSweepClosesTickedIssueAndStampsLastRun(t *testing.T) {
	var requests, comments, closes atomic.Int64
	srv := newTaskListSweepAPI(t, hiveFiledTickedIssueJSON, http.StatusOK, &requests, &comments, &closes)
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget"}, sweepTestLogger(nil))

	var buf strings.Builder
	var lastRun time.Time
	start := time.Now()
	runTaskListSweepIfDue(context.Background(), ghClient, nil, &lastRun, sweepTestLogger(&buf))

	if comments.Load() != 1 {
		t.Fatalf("due sweep must post exactly one audit comment, got %d", comments.Load())
	}
	if closes.Load() != 1 {
		t.Fatalf("due sweep must close exactly one issue, got %d", closes.Load())
	}
	if lastRun.Before(start) {
		t.Fatalf("due sweep must stamp lastRun, got %v", lastRun)
	}
	if !strings.Contains(buf.String(), "task-list sweep complete") {
		t.Fatalf("due sweep with activity must log completion, got: %s", buf.String())
	}
}

func TestRunTaskListSweepIfDue_NilLastRunStillSweeps(t *testing.T) {
	var requests, comments, closes atomic.Int64
	srv := newTaskListSweepAPI(t, `[]`, http.StatusOK, &requests, &comments, &closes)
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget"}, sweepTestLogger(nil))

	runTaskListSweepIfDue(context.Background(), ghClient, nil, nil, sweepTestLogger(nil))

	if requests.Load() == 0 {
		t.Fatal("sweep with nil lastRun pointer must still run (and not panic)")
	}
}

func TestRunTaskListSweepIfDue_SweepErrorLoggedAndLastRunStamped(t *testing.T) {
	var requests, comments, closes atomic.Int64
	srv := newTaskListSweepAPI(t, `[]`, http.StatusInternalServerError, &requests, &comments, &closes)
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget"}, sweepTestLogger(nil))

	var buf strings.Builder
	var lastRun time.Time
	start := time.Now()
	runTaskListSweepIfDue(context.Background(), ghClient, nil, &lastRun, sweepTestLogger(&buf))

	if !strings.Contains(buf.String(), "task-list sweep failed") {
		t.Fatalf("sweep error must be logged as a warning, got: %s", buf.String())
	}
	if lastRun.Before(start) {
		t.Fatalf("failed sweep must still stamp lastRun (no hot retry loop), got %v", lastRun)
	}
}

func TestRunTaskListSweepIfDue_QuietSweepDoesNotLogCompletion(t *testing.T) {
	var requests, comments, closes atomic.Int64
	srv := newTaskListSweepAPI(t, `[]`, http.StatusOK, &requests, &comments, &closes)
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget"}, sweepTestLogger(nil))

	var buf strings.Builder
	var lastRun time.Time
	runTaskListSweepIfDue(context.Background(), ghClient, nil, &lastRun, sweepTestLogger(&buf))

	if strings.Contains(buf.String(), "task-list sweep complete") {
		t.Fatalf("sweep with zero issues seen must stay quiet, got: %s", buf.String())
	}
}
