package ioscan

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const benignReply = `{"choices":[{"message":{"content":"{\"score\":0.1,\"category\":\"benign\",\"rationale\":\"ok\"}"}}]}`

// A single gateway blip must not reach the scheduler's fail mode: the
// transient statuses are retried once and the second reply is used
// (hivecommons/hive#18105).
func TestOpenAIChatClientRetriesTransientStatusOnce(t *testing.T) {
	for _, status := range []int{
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(status)
					return
				}
				fmt.Fprint(w, benignReply)
			}))
			defer srv.Close()
			client := &openAIChatClient{endpoint: srv.URL, model: "judge-1", http: srv.Client()}
			reply, err := client.Complete(context.Background(), nil, 10)
			if err != nil {
				t.Fatalf("Complete: %v, want the retry to succeed", err)
			}
			if !strings.Contains(reply, `"category":"benign"`) {
				t.Fatalf("reply = %q, want the retried reply", reply)
			}
			if got := calls.Load(); got != 2 {
				t.Fatalf("calls = %d, want 2 (one failure, one retry)", got)
			}
		})
	}
}

// A persistent transient failure is retried exactly once, then surfaces with
// the original status so the fail-mode WARN still names it.
func TestOpenAIChatClientGivesUpAfterOneRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	client := &openAIChatClient{endpoint: srv.URL, model: "judge-1", http: srv.Client()}
	_, err := client.Complete(context.Background(), nil, 10)
	if err == nil || !strings.Contains(err.Error(), "returned 503") || !strings.Contains(err.Error(), "after 1 retry") {
		t.Fatalf("Complete error = %v, want the 503 noted as retried", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
}

// Client errors (bad key, unknown model) will not fix themselves: no retry.
func TestOpenAIChatClientDoesNotRetryPermanentStatus(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
			}))
			defer srv.Close()
			client := &openAIChatClient{endpoint: srv.URL, model: "judge-1", http: srv.Client()}
			_, err := client.Complete(context.Background(), nil, 10)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("returned %d", status)) {
				t.Fatalf("Complete error = %v, want the %d status", err, status)
			}
			if strings.Contains(err.Error(), "retry") {
				t.Fatalf("Complete error = %v, want no retry for a permanent status", err)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("calls = %d, want 1", got)
			}
		})
	}
}

// A connection dropped before any reply (gateway restart) is retried.
func TestOpenAIChatClientRetriesDroppedConnection(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("response writer cannot hijack")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		fmt.Fprint(w, benignReply)
	}))
	defer srv.Close()
	client := &openAIChatClient{endpoint: srv.URL, model: "judge-1", http: srv.Client()}
	if _, err := client.Complete(context.Background(), nil, 10); err != nil {
		t.Fatalf("Complete: %v, want the retry after a dropped connection to succeed", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
}

// A timeout is not retried: a hung backend must cost one timeout per string,
// not two.
func TestOpenAIChatClientDoesNotRetryTimeout(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	httpClient := srv.Client()
	httpClient.Timeout = 50 * time.Millisecond
	client := &openAIChatClient{endpoint: srv.URL, model: "judge-1", http: httpClient}
	if _, err := client.Complete(context.Background(), nil, 10); err == nil {
		t.Fatal("Complete: expected a timeout error")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1 (timeouts are not retried)", got)
	}
}

// cancelAfterTransport cancels the caller's context as soon as the first
// response arrives, so the cancellation lands in the retry pause.
type cancelAfterTransport struct {
	next   http.RoundTripper
	cancel context.CancelFunc
}

func (c cancelAfterTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.next.RoundTrip(r)
	c.cancel()
	return resp, err
}

// A caller that cancels during the retry pause gets the original error back
// without a second request.
func TestOpenAIChatClientRetryHonoursCancellation(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	httpClient := srv.Client()
	httpClient.Transport = cancelAfterTransport{next: httpClient.Transport, cancel: cancel}
	client := &openAIChatClient{endpoint: srv.URL, model: "judge-1", http: httpClient, retryDelay: time.Hour}
	done := make(chan error, 1)
	go func() {
		_, err := client.Complete(ctx, nil, 10)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "returned 502") {
			t.Fatalf("Complete error = %v, want the original 502", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Complete did not return after cancellation during the retry pause")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1 (no retry after cancellation)", got)
	}
}

func TestNewLLMClassifierSetsRetryDelay(t *testing.T) {
	c, err := NewLLMClassifier(LLMClassifierConfig{Endpoint: "https://models.example"})
	if err != nil {
		t.Fatalf("NewLLMClassifier: %v", err)
	}
	client, ok := c.client.(*openAIChatClient)
	if !ok {
		t.Fatalf("client = %T, want the built-in OpenAI-compatible client", c.client)
	}
	if client.retryDelay != classifierRetryDelay {
		t.Fatalf("retryDelay = %v, want %v", client.retryDelay, classifierRetryDelay)
	}
}
