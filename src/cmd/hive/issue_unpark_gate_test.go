package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// Tests for runIssueUnparkSweepIfDue: the eval-cycle gate in front of the
// un-park sweep (hivecommons/hive#9879). The sweep reads comments on parked
// issues, so the contract pinned here is the throttle: at most one pass per
// issueUnparkSweepInterval however often the eval loop calls it, and no calls
// at all without a GitHub client.

type unparkGateServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests int
}

func newUnparkGateServer(t *testing.T) *unparkGateServer {
	t.Helper()
	s := &unparkGateServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests++
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *unparkGateServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

func TestIssueUnparkSweepGate_NoClientNoCalls(t *testing.T) {
	var lastRun time.Time

	runIssueUnparkSweepIfDue(context.Background(), nil, nil, nil, &lastRun, sweepQuietLogger())

	if !lastRun.IsZero() {
		t.Fatal("a sweep that did not run must not consume the throttle clock")
	}
}

func TestIssueUnparkSweepGate_ThrottledToOnePassPerInterval(t *testing.T) {
	srv := newUnparkGateServer(t)
	client := github.NewClientForTest(srv.URL, "o", []string{"o/r"}, sweepQuietLogger())
	var lastRun time.Time

	runIssueUnparkSweepIfDue(context.Background(), client, nil, nil, &lastRun, sweepQuietLogger())
	first := srv.count()
	if first == 0 {
		t.Fatal("first pass made no API calls, want at least the issue listing")
	}
	if lastRun.IsZero() {
		t.Fatal("first pass did not stamp the throttle clock")
	}

	runIssueUnparkSweepIfDue(context.Background(), client, nil, nil, &lastRun, sweepQuietLogger())
	if srv.count() != first {
		t.Fatalf("second pass inside the interval made %d calls, want %d", srv.count(), first)
	}

	lastRun = time.Now().Add(-2 * issueUnparkSweepInterval)
	runIssueUnparkSweepIfDue(context.Background(), client, nil, nil, &lastRun, sweepQuietLogger())
	if srv.count() <= first {
		t.Fatal("a pass after the interval elapsed should have run")
	}
}
