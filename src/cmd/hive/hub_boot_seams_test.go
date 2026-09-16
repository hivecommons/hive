package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hub"
)

// Tests for the hub boot seam (#7224). runHub was 0% covered because it
// started network pollers, bound a port and called os.Exit; runHubWithDeps
// makes the wiring above those effects reachable.

// capturingLogger returns a logger writing to a buffer, plus a func returning
// everything logged so far. Assertions are on the log because the wiring's
// observable effects live on unexported HubServer fields.
func capturingLogger() (*slog.Logger, func() string) {
	var mu sync.Mutex
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, buf: buf}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logger, func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type lockedWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// isolateHubServer keeps hub.NewHubServer from touching the real hub secret
// file: without a secret in the environment it generates one and writes it to
// a fixed absolute path.
func isolateHubServer(t *testing.T) {
	t.Helper()
	t.Setenv("HIVE_HUB_SECRET", "test-secret-not-a-real-credential")
}

func TestHubPortFromEnv(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want int
	}{
		// The fallback is written as a literal, NOT as defaultHubPort:
		// comparing the constant to itself would pass for any value and
		// would not notice the documented default silently changing.
		{"unset falls back to default", "", 3001},
		{"valid port is used", "8080", 8080},
		{"non-numeric falls back rather than failing boot", "not-a-port", 3001},
		{"trailing garbage falls back", "8080x", 3001},
		{"explicit default", "3001", 3001},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hubPortFromEnv(func(string) string { return tt.env })
			if got != tt.want {
				t.Fatalf("hubPortFromEnv(%q) = %d, want %d", tt.env, got, tt.want)
			}
		})
	}
}

// TestHubPortFromEnvReadsTheRightKey pins the variable name: a rename would
// silently move every hub to the default port.
func TestHubPortFromEnvReadsTheRightKey(t *testing.T) {
	var asked []string
	hubPortFromEnv(func(k string) string {
		asked = append(asked, k)
		return ""
	})
	if len(asked) != 1 || asked[0] != "HIVE_HUB_PORT" {
		t.Fatalf("hubPortFromEnv read %v, want exactly [HIVE_HUB_PORT]", asked)
	}
}

func TestWireHubHooksMissingConfigIsSilent(t *testing.T) {
	logger, logs := capturingLogger()
	wireHubHooks(logger, filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if strings.Contains(logs(), "hub hooks disabled") {
		t.Fatalf("a missing config is not an error, but it warned:\n%s", logs())
	}
}

func TestWireHubHooksMalformedConfigWarns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte("\tnot: [valid: yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger, logs := capturingLogger()
	wireHubHooks(logger, path)
	if !strings.Contains(logs(), "hub hooks disabled") {
		t.Fatalf("a config that exists but cannot be parsed must warn, got:\n%s", logs())
	}
}

func TestWireHubReachGatesPRSourceOnToken(t *testing.T) {
	isolateHubServer(t)
	tests := []struct {
		name        string
		token       string
		wantWired   bool
		wantDisable bool
	}{
		{"token present wires the github source", "ghp_testtoken", true, false},
		{"no token leaves the endpoint at 503", "", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, logs := capturingLogger()
			srv := hub.NewHubServer(0, logger, "abc1234", "v4")
			wireHubReach(srv, logger, tt.token)
			gotWired := strings.Contains(logs(), "reach PR source wired")
			gotDisabled := strings.Contains(logs(), "reach PR source disabled")
			if gotWired != tt.wantWired || gotDisabled != tt.wantDisable {
				t.Fatalf("token=%q: wired=%v disabled=%v, want wired=%v disabled=%v\n%s",
					tt.token, gotWired, gotDisabled, tt.wantWired, tt.wantDisable, logs())
			}
		})
	}
}

// TestWireHubReachNeverLeaksTheToken guards the log line added alongside the
// gate: it must name the condition, not the credential.
func TestWireHubReachNeverLeaksTheToken(t *testing.T) {
	isolateHubServer(t)
	const token = "ghp_supersecretvalue"
	logger, logs := capturingLogger()
	srv := hub.NewHubServer(0, logger, "abc1234", "v4")
	wireHubReach(srv, logger, token)
	if strings.Contains(logs(), token) {
		t.Fatalf("boot log leaked the GitHub token:\n%s", logs())
	}
}

func TestShutdownHubOnSignalShutsDownGracefully(t *testing.T) {
	isolateHubServer(t)
	logger, logs := capturingLogger()
	srv := hub.NewHubServer(0, logger, "abc1234", "v4")

	sigCh := make(chan os.Signal, 1)
	sigCh <- syscall.SIGTERM
	shutdownHubOnSignal(sigCh, srv, logger)

	if !strings.Contains(logs(), "hub received signal") {
		t.Fatalf("a termination signal must trigger graceful shutdown, got:\n%s", logs())
	}
}

// TestShutdownHubOnSignalReturnsOnClosedChannel proves the goroutine cannot
// leak: runHubWithDeps calls signal.Stop and the channel is abandoned, so the
// watcher must also survive a close without shutting the server down.
func TestShutdownHubOnSignalReturnsOnClosedChannel(t *testing.T) {
	isolateHubServer(t)
	logger, logs := capturingLogger()
	srv := hub.NewHubServer(0, logger, "abc1234", "v4")

	sigCh := make(chan os.Signal)
	close(sigCh)

	done := make(chan struct{})
	go func() {
		shutdownHubOnSignal(sigCh, srv, logger)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdownHubOnSignal blocked on a closed channel")
	}
	if strings.Contains(logs(), "hub received signal") {
		t.Fatalf("a closed channel is not a signal, but shutdown ran:\n%s", logs())
	}
}

// stubDeps records what runHubWithDeps did instead of performing it.
type stubDeps struct {
	pollerCtx   atomic.Value
	pollerCalls atomic.Int32
	servePort   atomic.Int32
	fatalCode   atomic.Int32
	fatalCalls  atomic.Int32
}

func (s *stubDeps) deps(serveErr error) hubDeps {
	s.fatalCode.Store(-1)
	return hubDeps{
		startPollers: func(ctx context.Context, _ *hub.HubServer) {
			s.pollerCalls.Add(1)
			s.pollerCtx.Store(ctx)
		},
		serve: func(_ *hub.HubServer, port int) error {
			s.servePort.Store(int32(port))
			return serveErr
		},
		fatal: func(code int) {
			s.fatalCalls.Add(1)
			s.fatalCode.Store(int32(code))
		},
	}
}

func TestRunHubWithDepsCleanStopDoesNotExit(t *testing.T) {
	isolateHubServer(t)
	t.Setenv("HIVE_HUB_PORT", "18080")
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	logger, logs := capturingLogger()

	st := &stubDeps{}
	runHubWithDeps(context.Background(), logger, filepath.Join(t.TempDir(), "none.yaml"), st.deps(http.ErrServerClosed))

	if st.fatalCalls.Load() != 0 {
		t.Fatalf("ErrServerClosed is a clean stop, but fatal was called with %d", st.fatalCode.Load())
	}
	if !strings.Contains(logs(), "hub server stopped") {
		t.Fatalf("clean stop must be logged, got:\n%s", logs())
	}
	if got := st.servePort.Load(); got != 18080 {
		t.Fatalf("served on port %d, want the HIVE_HUB_PORT value 18080", got)
	}
	if st.pollerCalls.Load() != 1 {
		t.Fatalf("background pollers started %d times, want exactly 1", st.pollerCalls.Load())
	}
}

func TestRunHubWithDepsServeFailureIsFatal(t *testing.T) {
	isolateHubServer(t)
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	logger, logs := capturingLogger()

	st := &stubDeps{}
	runHubWithDeps(context.Background(), logger, filepath.Join(t.TempDir(), "none.yaml"),
		st.deps(errors.New("bind: address already in use")))

	if st.fatalCalls.Load() != 1 || st.fatalCode.Load() != 1 {
		t.Fatalf("a real serve error must exit(1); calls=%d code=%d", st.fatalCalls.Load(), st.fatalCode.Load())
	}
	if !strings.Contains(logs(), "hub server failed") {
		t.Fatalf("serve failure must be logged, got:\n%s", logs())
	}
	if strings.Contains(logs(), "hub server stopped") {
		t.Fatalf("a failed server must not report a clean stop:\n%s", logs())
	}
}

// TestRunHubWithDepsPassesCallerContextToPollers is the reason the seam takes a
// ctx at all: the original hard-coded context.Background(), so the pollers
// could never be cancelled and nothing could test them.
func TestRunHubWithDepsPassesCallerContextToPollers(t *testing.T) {
	isolateHubServer(t)
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	logger, _ := capturingLogger()

	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "caller-owned")

	st := &stubDeps{}
	runHubWithDeps(ctx, logger, filepath.Join(t.TempDir(), "none.yaml"), st.deps(http.ErrServerClosed))

	got, _ := st.pollerCtx.Load().(context.Context)
	if got == nil || got.Value(ctxKey{}) != "caller-owned" {
		t.Fatal("pollers must receive the caller's context, not a fresh Background()")
	}
}

// TestRunHubWithDepsWiresReachBeforeStartingPollers pins the ordering: a
// poller must never observe a half-wired server.
func TestRunHubWithDepsWiresReachBeforeStartingPollers(t *testing.T) {
	isolateHubServer(t)
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	logger, logs := capturingLogger()

	var snapshotAtPollerStart string
	deps := hubDeps{
		startPollers: func(context.Context, *hub.HubServer) {
			snapshotAtPollerStart = logs()
		},
		serve: func(*hub.HubServer, int) error { return http.ErrServerClosed },
		fatal: func(int) { t.Error("unexpected fatal") },
	}
	runHubWithDeps(context.Background(), logger, filepath.Join(t.TempDir(), "none.yaml"), deps)

	if !strings.Contains(logs(), "reach PR source disabled") {
		t.Fatalf("reach wiring did not run at all:\n%s", logs())
	}
	if !strings.Contains(snapshotAtPollerStart, "reach PR source disabled") {
		t.Fatalf("pollers started before reach was wired; log at poller start was:\n%s", snapshotAtPollerStart)
	}
}

func TestRunHubDefaultDepsAreProductionWiring(t *testing.T) {
	d := defaultHubDeps()
	if d.startPollers == nil || d.serve == nil || d.fatal == nil {
		t.Fatal("defaultHubDeps must supply all three production effects")
	}
}
