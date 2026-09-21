package dashboard

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// contribute_ws_frame_limit_test.go covers the hub half of
// hivecommons/hive#7932. The relay half — trimming a frame to fit — lives in
// bin/contributor-relay.test.js.
//
// The hub reads contributor frames under a hard 64 KiB limit (wsMaxMessageSize,
// installed with conn.SetReadLimit). gorilla does not truncate an oversized
// message: it closes the connection with 1009 and the frame is lost. A relay
// that lost a task_complete that way reconnected, was handed the SAME task, and
// did work it had already shipped — four times over on the live Bluefin hub.
//
// Two things are asserted here. The limit is STATED on auth_ok, so a relay can
// trim to it instead of discovering it by being disconnected; and tripping it is
// LOGGED, because it used to be the one disconnect the hub recorded nothing
// about (ErrReadLimit is not a *CloseError, so IsUnexpectedCloseError never
// matched it) while the relay logged "code=1009 message too big" at the other
// end of the same socket.

// waitingLogSink is a log sink safe to read from the test goroutine while the
// hub's connection goroutine writes to it, and which SIGNALS the line the test
// is waiting for. The hub logs on its own goroutine, still unwinding when our
// read returns, so the alternative is polling — and a poll loop either sleeps
// (which this repository ratchets down on) or spins.
type waitingLogSink struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	want   string
	seen   chan struct{}
	closed bool
}

func newWaitingLogSink(want string) *waitingLogSink {
	return &waitingLogSink{want: want, seen: make(chan struct{})}
}

func (b *waitingLogSink) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buf.Write(p)
	if !b.closed && strings.Contains(b.buf.String(), b.want) {
		b.closed = true
		close(b.seen)
	}
	return n, err
}

func (b *waitingLogSink) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestWS_AuthOKAdvertisesMaxMessageBytes: the read limit is a number the relay
// has to know before it sends, and nothing else on the wire reveals it.
func TestWS_AuthOKAdvertisesMaxMessageBytes(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	token, _ := registerWSUser(t, s, "frame-limit-user")

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge
	if err := conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: token, CLIBackend: "claude"}); err != nil {
		t.Fatalf("write auth_response: %v", err)
	}
	authOK := readMsg(t, conn)
	if authOK.Type != "auth_ok" {
		t.Fatalf("expected auth_ok, got %s: %s", authOK.Type, authOK.Reason)
	}
	if authOK.MaxMessageBytes != wsMaxMessageSize {
		t.Fatalf("auth_ok max_message_bytes: want %d (the limit SetReadLimit actually installs), got %d",
			wsMaxMessageSize, authOK.MaxMessageBytes)
	}
}

// TestWS_OversizedFrameIsClosedAndLogged: an oversized task_complete still ends
// the connection — the bound is real — but the hub now says so.
func TestWS_OversizedFrameIsClosedAndLogged(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	logs := newWaitingLogSink("exceeded the read limit")
	s.contributeHub.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	token, _ := registerWSUser(t, s, "oversize-user")

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge
	if err := conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: token, CLIBackend: "claude"}); err != nil {
		t.Fatalf("write auth_response: %v", err)
	}
	if authOK := readMsg(t, conn); authOK.Type != "auth_ok" {
		t.Fatalf("expected auth_ok, got %s: %s", authOK.Type, authOK.Reason)
	}

	// The shape of the frame that started this: a completion carrying an
	// unbounded captured-output tail. Sized just past the limit rather than
	// wildly past it, so the write lands in the socket buffer before the hub
	// reacts — a multi-megabyte frame is reset mid-write and the test would be
	// racing the teardown rather than asserting on it.
	oversized := WSMessage{
		Type:       "task_complete",
		TaskID:     "ct-oversize-1",
		Result:     "completed",
		TmuxOutput: []string{strings.Repeat("x", wsMaxMessageSize)},
	}
	// A write error is an outcome here, not a test failure: the hub closes as
	// soon as it has read past the limit, which can be before the last byte is
	// on the wire. Either way the frame never becomes a task_complete.
	writeErr := conn.WriteJSON(oversized)

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, readErr := conn.ReadMessage()
	if readErr == nil {
		t.Fatal("an oversized frame must end the connection, not be silently accepted")
	}
	if writeErr == nil && !websocket.IsCloseError(readErr, websocket.CloseMessageTooBig) {
		t.Fatalf("want a 1009 CloseMessageTooBig, got %v", readErr)
	}

	// The log is written on the hub's connection goroutine, which is still
	// unwinding when our read returns.
	select {
	case <-logs.seen:
	case <-time.After(5 * time.Second):
		t.Fatalf("an oversized frame disconnected a contributor with nothing in the hub log: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "limit_bytes=65536") {
		t.Fatalf("the log must name the bound that was exceeded, got: %s", logs.String())
	}
}
