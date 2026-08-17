package apiproxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// eventRecorder collects proxied events for assertion.
type eventRecorder struct {
	mu     sync.Mutex
	events []Event
}

func (e *eventRecorder) handler(evt Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, evt)
}

func (e *eventRecorder) snapshot() []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Event, len(e.events))
	copy(out, e.events)
	return out
}

func (e *eventRecorder) byDirection(dir string) []Event {
	var out []Event
	for _, evt := range e.snapshot() {
		if evt.Direction == dir {
			out = append(out, evt)
		}
	}
	return out
}

// ---- New ----

func TestNewDefaultsToAnthropicUpstream(t *testing.T) {
	p, err := New("", nil, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.upstream.String() != defaultUpstream {
		t.Fatalf("upstream = %q, want %q", p.upstream, defaultUpstream)
	}
}

func TestNewRejectsInvalidUpstream(t *testing.T) {
	if _, err := New("://not a url", nil, ""); err == nil {
		t.Fatal("an unparseable upstream URL must be rejected")
	}
}

func TestNewAcceptsCustomUpstream(t *testing.T) {
	p, err := New("http://localhost:9999", nil, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.upstream.Host != "localhost:9999" {
		t.Fatalf("host = %q", p.upstream.Host)
	}
}

// ---- isValidAuth ----

// The proxy injects its own key ONLY when the caller has not supplied real
// credentials. Misjudging this either leaks the proxy's key over a request that
// already had one, or strips a caller's valid auth.
func TestIsValidAuth(t *testing.T) {
	longKey := strings.Repeat("k", 40)

	cases := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"no headers", nil, false},
		{"long Authorization", map[string]string{"Authorization": "Bearer " + longKey}, true},
		{"short Authorization", map[string]string{"Authorization": "Bearer x"}, false},
		{"empty Authorization", map[string]string{"Authorization": ""}, false},
		{"long X-Api-Key", map[string]string{"X-Api-Key": longKey}, true},
		{"short X-Api-Key", map[string]string{"X-Api-Key": "short"}, false},
		// The dummy key Claude Code sends must NOT count as real auth, or the
		// proxy never injects the configured key and every call 401s.
		{"dummy X-Api-Key", map[string]string{"X-Api-Key": "sk-ant-dummy" + longKey}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			if got := isValidAuth(req); got != tc.want {
				t.Fatalf("isValidAuth = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---- director ----

// With no real caller auth, the configured key is injected and the dummy
// X-Api-Key removed so upstream sees exactly one credential.
func TestDirectorInjectsConfiguredKey(t *testing.T) {
	p, err := New("https://upstream.example", nil, "secret-key")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("X-Api-Key", "sk-ant-dummy-placeholder")
	p.director(req)

	if got := req.Header.Get("Authorization"); got != "Bearer secret-key" {
		t.Fatalf("Authorization = %q, want the injected key", got)
	}
	if got := req.Header.Get("X-Api-Key"); got != "" {
		t.Fatalf("the dummy X-Api-Key must be removed, got %q", got)
	}
}

// A caller's real credentials must be left alone.
func TestDirectorPreservesCallerAuth(t *testing.T) {
	p, err := New("https://upstream.example", nil, "secret-key")
	if err != nil {
		t.Fatal(err)
	}
	callerAuth := "Bearer " + strings.Repeat("c", 40)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", callerAuth)
	p.director(req)

	if got := req.Header.Get("Authorization"); got != callerAuth {
		t.Fatalf("caller auth must be preserved, got %q", got)
	}
}

// With no configured key the proxy must not invent one.
func TestDirectorWithoutConfiguredKeyLeavesAuthAlone(t *testing.T) {
	p, err := New("https://upstream.example", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	p.director(req)

	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("no key configured, want no Authorization, got %q", got)
	}
}

// Accept-Encoding must be stripped so upstream returns plaintext SSE the proxy
// can parse line by line.
func TestDirectorStripsAcceptEncoding(t *testing.T) {
	p, err := New("https://upstream.example", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	p.director(req)

	if got := req.Header.Get("Accept-Encoding"); got != "" {
		t.Fatalf("Accept-Encoding must be stripped, got %q", got)
	}
}

// The request must be retargeted at the upstream host.
func TestDirectorRetargetsUpstream(t *testing.T) {
	p, err := New("https://upstream.example", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	p.director(req)

	if req.URL.Host != "upstream.example" || req.URL.Scheme != "https" {
		t.Fatalf("request not retargeted: %s://%s", req.URL.Scheme, req.URL.Host)
	}
	if req.Host != "upstream.example" {
		t.Fatalf("Host header = %q", req.Host)
	}
}

// ---- ServeHTTP: request logging + proxying ----

// A proxied JSON request is logged with its model and agent, and the body still
// reaches upstream intact — logging must not consume it.
func TestServeHTTPLogsRequestAndForwardsBody(t *testing.T) {
	var upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		upstreamBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"claude-opus-4","content":"hi"}`))
	}))
	defer upstream.Close()

	rec := &eventRecorder{}
	p, err := New(upstream.URL, rec.handler, "")
	if err != nil {
		t.Fatal(err)
	}

	reqBody := `{"model":"claude-opus-4","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("X-Hive-Agent", "scanner")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if upstreamBody != reqBody {
		t.Fatalf("upstream received %q, want the original body intact", upstreamBody)
	}

	reqs := rec.byDirection("request")
	if len(reqs) != 1 {
		t.Fatalf("want 1 request event, got %d", len(reqs))
	}
	if reqs[0].Model != "claude-opus-4" {
		t.Errorf("model = %q", reqs[0].Model)
	}
	if reqs[0].Agent != "scanner" {
		t.Errorf("agent = %q", reqs[0].Agent)
	}
	if reqs[0].Method != http.MethodPost || reqs[0].Path != "/v1/messages" {
		t.Errorf("method/path = %s %s", reqs[0].Method, reqs[0].Path)
	}
}

// A non-JSON request body must still be proxied, just without a parsed body in
// the event.
func TestServeHTTPHandlesNonJSONBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	rec := &eventRecorder{}
	p, err := New(upstream.URL, rec.handler, "")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("not json at all"))
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	reqs := rec.byDirection("request")
	if len(reqs) != 1 {
		t.Fatalf("want 1 request event, got %d", len(reqs))
	}
	if len(reqs[0].Body) != 0 {
		t.Errorf("a non-JSON body must not be recorded as JSON, got %s", reqs[0].Body)
	}
}

// With no handler the proxy still works — logging is optional.
func TestServeHTTPWithoutHandler(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	p, err := New(upstream.URL, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
}

// ---- modifyResponse: non-SSE ----

// A JSON response is logged with status and model, and the client still
// receives the full body.
func TestResponseLoggedAndPassedThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"model":"claude-sonnet-4","id":"msg_1"}`))
	}))
	defer upstream.Close()

	rec := &eventRecorder{}
	p, err := New(upstream.URL, rec.handler, "")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "msg_1") {
		t.Fatalf("client did not receive the full body: %s", w.Body.String())
	}

	resps := rec.byDirection("response")
	if len(resps) != 1 {
		t.Fatalf("want 1 response event, got %d", len(resps))
	}
	if resps[0].Status != http.StatusCreated {
		t.Errorf("status = %d", resps[0].Status)
	}
	if resps[0].Model != "claude-sonnet-4" {
		t.Errorf("model = %q", resps[0].Model)
	}
}

// An upstream error status is still logged rather than dropped — a 429 is
// exactly what an operator needs to see.
func TestErrorResponseIsLogged(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
	}))
	defer upstream.Close()

	rec := &eventRecorder{}
	p, err := New(upstream.URL, rec.handler, "")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	resps := rec.byDirection("response")
	if len(resps) != 1 {
		t.Fatalf("want 1 response event, got %d", len(resps))
	}
	if resps[0].Status != http.StatusTooManyRequests {
		t.Fatalf("a 429 must be logged, got status %d", resps[0].Status)
	}
	if !strings.Contains(string(resps[0].Body), "rate_limit_error") {
		t.Errorf("error body not captured: %s", resps[0].Body)
	}
}

// ---- SSE streaming ----

// The SSE path must emit message_start / message_delta / message_stop events,
// accumulate the streamed text, AND pass every byte through to the client.
func TestSSEStreamIsLoggedAndPassedThrough(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"model":"claude-opus-4","usage":{"input_tokens":10,"output_tokens":0}}}`,
		``,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}`,
		``,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":" world"}}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", sseContentType)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(stream))
	}))
	defer upstream.Close()

	rec := &eventRecorder{}
	p, err := New(upstream.URL, rec.handler, "")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	// Every SSE line must still reach the client.
	body := w.Body.String()
	for _, want := range []string{"message_start", "Hello", " world", "message_stop", "[DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("client stream is missing %q", want)
		}
	}

	sse := rec.byDirection("sse")
	byType := map[string]Event{}
	for _, e := range sse {
		byType[e.SSEType] = e
	}

	start, ok := byType[sseEventMsgStart]
	if !ok {
		t.Fatal("no message_start event was logged")
	}
	if start.Model != "claude-opus-4" {
		t.Errorf("model = %q", start.Model)
	}
	if start.Status != http.StatusOK {
		t.Errorf("status = %d", start.Status)
	}

	if _, ok := byType[sseEventMsgDelta]; !ok {
		t.Error("no message_delta event was logged")
	}

	stop, ok := byType[sseEventMsgStop]
	if !ok {
		t.Fatal("no message_stop event was logged")
	}
	// The stop summary must carry the accumulated text, which is what makes the
	// log useful rather than a pile of fragments.
	var summary map[string]any
	if err := json.Unmarshal(stop.Body, &summary); err != nil {
		t.Fatalf("stop summary is not JSON: %v", err)
	}
	if summary["text"] != "Hello world" {
		t.Fatalf("accumulated text = %v, want %q", summary["text"], "Hello world")
	}
	if summary["model"] != "claude-opus-4" {
		t.Errorf("summary model = %v", summary["model"])
	}
	if tl, _ := summary["text_length"].(float64); int(tl) != len("Hello world") {
		t.Errorf("text_length = %v", summary["text_length"])
	}
}

// Malformed SSE data lines must be passed through but not logged as events.
func TestSSEIgnoresMalformedDataLines(t *testing.T) {
	stream := strings.Join([]string{
		`data: {not valid json`,
		``,
		`data: `,
		``,
		`: a comment line`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", sseContentType)
		w.Write([]byte(stream))
	}))
	defer upstream.Close()

	rec := &eventRecorder{}
	p, err := New(upstream.URL, rec.handler, "")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	// The malformed line must still reach the client.
	if !strings.Contains(w.Body.String(), "{not valid json") {
		t.Error("malformed data line was not passed through to the client")
	}
	// Only the well-formed message_stop should have produced an event.
	sse := rec.byDirection("sse")
	if len(sse) != 1 || sse[0].SSEType != sseEventMsgStop {
		t.Fatalf("want only the message_stop event, got %+v", sse)
	}
}

// An SSE stream carrying no recognised events still passes through cleanly.
func TestSSEWithNoRecognisedEvents(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", sseContentType)
		w.Write([]byte("data: {\"type\":\"ping\"}\n\n"))
	}))
	defer upstream.Close()

	rec := &eventRecorder{}
	p, err := New(upstream.URL, rec.handler, "")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "ping") {
		t.Error("the ping line was not passed through")
	}
	if got := rec.byDirection("sse"); len(got) != 0 {
		t.Fatalf("an unrecognised SSE type must not be logged, got %+v", got)
	}
}

// ---- errorHandler ----

// An unreachable upstream must yield 502 rather than a hang or a panic.
func TestUnreachableUpstreamYields502(t *testing.T) {
	// Port 1 on loopback is not listening.
	p, err := New("http://127.0.0.1:1", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("want 502 for an unreachable upstream, got %d", w.Code)
	}
}
