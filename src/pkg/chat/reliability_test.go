package chat

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSSELoopBackoffResetsAfterConnectedSession(t *testing.T) {
	// A large base keeps scheduler/CI jitter small relative to the gap between a
	// reset delay (base..1.5*base) and an unreset one (>= 4*base).
	const base = 200 * time.Millisecond
	var requests atomic.Int64
	times := make(chan time.Time, 4)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		times <- time.Now()
		n := requests.Add(1)
		if n == 3 {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	s := newTestBot(ts, "ch")
	s.sseReconnectBase = base
	s.sseReconnectMax = 10 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.sseLoop(ctx)

	var seen []time.Time
	for len(seen) < 4 {
		select {
		case tm := <-times:
			seen = append(seen, tm)
		case <-time.After(15 * time.Second):
			t.Fatalf("timed out waiting for SSE reconnects; got %d", len(seen))
		}
	}
	cancel()
	postSuccessDelay := seen[3].Sub(seen[2])
	if postSuccessDelay >= 4*base {
		t.Fatalf("post-success reconnect delay = %v, want reset near base", postSuccessDelay)
	}
}

func TestConsumeSSEContextCancelUnblocksRead(t *testing.T) {
	started := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer ts.Close()

	s := newTestBot(ts, "ch")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		connected, err := s.consumeSSE(ctx)
		if !connected {
			done <- errors.New("consumeSSE did not report connected")
			return
		}
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("consumeSSE returned nil error after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("consumeSSE did not unblock after context cancellation")
	}
}

func TestConsumeSSEIdleWatchdogUnblocksRead(t *testing.T) {
	started := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer ts.Close()

	s := newTestBot(ts, "ch")
	s.sseIdleTimeout = 25 * time.Millisecond
	startedAt := time.Now()
	connected, err := s.consumeSSE(context.Background())
	if !connected {
		t.Fatal("consumeSSE did not report connected")
	}
	if !errors.Is(err, errSSEIdle) {
		t.Fatalf("consumeSSE error = %v, want idle timeout", err)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("idle watchdog took %v to stop the read", elapsed)
	}
}

func TestConsumeSSEBoundsIncompleteFrame(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, sseMaxPendingBytes+1))
	}))
	defer ts.Close()

	s := newTestBot(ts, "ch")
	s.sseIdleTimeout = time.Second
	connected, err := s.consumeSSE(context.Background())
	if !connected {
		t.Fatal("consumeSSE did not report connected")
	}
	if err == nil || !strings.Contains(err.Error(), "without terminator") {
		t.Fatalf("consumeSSE error = %v, want bounded buffer error", err)
	}
}

func TestDashboardGetPostContextCancelUnblocks(t *testing.T) {
	tests := []struct {
		name string
		call func(context.Context, *Service) error
	}{
		{name: "get", call: func(ctx context.Context, s *Service) error { _, err := s.dashboardGet(ctx, "/api/status"); return err }},
		{name: "post", call: func(ctx context.Context, s *Service) error {
			return s.dashboardPost(ctx, "/api/kick/scanner", []byte(`{}`))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-release
			}))
			defer func() {
				close(release)
				ts.Close()
			}()
			s := newTestBot(ts, "ch")
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- tt.call(ctx, s) }()
			<-started
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("expected cancellation error")
				}
			case <-time.After(time.Second):
				t.Fatal("dashboard request did not unblock after context cancellation")
			}
		})
	}
}
