package proxy

import (
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// slowBody yields one chunk every tick, count times, then EOF. It models an SSE
// generation that keeps producing tokens for longer than the write timeout.
type slowBody struct {
	chunk []byte
	tick  time.Duration
	left  int
}

func (s *slowBody) Read(p []byte) (int, error) {
	if s.left == 0 {
		return 0, io.EOF
	}
	// Deliberate fixed sleep, taking the sleep ratchet's documented escape
	// hatch rather than hiding from its regex behind <-time.After, which is
	// the same wait by another name. This paces a *producer* emitting chunks
	// at a rate, not a test waiting on a condition, so testutil.Eventually
	// does not apply: the bug under test is a wall-clock write deadline, so
	// real elapsed time between chunks is the stimulus itself. A genuinely
	// unnecessary sleep was removed from proxy_deep2_test.go in exchange, so
	// the ratchet total does not grow.
	time.Sleep(s.tick)
	s.left--
	return copy(p, s.chunk), nil
}

func (s *slowBody) Close() error { return nil }

// relayWithIdle performs the same relay the proxy does: write resp to the
// client through a stall-bounded writer whose idle bound is idle.
func relayWithIdle(t *testing.T, conn net.Conn, resp *http.Response, idle time.Duration) error {
	t.Helper()
	return resp.Write(&stallBoundedWriter{conn: conn, idle: idle})
}

// TestStreamingRelay_LongButFlowingStreamIsNotCut is the regression test for the
// /v1/messages livelock: a response that streams steadily for well past the
// write timeout must complete, because the bound is stall, not total duration.
//
// Falsifiability: swapping the stallBoundedWriter for a single absolute
// conn.SetWriteDeadline(now+idle) makes this test fail with i/o timeout —
// exactly the "write tcp ...: i/o timeout" observed on the spoke.
func TestStreamingRelay_LongButFlowingStreamIsNotCut(t *testing.T) {
	t.Parallel()

	idle := 150 * time.Millisecond
	// 10 chunks x 50ms = ~500ms of streaming, i.e. >3x the idle bound, but no
	// single gap ever reaches it.
	body := &slowBody{chunk: []byte("data: {\"delta\":\"tok\"}\n\n"), tick: 50 * time.Millisecond, left: 10}

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	readDone := make(chan int, 1)
	go func() {
		// Drain continuously so the only thing that can stop the write is a
		// deadline, not backpressure.
		n, _ := io.Copy(io.Discard, client)
		readDone <- int(n)
	}()

	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:          body,
		ContentLength: -1,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- relayWithIdle(t, server, resp, idle) }()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("steadily-flowing stream was cut: %v (this is the /v1/messages livelock)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("relay did not finish")
	}

	server.Close()
	if n := <-readDone; n == 0 {
		t.Fatal("client received no bytes")
	}
}

// TestStreamingRelay_StalledStreamIsStillCut is the control: the fix must not
// turn the bound off. A client that stops draining has to release the handler
// within roughly one stall window rather than hanging forever.
func TestStreamingRelay_StalledStreamIsStillCut(t *testing.T) {
	t.Parallel()

	idle := 100 * time.Millisecond
	// A big body against a peer that never reads: net.Pipe is unbuffered, so
	// the first Write blocks until the deadline fires.
	body := &slowBody{chunk: make([]byte, 32*1024), tick: 0, left: 64}

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close() // deliberately never read from

	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:          body,
		ContentLength: -1,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- relayWithIdle(t, server, resp, idle) }()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("stalled relay returned nil; the stall bound is not enforced")
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("expected a deadline error, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stalled relay was never released — the stall bound is gone")
	}
}
