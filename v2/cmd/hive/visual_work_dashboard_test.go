package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/dashboard"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	visualcontroller "github.com/kubestellar/hive/v2/pkg/visualhive/controller"
	"github.com/kubestellar/hive/v2/pkg/visualhive/normalservice"
)

func TestNormalVisualDashboardGateRequiresLiveHTTPAndClearsOnStop(t *testing.T) {
	stateDir := t.TempDir()
	lease, err := claimNormalVisualDaemonLease(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDaemonLease(lease)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := dashboard.NewServer(0, logger)
	gate, err := newNormalVisualDashboardGate(server, stateDir, "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if gate.Ready() {
		t.Fatal("dashboard gate was ready before listener startup")
	}
	serverResult := make(chan error, 1)
	go func() { serverResult <- server.Start() }()
	readyContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.WaitForHTTP(readyContext); err != nil {
		t.Fatalf("wait for dashboard HTTP: %v", err)
	}
	if !server.MarkReadyIfListening() {
		t.Fatal("live dashboard listener could not be marked ready")
	}
	if err := gate.Activate(time.Now().UTC()); err != nil {
		t.Fatalf("activate dashboard gate: %v", err)
	}
	if !gate.Ready() {
		t.Fatal("dashboard gate did not observe its live HTTP listener")
	}
	status := inspectNormalVisualServiceHealth(stateDir, "owner/repo", time.Minute, time.Now().UTC())
	if !status.DashboardReady || status.ServiceReady || !strings.Contains(status.Message, "has not initialized") {
		t.Fatalf("HTTP-ready dashboard assessment = %+v", status)
	}

	cycle := &dashboardQuiescenceSource{started: make(chan struct{}), canceled: make(chan struct{})}
	service, err := normalservice.New(normalservice.Options{
		StateDir: stateDir, PollInterval: time.Hour, MaxCycleDuration: time.Minute, LeaseRetry: time.Second, QuiesceInterval: 10 * time.Millisecond,
		AcquireLease:     func() (func(), error) { return func() {}, nil },
		ShouldQuiesce:    func() (bool, error) { return !gate.Ready(), nil },
		Source:           cycle,
		Intake:           dashboardQuiescenceIntake{},
		Repairer:         dashboardQuiescenceRepairer{},
		PullRequestState: dashboardQuiescencePullRequestState{},
		Logger:           logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	serviceContext, cancelService := context.WithCancel(context.Background())
	serviceResult := make(chan struct{})
	go func() {
		service.Run(serviceContext)
		close(serviceResult)
	}()
	select {
	case <-cycle.started:
	case <-time.After(2 * time.Second):
		cancelService()
		t.Fatal("normal Visual service did not start its cycle")
	}

	if err := server.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-serverResult:
	case <-time.After(2 * time.Second):
		t.Fatal("dashboard server did not stop")
	}
	if _, exists, err := readNormalVisualDashboardReady(stateDir); err != nil || !exists {
		t.Fatalf("pre-removal dashboard marker exists=%t err=%v", exists, err)
	}
	staleMarkerStatus := inspectNormalVisualServiceHealth(stateDir, "owner/repo", time.Minute, time.Now().UTC())
	if staleMarkerStatus.DashboardReady || staleMarkerStatus.ServiceReady || !strings.Contains(staleMarkerStatus.Message, "failed its live HTTP readiness probe") {
		t.Fatalf("stopped listener with retained marker was not doctor-failed closed: %+v", staleMarkerStatus)
	}
	if err := gate.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cycle.canceled:
	case <-time.After(2 * time.Second):
		cancelService()
		t.Fatal("dashboard listener stop did not cancel the in-flight Visual cycle")
	}
	if serviceContext.Err() != nil {
		t.Fatal("dashboard failure canceled the ordinary Hive parent context")
	}
	select {
	case <-serviceResult:
		t.Fatal("quiesced Visual loop terminated the otherwise-live ordinary Hive process")
	default:
	}
	cancelService()
	select {
	case <-serviceResult:
	case <-time.After(2 * time.Second):
		t.Fatal("normal Visual service did not stop with its parent context")
	}
	if gate.Ready() {
		t.Fatal("dashboard gate remained ready after listener stop")
	}
	if _, exists, err := readNormalVisualDashboardReady(stateDir); err != nil || exists {
		t.Fatalf("stopped dashboard marker exists=%t err=%v", exists, err)
	}
	status = inspectNormalVisualServiceHealth(stateDir, "owner/repo", time.Minute, time.Now().UTC())
	if status.DashboardReady || status.ServiceReady || !strings.Contains(status.Message, "has not proven HTTP readiness") {
		t.Fatalf("stopped dashboard assessment = %+v", status)
	}
}

func TestNormalVisualDashboardInvalidMarkerFailsDoctorClosed(t *testing.T) {
	stateDir := t.TempDir()
	lease, err := claimNormalVisualDaemonLease(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDaemonLease(lease)
	marker, err := writeNormalVisualDashboardReady(stateDir, "owner/repo", 12345, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	marker.Repository = "other/repo"
	if err := writeNormalVisualOwnerJSON(normalVisualDashboardReadyPath(stateDir), ".invalid-dashboard-ready-*.tmp", marker); err != nil {
		t.Fatal(err)
	}
	status := inspectNormalVisualServiceHealth(stateDir, "owner/repo", time.Minute, time.Now().UTC())
	checks := normalVisualServiceDoctorChecks(status)
	if status.DashboardReady || status.ServiceReady || len(checks) != 2 || checks[0].OK || checks[1].OK || !strings.Contains(status.Message, "does not match") {
		t.Fatalf("invalid dashboard marker assessment=%+v checks=%+v", status, checks)
	}
}

type dashboardQuiescenceSource struct {
	started  chan struct{}
	canceled chan struct{}
}

func (source *dashboardQuiescenceSource) Fetch(ctx context.Context) (integrated.NormalVisualWork, error) {
	close(source.started)
	<-ctx.Done()
	close(source.canceled)
	return integrated.NormalVisualWork{}, ctx.Err()
}

func (*dashboardQuiescenceSource) Consume(integrated.WorkflowRunEvidence, bool) error { return nil }

type dashboardQuiescenceIntake struct{}

func (dashboardQuiescenceIntake) Import(context.Context, hivegithub.VerifiedVisualHiveArtifact) (visualcontroller.Result, error) {
	return visualcontroller.Result{}, nil
}
func (dashboardQuiescenceIntake) RevalidateSpecialistBoundary(string, string, string) (visualcontroller.DispatchEnvelope, error) {
	return visualcontroller.DispatchEnvelope{}, nil
}
func (dashboardQuiescenceIntake) DeferUnselectedSpecialistDispatches(context.Context, string, []string, string) error {
	return nil
}
func (dashboardQuiescenceIntake) CompleteSpecialistPullRequest(string, visualcontroller.SpecialistPullRequestCompletion) error {
	return nil
}

type dashboardQuiescenceRepairer struct{}

func (dashboardQuiescenceRepairer) Run(context.Context, visualcontroller.DispatchEnvelope) (normalservice.RepairOutcome, error) {
	return normalservice.RepairOutcome{}, nil
}

type dashboardQuiescencePullRequestState struct{}

func (dashboardQuiescencePullRequestState) ObservePullRequestState(context.Context, normalservice.PullRequestStateRequest) (normalservice.PullRequestStateObservation, error) {
	return normalservice.PullRequestStateObservation{}, nil
}
