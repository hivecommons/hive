package github

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// revalidateServer serves a PR in prState whose head has actionRequired
// pending fork-approval runs.
func revalidateServer(t *testing.T, prState *atomic.Value, actionRequired *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pulls/"):
			_, _ = io.WriteString(w, `{"number":7,"state":"`+prState.Load().(string)+`","head":{"sha":"abc"},"base":{"ref":"main"}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/actions/runs"):
			if r.URL.Query().Get("status") != "action_required" || r.URL.Query().Get("head_sha") != "abc" {
				t.Errorf("unexpected runs query: %s", r.URL.RawQuery)
			}
			n := actionRequired.Load()
			_, _ = io.WriteString(w, `{"total_count":`+strconv.Itoa(int(n))+`,"workflow_runs":[]}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
}

const forkApprovalErr = "ci gate: fork PR workflow runs are awaiting maintainer approval (action_required): \"CI\"(41). Approve the runs or relax the repo setting at https://github.com/o/r/settings/actions"

func TestRevalidateMergeFailureAlerts_ClearsWhenRunsApproved(t *testing.T) {
	var prState atomic.Value
	prState.Store("open")
	var pending atomic.Int32
	pending.Store(1)
	srv := revalidateServer(t, &prState, &pending)
	defer srv.Close()

	c := testMergeClient(t, srv.URL)
	sink := &recordingMergeAlertSink{}
	c.SetMergeFailureAlertSink(sink)
	c.raiseMergeFailureAlert("o/r", 7, forkApprovalErr)
	if len(sink.adds) != 1 {
		t.Fatalf("expected 1 alert, got %d", len(sink.adds))
	}

	now := time.Date(2026, 9, 28, 18, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return now }

	// Still blocked: alert stays.
	c.revalidateMergeFailureAlerts(context.Background(), nowFn)
	if len(sink.clears) != 0 {
		t.Fatalf("alert cleared while runs still awaited approval")
	}

	// Operator approved the runs / relaxed the setting, but the next tick is
	// inside the throttle window: nothing happens yet.
	pending.Store(0)
	now = now.Add(time.Minute)
	c.revalidateMergeFailureAlerts(context.Background(), nowFn)
	if len(sink.clears) != 0 {
		t.Fatalf("revalidated inside the throttle window")
	}

	now = now.Add(mergeAlertRevalidateInterval)
	c.revalidateMergeFailureAlerts(context.Background(), nowFn)
	if len(sink.clears) != 1 || sink.clears[0] != sink.adds[0].id {
		t.Fatalf("expected alert %s cleared, got %v", sink.adds[0].id, sink.clears)
	}
	c.mergeAlertMu.Lock()
	remaining := len(c.mergeAlertIDsByRepo)
	c.mergeAlertMu.Unlock()
	if remaining != 0 {
		t.Fatalf("alert bookkeeping not dropped: %d repos remain", remaining)
	}
}

func TestRevalidateMergeFailureAlerts_ClearsWhenPRClosed(t *testing.T) {
	var prState atomic.Value
	prState.Store("open")
	var pending atomic.Int32
	pending.Store(1)
	srv := revalidateServer(t, &prState, &pending)
	defer srv.Close()

	c := testMergeClient(t, srv.URL)
	sink := &recordingMergeAlertSink{}
	c.SetMergeFailureAlertSink(sink)
	// A non-fork alert: only PR closure can resolve it.
	c.raiseMergeFailureAlert("o/r", 7, "GH006: Protected branch update failed: At least 1 approving review is required")
	if len(sink.adds) != 1 {
		t.Fatalf("expected 1 alert, got %d", len(sink.adds))
	}
	prState.Store("closed")
	c.revalidateMergeFailureAlerts(context.Background(), nil)
	if len(sink.clears) != 1 {
		t.Fatalf("expected alert cleared after PR closed, got %v", sink.clears)
	}
}

func TestRevalidateMergeFailureAlerts_APIErrorKeepsAlert(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := testMergeClient(t, srv.URL)
	sink := &recordingMergeAlertSink{}
	c.SetMergeFailureAlertSink(sink)
	c.raiseMergeFailureAlert("o/r", 7, forkApprovalErr)
	c.revalidateMergeFailureAlerts(context.Background(), nil)
	if len(sink.clears) != 0 {
		t.Fatalf("alert cleared on API error: %v", sink.clears)
	}
}

const appPermissionErr = "merging PR o/r#7 (squash): PUT https://api.github.test/repos/o/r/pulls/7/merge: 403 Resource not accessible by integration []"

func appPermissionRevalidateServer(checkRunsStatus *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pulls/"):
			_, _ = io.WriteString(w, `{"number":7,"state":"open","head":{"sha":"abc"},"base":{"ref":"main"}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/commits/abc/check-runs"):
			if code := int(checkRunsStatus.Load()); code != http.StatusOK {
				w.WriteHeader(code)
				return
			}
			_, _ = io.WriteString(w, `{"total_count":0,"check_runs":[]}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
}

// An app-permission alert retires on positive evidence while the PR is still
// open: a check-runs read on its head succeeds (#11924).
func TestRevalidateMergeFailureAlerts_AppPermissionClearsOnSuccessfulRead(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusForbidden)
	srv := appPermissionRevalidateServer(&status)
	defer srv.Close()

	c := testMergeClient(t, srv.URL)
	sink := &recordingMergeAlertSink{}
	c.SetMergeFailureAlertSink(sink)
	c.raiseMergeFailureAlert("o/r", 7, appPermissionErr)
	if len(sink.adds) != 1 {
		t.Fatalf("expected 1 alert, got %d", len(sink.adds))
	}

	now := time.Date(2026, 10, 14, 23, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return now }
	c.revalidateMergeFailureAlerts(context.Background(), nowFn)
	if len(sink.clears) != 0 {
		t.Fatalf("alert cleared while check-runs read still refused: %v", sink.clears)
	}

	status.Store(http.StatusOK)
	now = now.Add(mergeAlertRevalidateInterval)
	c.revalidateMergeFailureAlerts(context.Background(), nowFn)
	if len(sink.clears) != 1 || sink.clears[0] != sink.adds[0].id {
		t.Fatalf("expected app-permission alert cleared, got %v", sink.clears)
	}
}
