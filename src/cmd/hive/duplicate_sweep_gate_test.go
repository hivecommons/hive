package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// Tests for runDuplicateSweepIfDue: the eval-cycle gate in front of the
// duplicate-PR sweep. The sweep writes comments on contributors' PRs, so the
// contract pinned here is the anti-spam one: off by default, at most one pass
// per duplicateSweepInterval even when the eval loop calls it every cycle, and
// report-only unless post_comments is separately granted. None of these gates
// were covered before this file.

// sweepGateServer counts every request and rejects writes unless allowed, so
// a test can assert "zero API calls" or "reads only" precisely.
type sweepGateServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests int
	posts    int
	prs      []map[string]any
	files    map[int][]map[string]any // PR number -> changed files
}

func newSweepGateServer(t *testing.T) *sweepGateServer {
	t.Helper()
	s := &sweepGateServer{files: map[int][]map[string]any{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests++
		if r.Method == http.MethodPost {
			s.posts++
		}
		s.mu.Unlock()
		io.Copy(io.Discard, r.Body)

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls":
			json.NewEncoder(w).Encode(s.prs)
		case r.Method == http.MethodGet && len(r.URL.Path) > len("/repos/o/r/pulls/"):
			var num int
			if n, _ := fmt.Sscanf(r.URL.Path, "/repos/o/r/pulls/%d/files", &num); n == 1 {
				json.NewEncoder(w).Encode(s.files[num])
				return
			}
			// issue comment listings and anything else read-only: empty
			json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
		default:
			json.NewEncoder(w).Encode([]map[string]any{})
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *sweepGateServer) counts() (requests, posts int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests, s.posts
}

// addDuplicatePair registers two open non-draft PRs by different humans that
// touch the identical file set — the strongest cluster the sweep recognizes.
func (s *sweepGateServer) addDuplicatePair() {
	mk := func(num int, author string) map[string]any {
		return map[string]any{
			"number":        num,
			"title":         fmt.Sprintf("fix: same thing %d", num),
			"state":         "open",
			"draft":         false,
			"user":          map[string]any{"login": author},
			"head":          map[string]any{"sha": fmt.Sprintf("sha-%d", num)},
			"changed_files": 2,
			"html_url":      fmt.Sprintf("https://example.test/o/r/pull/%d", num),
			"created_at":    time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
		}
	}
	files := []map[string]any{
		{"filename": "pkg/a/a.go", "sha": "f1"},
		{"filename": "pkg/a/a_test.go", "sha": "f2"},
	}
	s.prs = append(s.prs, mk(11, "alice"), mk(12, "bob"))
	s.files[11] = files
	s.files[12] = files
}

func sweepGateClient(t *testing.T, srv *sweepGateServer) *github.Client {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return github.NewClientForTest(srv.URL, "o", []string{"o/r"}, logger)
}

func sweepGateConfig(enabled, postComments bool) *config.Config {
	cfg := &config.Config{}
	cfg.DuplicateSweep = config.DuplicateSweepConfig{
		Enabled:      enabled,
		PostComments: postComments,
	}
	return cfg
}

func sweepQuietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Off by default: a hive that has not opted in performs no API calls at all,
// and the throttle clock is not consumed.
func TestDuplicateSweepGate_DisabledMakesNoCalls(t *testing.T) {
	srv := newSweepGateServer(t)
	var lastRun time.Time

	runDuplicateSweepIfDue(context.Background(), sweepGateConfig(false, false), sweepGateClient(t, srv), nil, &lastRun, sweepQuietLogger())

	if reqs, _ := srv.counts(); reqs != 0 {
		t.Fatalf("disabled sweep made %d API calls, want 0", reqs)
	}
	if !lastRun.IsZero() {
		t.Fatalf("disabled sweep consumed the throttle clock: lastRun=%v", lastRun)
	}
}

// Nil config or nil client must be inert, not a panic: the eval loop calls
// this unconditionally on every cycle.
func TestDuplicateSweepGate_NilInputsAreInert(t *testing.T) {
	srv := newSweepGateServer(t)
	var lastRun time.Time

	runDuplicateSweepIfDue(context.Background(), nil, sweepGateClient(t, srv), nil, &lastRun, sweepQuietLogger())
	runDuplicateSweepIfDue(context.Background(), sweepGateConfig(true, false), nil, nil, &lastRun, sweepQuietLogger())

	if reqs, _ := srv.counts(); reqs != 0 {
		t.Fatalf("nil-input sweep made %d API calls, want 0", reqs)
	}
	if !lastRun.IsZero() {
		t.Fatalf("nil-input sweep consumed the throttle clock: lastRun=%v", lastRun)
	}
}

// A sweep that ran recently must not run again within duplicateSweepInterval,
// and must not push lastRun forward (which would starve the next due run).
func TestDuplicateSweepGate_ThrottledWithinInterval(t *testing.T) {
	srv := newSweepGateServer(t)
	recent := time.Now().Add(-duplicateSweepInterval / 2)
	lastRun := recent

	runDuplicateSweepIfDue(context.Background(), sweepGateConfig(true, false), sweepGateClient(t, srv), nil, &lastRun, sweepQuietLogger())

	if reqs, _ := srv.counts(); reqs != 0 {
		t.Fatalf("throttled sweep made %d API calls, want 0", reqs)
	}
	if !lastRun.Equal(recent) {
		t.Fatalf("throttled sweep moved lastRun from %v to %v", recent, lastRun)
	}
}

// A due sweep runs, and stamps lastRun BEFORE the API work, so a failing or
// slow sweep still waits a full interval instead of hot-looping every cycle.
func TestDuplicateSweepGate_DueRunStampsLastRun(t *testing.T) {
	srv := newSweepGateServer(t)
	lastRun := time.Now().Add(-2 * duplicateSweepInterval)
	before := time.Now()

	runDuplicateSweepIfDue(context.Background(), sweepGateConfig(true, false), sweepGateClient(t, srv), nil, &lastRun, sweepQuietLogger())

	if reqs, _ := srv.counts(); reqs == 0 {
		t.Fatal("due sweep made no API calls")
	}
	if lastRun.Before(before) {
		t.Fatalf("due sweep did not stamp lastRun: %v", lastRun)
	}

	// The immediately following cycle is throttled.
	srv.mu.Lock()
	srv.requests = 0
	srv.mu.Unlock()
	runDuplicateSweepIfDue(context.Background(), sweepGateConfig(true, false), sweepGateClient(t, srv), nil, &lastRun, sweepQuietLogger())
	if reqs, _ := srv.counts(); reqs != 0 {
		t.Fatalf("second cycle within interval made %d API calls, want 0", reqs)
	}
}

// A server error must not panic and must still consume the throttle clock —
// a broken remote gets one attempt per interval, not one per eval cycle.
func TestDuplicateSweepGate_ErrorStillThrottles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	client := github.NewClientForTest(srv.URL, "o", []string{"o/r"}, sweepQuietLogger())
	var lastRun time.Time

	runDuplicateSweepIfDue(context.Background(), sweepGateConfig(true, false), client, nil, &lastRun, sweepQuietLogger())

	if lastRun.IsZero() {
		t.Fatal("failed sweep did not consume the throttle clock; it would retry every eval cycle")
	}
}

// Report-only is the default posture: with Enabled set but PostComments off,
// a genuine duplicate cluster produces reads only — not one POST anywhere.
func TestDuplicateSweepGate_ReportOnlyNeverWrites(t *testing.T) {
	srv := newSweepGateServer(t)
	srv.addDuplicatePair()
	var lastRun time.Time

	runDuplicateSweepIfDue(context.Background(), sweepGateConfig(true, false), sweepGateClient(t, srv), nil, &lastRun, sweepQuietLogger())

	reqs, posts := srv.counts()
	if reqs == 0 {
		t.Fatal("sweep made no API calls; duplicate pair was never scanned")
	}
	if posts != 0 {
		t.Fatalf("report-only sweep issued %d POSTs, want 0", posts)
	}
}

// With the second grant (post_comments) the same cluster produces the
// suggestion comment — proving report-only above is the gate, not an
// inability to detect the cluster.
func TestDuplicateSweepGate_PostCommentsGrantWrites(t *testing.T) {
	srv := newSweepGateServer(t)
	srv.addDuplicatePair()
	var lastRun time.Time

	runDuplicateSweepIfDue(context.Background(), sweepGateConfig(true, true), sweepGateClient(t, srv), nil, &lastRun, sweepQuietLogger())

	if _, posts := srv.counts(); posts == 0 {
		t.Fatal("post_comments-granted sweep wrote nothing; the report-only test would pass vacuously")
	}
}
