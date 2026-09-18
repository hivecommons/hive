package chat

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSSELoopBackoffResetsAfterConnectedSession(t *testing.T) {
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
	s.sseReconnectBase = 30 * time.Millisecond
	s.sseReconnectMax = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.sseLoop(ctx)

	var seen []time.Time
	for len(seen) < 4 {
		select {
		case tm := <-times:
			seen = append(seen, tm)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for SSE reconnects; got %d", len(seen))
		}
	}
	cancel()
	postSuccessDelay := seen[3].Sub(seen[2])
	if postSuccessDelay > 90*time.Millisecond {
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
