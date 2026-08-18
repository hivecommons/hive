package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ── SSE edge/error path tests — complements contribute_sse_test.go ──────────

// TestBroadcastDropsSlowSubscriber asserts that broadcast never blocks when a
// subscriber's buffer is full. It fills the channel completely, broadcasts one
// more event, and verifies the call returns promptly without panic or deadlock.
func TestBroadcastDropsSlowSubscriber(t *testing.T) {
	reg := newSSERegistry()
	sub := reg.subscribe()

	// Fill the subscriber buffer completely.
	for i := 0; i < sseSubscriberBuffer; i++ {
		sub.events <- sseEvent{Type: "activity"}
	}

	// Broadcast must NOT block even though the subscriber is full.
	done := make(chan struct{})
	go func() {
		reg.broadcast(sseEvent{Type: "activity", Activity: &ActivityEntry{Username: "overflow"}})
		close(done)
	}()
	select {
	case <-done:
		// success — broadcast returned without blocking
	case <-time.After(2 * time.Second):
		t.Fatal("broadcast blocked on a full subscriber channel — back-pressure violated")
	}

	// The overflowed event should NOT be in the channel (it was dropped).
	if len(sub.events) != sseSubscriberBuffer {
		t.Errorf("subscriber channel length = %d, want %d (overflow was not dropped)", len(sub.events), sseSubscriberBuffer)
	}
}

// TestHandleContributeQueueJSON asserts the /api/contribute/queue handler returns
// the expected JSON shape {"queue": [...]} and 200 even when the queue is empty.
func TestHandleContributeQueueJSON(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/queue", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/contribute/queue returned %d, want 200", rec.Code)
	}

	var resp map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	queueRaw, ok := resp["queue"]
	if !ok {
		t.Fatal("response missing 'queue' key")
	}
	var items []ReadyQueueItem
	if err := json.Unmarshal(queueRaw, &items); err != nil {
		t.Fatalf("'queue' is not a valid []ReadyQueueItem: %v", err)
	}
	// With no actionable issues configured, the queue should be empty.
	if len(items) != 0 {
		t.Errorf("expected empty queue, got %d items", len(items))
	}
}

// TestHandleContributeEventsNoHub asserts that when contributeHub is nil, the
// events endpoint returns 503 Service Unavailable.
func TestHandleContributeEventsNoHub(t *testing.T) {
	s := NewServer(0, slog.Default())
	// Do NOT call registerContributeRoutes — contributeHub stays nil.
	// Call the handler directly.
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/events", nil)
	rec := httptest.NewRecorder()
	s.handleContributeEvents(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("handleContributeEvents with nil hub returned %d, want 503", rec.Code)
	}
}

// nonFlusherWriter is an http.ResponseWriter that does NOT implement http.Flusher.
type nonFlusherWriter struct {
	header http.Header
	code   int
	body   []byte
}

func (w *nonFlusherWriter) Header() http.Header         { return w.header }
func (w *nonFlusherWriter) Write(b []byte) (int, error) { w.body = append(w.body, b...); return len(b), nil }
func (w *nonFlusherWriter) WriteHeader(code int)        { w.code = code }

// TestHandleContributeEventsNoFlusher asserts that when the ResponseWriter does not
// implement http.Flusher, the handler returns 500 Internal Server Error.
func TestHandleContributeEventsNoFlusher(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/events", nil)
	w := &nonFlusherWriter{header: make(http.Header)}
	s.handleContributeEvents(w, req)

	if w.code != http.StatusInternalServerError {
		t.Fatalf("handleContributeEvents with non-flusher returned %d, want 500", w.code)
	}
}

// TestWriteSSESuccess asserts that writeSSE writes a valid SSE data frame.
func TestWriteSSESuccess(t *testing.T) {
	rec := httptest.NewRecorder()
	ev := sseEvent{Type: "hello", Queue: []ReadyQueueItem{}}
	if !writeSSE(rec, ev) {
		t.Fatal("writeSSE returned false for a valid event")
	}
	body := rec.Body.String()
	if len(body) == 0 {
		t.Fatal("writeSSE wrote nothing")
	}
	// Must start with "data: " and end with double newline (SSE spec).
	if body[:6] != "data: " {
		t.Errorf("SSE frame must start with 'data: ', got %q", body[:20])
	}
	if body[len(body)-2:] != "\n\n" {
		t.Errorf("SSE frame must end with double newline, got %q", body[len(body)-4:])
	}
}
