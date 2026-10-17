package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/fleetreport"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/nous"
)

// fakeSelfReportWriter scripts the GitHub seam the publisher drives.
type fakeSelfReportWriter struct {
	ensure    func(report fleetreport.Report, opts github.FleetReportOptions) (github.FleetReportResult, error)
	ensures   []github.FleetReportOptions
	cleared   []int
	recovered []int
	lookup    func(fp string) (github.FleetReportResult, bool, error)
}

func (f *fakeSelfReportWriter) EnsureFleetReportWithOptions(_ context.Context, report fleetreport.Report, opts github.FleetReportOptions) (github.FleetReportResult, error) {
	f.ensures = append(f.ensures, opts)
	return f.ensure(report, opts)
}

func (f *fakeSelfReportWriter) PostFleetReportRecovery(_ context.Context, n int, _ fleetreport.Report, _ bool) error {
	f.recovered = append(f.recovered, n)
	return nil
}

func (f *fakeSelfReportWriter) PostFleetReportCleared(_ context.Context, _ string, n int, body string) error {
	if !strings.Contains(body, "Cleared on") {
		return errors.New("not a cleared comment: " + body)
	}
	f.cleared = append(f.cleared, n)
	return nil
}

func (f *fakeSelfReportWriter) FleetReportIssue(_ context.Context, fp string) (github.FleetReportResult, bool, error) {
	if f.lookup != nil {
		return f.lookup(fp)
	}
	return github.FleetReportResult{}, false, nil
}

func selfReportTestServer(t *testing.T) *dashboard.Server {
	t.Helper()
	dashboard.SetFleetReportStatePathForTest(t, filepath.Join(t.TempDir(), "fleet-report-state.json"))
	return dashboard.NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func runtimeReport(fp, class string) fleetreport.Report {
	return fleetreport.Report{Fingerprint: fp, Class: class, Trigger: fleetreport.TriggerRuntimeError, Title: "[selfreport] " + class, Body: "body " + fp, Labels: []string{"fleet-report"}}
}

func selfReportDeps(w *fakeSelfReportWriter, now *time.Time) selfReportPublishDeps {
	return selfReportPublishDeps{
		gh: w, repo: "hivecommons/hive", hiveSlug: "hosted-x", version: "v5.1.0", cluster: "oke-11",
		limits: fleetreport.DefaultFilingLimits.WithMaxPerDay(2), now: func() time.Time { return *now }, tracker: fleetreport.NewRuntimeTracker(nil),
	}
}

func TestPublishFleetReportsDryRunAndBudgetWriteNothing(t *testing.T) {
	srv := selfReportTestServer(t)
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	w := &fakeSelfReportWriter{ensure: func(fleetreport.Report, github.FleetReportOptions) (github.FleetReportResult, error) {
		t.Fatal("writer called")
		return github.FleetReportResult{}, nil
	}}
	res := &fleetreport.Result{Reports: []fleetreport.Report{runtimeReport("fp1", fleetreport.ClassRestartStorm)}}
	deps := selfReportDeps(w, &now)
	deps.dryRun = true
	publishFleetReportsWith(context.Background(), slog.Default(), srv, res, deps)
	deps.dryRun = false
	deps.budgetOK = func() bool { return false }
	publishFleetReportsWith(context.Background(), slog.Default(), srv, res, deps)
	if len(w.ensures) != 0 {
		t.Fatalf("ensures = %d", len(w.ensures))
	}
}

func TestPublishFleetReportsCreatesJoinsAndRateLimits(t *testing.T) {
	srv := selfReportTestServer(t)
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	created := 0
	w := &fakeSelfReportWriter{}
	w.ensure = func(report fleetreport.Report, opts github.FleetReportOptions) (github.FleetReportResult, error) {
		if !opts.JoinExisting || opts.HiveSlug != "hosted-x" || opts.Repo != "hivecommons/hive" {
			t.Fatalf("opts = %#v", opts)
		}
		if report.Fingerprint == "fp-join" {
			return github.FleetReportResult{Number: 7, URL: "u7", Joined: true, Commented: true}, nil
		}
		if opts.CreateGate != nil {
			if err := opts.CreateGate(); err != nil {
				return github.FleetReportResult{}, err
			}
		}
		created++
		return github.FleetReportResult{Number: 100 + created, URL: "u", Created: true}, nil
	}
	deps := selfReportDeps(w, &now)

	// Cycle 1: one new filing plus one join; the join never counts.
	res := &fleetreport.Result{Reports: []fleetreport.Report{runtimeReport("fp-a", fleetreport.ClassRestartStorm), runtimeReport("fp-join", fleetreport.ClassPanicRecovered)}}
	publishFleetReportsWith(context.Background(), slog.Default(), srv, res, deps)
	st := srv.FleetReportState()
	if created != 1 || !st.Open["fp-a"].OpenedByHive || st.Open["fp-a"].Number != 101 || st.Open["fp-a"].PostedAt.IsZero() || len(st.Filings) != 1 {
		t.Fatalf("after cycle 1: created=%d state=%#v", created, st)
	}
	if o := st.Open["fp-join"]; !o.Joined || o.OpenedByHive || o.Number != 7 || o.LastSeenCommentAt.IsZero() {
		t.Fatalf("join not recorded: %#v", o)
	}

	// Cycle 2 (30m later): a different class is blocked by the 1h gap, a join still goes through.
	now = now.Add(30 * time.Minute)
	res = &fleetreport.Result{Reports: []fleetreport.Report{runtimeReport("fp-b", fleetreport.ClassKickFailedRepeatedly), runtimeReport("fp-join", fleetreport.ClassPanicRecovered)}}
	publishFleetReportsWith(context.Background(), slog.Default(), srv, res, deps)
	st = srv.FleetReportState()
	if created != 1 || !strings.Contains(st.Open["fp-b"].Deferred, "min gap") {
		t.Fatalf("min gap not enforced: created=%d fp-b=%#v", created, st.Open["fp-b"])
	}
	// An issue this hive already joined inside the 24h seen-gap is not
	// touched again: only fp-b reached the writer this cycle.
	if len(w.ensures) != 3 {
		t.Fatalf("writer calls = %d, want 3 (joined issue re-offered inside the seen gap)", len(w.ensures))
	}
	// Past the gap the join is offered again with the comment allowed.
	now = now.Add(24 * time.Hour)
	publishFleetReportsWith(context.Background(), slog.Default(), srv, &fleetreport.Result{Reports: []fleetreport.Report{runtimeReport("fp-join", fleetreport.ClassPanicRecovered)}}, deps)
	if last := w.ensures[len(w.ensures)-1]; len(w.ensures) != 4 || !last.SeenCommentAllowed {
		t.Fatalf("seen comment not re-offered after 24h: %#v", last)
	}
	now = now.Add(-24 * time.Hour)

	// Cycle 3 (1h later): second filing allowed; cycle 4: cap of 2/day hit.
	now = now.Add(31 * time.Minute)
	publishFleetReportsWith(context.Background(), slog.Default(), srv, &fleetreport.Result{Reports: []fleetreport.Report{runtimeReport("fp-b", fleetreport.ClassKickFailedRepeatedly)}}, deps)
	now = now.Add(2 * time.Hour)
	publishFleetReportsWith(context.Background(), slog.Default(), srv, &fleetreport.Result{Reports: []fleetreport.Report{runtimeReport("fp-c", fleetreport.ClassModelDiscoveryDegraded)}}, deps)
	st = srv.FleetReportState()
	if created != 2 || !strings.Contains(st.Open["fp-c"].Deferred, "daily cap") {
		t.Fatalf("daily cap not enforced: created=%d fp-c=%#v", created, st.Open["fp-c"])
	}
}

func TestPublishFleetReportsHonoursClosedCooldownAndClearedComment(t *testing.T) {
	srv := selfReportTestServer(t)
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	calls := 0
	w := &fakeSelfReportWriter{}
	w.ensure = func(report fleetreport.Report, opts github.FleetReportOptions) (github.FleetReportResult, error) {
		calls++
		return github.FleetReportResult{Number: 5, URL: "u5", ClosedAt: now.Add(-2 * 24 * time.Hour)}, nil
	}
	deps := selfReportDeps(w, &now)
	res := &fleetreport.Result{Reports: []fleetreport.Report{runtimeReport("fp-closed", fleetreport.ClassRestartStorm)}}
	publishFleetReportsWith(context.Background(), slog.Default(), srv, res, deps)
	publishFleetReportsWith(context.Background(), slog.Default(), srv, res, deps)
	st := srv.FleetReportState()
	if calls != 1 || st.Open["fp-closed"].ClosedAt.IsZero() || st.Open["fp-closed"].OpenedByHive || len(st.Filings) != 0 {
		t.Fatalf("closed cooldown: calls=%d state=%#v", calls, st.Open["fp-closed"])
	}
	// After the 7-day cooldown the fingerprint is searched again.
	now = now.Add(6 * 24 * time.Hour)
	publishFleetReportsWith(context.Background(), slog.Default(), srv, res, deps)
	if calls != 2 {
		t.Fatalf("cooldown expiry did not re-search: calls=%d", calls)
	}

	// A runtime recovery posts one cleared comment and never closes.
	srv.UpdateFleetReportState(func(st *fleetreport.State) {
		st.Open["fp-r"] = fleetreport.OpenIssue{Number: 9, OpenedByHive: true, Trigger: fleetreport.TriggerRuntimeError, Class: fleetreport.ClassPanicRecovered, PostedAt: now}
	})
	rec := &fleetreport.Result{Recoveries: []fleetreport.Report{{Fingerprint: "fp-r", Trigger: fleetreport.TriggerRuntimeError, Class: fleetreport.ClassPanicRecovered, Body: "recovered"}}}
	publishFleetReportsWith(context.Background(), slog.Default(), srv, rec, deps)
	if len(w.cleared) != 1 || w.cleared[0] != 9 || len(w.recovered) != 0 {
		t.Fatalf("cleared=%v recovered=%v", w.cleared, w.recovered)
	}
	if !srv.FleetReportState().Open["fp-r"].Recovered {
		t.Fatal("recovery not recorded")
	}
	// Legacy triggers keep the old recovery (comment + close-if-owner) path.
	srv.UpdateFleetReportState(func(st *fleetreport.State) {
		st.Open["fp-d"] = fleetreport.OpenIssue{Number: 3, OpenedByHive: true, Trigger: fleetreport.TriggerHiveDefect, PostedAt: now}
	})
	publishFleetReportsWith(context.Background(), slog.Default(), srv, &fleetreport.Result{Recoveries: []fleetreport.Report{{Fingerprint: "fp-d", Trigger: fleetreport.TriggerHiveDefect, Body: "r"}}}, deps)
	if len(w.recovered) != 1 || w.recovered[0] != 3 {
		t.Fatalf("legacy recovery = %v", w.recovered)
	}
}

func TestPublishFleetReportsRelaysToHubOnForbidden(t *testing.T) {
	srv := selfReportTestServer(t)
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	w := &fakeSelfReportWriter{ensure: func(fleetreport.Report, github.FleetReportOptions) (github.FleetReportResult, error) {
		return github.FleetReportResult{}, &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusForbidden}, Message: "Resource not accessible by integration"}
	}}
	var relayed []selfReportRelayRequest
	deps := selfReportDeps(w, &now)
	deps.relay = func(_ context.Context, req selfReportRelayRequest) (selfReportRelayResponse, error) {
		relayed = append(relayed, req)
		return selfReportRelayResponse{OK: true, Number: 77, URL: "u77"}, nil
	}
	res := &fleetreport.Result{Reports: []fleetreport.Report{runtimeReport("fp-relay", fleetreport.ClassInferenceAuthFailed)}}
	publishFleetReportsWith(context.Background(), slog.Default(), srv, res, deps)
	if len(relayed) != 1 || relayed[0].HiveID != "hosted-x" || relayed[0].Fingerprint != "fp-relay" || relayed[0].Repo != "hivecommons/hive" || relayed[0].Class != fleetreport.ClassInferenceAuthFailed {
		t.Fatalf("relayed = %#v", relayed)
	}
	o := srv.FleetReportState().Open["fp-relay"]
	if o.Number != 77 || !o.OpenedByHive || o.PostedAt.IsZero() {
		t.Fatalf("relay result not recorded: %#v", o)
	}
	// A non-access error is NOT relayed.
	w.ensure = func(fleetreport.Report, github.FleetReportOptions) (github.FleetReportResult, error) {
		return github.FleetReportResult{}, errors.New("network down")
	}
	publishFleetReportsWith(context.Background(), slog.Default(), srv, &fleetreport.Result{Reports: []fleetreport.Report{runtimeReport("fp-net", fleetreport.ClassRestartStorm)}}, deps)
	if len(relayed) != 1 {
		t.Fatalf("generic error was relayed: %d", len(relayed))
	}
}

func TestPublishObservationAlwaysUsesHubRelay(t *testing.T) {
	srv := selfReportTestServer(t)
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	w := &fakeSelfReportWriter{ensure: func(fleetreport.Report, github.FleetReportOptions) (github.FleetReportResult, error) {
		t.Fatal("observation used the spoke GitHub writer")
		return github.FleetReportResult{}, nil
	}}
	var relayed selfReportRelayRequest
	deps := selfReportDeps(w, &now)
	deps.budgetOK = func() bool { return false }
	deps.relay = func(_ context.Context, req selfReportRelayRequest) (selfReportRelayResponse, error) {
		relayed = req
		return selfReportRelayResponse{OK: true, Number: 88, URL: "u88"}, nil
	}
	report := fleetreport.BuildObservationReport(fleetreport.ObservationFinding{
		Kind:        "queue-growth",
		Title:       "Queue growth",
		Evidence:    "p90 rose",
		Fingerprint: "hosted-x:queue-growth",
		Series:      []float64{1, 2, 3},
	}, fleetreport.ObservationMetadata{HiveID: "hosted-x", HiveSlug: "hosted-x", Now: now})
	publishFleetReportsWith(context.Background(), slog.Default(), srv, &fleetreport.Result{Reports: []fleetreport.Report{report}}, deps)
	if relayed.Class != fleetreport.TriggerObservation || relayed.Repo != "hivecommons/hive" || relayed.Observation == nil {
		t.Fatalf("relayed observation = %#v", relayed)
	}
	if got := srv.FleetReportState().Open[report.Fingerprint]; got.Number != 88 || !got.OpenedByHive {
		t.Fatalf("relay result not recorded: %#v", got)
	}
}

func TestSelfReportShouldRelay(t *testing.T) {
	cases := map[error]bool{
		&gh.ErrorResponse{Response: &http.Response{StatusCode: 403}}: true,
		&gh.ErrorResponse{Response: &http.Response{StatusCode: 404}}: true,
		&gh.ErrorResponse{Response: &http.Response{StatusCode: 500}}: false,
		errSelfReportNoDirectWriter:                                  true,
		errors.New("boom"):                                           false,
	}
	for err, want := range cases {
		if got := selfReportShouldRelay(err); got != want {
			t.Fatalf("selfReportShouldRelay(%v) = %v want %v", err, got, want)
		}
	}
}

func TestPublishObservationRecoveryUnknownHubIssueIsNoop(t *testing.T) {
	srv := selfReportTestServer(t)
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	fp := "observation-recovery"
	ledger := nous.NewObservationLedger(filepath.Join(t.TempDir(), "observations.json"))
	if _, err := ledger.Reconcile([]nous.ActiveObservation{{
		Fingerprint: fp,
		IssueNumber: 42,
		URL:         "https://github.com/hivecommons/hive/issues/42",
		Before:      "p90=900ms",
		After:       "p90=100ms",
		Evidence:    "last sample",
	}}, now); err != nil {
		t.Fatal(err)
	}
	if err := ledger.RecordIssue(fp, 42, "https://github.com/hivecommons/hive/issues/42", now); err != nil {
		t.Fatal(err)
	}
	srv.UpdateFleetReportState(func(st *fleetreport.State) {
		st.Open[fp] = fleetreport.OpenIssue{
			Number: 42, URL: "https://github.com/hivecommons/hive/issues/42",
			Trigger: selfReportObservationClass, Class: selfReportObservationClass, PostedAt: now,
		}
	})
	w := &fakeSelfReportWriter{}
	var relayed []selfReportRelayRequest
	deps := selfReportDeps(w, &now)
	deps.observations = ledger
	deps.relay = func(_ context.Context, req selfReportRelayRequest) (selfReportRelayResponse, error) {
		relayed = append(relayed, req)
		return selfReportRelayResponse{Reason: "unknown_fingerprint"}, nil
	}
	// The first cycle without the finding starts the seven-day window; the
	// recovery request is produced only by a later cycle past that window.
	publishFleetReportsWith(context.Background(), slog.Default(), srv, &fleetreport.Result{}, deps)
	if len(relayed) != 0 {
		t.Fatalf("recovery relayed before the seven-day window: %#v", relayed)
	}
	now = now.Add(8 * 24 * time.Hour)
	publishFleetReportsWith(context.Background(), slog.Default(), srv, &fleetreport.Result{}, deps)
	if len(relayed) != 1 {
		t.Fatalf("recovery relays = %d, want 1", len(relayed))
	}
	if relayed[0].Action != "recover" || relayed[0].Class != selfReportObservationClass || !strings.Contains(relayed[0].Body, "p90=900ms") || !strings.Contains(relayed[0].Body, "p90=100ms") {
		t.Fatalf("recovery relay = %#v", relayed[0])
	}
	records, err := ledger.Records()
	if err != nil {
		t.Fatal(err)
	}
	if records[fp].ClosedAt.IsZero() {
		t.Fatalf("unknown hub fingerprint was not acknowledged locally: %#v", records[fp])
	}
	if !srv.FleetReportState().Open[fp].Recovered {
		t.Fatal("dashboard state did not record observation recovery")
	}
}

func TestPublishObservationReportsRelayThroughHubOnly(t *testing.T) {
	srv := selfReportTestServer(t)
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	fp := "h1:queue-growth"
	ledger := nous.NewObservationLedger(filepath.Join(t.TempDir(), "observations.json"))
	w := &fakeSelfReportWriter{ensure: func(fleetreport.Report, github.FleetReportOptions) (github.FleetReportResult, error) {
		t.Fatal("observation used the spoke GitHub writer")
		return github.FleetReportResult{}, nil
	}}
	var relayed []selfReportRelayRequest
	deps := selfReportDeps(w, &now)
	deps.observations = ledger
	deps.budgetOK = func() bool { return false }
	deps.relay = func(_ context.Context, req selfReportRelayRequest) (selfReportRelayResponse, error) {
		relayed = append(relayed, req)
		return selfReportRelayResponse{OK: true, Number: 7, URL: "https://github.com/hivecommons/hive/issues/7"}, nil
	}
	report := fleetreport.Report{
		Fingerprint: fp,
		Trigger:     selfReportObservationClass,
		Class:       selfReportObservationClass,
		Title:       "Queue depth is growing",
		Body:        "<!-- hive-fleet-fingerprint:h1:queue-growth -->\n| series | `[10,20]` |",
	}
	publishFleetReportsWith(context.Background(), slog.Default(), srv, &fleetreport.Result{Reports: []fleetreport.Report{report}}, deps)
	if len(relayed) != 1 || relayed[0].Repo != "hivecommons/hive" || relayed[0].Action != "" || relayed[0].Class != selfReportObservationClass {
		t.Fatalf("observation relay = %#v", relayed)
	}
	records, err := ledger.Records()
	if err != nil {
		t.Fatal(err)
	}
	if records[fp].IssueNumber != 7 || records[fp].Before != "10" || records[fp].After != "20" {
		t.Fatalf("observation ledger = %#v", records[fp])
	}
}
