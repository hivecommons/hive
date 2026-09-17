package main

// publishFleetReports (main.go) is the only bridge between the fleet-report
// detector and GitHub: it posts/refreshes upstream issues, records them in
// dashboard state, and drives the recovery lifecycle. These tests exercise the
// real github.Client against an httptest server (via NewClientForTest, the
// same seam pkg/github's own fleet_report_test.go uses) and the real
// dashboard.Server with its state file redirected to a temp path (the
// SetFleetReportStatePathForTest seam), so every branch of the wiring —
// post, post-failure continue, recovery, number lookup, vanished-issue clear,
// lookup failure, and write failure — is pinned end to end.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/fleetreport"
	"github.com/hivecommons/hive/pkg/github"
)

func newFleetReportTestDash(t *testing.T) *dashboard.Server {
	t.Helper()
	dashboard.SetFleetReportStatePathForTest(t, filepath.Join(t.TempDir(), "fleet-report-state.json"))
	return dashboard.NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestPublishFleetReports_NoOpGuards pins the early return: a nil result, a
// dry run, a nil GitHub client, or a nil dashboard each produce zero upstream
// traffic and zero state writes.
func TestPublishFleetReports_NoOpGuards(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer srv.Close()
	ghClient := github.NewClientForTest(srv.URL, "hivecommons", nil, discardLogger())
	dashSrv := newFleetReportTestDash(t)
	res := &fleetreport.Result{
		Reports:    []fleetreport.Report{{Fingerprint: "fp", Body: "b"}},
		Recoveries: []fleetreport.Report{{Fingerprint: "fp", Body: "b"}},
	}
	ctx := context.Background()

	publishFleetReports(ctx, discardLogger(), ghClient, dashSrv, nil, false)
	publishFleetReports(ctx, discardLogger(), ghClient, dashSrv, res, true)
	publishFleetReports(ctx, discardLogger(), nil, dashSrv, res, false)
	publishFleetReports(ctx, discardLogger(), ghClient, nil, res, false)

	if requests.Load() != 0 {
		t.Fatalf("guarded calls made %d upstream requests, want 0", requests.Load())
	}
	if _, ok := dashSrv.FleetReportOpenIssue("fp"); ok {
		t.Fatal("guarded calls wrote fleet-report state")
	}
}

// TestPublishFleetReports_PostsReportAndRecordsState pins the happy path AND
// the continue-on-error branch: the first report's fingerprint search fails
// upstream (warn + continue, no state), while the second matches an existing
// issue and lands in dashboard state with that issue's number and URL.
func TestPublishFleetReports_PostsReportAndRecordsState(t *testing.T) {
	var comments atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/search/issues" && strings.Contains(r.URL.Query().Get("q"), "fp-bad"):
			http.Error(w, "boom", http.StatusInternalServerError)
		case r.URL.Path == "/search/issues":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"number": 7, "html_url": "https://github.com/hivecommons/hive/issues/7"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/issues/7/comments":
			comments.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/issues/7/reactions":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 2})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ghClient := github.NewClientForTest(srv.URL, "hivecommons", nil, discardLogger())
	dashSrv := newFleetReportTestDash(t)
	res := &fleetreport.Result{Reports: []fleetreport.Report{
		{Fingerprint: "fp-bad", Body: "evidence"},
		{Fingerprint: "fp-ok", Body: "evidence"},
	}}

	publishFleetReports(context.Background(), discardLogger(), ghClient, dashSrv, res, false)

	if comments.Load() != 1 {
		t.Fatalf("comments = %d, want 1", comments.Load())
	}
	if _, ok := dashSrv.FleetReportOpenIssue("fp-bad"); ok {
		t.Fatal("failed report must not be recorded as posted")
	}
	open, ok := dashSrv.FleetReportOpenIssue("fp-ok")
	if !ok || open.Number != 7 || open.URL != "https://github.com/hivecommons/hive/issues/7" {
		t.Fatalf("posted state = %#v ok=%v, want issue 7 recorded", open, ok)
	}
}

// TestPublishFleetReports_RecoveryPostsAndMarks pins the known-number recovery
// path: the recovery comment lands on the recorded issue, the issue is closed
// because Hive opened it, and the fingerprint is marked recovered.
func TestPublishFleetReports_RecoveryPostsAndMarks(t *testing.T) {
	var comments, patches atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/issues/5/comments":
			comments.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/hivecommons/hive/issues/5":
			patches.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 5})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ghClient := github.NewClientForTest(srv.URL, "hivecommons", nil, discardLogger())
	dashSrv := newFleetReportTestDash(t)
	dashSrv.MarkFleetReportPosted("fp", 5, "https://github.com/hivecommons/hive/issues/5", true, "body")
	res := &fleetreport.Result{Recoveries: []fleetreport.Report{{Fingerprint: "fp", Body: "recovered", Recovered: true}}}

	publishFleetReports(context.Background(), discardLogger(), ghClient, dashSrv, res, false)

	if comments.Load() != 1 || patches.Load() != 1 {
		t.Fatalf("comments=%d patches=%d, want 1 and 1 (close because Hive opened it)", comments.Load(), patches.Load())
	}
	open, ok := dashSrv.FleetReportOpenIssue("fp")
	if !ok || !open.Recovered {
		t.Fatalf("state = %#v ok=%v, want Recovered=true", open, ok)
	}
}

// TestPublishFleetReports_RecoverySkipsUnknownFingerprint pins that a recovery
// for a fingerprint with no recorded open issue is a no-op — nothing upstream,
// nothing in state.
func TestPublishFleetReports_RecoverySkipsUnknownFingerprint(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer srv.Close()
	ghClient := github.NewClientForTest(srv.URL, "hivecommons", nil, discardLogger())
	dashSrv := newFleetReportTestDash(t)
	res := &fleetreport.Result{Recoveries: []fleetreport.Report{{Fingerprint: "fp-unknown", Body: "recovered"}}}

	publishFleetReports(context.Background(), discardLogger(), ghClient, dashSrv, res, false)

	if requests.Load() != 0 {
		t.Fatalf("unknown fingerprint made %d upstream requests, want 0", requests.Load())
	}
}

// TestPublishFleetReports_RecoveryLooksUpNumberWhenUnknown pins the Number<=0
// branch: state recorded without an issue number resolves it via the
// fingerprint search, posts the recovery there, and marks recovered without
// closing (the issue was not opened by Hive).
func TestPublishFleetReports_RecoveryLooksUpNumberWhenUnknown(t *testing.T) {
	var comments atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/search/issues":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"number": 9, "html_url": "https://github.com/hivecommons/hive/issues/9"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/issues/9/comments":
			comments.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ghClient := github.NewClientForTest(srv.URL, "hivecommons", nil, discardLogger())
	dashSrv := newFleetReportTestDash(t)
	dashSrv.MarkFleetReportPosted("fp", 0, "", false, "body")
	res := &fleetreport.Result{Recoveries: []fleetreport.Report{{Fingerprint: "fp", Body: "recovered"}}}

	publishFleetReports(context.Background(), discardLogger(), ghClient, dashSrv, res, false)

	if comments.Load() != 1 {
		t.Fatalf("comments = %d, want 1 on looked-up issue 9", comments.Load())
	}
	open, ok := dashSrv.FleetReportOpenIssue("fp")
	if !ok || !open.Recovered {
		t.Fatalf("state = %#v ok=%v, want Recovered=true after lookup", open, ok)
	}
}

// TestPublishFleetReports_RecoveryClearsStateWhenIssueVanished pins that a
// recorded-but-numberless fingerprint whose issue no longer exists upstream is
// cleared from state instead of being commented on.
func TestPublishFleetReports_RecoveryClearsStateWhenIssueVanished(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search/issues" {
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
		http.NotFound(w, r)
	}))
	defer srv.Close()
	ghClient := github.NewClientForTest(srv.URL, "hivecommons", nil, discardLogger())
	dashSrv := newFleetReportTestDash(t)
	dashSrv.MarkFleetReportPosted("fp", 0, "", false, "body")
	res := &fleetreport.Result{Recoveries: []fleetreport.Report{{Fingerprint: "fp", Body: "recovered"}}}

	publishFleetReports(context.Background(), discardLogger(), ghClient, dashSrv, res, false)

	if _, ok := dashSrv.FleetReportOpenIssue("fp"); ok {
		t.Fatal("vanished issue must clear the open-fingerprint state")
	}
}

// TestPublishFleetReports_RecoveryLookupErrorLeavesState pins the
// lookup-failure branch: a search error must NOT clear state or mark the
// fingerprint recovered — the next cycle retries.
func TestPublishFleetReports_RecoveryLookupErrorLeavesState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	ghClient := github.NewClientForTest(srv.URL, "hivecommons", nil, discardLogger())
	dashSrv := newFleetReportTestDash(t)
	dashSrv.MarkFleetReportPosted("fp", 0, "", false, "body")
	res := &fleetreport.Result{Recoveries: []fleetreport.Report{{Fingerprint: "fp", Body: "recovered"}}}

	publishFleetReports(context.Background(), discardLogger(), ghClient, dashSrv, res, false)

	open, ok := dashSrv.FleetReportOpenIssue("fp")
	if !ok {
		t.Fatal("lookup failure must not clear the open-fingerprint state")
	}
	if open.Recovered {
		t.Fatalf("state = %#v, want Recovered=false after lookup failure", open)
	}
}

// TestPublishFleetReports_RecoveryWriteFailureLeavesUnrecovered pins the
// write-failure branch: if the recovery comment fails upstream, the
// fingerprint must stay unrecovered so the next cycle retries.
func TestPublishFleetReports_RecoveryWriteFailureLeavesUnrecovered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/issues/5/comments" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
		http.NotFound(w, r)
	}))
	defer srv.Close()
	ghClient := github.NewClientForTest(srv.URL, "hivecommons", nil, discardLogger())
	dashSrv := newFleetReportTestDash(t)
	dashSrv.MarkFleetReportPosted("fp", 5, "https://github.com/hivecommons/hive/issues/5", false, "body")
	res := &fleetreport.Result{Recoveries: []fleetreport.Report{{Fingerprint: "fp", Body: "recovered"}}}

	publishFleetReports(context.Background(), discardLogger(), ghClient, dashSrv, res, false)

	open, ok := dashSrv.FleetReportOpenIssue("fp")
	if !ok || open.Recovered {
		t.Fatalf("state = %#v ok=%v, want present and Recovered=false after write failure", open, ok)
	}
}
