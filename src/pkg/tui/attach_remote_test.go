package tui

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hivecommons/hive/pkg/tui/client"
)

// remoteWait bounds every wait in this file. Generous for the same reason the
// client package's streamWait is: these assert sequencing, not latency.
const remoteWait = 2 * time.Second

func TestDashboardIsLoopback(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"http://localhost:3001", true},
		{"http://127.0.0.1:3001", true},
		{"http://[::1]:3001", true},
		{"http://LOCALHOST:3001", true},
		{"https://hive.example.com", false},
		{"https://myhive.hive.kubestellar.io", false},
		{"http://192.168.4.56:3001", false},
		// Unparseable/hostless URLs keep the historical local-only behavior:
		// the local error is clearer than a dial error against garbage.
		{"://not-a-url", true},
		{"", true},
	}
	for _, tc := range cases {
		if got := dashboardIsLoopback(tc.url); got != tc.want {
			t.Errorf("dashboardIsLoopback(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

// startAttachFixture serves a minimal ttyd-shaped websocket at /terminal/ws
// and records how often it was dialled. The script function runs the server
// side of one attached session.
func startAttachFixture(t *testing.T, script func(conn *websocket.Conn)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var dials atomic.Int32
	upgrader := websocket.Upgrader{Subprotocols: []string{"tty"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != client.TerminalWSPath {
			http.NotFound(w, r)
			return
		}
		dials.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if script != nil {
			script(conn)
		}
	}))
	t.Cleanup(server.Close)
	return server, &dials
}

// TestPrepareAttachFallsBackToProxyWhenLocalSessionMissing is the Podman
// topology (#5644): dashboard on loopback, agents' tmux sessions inside the
// container where the operator's own tmux cannot see them. The missing local
// session must route to the dashboard's terminal proxy instead of dead-ending.
func TestPrepareAttachFallsBackToProxyWhenLocalSessionMissing(t *testing.T) {
	installFakeTmux(t, `
if [ "$1" = "has-session" ]; then
  echo "can't find session: hive-scanner" >&2
  exit 1
fi
exit 99`)
	server, dials := startAttachFixture(t, func(conn *websocket.Conn) {
		// Hold the session open until the test closes the client side.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	t.Setenv(client.BaseURLEnv, server.URL) // 127.0.0.1 — loopback
	t.Setenv(client.TokenEnv, "tok")

	msg, ok := prepareAttach("scanner", client.New())().(attachReadyMsg)
	if !ok {
		t.Fatalf("prepareAttach returned %T, want attachReadyMsg", msg)
	}
	if msg.err != nil {
		t.Fatalf("prepareAttach error = %v, want a remote session", msg.err)
	}
	if msg.cmd != nil {
		t.Fatal("missing local session still produced a local attach command")
	}
	if msg.remote == nil {
		t.Fatal("missing local session did not fall back to the dashboard proxy")
	}
	if !msg.remote.viaFallback {
		t.Error("fallback attach is not marked viaFallback — the banner would not say so")
	}
	if got := dials.Load(); got != 1 {
		t.Errorf("proxy dialled %d times, want 1", got)
	}
	_ = msg.remote.session.Close()
}

// TestPrepareAttachKeepsLocalFastPath: a loopback dashboard with a genuinely
// local session must exec tmux directly and never touch the network — that is
// the co-located fast path the issue preserves.
func TestPrepareAttachKeepsLocalFastPath(t *testing.T) {
	installFakeTmux(t, "exit 0")
	server, dials := startAttachFixture(t, nil)
	t.Setenv(client.BaseURLEnv, server.URL)
	t.Setenv(client.TokenEnv, "tok")

	msg, ok := prepareAttach("scanner", client.New())().(attachReadyMsg)
	if !ok {
		t.Fatalf("prepareAttach returned %T, want attachReadyMsg", msg)
	}
	if msg.err != nil {
		t.Fatalf("prepareAttach error = %v, want a local command", msg.err)
	}
	if msg.cmd == nil || msg.remote != nil {
		t.Fatalf("local session present: cmd=%v remote=%v, want local exec only", msg.cmd, msg.remote)
	}
	if got := dials.Load(); got != 0 {
		t.Errorf("proxy dialled %d times for a local attach, want 0", got)
	}
}

// TestRemoteAttachBridgeRoundTrip runs the whole bridge against the fixture:
// init carries the size, a keystroke travels out as an INPUT frame, remote
// output lands on the local stdout, a normal close reads as a detach (nil),
// and the banner names the session and hive WITHOUT echoing the token.
func TestRemoteAttachBridgeRoundTrip(t *testing.T) {
	const token = "secret-token-do-not-echo"
	gotInput := make(chan []byte, 1)
	server, _ := startAttachFixture(t, func(conn *websocket.Conn) {
		// init JSON first
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		// then the operator's keystrokes
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		gotInput <- msg
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte("0hello from tmux"))
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		// Let the close frame flush before Close tears down the TCP stream.
		time.Sleep(10 * time.Millisecond)
	})
	t.Setenv(client.BaseURLEnv, server.URL)
	t.Setenv(client.TokenEnv, token)

	api := client.New()
	msg := prepareRemoteAttach("scanner", api, nil).(attachReadyMsg)
	if msg.err != nil {
		t.Fatalf("prepareRemoteAttach: %v", msg.err)
	}

	var stdout bytes.Buffer
	bridge := msg.remote
	bridge.SetStdin(strings.NewReader("ls\r"))
	bridge.SetStdout(&stdout)
	bridge.SetStderr(io.Discard)

	runErr := make(chan error, 1)
	go func() { runErr <- bridge.Run() }()

	select {
	case in := <-gotInput:
		if string(in) != "0ls\r" {
			t.Errorf("input frame = %q, want %q", in, "0ls\r")
		}
	case <-time.After(remoteWait):
		t.Fatal("no input frame reached the fixture")
	}
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() = %v, want nil for a normal close (detach)", err)
		}
	case <-time.After(remoteWait):
		t.Fatal("Run did not return after the server closed")
	}

	out := stdout.String()
	if !strings.Contains(out, "hello from tmux") {
		t.Errorf("stdout %q missing remote output", out)
	}
	if !strings.Contains(out, "hive-scanner") {
		t.Errorf("banner in %q does not name the session", out)
	}
	if !strings.Contains(out, server.URL) {
		t.Errorf("banner in %q does not name the hive it attached through", out)
	}
	if strings.Contains(out, token) {
		t.Error("the dashboard token was echoed to the terminal")
	}
}

// TestRemoteAttachRunReportsStartFailure: a session whose transport dies
// between the dial and the bridge starting (here: closed under it) must
// surface as an error naming the start, not hang or pretend to attach.
func TestRemoteAttachRunReportsStartFailure(t *testing.T) {
	server, _ := startAttachFixture(t, func(conn *websocket.Conn) {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	t.Setenv(client.BaseURLEnv, server.URL)
	t.Setenv(client.TokenEnv, "tok")

	msg := prepareRemoteAttach("scanner", client.New(), nil).(attachReadyMsg)
	if msg.err != nil {
		t.Fatalf("prepareRemoteAttach: %v", msg.err)
	}
	_ = msg.remote.session.Close()

	bridge := msg.remote
	bridge.SetStdin(strings.NewReader(""))
	var stdout bytes.Buffer
	bridge.SetStdout(&stdout)
	bridge.SetStderr(io.Discard)

	err := bridge.Run()
	if err == nil {
		t.Fatal("Run() = nil on a dead session, want a start error")
	}
	if !strings.Contains(err.Error(), "start remote terminal") {
		t.Errorf("Run error = %v, want it to name the failed start", err)
	}
}

// TestPumpBlockingStops pins the blocking pump's two exits: a closed done
// channel is honoured between reads, and a failed send ends the pump rather
// than looping on a dead websocket.
func TestPumpBlockingStops(t *testing.T) {
	t.Run("done closed", func(t *testing.T) {
		done := make(chan struct{})
		close(done)
		finished := make(chan struct{})
		go func() {
			// The reader has bytes ready; the pump must still notice done
			// first and forward nothing.
			pumpBlocking(done, strings.NewReader("unsent"), func(p []byte) error {
				t.Errorf("pump forwarded %q after done closed", p)
				return nil
			})
			close(finished)
		}()
		select {
		case <-finished:
		case <-time.After(remoteWait):
			t.Fatal("pumpBlocking did not stop on a closed done channel")
		}
	})

	t.Run("send failure", func(t *testing.T) {
		done := make(chan struct{})
		defer close(done)
		calls := 0
		pumpBlocking(done, strings.NewReader("xy"), func([]byte) error {
			calls++
			return io.ErrClosedPipe
		})
		if calls != 1 {
			t.Errorf("send called %d times after failing, want 1", calls)
		}
	})
}

// TestIsDetachClassification pins which endings are quiet. 1000/1001/1005 and
// EOF are how detaches and session exits arrive; an abnormal 1006 is a broken
// transport mid-session, which the operator must hear about.
func TestIsDetachClassification(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, true},
		{io.EOF, true},
		{&websocket.CloseError{Code: websocket.CloseNormalClosure}, true},
		{&websocket.CloseError{Code: websocket.CloseGoingAway}, true},
		{&websocket.CloseError{Code: websocket.CloseNoStatusReceived}, true},
		{&websocket.CloseError{Code: websocket.CloseAbnormalClosure}, false},
		{errors.New("read tcp: connection reset"), false},
	}
	for _, tc := range cases {
		if got := isDetach(tc.err); got != tc.want {
			t.Errorf("isDetach(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// TestAttachFailureStatusClassification is the app half of the auth-gate
// invariant: an unauthenticated or under-privileged attach is REFUSED with a
// message that says which of the two it was, in the wording convention the
// other actions use — never suspended into a dead terminal.
func TestAttachFailureStatusClassification(t *testing.T) {
	forbidden := &client.APIError{StatusCode: http.StatusForbidden, Method: "GET", Path: client.TerminalWSPath}
	if got := attachFailureStatus(forbidden); got != "Attach failed: owner access required" {
		t.Errorf("403 footer = %q", got)
	}

	unauthorized := &client.APIError{StatusCode: http.StatusUnauthorized, Method: "GET", Path: client.TerminalWSPath}
	got := attachFailureStatus(unauthorized)
	for _, want := range []string{"Attach failed: authentication required", client.TokenEnv, client.CookieEnv} {
		if !strings.Contains(got, want) {
			t.Errorf("401 footer %q missing %q", got, want)
		}
	}

	plain := errors.New("dial tcp: connection refused")
	if got := attachFailureStatus(plain); got != "Attach failed: dial tcp: connection refused" {
		t.Errorf("plain footer = %q", got)
	}
}

// TestRemoteAttachReadySuspendsViaExec: a prepared remote session must hand
// the TUI to Bubble Tea's exec machinery exactly as a local command does —
// pending stays set until attachDoneMsg restores it.
func TestRemoteAttachReadySuspendsViaExec(t *testing.T) {
	m := modelWithAgent(false)
	m.attachPending = true
	next, cmd := m.Update(attachReadyMsg{remote: &remoteAttach{agent: "scanner"}})
	if cmd == nil {
		t.Fatal("remote attachReadyMsg produced no exec command")
	}
	if !next.(model).attachPending {
		t.Fatal("attach stopped being pending before the bridge completed")
	}
}
