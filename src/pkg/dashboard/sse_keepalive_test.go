package dashboard

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A long-open dashboard tab froze on an hours-old snapshot (6/12 open items
// on a repo with 17/9) while a fresh tab beside it was right: its
// EventSource had died silently and nothing — no server keepalive, no
// client watchdog, no status poll — ever noticed. These tests pin both
// halves of the fix: the server emits keepalive events on an idle stream
// and drops a client whose socket is dead; the page watches the cadence.

func withSSEKeepalive(t *testing.T, d time.Duration) {
	t.Helper()
	prev := sseKeepaliveInterval
	sseKeepaliveInterval = d
	t.Cleanup(func() { sseKeepaliveInterval = prev })
}

func TestHandleSSEEmitsKeepaliveOnIdleStream(t *testing.T) {
	withSSEKeepalive(t, 10*time.Millisecond)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	s.UpdateStatus(&StatusPayload{Agents: []FrontendAgent{{Name: "test"}}})

	// The request context's deadline ends the stream after several keepalive
	// ticks; the handler returns on ctx.Done() so no sleep is needed.
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		s.mux.ServeHTTP(rec, req)
		close(done)
	}()
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, "retry: ") || !strings.Contains(body, "data: {") {
		t.Fatalf("expected retry hint and initial status frame, got %q", body)
	}
	if n := strings.Count(body, "event: keepalive\ndata: "); n < 2 {
		t.Fatalf("expected >=2 keepalive events on an idle stream, got %d in %q", n, body)
	}
}

// deadSSEWriter is a flushing ResponseWriter whose socket "dies" after the
// first write — every later write fails like a kernel reporting a reset peer.
type deadSSEWriter struct {
	mu     sync.Mutex
	hdr    http.Header
	writes int
}

func (w *deadSSEWriter) Header() http.Header { return w.hdr }
func (w *deadSSEWriter) WriteHeader(int)     {}
func (w *deadSSEWriter) Flush()              {}
func (w *deadSSEWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	if w.writes > 1 {
		return 0, errors.New("write: broken pipe")
	}
	return len(p), nil
}

func TestHandleSSEDropsClientWhenKeepaliveWriteFails(t *testing.T) {
	withSSEKeepalive(t, 10*time.Millisecond)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	// No status yet: the only writes are the retry hint and keepalives.

	w := &deadSSEWriter{hdr: http.Header{}}
	// The request context is never cancelled — only the failed write may end
	// the handler. Bound the test so a regression fails instead of hanging.
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)

	done := make(chan struct{})
	go func() {
		s.handleSSE(w, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleSSE kept serving a dead socket; a failed keepalive write must end the stream")
	}

	s.sseMu.Lock()
	n := len(s.sseClients)
	s.sseMu.Unlock()
	if n != 0 {
		t.Fatalf("dead client still registered: %d sseClients (slot leak against maxSSEClients)", n)
	}
}

// TestDashboardSSEIdleWatchdogWired pins the browser half: the page listens
// for the keepalive event, counts every frame kind as liveness, runs the
// idle watchdog on a timer, forces a status refetch when the tab becomes
// visible again, and expects keepalives at the cadence the server sends.
func TestDashboardSSEIdleWatchdogWired(t *testing.T) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	for _, want := range []string{
		"source.addEventListener('keepalive', noteSSEFrame);",
		"source.onopen = noteSSEFrame;",
		"dashboardSetInterval('sse-watchdog', function() { sseWatchdogTick(false); }, SSE_WATCHDOG_TICK_MS);",
		"if (document.visibilityState === 'visible') sseWatchdogTick(true);",
		"const SSE_IDLE_RECONNECT_MS = STATUS_STALE_AFTER_MS;",
		"function sseStreamIdle(nowMs, lastFrameAtMs, openedAtMs, idleMs)",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing SSE watchdog wiring %q", want)
		}
	}
	// Every listener on the stream must reset the idle clock, else a hive
	// that only publishes agent-status/stage-tracker frames would reconnect
	// needlessly.
	for _, ev := range []string{"stage-tracker", "agent-status"} {
		i := strings.Index(html, "source.addEventListener('"+ev+"'")
		if i < 0 {
			t.Fatalf("missing %s listener", ev)
		}
		if !strings.Contains(html[i:i+200], "noteSSEFrame();") {
			t.Errorf("%s listener does not call noteSSEFrame()", ev)
		}
	}
	want := "const SSE_KEEPALIVE_EXPECTED_MS = " + strconv.Itoa(int(sseKeepaliveInterval/time.Millisecond)) + ";"
	if !strings.Contains(html, want) {
		t.Errorf("index.html SSE_KEEPALIVE_EXPECTED_MS does not match server sseKeepaliveInterval %v (want %q)", sseKeepaliveInterval, want)
	}
	// The watchdog threshold must leave room for several keepalives so a
	// single delayed one never triggers a reconnect storm.
	if sseKeepaliveInterval*3 > 90*time.Second {
		t.Errorf("sseKeepaliveInterval %v too long for the 90s client idle window", sseKeepaliveInterval)
	}
}
