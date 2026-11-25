package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const streamTestRequest = `{"model":"claude-cli-alias","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`

func setStreamPingIntervalForTest(t *testing.T, d time.Duration) {
	t.Helper()
	prev := inferenceStreamPingInterval
	inferenceStreamPingInterval = func() time.Duration { return d }
	t.Cleanup(func() { inferenceStreamPingInterval = prev })
}

// sseEventSummary is "event index delta-or-block-type" for each event, the
// shape the CLI assembles content blocks from.
func sseEventSummary(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, ev := range parseGatewayCanarySSE(t, body) {
		var payload struct {
			Index        *int `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
			Delta struct {
				Type string `json:"type"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
			t.Fatalf("event %s: %v", ev.Event, err)
		}
		s := ev.Event
		if payload.Index != nil {
			s += " " + strconv.Itoa(*payload.Index)
		}
		if payload.ContentBlock.Type != "" {
			s += " " + payload.ContentBlock.Type
		}
		if payload.Delta.Type != "" {
			s += " " + payload.Delta.Type
		}
		out = append(out, s)
	}
	return out
}

func TestRelayOpenAISSE_ReasoningBecomesThinkingBlock(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"Let me "}}]}`,
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"think."}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"Answer"}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	var buf bytes.Buffer
	if _, err := translateOpenAISSEToAnthropicUsage(strings.NewReader(upstream), &buf, "m"); err != nil {
		t.Fatal(err)
	}

	got := sseEventSummary(t, buf.String())
	want := []string{
		"message_start",
		"content_block_start 0 thinking",
		"content_block_delta 0 thinking_delta",
		"content_block_delta 0 thinking_delta",
		"content_block_stop 0",
		"content_block_start 1 text",
		"content_block_delta 1 text_delta",
		"content_block_stop 1",
		"message_delta",
		"message_stop",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(buf.String(), `"thinking":"Let me "`) || !strings.Contains(buf.String(), `"thinking":"think."`) {
		t.Fatalf("reasoning text not forwarded:\n%s", buf.String())
	}
}

// flushCountingWriter counts the flushes an http.ResponseWriter would see.
type flushCountingWriter struct {
	bytes.Buffer
	flushes int
}

func (w *flushCountingWriter) Flush() { w.flushes++ }

func TestRelayOpenAISSE_FlushesEveryEvent(t *testing.T) {
	upstream := sseChunk("a") + sseChunk("b") + "data: [DONE]\n\n"
	var w flushCountingWriter
	if _, err := translateOpenAISSEToAnthropicUsage(strings.NewReader(upstream), &w, "m"); err != nil {
		t.Fatal(err)
	}
	events := strings.Count(w.String(), "\n\n")
	if events == 0 || w.flushes != events {
		t.Fatalf("%d events written, %d flushed", events, w.flushes)
	}
}

// newHeldStreamUpstream is an upstream that answers with a streamed "hello"
// only after release is called.
func newHeldStreamUpstream(t *testing.T) (up *httptest.Server, release func()) {
	t.Helper()
	held := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(held) }) }
	up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-held:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseChunk("hello"))
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	// Cleanups run last-in first-out: release the held handler before Close
	// waits for it.
	t.Cleanup(up.Close)
	t.Cleanup(release)
	return up, release
}

// expectPingThenAnswer reads br until a ping, releases the upstream, and
// expects the upstream's text and message_stop to follow.
func expectPingThenAnswer(t *testing.T, br *bufio.Reader, release func()) {
	t.Helper()
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended before a ping: %v", err)
		}
		if line == "event: ping\n" {
			break
		}
	}
	release()
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rest), `"text":"hello"`) || !strings.Contains(string(rest), "event: message_stop\n") {
		t.Fatalf("the upstream answer did not follow the pings:\n%s", rest)
	}
}

// The CLI must see headers and pings while the upstream has not answered yet:
// its byte-level watchdog counts the wait for response headers.
func TestInferenceTranslator_StreamPingsBeforeUpstreamAnswers(t *testing.T) {
	setStreamPingIntervalForTest(t, 10*time.Millisecond)
	up, release := newHeldStreamUpstream(t)

	var logs bytes.Buffer
	p := newKeyResolverTestProxy(&logs)
	p.inference.Set("scanner", &InferenceRoute{Backend: "litellm", Endpoint: up.URL, Model: "m", APIKey: "k"})
	translator := httptest.NewServer(p.inferenceTranslatorHandler())
	t.Cleanup(translator.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, translator.URL+"/v1/messages", strings.NewReader(streamTestRequest))
	req.Header.Set("x-api-key", "sk-hive-scanner")
	// Do returns on the response headers; the upstream is still held, so the
	// translator must have sent them before the upstream answered.
	resp, err := translator.Client().Do(req)
	if err != nil {
		t.Fatalf("no response headers before the upstream answered: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d, content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	expectPingThenAnswer(t, bufio.NewReader(resp.Body), release)
}

// The MITM reroute writes on the raw connection; it too must send headers and
// pings before the upstream answers.
func TestInferenceReroute_StreamPingsBeforeUpstreamAnswers(t *testing.T) {
	setStreamPingIntervalForTest(t, 10*time.Millisecond)
	up, release := newHeldStreamUpstream(t)

	var logs bytes.Buffer
	p := newKeyResolverTestProxy(&logs)
	route := &InferenceRoute{Backend: "litellm", Endpoint: up.URL, Model: "m", APIKey: "k"}
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(streamTestRequest))
		p.handleInferenceRequest(server, req, "scanner", route)
	}()
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))

	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("no response headers before the upstream answered: %v", err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d, content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	expectPingThenAnswer(t, bufio.NewReader(resp.Body), release)
	<-done
}

// lastSSEError returns the error type and message of body's last event, which
// must be an Anthropic error event.
func lastSSEError(t *testing.T, body string) (errType, message string) {
	t.Helper()
	events := parseGatewayCanarySSE(t, body)
	last := events[len(events)-1]
	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(last.Data), &payload); err != nil {
		t.Fatal(err)
	}
	if last.Event != "error" || payload.Type != "error" {
		t.Fatalf("last event %s %s, want an error event:\n%s", last.Event, last.Data, body)
	}
	return payload.Error.Type, payload.Error.Message
}

// Once the 200 is out, an upstream error status reaches the CLI as an SSE
// error event the CLI can classify, not as a stream that just stops.
func TestInferenceTranslator_StreamUpstreamErrorIsErrorEvent(t *testing.T) {
	setStreamPingIntervalForTest(t, time.Hour)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
	}))
	t.Cleanup(up.Close)

	var logs bytes.Buffer
	p := newKeyResolverTestProxy(&logs)
	p.inference.Set("scanner", &InferenceRoute{Backend: "litellm", Endpoint: up.URL, Model: "m", APIKey: "k"})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(streamTestRequest))
	req.Header.Set("x-api-key", "sk-hive-scanner")
	w := httptest.NewRecorder()
	p.inferenceTranslatorHandler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d; the stream had started, so the error must be an event", w.Code)
	}
	errType, msg := lastSSEError(t, w.Body.String())
	if errType != "rate_limit_error" || !strings.Contains(msg, "inference backend returned 429") {
		t.Fatalf("error %s %q, want a rate_limit_error for the 429:\n%s", errType, msg, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "message_stop") {
		t.Fatalf("a failed stream must not end with message_stop:\n%s", w.Body.String())
	}
}

// An upstream stream cut mid-response ends in an error event, not
// message_stop, and counts as an upstream error in the translator metric.
func TestInferenceTranslator_BrokenStreamIsErrorEvent(t *testing.T) {
	setStreamPingIntervalForTest(t, time.Hour)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m := newTranslatorMetrics(mp.Meter(instrumentationName))
	prev := globalTranslatorMetrics
	globalTranslatorMetrics = func() translatorMetrics { return m }
	t.Cleanup(func() {
		globalTranslatorMetrics = prev
		_ = mp.Shutdown(context.Background())
	})

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseChunk("partial"))
		w.(http.Flusher).Flush()
		// Drop the connection without the chunked terminator.
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(up.Close)

	var logs bytes.Buffer
	p := newKeyResolverTestProxy(&logs)
	p.inference.Set("scanner", &InferenceRoute{Backend: "litellm", Endpoint: up.URL, Model: "m", APIKey: "k"})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(streamTestRequest))
	req.Header.Set("x-api-key", "sk-hive-scanner")
	w := httptest.NewRecorder()
	p.inferenceTranslatorHandler().ServeHTTP(w, req)

	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, `"text":"partial"`) {
		t.Fatalf("status %d; want the partial text relayed on a 200 stream:\n%s", w.Code, body)
	}
	if errType, msg := lastSSEError(t, body); errType != "api_error" || !strings.Contains(msg, "inference upstream stream failed") {
		t.Fatalf("error %s %q, want an api_error for the cut stream:\n%s", errType, msg, body)
	}
	if strings.Contains(body, "message_stop") {
		t.Fatalf("a cut stream must not end with message_stop:\n%s", body)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var outcomes []string
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if sum, ok := mt.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range sum.DataPoints {
					v, _ := dp.Attributes.Value(attrOutcome)
					outcomes = append(outcomes, v.AsString())
				}
			}
		}
	}
	if len(outcomes) != 1 || outcomes[0] != outcomeUpstreamError {
		t.Fatalf("translator outcomes %v, want [%s]", outcomes, outcomeUpstreamError)
	}
}

func TestParseInferenceStreamPingInterval(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"":     defaultInferenceStreamPingInterval,
		"5s":   5 * time.Second,
		"soon": defaultInferenceStreamPingInterval,
		"0s":   defaultInferenceStreamPingInterval,
		"-1s":  defaultInferenceStreamPingInterval,
	} {
		if got := parseInferenceStreamPingInterval(in); got != want {
			t.Errorf("%q: got %s, want %s", in, got, want)
		}
	}
}
