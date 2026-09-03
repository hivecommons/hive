//go:build unix

package tui

import (
	"bytes"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hivecommons/hive/pkg/tui/client"
)

// syncBuffer lets the bridge's output goroutine and the test's assertions
// share a stdout without racing.
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

// TestRemoteAttachBridgeFileStdinAndAbnormalClose drives the bridge the way
// the real program does — stdin as an *os.File — which is the configuration
// the round-trip test's strings.Reader cannot reach: the poll(2) stdin pump
// and the SIGWINCH watcher only run for a file. It also ends the session the
// bad way, an abrupt TCP teardown with no close frame, which must come back
// from Run as an error: a broken transport mid-session means the operator's
// last keystrokes may never have arrived, and a quiet return would hide that.
func TestRemoteAttachBridgeFileStdinAndAbnormalClose(t *testing.T) {
	gotInput := make(chan []byte, 1)
	closeNow := make(chan struct{})
	server, _ := startAttachFixture(t, func(conn *websocket.Conn) {
		if _, _, err := conn.ReadMessage(); err != nil { // init JSON
			return
		}
		_, msg, err := conn.ReadMessage() // the keystrokes
		if err != nil {
			return
		}
		gotInput <- msg
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte("0remote bytes"))
		<-closeNow
		// No close frame: the deferred conn.Close() in the fixture tears the
		// TCP stream down under the client — an abnormal closure.
	})
	t.Setenv(client.BaseURLEnv, server.URL)
	t.Setenv(client.TokenEnv, "tok")

	msg := prepareRemoteAttach("scanner", client.New(), &tmuxNotFoundError{}).(attachReadyMsg)
	if msg.err != nil {
		t.Fatalf("prepareRemoteAttach: %v", msg.err)
	}
	if !msg.remote.viaFallback {
		t.Fatal("a non-nil local error did not mark the attach viaFallback")
	}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer func() { _ = stdinR.Close() }()

	var stdout syncBuffer
	bridge := msg.remote
	bridge.SetStdin(stdinR)
	bridge.SetStdout(&stdout)
	bridge.SetStderr(io.Discard)

	runErr := make(chan error, 1)
	go func() { runErr <- bridge.Run() }()

	if _, err := stdinW.Write([]byte("ls\r")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	select {
	case in := <-gotInput:
		if string(in) != "0ls\r" {
			t.Errorf("input frame = %q, want %q", in, "0ls\r")
		}
	case <-time.After(remoteWait):
		t.Fatal("the poll-based pump forwarded nothing")
	}

	// A resize signal mid-session must not disturb the bridge. The pipe has
	// no window size, so no resize frame goes out — the point here is that
	// the watcher handles the signal and the session carries on.
	_ = syscall.Kill(os.Getpid(), syscall.SIGWINCH)
	time.Sleep(50 * time.Millisecond)

	// EOF on stdin ends the pump via its read error path; the session itself
	// stays up until the server tears it down.
	_ = stdinW.Close()
	close(closeNow)

	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("Run() = nil for an abnormal close, want an error — the operator must hear the transport broke")
		}
		if !strings.Contains(err.Error(), "remote attach ended") {
			t.Errorf("Run error = %v, want it to say the attach ended abnormally", err)
		}
	case <-time.After(remoteWait):
		t.Fatal("Run did not return after the abnormal close")
	}

	out := stdout.String()
	if !strings.Contains(out, "remote bytes") {
		t.Errorf("stdout %q missing the remote output", out)
	}
	if !strings.Contains(out, "no local tmux session") {
		t.Errorf("fallback banner missing from %q — the operator would not know why the attach went remote", out)
	}
}

// TestPumpFileStopsOnDone pins the property the poll loop exists for: with no
// input arriving at all, a closed done channel ends the pump within its poll
// interval — no keystroke is needed to unblock it, so none can be swallowed.
func TestPumpFileStopsOnDone(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()

	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		pumpFile(done, r, func([]byte) error { return nil })
		close(finished)
	}()

	close(done)
	select {
	case <-finished:
	case <-time.After(remoteWait):
		t.Fatal("pumpFile did not stop after done closed, despite no input")
	}
}

// TestPumpFileStopsOnSendFailure: once the websocket is gone, the first
// forwarded read ends the pump rather than looping on a dead sender.
func TestPumpFileStopsOnSendFailure(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()

	done := make(chan struct{})
	defer close(done)
	finished := make(chan struct{})
	go func() {
		pumpFile(done, r, func([]byte) error { return io.ErrClosedPipe })
		close(finished)
	}()

	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case <-finished:
	case <-time.After(remoteWait):
		t.Fatal("pumpFile kept running after its sender failed")
	}
}
