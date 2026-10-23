package main

import (
	"context"
	"encoding/json"
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
	rollups   []int
	rollupMsg []string
	lookup    func(fp string) (github.FleetReportResult, bool, error)
	// clearedErr, when set, fails PostFleetReportCleared; clearCalls counts
	// every attempt, successful or not.
	clearedErr func() error
	clearCalls int
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
	f.clearCalls++
	if f.clearedErr != nil {
		if err := f.clearedErr(); err != nil {
			return err
		}
	}
	if !strings.Contains(body, "Cleared on") {
		return errors.New("not a cleared comment: " + body)
	}
	f.cleared = append(f.cleared, n)
	return nil
}

func (f *fakeSelfReportWriter) PostFleetReportRollup(_ context.Context, _ string, n int, report fleetreport.Report) error {
	f.rollups = append(f.rollups, n)
	f.rollupMsg = append(f.rollupMsg, report.Body)
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
	// ACMM shortfalls keep the recovery (comment + close-if-owner) path.
	srv.UpdateFleetReportState(func(st *fleetreport.State) {
		st.Open["fp-d"] = fleetreport.OpenIssue{Number: 3, OpenedByHive: true, Trigger: fleetreport.TriggerACMMShortfall, Criterion: "green-ci-streak", PostedAt: now}
	})
	publishFleetReportsWith(context.Background(), slog.Default(), srv, &fleetreport.Result{Recoveries: []fleetreport.Report{{Fingerprint: "fp-d", Trigger: fleetreport.TriggerACMMShortfall, Criterion: "green-ci-streak", Body: "r"}}}, deps)
	if len(w.recovered) != 1 || w.recovered[0] != 3 {
		t.Fatalf("acmm recovery = %v", w.recovered)
	}
}

// An ACMM shortfall that Evaluate rolled up onto this hive's open
// self-report (#13470) is posted as one comment on that issue, never as a
// new issue, and its state entry points at the self-report.
func TestPublishFleetReportsRollsUpOntoOpenSelfReport(t *testing.T) {
	srv := selfReportTestServer(t)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	w := &fakeSelfReportWriter{ensure: func(fleetreport.Report, github.FleetReportOptions) (github.FleetReportResult, error) {
		t.Fatal("a roll-up must not reach the create/join writer")
		return github.FleetReportResult{}, nil
	}}
	deps := selfReportDeps(w, &now)
	srv.UpdateFleetReportState(func(st *fleetreport.State) {
		st.Open["fp-self"] = fleetreport.OpenIssue{Number: 21, URL: "https://github.com/hivecommons/hive/issues/21", OpenedByHive: true, Trigger: fleetreport.TriggerRuntimeError, Class: fleetreport.ClassAgentStall, Component: "agent-runtime", PostedAt: now}
	})
	report := fleetreport.Report{Fingerprint: "fp-acmm", Trigger: fleetreport.TriggerACMMShortfall, Criterion: "green-ci-streak", RollupOf: "fp-self", Body: "rolled up", Evidence: []fleetreport.Evidence{{Component: "agent-runtime"}}}
	publishFleetReportsWith(context.Background(), slog.Default(), srv, &fleetreport.Result{Reports: []fleetreport.Report{report}}, deps)
	if len(w.rollups) != 1 || w.rollups[0] != 21 || w.rollupMsg[0] != "rolled up" {
		t.Fatalf("rollups=%v msgs=%v", w.rollups, w.rollupMsg)
	}
	got := srv.FleetReportState().Open["fp-acmm"]
	if got.Number != 21 || got.OpenedByHive || !got.Joined || got.RollupOf != "fp-self" || got.Component != "agent-runtime" || got.Trigger != fleetreport.TriggerACMMShortfall {
		t.Fatalf("state = %+v", got)
	}
	if len(srv.FleetReportState().Filings) != 0 {
		t.Fatal("a roll-up must not count as a new filing")
	}

	// A target that has no issue number yet is skipped, not created.
	w2 := &fakeSelfReportWriter{ensure: w.ensure}
	srv2 := selfReportTestServer(t)
	srv2.UpdateFleetReportState(func(st *fleetreport.State) {
		st.Open["fp-self"] = fleetreport.OpenIssue{Trigger: fleetreport.TriggerRuntimeError, Component: "agent-runtime"}
	})
	publishFleetReportsWith(context.Background(), slog.Default(), srv2, &fleetreport.Result{Reports: []fleetreport.Report{report}}, selfReportDeps(w2, &now))
	if len(w2.rollups) != 0 || srv2.FleetReportState().Open["fp-acmm"].Number != 0 {
		t.Fatalf("rollup without a target number: rollups=%v state=%+v", w2.rollups, srv2.FleetReportState().Open["fp-acmm"])
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
	report := runtimeReport("fp-relay", fleetreport.ClassInferenceAuthFailed)
	report.InstanceID, report.Repos, report.Owners = "inst-x", []string{"acme/widgets"}, []string{"alice", "bob"}
	report.HiveName, report.SpokeURL = "kellya", "https://kellya.hive.example.dev"
	res := &fleetreport.Result{Reports: []fleetreport.Report{report}}
	publishFleetReportsWith(context.Background(), slog.Default(), srv, res, deps)
	if len(relayed) != 1 || relayed[0].HiveID != "hosted-x" || relayed[0].Fingerprint != "fp-relay" || relayed[0].Repo != "hivecommons/hive" || relayed[0].Class != fleetreport.ClassInferenceAuthFailed {
		t.Fatalf("relayed = %#v", relayed)
	}
	// The hive's identity rides on the wire so the hub can name WHO is
	// affected (#12529): instance, repos and owner logins as explicit fields.
	if relayed[0].InstanceID != "inst-x" || strings.Join(relayed[0].Repos, ",") != "acme/widgets" || strings.Join(relayed[0].Owners, ",") != "alice,bob" || relayed[0].OwnersHidden {
		t.Fatalf("relayed identity = %#v", relayed[0])
	}
	wire, _ := json.Marshal(relayed[0])
	if !strings.Contains(string(wire), `"owners":["alice","bob"]`) {
		t.Fatalf("wire payload lacks owners: %s", wire)
	}
	// #13211: display name and dashboard URL travel as explicit fields too.
	if !strings.Contains(string(wire), `"hive_name":"kellya"`) || !strings.Contains(string(wire), `"spoke_url":"https://kellya.hive.example.dev"`) {
		t.Fatalf("wire payload lacks hive identity: %s", wire)
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

func TestLogSelfReportFailureWarnsOncePerError(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	msg, fp := selfReportWriteFailedMsg, "fp-warn-once"
	t.Cleanup(func() { clearSelfReportFailure(msg, fp) })
	count := func(level string) int { return strings.Count(buf.String(), "level="+level) }

	errA := errors.New("boom")
	logSelfReportFailure(logger, msg, fp, errA)
	logSelfReportFailure(logger, msg, fp, errA)
	logSelfReportFailure(logger, msg, fp, errA)
	if count("WARN") != 1 || count("DEBUG") != 2 {
		t.Fatalf("want 1 warn + 2 debug, got:\n%s", buf.String())
	}
	logSelfReportFailure(logger, msg, fp, errors.New("different"))
	if count("WARN") != 2 {
		t.Fatalf("changed error should warn again:\n%s", buf.String())
	}
	clearSelfReportFailure(msg, fp)
	logSelfReportFailure(logger, msg, fp, errA)
	if count("WARN") != 3 {
		t.Fatalf("failure after success should warn again:\n%s", buf.String())
	}
}

// #13827: a cleared comment GitHub refuses with a permanent 403 is tried
// once, warned once and recorded as given up; later cycles never retry it.
func TestPublishClearedCommentPermanentErrorIsTerminal(t *testing.T) {
	srv := selfReportTestServer(t)
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	w := &fakeSelfReportWriter{clearedErr: func() error {
		return &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusForbidden}, Message: "Resource not accessible by integration"}
	}}
	deps := selfReportDeps(w, &now)
	fp := "fp-cleared-403"
	t.Cleanup(func() { clearSelfReportFailure(selfReportClearedFailedMsg, fp) })
	srv.UpdateFleetReportState(func(st *fleetreport.State) {
		st.Open[fp] = fleetreport.OpenIssue{Number: 13666, Trigger: fleetreport.TriggerRuntimeError, Class: fleetreport.ClassPanicRecovered, PostedAt: now}
	})
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rec := &fleetreport.Result{Recoveries: []fleetreport.Report{{Fingerprint: fp, Trigger: fleetreport.TriggerRuntimeError, Class: fleetreport.ClassPanicRecovered}}}
	for i := 0; i < 5; i++ {
		publishFleetReportsWith(context.Background(), logger, srv, rec, deps)
		now = now.Add(time.Second)
	}
	if w.clearCalls != 1 {
		t.Fatalf("cleared attempts = %d, want 1", w.clearCalls)
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Fatalf("warns = %d, want 1:\n%s", n, buf.String())
	}
	o := srv.FleetReportState().Open[fp]
	if !o.Recovered || !strings.Contains(o.ClearedNotifyFailed, "Resource not accessible") || !o.ClearedNextAttemptAt.IsZero() {
		t.Fatalf("terminal failure not recorded: %#v", o)
	}
	// A day later (and across a restart: the state is persisted) still nothing.
	now = now.Add(24 * time.Hour)
	publishFleetReportsWith(context.Background(), logger, srv, rec, deps)
	if w.clearCalls != 1 {
		t.Fatalf("terminal cleared comment retried: calls = %d", w.clearCalls)
	}
}

// A transient 5xx is retried, but only after an exponential backoff, and is
// given up after selfReportClearedMaxAttempts.
func TestPublishClearedCommentTransientErrorBacksOff(t *testing.T) {
	srv := selfReportTestServer(t)
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	failing := true
	w := &fakeSelfReportWriter{clearedErr: func() error {
		if failing {
			return &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusBadGateway}, Message: "bad gateway"}
		}
		return nil
	}}
	deps := selfReportDeps(w, &now)
	fp := "fp-cleared-502"
	t.Cleanup(func() { clearSelfReportFailure(selfReportClearedFailedMsg, fp) })
	srv.UpdateFleetReportState(func(st *fleetreport.State) {
		st.Open[fp] = fleetreport.OpenIssue{Number: 21, Trigger: fleetreport.TriggerRuntimeError, Class: fleetreport.ClassPanicRecovered, PostedAt: now}
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := &fleetreport.Result{Recoveries: []fleetreport.Report{{Fingerprint: fp, Trigger: fleetreport.TriggerRuntimeError, Class: fleetreport.ClassPanicRecovered}}}

	publishFleetReportsWith(context.Background(), logger, srv, rec, deps)
	o := srv.FleetReportState().Open[fp]
	if w.clearCalls != 1 || o.Recovered || o.ClearedAttempts != 1 || !o.ClearedNextAttemptAt.Equal(now.Add(selfReportClearedBaseBackoff)) {
		t.Fatalf("after first failure: calls=%d state=%#v", w.clearCalls, o)
	}
	// Inside the backoff window: no call.
	now = now.Add(30 * time.Second)
	publishFleetReportsWith(context.Background(), logger, srv, rec, deps)
	if w.clearCalls != 1 {
		t.Fatalf("retried inside backoff: calls=%d", w.clearCalls)
	}
	// Past it: retried, and the next window doubles.
	now = now.Add(31 * time.Second)
	publishFleetReportsWith(context.Background(), logger, srv, rec, deps)
	o = srv.FleetReportState().Open[fp]
	if w.clearCalls != 2 || o.ClearedAttempts != 2 || !o.ClearedNextAttemptAt.Equal(now.Add(2*selfReportClearedBaseBackoff)) {
		t.Fatalf("after second failure: calls=%d state=%#v", w.clearCalls, o)
	}
	// Recovery of GitHub: the comment lands and the retry state is cleared.
	failing = false
	now = now.Add(2 * selfReportClearedBaseBackoff)
	publishFleetReportsWith(context.Background(), logger, srv, rec, deps)
	o = srv.FleetReportState().Open[fp]
	if w.clearCalls != 3 || len(w.cleared) != 1 || !o.Recovered || o.ClearedAttempts != 0 || !o.ClearedNextAttemptAt.IsZero() || o.ClearedNotifyFailed != "" {
		t.Fatalf("after success: calls=%d cleared=%v state=%#v", w.clearCalls, w.cleared, o)
	}

	// A comment that keeps failing transiently is given up at the cap.
	fp2 := "fp-cleared-502-cap"
	t.Cleanup(func() { clearSelfReportFailure(selfReportClearedFailedMsg, fp2) })
	failing = true
	srv.UpdateFleetReportState(func(st *fleetreport.State) {
		st.Open[fp2] = fleetreport.OpenIssue{Number: 22, Trigger: fleetreport.TriggerRuntimeError, Class: fleetreport.ClassPanicRecovered, PostedAt: now}
	})
	rec2 := &fleetreport.Result{Recoveries: []fleetreport.Report{{Fingerprint: fp2, Trigger: fleetreport.TriggerRuntimeError, Class: fleetreport.ClassPanicRecovered}}}
	start := w.clearCalls
	for i := 0; i < selfReportClearedMaxAttempts+3; i++ {
		publishFleetReportsWith(context.Background(), logger, srv, rec2, deps)
		now = now.Add(selfReportClearedMaxBackoff)
	}
	o = srv.FleetReportState().Open[fp2]
	if w.clearCalls-start != selfReportClearedMaxAttempts || !o.Recovered || o.ClearedNotifyFailed == "" {
		t.Fatalf("attempt cap: calls=%d state=%#v", w.clearCalls-start, o)
	}
}

func TestSelfReportClearedPermanent(t *testing.T) {
	cases := map[error]bool{
		&gh.ErrorResponse{Response: &http.Response{StatusCode: 403}}:                                                                          true,
		&gh.ErrorResponse{Response: &http.Response{StatusCode: 404}}:                                                                          true,
		&gh.ErrorResponse{Response: &http.Response{StatusCode: 410}}:                                                                          true,
		&gh.ErrorResponse{Response: &http.Response{StatusCode: 422}}:                                                                          true,
		&gh.ErrorResponse{Response: &http.Response{StatusCode: 429}}:                                                                          false,
		&gh.ErrorResponse{Response: &http.Response{StatusCode: 502}}:                                                                          false,
		errors.New("POST https://api.github.com/repos/hivecommons/hive/issues/13666/comments: 403 Resource not accessible by integration []"): true,
		errors.New("network down"): false,
	}
	for err, want := range cases {
		if got := selfReportClearedPermanent(err); got != want {
			t.Fatalf("selfReportClearedPermanent(%v) = %v want %v", err, got, want)
		}
	}
}
