package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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

// The CLI must see headers and pings while the upstream has not answered yet:
// its byte-level watchdog counts the wait for response headers.
func TestInferenceTranslator_StreamPingsBeforeUpstreamAnswers(t *testing.T) {
	setStreamPingIntervalForTest(t, 10*time.Millisecond)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseUpstream)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseChunk("hello"))
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(up.Close)

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

	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended before a ping: %v", err)
		}
		if line == "event: ping\n" {
			break
		}
	}

	releaseUpstream()
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rest), `"text":"hello"`) || !strings.Contains(string(rest), "event: message_stop\n") {
		t.Fatalf("the upstream answer did not follow the pings:\n%s", rest)
	}
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
	events := parseGatewayCanarySSE(t, w.Body.String())
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
	if last.Event != "error" || payload.Type != "error" || payload.Error.Type != "rate_limit_error" ||
		!strings.Contains(payload.Error.Message, "inference backend returned 429") {
		t.Fatalf("last event %s %s, want a rate_limit_error event:\n%s", last.Event, last.Data, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "message_stop") {
		t.Fatalf("a failed stream must not end with message_stop:\n%s", w.Body.String())
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
