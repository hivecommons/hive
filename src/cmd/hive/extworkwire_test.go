package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/extwork"
	"github.com/hivecommons/hive/pkg/outputschema"
)

type fakeExtAdapter struct{ starts int }

func (f *fakeExtAdapter) Engine() string { return extExecEngineFlue }
func (f *fakeExtAdapter) Start(context.Context, extwork.StartRequest) (extwork.StartResult, error) {
	f.starts++
	return extwork.StartResult{RemoteRunID: "r", RemoteIncarnation: "i"}, nil
}
func (f *fakeExtAdapter) Observe(context.Context, extwork.ExecutionKey, string) (extwork.Observation, error) {
	return extwork.Observation{}, extwork.ErrNotFound
}
func (f *fakeExtAdapter) Cancel(context.Context, extwork.ExecutionKey, string) (extwork.CancelFacts, error) {
	return extwork.CancelFacts{}, nil
}
func (f *fakeExtAdapter) OpenArtifact(context.Context, extwork.ExecutionKey, string, string) (io.ReadCloser, error) {
	return nil, extwork.ErrNotFound
}

// TestExternalFlueBindingFailsClosed: every refusal is one of the typed
// fail-closed sentinels and nothing is dispatched. The default registry is
// checked as this build sees it, so the test holds with and without the
// extwork_flue tag.
func TestExternalFlueBindingFailsClosed(t *testing.T) {
	prevDir := outputschema.AgentReportDir
	outputschema.AgentReportDir = t.TempDir()
	t.Cleanup(func() { outputschema.AgentReportDir = prevDir })
	srv := dashboard.NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cfg := &config.Config{}
	clock := func() time.Time { return time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC) }

	failClosed := func(err error) bool {
		return errors.Is(err, errExternalBindingOff) || errors.Is(err, errExternalHubNotRunning) || errors.Is(err, extwork.ErrEngineNotLinked)
	}
	if _, err := newExternalFlueBinding(srv, cfg, extwork.DefaultRegistry, clock); !errors.Is(err, errExternalBindingOff) {
		t.Fatalf("default config = %v, want off", err)
	}
	cfg.Runs.External.Flue.Enabled = true
	if _, err := newExternalFlueBinding(srv, cfg, extwork.DefaultRegistry, clock); !failClosed(err) {
		t.Fatalf("enabled without hub = %v, want a fail-closed sentinel", err)
	}
	if _, err := newExternalFlueBinding(srv, cfg, extwork.NewRegistry(), clock); !errors.Is(err, extwork.ErrEngineNotLinked) {
		t.Fatalf("empty registry = %v", err)
	}
	status := extworkStatus{registry: extwork.DefaultRegistry}
	if status.Linked("nope") || (extworkStatus{}).Linked(extExecEngineFlue) {
		t.Fatal("status reported an unlinked engine")
	}

	// A linked fake engine: no hub is still fail-closed; with a hub the
	// binding builds and shadow mode never starts.
	fake := &fakeExtAdapter{}
	reg := extwork.NewRegistry()
	reg.Register(extExecEngineFlue, func(settings map[string]string) (extwork.Adapter, error) {
		if settings[extwork.SettingEndpoint] != "http://127.0.0.1:1" {
			t.Errorf("settings = %v", settings)
		}
		return fake, nil
	})
	cfg.Runs.External.Flue.Endpoint = "http://127.0.0.1:1"
	if _, err := newExternalFlueBinding(srv, cfg, reg, clock); !errors.Is(err, errExternalHubNotRunning) {
		t.Fatalf("linked engine without hub = %v", err)
	}
	if !(extworkStatus{registry: reg}).Linked(extExecEngineFlue) {
		t.Fatal("status must report the linked fake engine")
	}
	srv.RegisterAPI(&dashboard.Dependencies{Config: cfg, ExternalExec: extworkStatus{registry: reg}})
	if srv.ContributeHub() == nil {
		t.Skip("RegisterAPI did not create a contributor hub in this configuration")
	}
	b, err := newExternalFlueBinding(srv, cfg, reg, nil)
	if err != nil || b.Mode() != extwork.ModeShadow {
		t.Fatalf("shadow binding = %v %v", b, err)
	}
	adm := extwork.Admission{
		WorkKey: "hivecommons/hive#8361", AssignmentID: "task-8361", Generation: 1, Stage: "implement",
		ContractRevision: "c1", Engine: extExecEngineFlue, WorkflowVersion: "v1",
		InputRevision: "0123456789abcdef0123456789abcdef01234567", RequestDigest: extwork.RequestDigest([]byte("bundle")),
		Authority: extwork.AuthorityBinding{Identity: "relay", Tier: "C4", Capability: "ext-exec/flue", Mode: extwork.ModeShadow},
	}
	// No lease is held for this admission: the lease authority refuses it
	// before anything is written or started.
	if _, err := b.Dispatch(context.Background(), adm, []byte("bundle")); !errors.Is(err, extwork.ErrLeaseAuthority) || fake.starts != 0 {
		t.Fatalf("dispatch without a lease = %v starts=%d", err, fake.starts)
	}
	cfg.Runs.External.Flue.Mode = config.FlueBindingModeReportOnly
	if b, err := newExternalFlueBinding(srv, cfg, reg, clock); err != nil || b.Mode() != extwork.ModeReportOnly {
		t.Fatalf("report-only binding = %v %v", b, err)
	}
}
