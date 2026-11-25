package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Keep-alive for streamed inference responses (hivecommons/hive#19793).
//
// The Claude CLI aborts a stream that carries no bytes for 300 s, counting the
// wait for response headers, and one that carries no parsed event for 300 s.
// A model behind a gateway can wait in a queue, or reason, for longer than
// that before it writes a content token. For a streaming request the
// translator therefore sends its 200 and message_start before it calls the
// upstream, writes an Anthropic ping event whenever nothing else has reached
// the CLI for the ping interval, and forwards reasoning deltas as a thinking
// block. An upstream failure after the 200 reaches the CLI as an SSE error
// event.

const (
	// inferenceStreamPingIntervalEnv overrides the ping interval with a Go
	// duration (for example "5s").
	inferenceStreamPingIntervalEnv     = "HIVE_INFERENCE_STREAM_PING_INTERVAL"
	defaultInferenceStreamPingInterval = 10 * time.Second
)

// inferenceStreamPingInterval is the resolved ping interval, read once. A
// variable so tests can shorten it.
var inferenceStreamPingInterval = sync.OnceValue(func() time.Duration {
	return parseInferenceStreamPingInterval(os.Getenv(inferenceStreamPingIntervalEnv))
})

// parseInferenceStreamPingInterval returns the default for an empty value and,
// with a warning, for one that is not a positive Go duration.
func parseInferenceStreamPingInterval(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return defaultInferenceStreamPingInterval
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		slog.Warn("invalid inference stream ping interval, using the default",
			"env", inferenceStreamPingIntervalEnv, "value", v,
			"default", defaultInferenceStreamPingInterval.String())
		return defaultInferenceStreamPingInterval
	}
	return d
}

// anthropicSSEHeaders is the response head of a streamed Messages response on
// the raw MITM connection.
const anthropicSSEHeaders = "HTTP/1.1 200 OK\r\n" +
	"Content-Type: text/event-stream; charset=utf-8\r\n" +
	"Cache-Control: no-cache\r\n" +
	"Connection: keep-alive\r\n\r\n"

// anthropicStream writes one Anthropic Messages SSE response. Every write goes
// through it, so the keep-alive goroutine and the translation never interleave
// bytes on w.
type anthropicStream struct {
	mu        sync.Mutex
	w         io.Writer
	lastWrite time.Time
	stop      chan struct{}
	stopped   chan struct{}
}

// startAnthropicSSE sends the 200 and SSE headers on w, then message_start.
func startAnthropicSSE(w http.ResponseWriter, model string) *anthropicStream {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	return newAnthropicStream(w, model)
}

// startAnthropicSSEConn is startAnthropicSSE for the raw MITM connection. It
// fails only when the client is gone.
func startAnthropicSSEConn(conn net.Conn, model string) (*anthropicStream, error) {
	if _, err := io.WriteString(conn, anthropicSSEHeaders); err != nil {
		return nil, err
	}
	return newAnthropicStream(conn, model), nil
}

// newAnthropicStream writes message_start for model on w.
func newAnthropicStream(w io.Writer, model string) *anthropicStream {
	s := &anthropicStream{w: w}
	s.event("message_start", anthropicSSEMessageStart{
		Type: "message_start",
		Message: anthropicSSEMessage{
			ID:      fmt.Sprintf("msg_%d", time.Now().UnixNano()),
			Type:    "message",
			Role:    "assistant",
			Model:   model,
			Content: []anthropicContentBlock{},
			Usage:   &anthropicUsage{},
		},
	})
	return s
}

func (s *anthropicStream) event(eventType string, data any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeSSE(s.w, eventType, data)
	s.lastWrite = time.Now()
}

// keepAlive writes a ping event whenever nothing has been written for
// interval, until Close.
func (s *anthropicStream) keepAlive(interval time.Duration) {
	s.stop = make(chan struct{})
	s.stopped = make(chan struct{})
	go s.pingLoop(interval)
}

func (s *anthropicStream) pingLoop(interval time.Duration) {
	defer close(s.stopped)
	t := time.NewTimer(interval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
		s.mu.Lock()
		wait := interval - time.Since(s.lastWrite)
		if wait <= 0 {
			writeSSE(s.w, "ping", map[string]string{"type": "ping"})
			s.lastWrite = time.Now()
			wait = interval
		}
		s.mu.Unlock()
		t.Reset(wait)
	}
}

// Close stops the keep-alive and waits for it, so the caller may return and
// release w. It is a no-op on a stream without keep-alive.
func (s *anthropicStream) Close() {
	if s.stop == nil {
		return
	}
	close(s.stop)
	<-s.stopped
	s.stop = nil
}

// fail ends the stream with an Anthropic error event. Once the 200 is sent,
// this is the only way an upstream status can reach the CLI.
func (s *anthropicStream) fail(status int, message string) {
	s.event("error", map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    anthropicErrorType(status),
			"message": message,
		},
	})
}

// anthropicErrorType is the Anthropic error type for an HTTP status, so the
// CLI classifies an SSE error the way it would the status.
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable, 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// relayOpenAISSE translates an OpenAI SSE stream into the rest of the
// Anthropic stream, through message_stop. It returns the token usage of the
// stream's usage chunk (present when the outbound request set
// stream_options.include_usage). A read error ends the stream with an error
// event instead of message_stop, so the CLI does not take a cut response for
// a complete one.
func (s *anthropicStream) relayOpenAISSE(r io.Reader) (gatewayUsage, error) {
	scanner := bufio.NewScanner(r)
	const maxSSELine = 256 * 1024
	scanner.Buffer(make([]byte, maxSSELine), maxSSELine)

	var totalInputTokens int
	var totalOutputTokens int
	var totalCachedTokens int
	// next is the index of the next content block. open names the thinking or
	// text block open at index next, "" when neither is. Tool-use blocks stay
	// open until the finish chunk, so they take their index at once.
	next := 0
	open := ""
	closeOpen := func() {
		if open == "" {
			return
		}
		s.event("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": next,
		})
		next++
		open = ""
	}
	openBlock := func(kind string, block any) {
		if open == kind {
			return
		}
		closeOpen()
		s.event("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         next,
			"content_block": block,
		})
		open = kind
	}
	toolCallState := make(map[int]*sseToolCallAccumulator)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := line[len("data: "):]
		if data == "[DONE]" {
			break
		}

		var chunk openaiSSEChunk
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}

		if chunk.Usage != nil {
			totalOutputTokens = chunk.Usage.CompletionTokens
			if chunk.Usage.PromptTokens > 0 {
				totalInputTokens = chunk.Usage.PromptTokens
			}
			if c := chunk.Usage.cachedTokens(); c > 0 {
				totalCachedTokens = c
			}
		}

		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta

		if delta.ReasoningContent != "" {
			openBlock("thinking", map[string]string{"type": "thinking", "thinking": "", "signature": ""})
			s.event("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": next,
				"delta": map[string]string{
					"type":     "thinking_delta",
					"thinking": delta.ReasoningContent,
				},
			})
		}

		if delta.Content != "" {
			openBlock("text", map[string]string{"type": "text", "text": ""})
			s.event("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": next,
				"delta": map[string]string{
					"type": "text_delta",
					"text": delta.Content,
				},
			})
		}

		for _, tc := range delta.ToolCalls {
			acc, exists := toolCallState[tc.Index]
			if !exists {
				closeOpen()
				acc = &sseToolCallAccumulator{
					id:         tc.ID,
					name:       tc.Function.Name,
					blockIndex: next,
				}
				toolCallState[tc.Index] = acc
				s.event("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": next,
					"content_block": map[string]any{
						"type":  "tool_use",
						"id":    tc.ID,
						"name":  tc.Function.Name,
						"input": map[string]any{},
					},
				})
				next++
			}

			if tc.Function.Arguments != "" {
				acc.args.WriteString(tc.Function.Arguments)
				s.event("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": acc.blockIndex,
					"delta": map[string]any{
						"type":         "input_json_delta",
						"partial_json": tc.Function.Arguments,
					},
				})
			}
		}

		if finish := chunk.Choices[0].FinishReason; finish != "" {
			closeOpen()
			for _, acc := range toolCallState {
				s.event("content_block_stop", map[string]any{
					"type":  "content_block_stop",
					"index": acc.blockIndex,
				})
			}
			s.event("message_delta", map[string]any{
				"type": "message_delta",
				"delta": map[string]string{
					"stop_reason": mapFinishReason(finish),
				},
				"usage": map[string]int{
					"output_tokens": totalOutputTokens,
				},
			})
		}
	}

	usage := gatewayUsage{Input: int64(totalInputTokens), Output: int64(totalOutputTokens), Cached: int64(totalCachedTokens)}
	if err := scanner.Err(); err != nil {
		s.fail(http.StatusBadGateway, "inference upstream stream failed: "+err.Error())
		return usage, err
	}
	s.event("message_stop", map[string]string{"type": "message_stop"})
	return usage, nil
}

// relayMessage writes a complete Anthropic message as the rest of the stream,
// for an upstream that answered a streaming request with one JSON body.
func (s *anthropicStream) relayMessage(msg anthropicResponse) {
	for i, block := range msg.Content {
		if block.Type == "tool_use" {
			s.event("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": i,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    block.ID,
					"name":  block.Name,
					"input": map[string]any{},
				},
			})
			s.event("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": i,
				"delta": map[string]string{
					"type":         "input_json_delta",
					"partial_json": string(block.Input),
				},
			})
		} else {
			s.event("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         i,
				"content_block": map[string]string{"type": "text", "text": ""},
			})
			if block.Text != "" {
				s.event("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": i,
					"delta": map[string]string{"type": "text_delta", "text": block.Text},
				})
			}
		}
		s.event("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": i,
		})
	}
	outputTokens := 0
	if msg.Usage != nil {
		outputTokens = msg.Usage.OutputTokens
	}
	s.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]string{"stop_reason": msg.StopReason},
		"usage": map[string]int{"output_tokens": outputTokens},
	})
	s.event("message_stop", map[string]string{"type": "message_stop"})
}
