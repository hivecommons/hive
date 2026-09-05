package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// These tests run against an httptest server speaking ttyd's handshake and
// framing — no real ttyd, container, tmux or network, per the epic's testing
// convention. The fixture stands in for the WHOLE server chain the real dial
// crosses (dashboard auth gate → reverse proxy → ttyd), because from this
// client's side that chain is one websocket endpoint: /terminal/ws.

// terminalFixture records what the dial presented and speaks just enough of
// ttyd's protocol to exercise the session methods.
type terminalFixture struct {
	server *httptest.Server

	// captured from the upgrade request
	path         string
	query        string
	authz        string
	cookie       string
	subproto     string
	arg          string
	handoffAuthz string
	initMsg      chan []byte // the first (JSON) client message
	clientMsg    chan []byte // every subsequent client message, command byte included

	// serverSend queues frames for the handler to write to the client.
	serverSend chan []byte
	// refuse, when non-zero, answers the upgrade with this HTTP status
	// instead of a websocket — the dashboard gate saying no.
	refuse int
}

func newTerminalFixture(t *testing.T, refuse int) *terminalFixture {
	t.Helper()
	f := &terminalFixture{
		initMsg:    make(chan []byte, 1),
		clientMsg:  make(chan []byte, 16),
		serverSend: make(chan []byte, 16),
		refuse:     refuse,
	}
	upgrader := websocket.Upgrader{Subprotocols: []string{"tty"}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/terminal/handoff" {
			f.handoffAuthz = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":"handoff-123"}`))
			return
		}
		f.path = r.URL.Path
		f.query = r.URL.RawQuery
		f.authz = r.Header.Get("Authorization")
		f.cookie = r.Header.Get("Cookie")
		f.subproto = r.Header.Get("Sec-WebSocket-Protocol")
		f.arg = r.URL.Query().Get("arg")
		if f.refuse != 0 {
			http.Error(w, http.StatusText(f.refuse), f.refuse)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		// First message is ttyd's init JSON; everything after is command-
		// prefixed. Reader loop feeds both channels and a writer drains
		// serverSend until it closes.
		go func() {
			first := true
			for {
				_, msg, err := conn.ReadMessage()
				if err != nil {
					return
				}
				if first {
					first = false
					f.initMsg <- msg
					continue
				}
				f.clientMsg <- msg
			}
		}()
		for frame := range f.serverSend {
			if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
				return
			}
		}
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		// Give the close frame a moment to flush before the deferred Close
		// tears the TCP connection down under it.
		time.Sleep(10 * time.Millisecond)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *terminalFixture) client(token, cookie, ttydCred string) *Client {
	c := New()
	c.baseURL = f.server.URL
	c.token = token
	c.cookie = cookie
	c.ttydCred = ttydCred
	return c
}

// recv reads one queued client frame or fails the test.
func (f *terminalFixture) recv(t *testing.T) []byte {
	t.Helper()
	select {
	case msg := <-f.clientMsg:
		return msg
	case <-time.After(streamWait):
		t.Fatal("no client frame arrived")
		return nil
	}
}

// TestDialTerminalPresentsEveryCredentialLane pins the three-lane handshake:
// dashboard Authorization on the handoff POST, a short-lived handoff code in
// the websocket query, the session cookie verbatim, and ttyd's own basic-auth
// credential — derived exactly as the entrypoint derives it, hive:<token> — in
// the Authorization header. Getting any lane wrong locks out one deployment
// shape while the others keep working, which is precisely the class of bug a
// test must pin per lane.
func TestDialTerminalPresentsEveryCredentialLane(t *testing.T) {
	f := newTerminalFixture(t, 0)
	const token = "tok-123"
	const cookie = "hive_session=abc; hive_terminal_assertion=xyz"

	sess, err := f.client(token, cookie, "").DialTerminal(context.Background(), "hive-scanner")
	if err != nil {
		t.Fatalf("DialTerminal: %v", err)
	}
	defer func() { _ = sess.Close() }()

	if f.path != TerminalWSPath {
		t.Errorf("dial path = %q, want %q", f.path, TerminalWSPath)
	}
	if f.arg != "hive-scanner" {
		t.Errorf("?arg = %q, want hive-scanner", f.arg)
	}
	if strings.Contains(f.query, "token=") {
		t.Errorf("query %q leaked the dashboard token", f.query)
	}
	if !strings.Contains(f.query, "code=handoff-123") {
		t.Errorf("query %q does not carry the terminal handoff code", f.query)
	}
	if f.handoffAuthz != "Bearer "+token {
		t.Errorf("handoff Authorization = %q, want bearer dashboard token", f.handoffAuthz)
	}
	if f.cookie != cookie {
		t.Errorf("Cookie = %q, want %q (verbatim)", f.cookie, cookie)
	}
	wantBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte("hive:"+token))
	if f.authz != wantBasic {
		t.Errorf("Authorization = %q, want %q", f.authz, wantBasic)
	}
	if f.subproto != "tty" {
		t.Errorf("subprotocol = %q, want tty", f.subproto)
	}
}

// TestDialTerminalOmitsAbsentCredentials: an unconfigured client dials bare.
// Sending an empty token= or "Basic " would turn an open hive's accept into a
// reject — the same lesson authorize() already encodes for the Bearer header.
func TestDialTerminalOmitsAbsentCredentials(t *testing.T) {
	f := newTerminalFixture(t, 0)

	sess, err := f.client("", "", "").DialTerminal(context.Background(), "hive-scanner")
	if err != nil {
		t.Fatalf("DialTerminal: %v", err)
	}
	defer func() { _ = sess.Close() }()

	if strings.Contains(f.query, "token=") {
		t.Errorf("query %q carries a token= with no token configured", f.query)
	}
	if f.authz != "" {
		t.Errorf("Authorization = %q, want empty with no credential", f.authz)
	}
	if f.cookie != "" {
		t.Errorf("Cookie = %q, want empty with no session", f.cookie)
	}
	if err := sess.Start(80, 24); err != nil {
		t.Fatalf("Start: %v", err)
	}
	init := <-f.initMsg
	var got ttydInit
	if err := json.Unmarshal(init, &got); err != nil {
		t.Fatalf("init message %q is not JSON: %v", init, err)
	}
	if got.AuthToken != "" {
		t.Errorf("AuthToken = %q, want empty for an uncredentialed ttyd", got.AuthToken)
	}
}

// TestDialTerminalHonorsTtydCredentialOverride mirrors the entrypoint's
// precedence, pinned server-side by deploy/test_ttyd_url_arg.sh: an explicit
// HIVE_TTYD_CREDENTIAL wins over the derived hive:<token>.
func TestDialTerminalHonorsTtydCredentialOverride(t *testing.T) {
	f := newTerminalFixture(t, 0)

	sess, err := f.client("tok-123", "", "custom:secret").DialTerminal(context.Background(), "hive-scanner")
	if err != nil {
		t.Fatalf("DialTerminal: %v", err)
	}
	defer func() { _ = sess.Close() }()

	wantBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte("custom:secret"))
	if f.authz != wantBasic {
		t.Errorf("Authorization = %q, want %q (explicit credential wins)", f.authz, wantBasic)
	}
}

// TestDialTerminalRefusalsAreTyped is THE auth invariant for the attach path:
// a dashboard that says 401 or 403 to the upgrade must surface as a typed,
// classifiable refusal — never as a session the TUI would then suspend the
// terminal for. IsUnauthorized/IsForbidden are how the footer distinguishes
// "set a credential" from "your role cannot do this", so the classification
// itself is the contract under test.
func TestDialTerminalRefusalsAreTyped(t *testing.T) {
	cases := []struct {
		status   int
		classify func(error) bool
		name     string
	}{
		{http.StatusUnauthorized, IsUnauthorized, "unauthenticated"},
		{http.StatusForbidden, IsForbidden, "insufficient role"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTerminalFixture(t, tc.status)
			sess, err := f.client("tok", "", "").DialTerminal(context.Background(), "hive-scanner")
			if sess != nil {
				t.Fatal("a refused upgrade returned a session")
			}
			if err == nil {
				t.Fatal("a refused upgrade returned no error")
			}
			if !tc.classify(err) {
				t.Fatalf("error %v did not classify as HTTP %d", err, tc.status)
			}
		})
	}
}

// TestTerminalSessionFraming pins the ttyd wire protocol version this client
// speaks: init as a bare JSON object carrying AuthToken and size, input as
// '0'+bytes, resize as '1'+JSON — and on the way back, OUTPUT unwrapped while
// title and preference frames are consumed silently.
func TestTerminalSessionFraming(t *testing.T) {
	f := newTerminalFixture(t, 0)
	const token = "tok-123"

	sess, err := f.client(token, "", "").DialTerminal(context.Background(), "hive-scanner")
	if err != nil {
		t.Fatalf("DialTerminal: %v", err)
	}
	defer func() { _ = sess.Close() }()

	if err := sess.Start(120, 40); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var init ttydInit
	if err := json.Unmarshal(<-f.initMsg, &init); err != nil {
		t.Fatalf("init not JSON: %v", err)
	}
	wantAuth := base64.StdEncoding.EncodeToString([]byte("hive:" + token))
	if init.AuthToken != wantAuth {
		t.Errorf("AuthToken = %q, want %q", init.AuthToken, wantAuth)
	}
	if init.Columns != 120 || init.Rows != 40 {
		t.Errorf("init size = %dx%d, want 120x40", init.Columns, init.Rows)
	}

	if err := sess.SendInput([]byte("ls\r")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if got := f.recv(t); string(got) != "0ls\r" {
		t.Errorf("input frame = %q, want %q", got, "0ls\r")
	}

	if err := sess.Resize(80, 24); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	resize := f.recv(t)
	if resize[0] != '1' {
		t.Fatalf("resize frame starts with %q, want '1'", resize[0])
	}
	var size struct{ Columns, Rows int }
	if err := json.Unmarshal(resize[1:], &size); err != nil {
		t.Fatalf("resize payload not JSON: %v", err)
	}
	if size.Columns != 80 || size.Rows != 24 {
		t.Errorf("resize = %dx%d, want 80x24", size.Columns, size.Rows)
	}

	// Title and preferences are browser furniture; only OUTPUT reaches the
	// caller, unwrapped.
	f.serverSend <- []byte("1a title")
	f.serverSend <- []byte(`2{"fontSize":14}`)
	f.serverSend <- []byte("0hello from tmux")
	out, err := sess.ReadOutput()
	if err != nil {
		t.Fatalf("ReadOutput: %v", err)
	}
	if string(out) != "hello from tmux" {
		t.Errorf("output = %q, want %q", out, "hello from tmux")
	}

	// A normal server close is how every attach ends; it must come back as a
	// CloseError the bridge can recognize as a detach, not as output.
	close(f.serverSend)
	if _, err := sess.ReadOutput(); err == nil {
		t.Fatal("ReadOutput after close returned no error")
	}
}
