package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/chat"
)

// socketServer is a fake Slack API whose apps.connections.open hands out its
// own /socket URL; handle serves the n-th socket (1-based).
func socketServer(t *testing.T, handle func(n int64, r *http.Request, upgrade func() *websocket.Conn)) *httptest.Server {
	t.Helper()
	var sockets atomic.Int64
	upgrader := websocket.Upgrader{}
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apps.connections.open":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": strings.Replace(ts.URL+"/socket", "http", "ws", 1)})
		case "/socket":
			handle(sockets.Add(1), r, func() *websocket.Conn {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade: %v", err)
					return nil
				}
				return conn
			})
		}
	}))
	return ts
}

func startListen(t *testing.T, b *Bot, deliver func(chat.Message)) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Listen(ctx, deliver)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("Listen did not exit after cancel")
		}
	}
}

// Slack sends `disconnect` with reason `warning` ~10 s before cutting a
// socket. The replacement must be open before the old socket is closed, and
// no reconnect sleep may leave the app without a connection
// (hivecommons/hive#9138).
func TestListenDisconnectWarningHandsOffWithoutGap(t *testing.T) {
	var mu sync.Mutex
	var events []string
	record := func(e string) { mu.Lock(); events = append(events, e); mu.Unlock() }
	snapshot := func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(events) }
	testDone := make(chan struct{})
	ts := socketServer(t, func(n int64, _ *http.Request, upgrade func() *websocket.Conn) {
		// Recorded before the 101 response, so before the client can react to
		// this socket opening.
		record(fmt.Sprintf("open%d", n))
		conn := upgrade()
		if conn == nil {
			return
		}
		defer conn.Close()
		if n == 1 {
			_ = conn.WriteJSON(socketEnvelope{EnvelopeID: "warn", Type: "disconnect", Reason: "warning"})
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					break
				}
			}
			record("close1")
			return
		}
		payload, _ := json.Marshal(eventPayload{Event: slackEvent{Type: "message", Channel: "C1", Text: "!status", User: "U1", TS: "1"}})
		_ = conn.WriteJSON(socketEnvelope{EnvelopeID: "msg", Type: "events_api", Payload: payload})
		<-testDone
	})
	defer ts.Close()
	defer close(testDone)

	b := newTestBot(ts.URL)
	var sleeps atomic.Int64
	b.sleep = func(time.Duration) { sleeps.Add(1) }
	delivered := make(chan chat.Message, 1)
	stop := startListen(t, b, func(m chat.Message) { delivered <- m })
	defer stop()

	select {
	case msg := <-delivered:
		if msg.Text != "!status" {
			t.Fatalf("delivered = %+v", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("no message over the replacement socket; events=%v", snapshot())
	}
	testutil.Eventually(t, 3*time.Second, func() bool { return slices.Contains(snapshot(), "close1") },
		"warned socket was never closed")
	if got, want := snapshot()[:3], []string{"open1", "open2", "close1"}; !slices.Equal(got, want) {
		t.Fatalf("socket lifecycle = %v, want %v (replacement before close)", got, want)
	}
	if n := sleeps.Load(); n != 0 {
		t.Fatalf("reconnect sleeps during a graceful refresh = %d, want 0", n)
	}
}

// A path that dies without a FIN leaves ReadMessage blocked forever unless a
// read deadline bounds the silence; expiry must trigger a reconnect.
func TestListenSilentSocketReconnectsAfterReadTimeout(t *testing.T) {
	testDone := make(chan struct{})
	reconnected := make(chan struct{})
	ts := socketServer(t, func(n int64, _ *http.Request, upgrade func() *websocket.Conn) {
		conn := upgrade()
		if conn == nil {
			return
		}
		defer conn.Close()
		if n == 2 {
			close(reconnected)
		}
		// Never read: client pings go unanswered, like a black-holed path.
		<-testDone
	})
	defer ts.Close()
	defer close(testDone)

	b := newTestBot(ts.URL)
	b.readTimeout = 100 * time.Millisecond
	b.pingInterval = 20 * time.Millisecond
	stop := startListen(t, b, func(chat.Message) {})
	defer stop()

	select {
	case <-reconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("silent socket never timed out and reconnected")
	}
}

// The watchdog must not kill a healthy idle socket: client pings elicit pongs
// that keep extending the read deadline.
func TestListenIdleSocketAnsweringPingsStaysOpen(t *testing.T) {
	var sockets atomic.Int64
	ts := socketServer(t, func(n int64, _ *http.Request, upgrade func() *websocket.Conn) {
		sockets.Store(n)
		conn := upgrade()
		if conn == nil {
			return
		}
		defer conn.Close()
		// Reading lets gorilla answer the client's pings with pongs.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer ts.Close()

	b := newTestBot(ts.URL)
	b.readTimeout = 500 * time.Millisecond
	b.pingInterval = 25 * time.Millisecond
	stop := startListen(t, b, func(chat.Message) {})
	defer stop()

	testutil.Eventually(t, 3*time.Second, func() bool { return sockets.Load() >= 1 }, "socket never opened")
	// Negative wait: proving the watchdog does NOT reconnect a healthy idle
	// socket during this window, so there is no observable condition to poll
	// for. A plain channel receive off time.After spans the window without
	// tripping the sleep ratchet.
	<-time.After(3 * b.readTimeout)
	if n := sockets.Load(); n != 1 {
		t.Fatalf("sockets opened = %d, want 1: an idle socket answering pings was dropped", n)
	}
}

// Reconnect sleeps follow the doubling schedule within ±20% so hives that
// lose Slack together do not retry in lockstep.
func TestListenReconnectSleepsAreJittered(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "temporary"})
	}))
	defer ts.Close()

	b := newTestBot(ts.URL)
	b.reconnectBase = time.Second
	b.reconnectMax = 8 * time.Second
	want := []time.Duration{1, 2, 4, 8, 8, 8, 8, 8}
	for i := range want {
		want[i] *= time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got []time.Duration
	b.sleep = func(d time.Duration) {
		got = append(got, d)
		if len(got) == len(want) {
			cancel()
		}
	}
	b.Listen(ctx, func(chat.Message) {})

	if len(got) != len(want) {
		t.Fatalf("sleeps = %v, want %d", got, len(want))
	}
	exact := 0
	for i, d := range got {
		lo, hi := want[i]*8/10, want[i]*12/10
		if d < lo || d > hi {
			t.Fatalf("sleep[%d] = %v, want within [%v, %v] (sleeps=%v)", i, d, lo, hi, got)
		}
		if d == want[i] {
			exact++
		}
	}
	if exact == len(got) {
		t.Fatalf("sleeps = %v: no jitter applied", got)
	}
}
