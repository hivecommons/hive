package main

// Tests for superviseLocalLiteLLM's supervision loop (30% covered: only the
// pre-cancelled-context guard was pinned, in boot_observability_test.go).
// These exercise the loop's real work through a PATH-stubbed `litellm`
// binary — the same technique pkg/agent tests use for CLI stubs — covering:
//
//   - the exec itself: the supervisor must invoke `litellm` with the loopback
//     host, the reserved local-proxy port, and the operator config path, or
//     the Go translator in front of it forwards to a proxy that never bound;
//   - the error-exit branch ("local litellm proxy exited") and the return
//     via the post-run select when the context is cancelled during backoff;
//   - the clean-exit branch ("exited cleanly; restarting") and the
//     time.After restart arm — the loop must actually respawn the proxy,
//     since a fallback that dies once and stays dead defeats supervision.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a goroutine-safe log sink: the supervisor goroutine writes
// while the test goroutine reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// stubLitellm places a fake `litellm` executable on PATH that appends its
// arguments as one line to argsFile and exits with the given code.
func stubLitellm(t *testing.T, exitCode int) (argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "invocations")
	script := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\nexit %d\n", argsFile, exitCode)
	if err := os.WriteFile(filepath.Join(dir, "litellm"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing litellm stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
}

// invocationCount returns how many times the stub has run so far.
func invocationCount(argsFile string) int {
	data, err := os.ReadFile(argsFile)
	if err != nil {
		return 0
	}
	return len(strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"))
}

// waitForInvocations polls until the stub has run at least n times or the
// deadline passes.
func waitForInvocations(t *testing.T, argsFile string, n int, deadline time.Duration) {
	t.Helper()
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		if invocationCount(argsFile) >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("litellm stub not invoked %d time(s) within %v (got %d)", n, deadline, invocationCount(argsFile))
}

// runSupervisor starts superviseLocalLiteLLM against a captured logger and
// returns the log sink, the cancel func, and the done channel.
func runSupervisor(t *testing.T) (*syncBuffer, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		superviseLocalLiteLLM(ctx, logger)
		close(done)
	}()
	return logs, cancel, done
}

// awaitReturn fails the test if the supervisor does not return after cancel.
func awaitReturn(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("superviseLocalLiteLLM did not return after context cancellation")
	}
}

// TestSuperviseLocalLiteLLMSpawnsWithLoopbackArgsAndLogsErrorExit pins the
// exec contract (host/port/config flags) and the error-exit warn branch, then
// the shutdown path: cancelling during the restart backoff must end the loop.
func TestSuperviseLocalLiteLLMSpawnsWithLoopbackArgsAndLogsErrorExit(t *testing.T) {
	argsFile := stubLitellm(t, 7)
	logs, cancel, done := runSupervisor(t)
	defer cancel()

	waitForInvocations(t, argsFile, 1, 5*time.Second)
	cancel()
	awaitReturn(t, done)

	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading stub invocation record: %v", err)
	}
	line := strings.SplitN(strings.TrimSpace(string(args)), "\n", 2)[0]
	for _, want := range []string{
		"--host 127.0.0.1",
		"--port " + strconv.Itoa(litellmLocalProxyPort),
		"--config " + litellmLocalConfigPath,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("litellm invoked without %q: %q", want, line)
		}
	}
	if !strings.Contains(logs.String(), "local litellm proxy exited") {
		t.Errorf("error exit (code 7) not logged; logs:\n%s", logs.String())
	}
}

// TestSuperviseLocalLiteLLMRestartsAfterCleanExit pins the supervision
// promise itself: a proxy that exits cleanly is logged as such and respawned
// after the backoff. Two recorded invocations prove the time.After arm of the
// restart select ran; the deadline allows for the 5s litellmRestartDelay.
func TestSuperviseLocalLiteLLMRestartsAfterCleanExit(t *testing.T) {
	argsFile := stubLitellm(t, 0)
	logs, cancel, done := runSupervisor(t)
	defer cancel()

	waitForInvocations(t, argsFile, 2, litellmRestartDelay+10*time.Second)
	cancel()
	awaitReturn(t, done)

	if !strings.Contains(logs.String(), "exited cleanly; restarting") {
		t.Errorf("clean exit not logged as restarting; logs:\n%s", logs.String())
	}
}
