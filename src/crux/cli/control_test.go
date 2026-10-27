package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agent"
	"github.com/hivecommons/hive/crux/config"
)

// shortSocketDir returns a directory whose path is short enough for a unix
// socket on every platform (macOS caps sun_path at 104 bytes and t.TempDir
// can exceed it).
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "crux")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// startServingRun launches a foreground `crux run` in the background with the
// control socket at workDir/crux.sock and returns a cancel that ends it and
// waits for its exit code.
func startServingRun(t *testing.T, h *harness, workDir string) (socket string, finish func() int) {
	t.Helper()
	def := writeFile(t, filepath.Join(t.TempDir(), "fixer.yaml"), overlayDefinition)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- h.env.Run(ctx, []string{"--workdir", workDir, "run", def}) }()
	<-h.rt.started
	socket = filepath.Join(workDir, controlSocketName)
	waitForSocket(t, socket)
	return socket, func() int {
		cancel()
		return <-done
	}
}

// waitForSocket blocks until something answers on path. It dials rather
// than sleeps, so it is bounded by the test deadline only.
func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", path)
		if err == nil {
			_ = conn.Close()
			return
		}
	}
	t.Fatalf("control socket %s never came up", path)
}

func TestControlSocketServesObserverCommands(t *testing.T) {
	workDir := shortSocketDir(t)
	server := newHarness(t)
	server.rt.pane["fixer"] = []string{"line one"}
	socket, finish := startServingRun(t, server, workDir)

	// A second CLI process (a second Env, here) with no --config of its own
	// sees the serving runtime through the socket.
	client := &Env{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if code := client.Run(context.Background(), []string{"--workdir", workDir, "ps"}); code != ExitOK {
		t.Fatalf("ps: code %d stderr %q", code, client.Stderr)
	}
	if out := client.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "fixer") || !strings.Contains(out, string(crux.StateRunning)) {
		t.Fatalf("ps over socket = %q", out)
	}

	client = &Env{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if code := client.Run(context.Background(), []string{"--workdir", workDir, "logs", "fixer"}); code != ExitOK || client.Stdout.(*bytes.Buffer).String() != "line one\n" {
		t.Fatalf("logs: code %d out %q stderr %q", code, client.Stdout, client.Stderr)
	}

	client = &Env{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if code := client.Run(context.Background(), []string{"--workdir", workDir, "kick", "fixer", "go"}); code != ExitOK {
		t.Fatalf("kick: code %d stderr %q", code, client.Stderr)
	}
	if kicks := server.rt.Kicks(); len(kicks) != 1 || kicks[0] != "go" {
		t.Fatalf("kicks = %v", kicks)
	}

	// Errors and their exit codes cross the socket too.
	client = &Env{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if code := client.Run(context.Background(), []string{"--workdir", workDir, "stop", "ghost"}); code != ExitError || !strings.Contains(client.Stderr.(*bytes.Buffer).String(), "not found") {
		t.Fatalf("stop ghost: code %d stderr %q", code, client.Stderr)
	}

	// A second foreground run in the same work dir keeps running but does
	// not take the socket over.
	second := newHarness(t)
	def := writeFile(t, filepath.Join(t.TempDir(), "other.yaml"), overlayDefinition)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- second.env.Run(ctx, []string{"--workdir", workDir, "run", def}) }()
	<-second.rt.started
	// Streaming begins only after the socket probe, so a consumed tick means
	// the probe has run.
	second.ticks <- time.Time{}
	cancel()
	if code := <-done; code != ExitOK || !strings.Contains(second.stderr.String(), "already serving") {
		t.Fatalf("second run: code %d stderr %q", code, second.stderr)
	}

	if code := finish(); code != ExitOK {
		t.Fatalf("run exit %d stderr %q", code, server.stderr)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("socket not removed on exit: %v", err)
	}
}

func TestControlSocketRejectsBadRequests(t *testing.T) {
	workDir := shortSocketDir(t)
	server := newHarness(t)
	socket, finish := startServingRun(t, server, workDir)
	defer finish()

	send := func(raw string) controlResponse {
		t.Helper()
		conn, err := net.Dial("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		if _, err := conn.Write([]byte(raw)); err != nil {
			t.Fatal(err)
		}
		var resp controlResponse
		if err := json.NewDecoder(conn).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := send("not json\n"); resp.Code != ExitUsage || !strings.Contains(resp.Stderr, "bad control request") {
		t.Fatalf("garbage: %+v", resp)
	}
	if resp := send(`{"args":[]}`); resp.Code != ExitUsage || !strings.Contains(resp.Stderr, "empty") {
		t.Fatalf("empty: %+v", resp)
	}
	if resp := send(`{"args":["run","x.yaml"]}`); resp.Code != ExitUsage || !strings.Contains(resp.Stderr, "not available") {
		t.Fatalf("run over socket: %+v", resp)
	}
}

func TestForwardFallsBackWhenSocketIsStale(t *testing.T) {
	workDir := shortSocketDir(t)
	socket := filepath.Join(workDir, controlSocketName)
	// A socket file nobody listens on: a run that died without cleanup.
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t)
	h.rt.procs["a"] = &crux.Process{Name: "a", ID: "a"}
	if code := h.env.Run(context.Background(), []string{"--workdir", workDir, "ps"}); code != ExitOK {
		t.Fatalf("code %d stderr %q", code, h.stderr)
	}
	if !strings.Contains(h.stderr.String(), "not answering") || !strings.Contains(h.stdout.String(), "a") {
		t.Fatalf("stdout %q stderr %q", h.stdout, h.stderr)
	}
}

func TestForwardReportsTransportErrors(t *testing.T) {
	workDir := shortSocketDir(t)
	socket := filepath.Join(workDir, controlSocketName)
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	// A dialer that hands back a connection the far end closed immediately:
	// the request write may succeed but no response ever comes.
	h.env.dial = func(context.Context, string) (net.Conn, error) {
		c1, c2 := net.Pipe()
		_ = c2.Close()
		return c1, nil
	}
	if code := h.env.Run(context.Background(), []string{"--workdir", workDir, "ps"}); code != ExitError || !strings.Contains(h.stderr.String(), "control socket") {
		t.Fatalf("code %d stderr %q", code, h.stderr)
	}

	// Response that is not JSON.
	h = newHarness(t)
	h.env.dial = func(context.Context, string) (net.Conn, error) {
		c1, c2 := net.Pipe()
		go func() {
			var req controlRequest
			_ = json.NewDecoder(c2).Decode(&req)
			_, _ = c2.Write([]byte("garbage\n"))
			_ = c2.Close()
		}()
		return c1, nil
	}
	if code := h.env.Run(context.Background(), []string{"--workdir", workDir, "ps"}); code != ExitError || !strings.Contains(h.stderr.String(), "reading response") {
		t.Fatalf("code %d stderr %q", code, h.stderr)
	}
}

func TestServeControlListenError(t *testing.T) {
	h := newHarness(t)
	h.env.defaults()
	h.env.dial = func(context.Context, string) (net.Conn, error) { return nil, errors.New("nobody home") }
	// A socket inside a directory that does not exist cannot be listened on.
	path := filepath.Join(shortSocketDir(t), "missing", controlSocketName)
	if _, err := h.env.serveControl(context.Background(), h.rt, path); err == nil {
		t.Fatal("listen under a missing directory: want error")
	}
}

func TestRunContinuesWhenSocketCannotBeServed(t *testing.T) {
	workDir := shortSocketDir(t)
	h := newHarness(t)
	def := writeFile(t, filepath.Join(t.TempDir(), "fixer.yaml"), overlayDefinition)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	// Point the socket under a missing directory so Listen fails.
	go func() {
		done <- h.env.Run(ctx, []string{"--workdir", workDir, "--socket", filepath.Join(workDir, "missing", controlSocketName), "run", def})
	}()
	<-h.rt.started
	h.ticks <- time.Time{}
	cancel()
	if code := <-done; code != ExitOK || !strings.Contains(h.stderr.String(), "continuing without it") {
		t.Fatalf("code %d stderr %q", code, h.stderr)
	}
}

func TestLocalDispatchBuildsRuntimeOncePerCommand(t *testing.T) {
	h := newHarness(t)
	calls := 0
	h.env.newRuntime = func(defs map[string]config.AgentConfig, project agent.ProjectContext) crux.Runtime {
		calls++
		return h.rt
	}
	if code := h.run("--local", "ps"); code != ExitOK || calls != 1 {
		t.Fatalf("code %d calls %d", code, calls)
	}
}
