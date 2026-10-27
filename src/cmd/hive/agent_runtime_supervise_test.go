package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api/callback"
	"github.com/hivecommons/hive/crux/api/client"
	cruxconfig "github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/cruxtest"
	cruxruntime "github.com/hivecommons/hive/crux/runtime"
	"github.com/hivecommons/hive/crux/serve"
	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/fleetreport"
)

// conditionLog records fleet-report Observe/Clear calls.
type conditionLog struct {
	mu      sync.Mutex
	raised  []string
	cleared []string
}

func (c *conditionLog) observe(class, key, _, _ string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.raised = append(c.raised, class+"/"+key)
}

func (c *conditionLog) clear(class, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cleared = append(c.cleared, class+"/"+key)
}

func (c *conditionLog) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.raised), len(c.cleared)
}

// liveServe is a `crux serve` over a recorder, reachable at URL.
func liveServe(t *testing.T) (*cruxtest.Recorder, *httptest.Server) {
	t.Helper()
	rec := cruxtest.NewRecorder()
	srv := serve.New(rec)
	srv.Token = "tok"
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		// httptest.Server.Close waits for in-flight requests; a runtime
		// holding a long-lived request open would hang it until the go test
		// deadline. Drop the connections first and bound the wait.
		ts.CloseClientConnections()
		done := make(chan struct{})
		go func() { ts.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("live crux serve did not shut down within 10s")
		}
	})
	return rec, ts
}

func newSupervisionFor(t *testing.T, server string, supervise cruxconfig.Supervision) (*cruxRuntimeSupervision, *conditionLog, *syncLogBuffer) {
	t.Helper()
	cfg := cruxconfig.RuntimeConfig{Mode: cruxconfig.ModeSidecar, Server: server, Token: "tok", Supervise: supervise, Binary: filepath.Join(t.TempDir(), "no-such-crux")}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	rt, err := client.New(cfg.Server, cfg.Token)
	if err != nil {
		t.Fatal(err)
	}
	rt.Timeout = 2 * time.Second
	// The supervisor logs from a background goroutine; a bare bytes.Buffer
	// races with the test's log.String() reads under -race.
	var log syncLogBuffer
	logger := slog.New(slog.NewTextHandler(&log, nil))
	hr, err := hostRuntime(cfg, rt, logger)
	if err != nil {
		t.Fatal(err)
	}
	s := newCruxRuntimeSupervision(cfg, hr, logger)
	if s == nil {
		t.Fatal("no supervision for a sidecar client")
	}
	cl := &conditionLog{}
	s.observe, s.clear = cl.observe, cl.clear
	s.interval = 20 * time.Millisecond
	return s, cl, &log
}

func TestCruxRuntimeSupervision_NilOutsideSidecar(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if s := newCruxRuntimeSupervision(cruxconfig.RuntimeConfig{Mode: cruxconfig.ModeInProcess}, cruxtest.NewRecorder(), logger); s != nil {
		t.Fatal("in-process must not be supervised")
	}
	// A sidecar runtime without the client's probe surface (test fakes) is
	// left alone rather than panicking.
	cfg := cruxconfig.RuntimeConfig{Mode: cruxconfig.ModeSidecar, Server: "127.0.0.1:1", Supervise: cruxconfig.SuperviseChild}
	if s := newCruxRuntimeSupervision(cfg, newSidecarHost(newSidecarRuntimeFake(), logger), logger); s != nil {
		t.Fatal("non-prober runtime supervised")
	}
	if cruxRuntimeHealthFunc(nil) != nil {
		t.Fatal("health hook for nil supervision")
	}
}

func TestCruxRuntimeSupervision_ConditionFollowsReachability(t *testing.T) {
	_, ts := liveServe(t)
	s, cl, log := newSupervisionFor(t, ts.URL, cruxconfig.SuperviseExternal)
	if s.supervisor != nil {
		t.Fatal("external supervision spawned a child supervisor")
	}
	ctx := context.Background()

	s.probe(ctx)
	if down := s.reconcile(false); down {
		t.Fatal("live server reported down")
	}
	h := s.health()
	if !h.Reachable || h.Mode != "sidecar" || h.Supervise != "external" || h.Child != nil {
		t.Fatalf("health up = %+v", h)
	}

	ts.Close()
	s.probe(ctx)
	if down := s.reconcile(false); !down {
		t.Fatal("closed server reported up")
	}
	// Staying down does not re-raise.
	s.probe(ctx)
	s.reconcile(true)
	if r, c := cl.counts(); r != 1 || c != 0 || cl.raised[0] != fleetreport.ClassCruxRuntimeUnavailable+"/"+cruxRuntimeConditionKey {
		t.Fatalf("conditions after outage = %v / %v", cl.raised, cl.cleared)
	}
	h = s.health()
	if h.Reachable || h.LastError == "" || h.Since.IsZero() {
		t.Fatalf("health down = %+v", h)
	}
	if !strings.Contains(log.String(), "kicks refused until it returns") {
		t.Fatalf("outage not logged:\n%s", log.String())
	}
	// Kicks through the hosted runtime are refused with the sentinel.
	if err := s.prober.(crux.Runtime).SendKick("a", "m"); !errors.Is(err, crux.ErrRuntimeUnavailable) {
		t.Fatalf("SendKick during outage = %v", err)
	}
	// A transition back up clears exactly once.
	s2, cl2, _ := newSupervisionFor(t, liveServeURL(t), cruxconfig.SuperviseExternal)
	s2.probe(ctx)
	if down := s2.reconcile(true); down {
		t.Fatal("recovered server still down")
	}
	if r, c := cl2.counts(); r != 0 || c != 1 {
		t.Fatalf("conditions after recovery = %v / %v", cl2.raised, cl2.cleared)
	}
}

func liveServeURL(t *testing.T) string {
	t.Helper()
	_, ts := liveServe(t)
	return ts.URL
}

func TestCruxRuntimeSupervision_StartWatchesAndReportsMissingBinary(t *testing.T) {
	s, cl, log := newSupervisionFor(t, "127.0.0.1:1", cruxconfig.SuperviseChild)
	if s.supervisor == nil {
		t.Fatal("child supervision without a supervisor")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.start(ctx)
	testutil.Eventually(t, 5*time.Second, func() bool {
		r, _ := cl.counts()
		return r == 1 && strings.Contains(log.String(), "child supervision disabled: binary not found")
	}, "watcher did not raise the condition / supervisor did not report the missing binary:\n%s", log.String())
	h := s.health()
	if h.Reachable || h.Supervise != "child" || h.Child == nil || h.Child.Running {
		t.Fatalf("health = %+v child=%+v", h, h.Child)
	}
	cancel()
}

func TestBootAgentsWith_SidecarStartsSupervision(t *testing.T) {
	t.Setenv(cruxconfig.ModeEnvVar, string(cruxconfig.ModeSidecar))
	t.Setenv(cruxconfig.SuperviseEnvVar, string(cruxconfig.SuperviseExternal))
	_, ts := liveServe(t)
	t.Setenv(cruxconfig.ServerEnvVar, ts.URL)
	t.Setenv(cruxconfig.ServeTokenEnvVar, "tok")
	f := newBootAgentsFake()
	f.deps.newRuntime = func(cfg cruxconfig.RuntimeConfig, opts ...cruxruntime.Option) (hostedRuntime, error) {
		return newHostedRuntime(cfg, opts...)
	}
	f.deps.startHiveCallback = func(context.Context, callback.Capabilities, *slog.Logger) error { return nil }
	b := newBootAgentsBoot(t, f, bootAgentsConfig())

	b.bootAgentsWith(f.deps)

	if len(f.exitCodes) != 0 {
		t.Fatalf("exit codes = %v", f.exitCodes)
	}
	if f.supervised == nil || b.cruxSupervision != f.supervised {
		t.Fatal("sidecar boot did not hand its supervision to the seam")
	}
	if f.supervised.supervisor != nil || f.supervised.cfg.Supervise != cruxconfig.SuperviseExternal {
		t.Fatalf("CRUX_SUPERVISE=external not honoured: %+v", f.supervised.cfg)
	}
	hf := cruxRuntimeHealthFunc(b.cruxSupervision)
	if hf == nil {
		t.Fatal("no /api/health crux_runtime hook")
	}
	if h := hf(); h.Mode != "sidecar" || !h.Reachable {
		t.Fatalf("health = %+v", h)
	}
}

func TestBootAgentsWith_InProcessHasNoSupervision(t *testing.T) {
	f := newBootAgentsFake()
	b := newBootAgentsBoot(t, f, bootAgentsConfig())
	b.bootAgentsWith(f.deps)
	if f.supervised != nil || b.cruxSupervision != nil || cruxRuntimeHealthFunc(b.cruxSupervision) != nil {
		t.Fatal("in-process boot started sidecar supervision")
	}
}
