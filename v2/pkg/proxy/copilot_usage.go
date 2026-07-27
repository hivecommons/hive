package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

// copilotCompletionsPathSuffix is the path suffix of the Copilot chat
// completions endpoint. Copilot's OpenAI-compatible API serves completions at
// /chat/completions (sometimes prefixed, e.g. /v1/chat/completions), so a
// suffix match identifies the request whose response carries a usage block.
const copilotCompletionsPathSuffix = "/chat/completions"

// copilotAutoModelSentinel is the Copilot `--model auto` value: a request may
// send "auto" instead of a concrete model id, letting Copilot pick per task. It
// is not a real model row, so usage recorded against it would pollute the cost
// table — callers resolve the concrete model from the response instead.
const copilotAutoModelSentinel = "auto"

// copilotResponseModel is the minimal view of a Copilot completion response used
// to recover the concrete model id when the request used the "auto" sentinel.
type copilotResponseModel struct {
	Model string `json:"model"`
}

// copilotUsageRequest is the minimal view of a Copilot (OpenAI-shaped) chat
// completion request the proxy needs: the resolved model id (for cost
// attribution) and whether the response will be streamed (so the proxy can
// ensure a terminal usage chunk is emitted).
type copilotUsageRequest struct {
	Model         string               `json:"model"`
	Stream        bool                 `json:"stream,omitempty"`
	StreamOptions *openaiStreamOptions `json:"stream_options,omitempty"`
}

// isCopilotCompletionsPath reports whether an HTTP request path targets the
// Copilot chat completions endpoint.
func isCopilotCompletionsPath(path string) bool {
	return strings.HasSuffix(path, copilotCompletionsPathSuffix)
}

// parseCopilotRequest extracts the model id and streaming flag from a Copilot
// completion request body. Returns ("", false) when the body is not a parseable
// completion request.
func parseCopilotRequest(body []byte) (model string, streaming bool) {
	var req copilotUsageRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return "", false
	}
	return req.Model, req.Stream
}

// ensureStreamUsageRequested rewrites a streaming Copilot completion request
// body so it carries stream_options.include_usage=true. Without this hint the
// OpenAI-compatible endpoint omits the terminal usage chunk from the SSE
// response, leaving streamed completions uncountable. It is a no-op (returns the
// original bytes) when the body is not a streaming request, cannot be parsed as
// a JSON object, or already requests usage. Only the stream_options field is
// touched; every other field is preserved byte-for-byte via a generic map so
// the request forwarded to Copilot stays otherwise identical.
func ensureStreamUsageRequested(body []byte) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}

	streamRaw, ok := obj["stream"]
	if !ok {
		return body
	}
	var streaming bool
	if json.Unmarshal(streamRaw, &streaming) != nil || !streaming {
		return body
	}

	// Already asking for usage? Leave untouched.
	if soRaw, ok := obj["stream_options"]; ok {
		var so openaiStreamOptions
		if json.Unmarshal(soRaw, &so) == nil && so.IncludeUsage {
			return body
		}
	}

	soBytes, err := json.Marshal(openaiStreamOptions{IncludeUsage: true})
	if err != nil {
		return body
	}
	obj["stream_options"] = soBytes

	rewritten, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return rewritten
}

// extractCopilotSSEUsage scans an OpenAI-format SSE stream (already buffered in
// full) and returns the token usage from its terminal usage chunk. Returns
// (0, 0) when no usage chunk is present. Unlike translateOpenAISSEToAnthropic
// this does not transform the stream — the Copilot proxy forwards SSE bytes
// verbatim and only reads the usage out of a copy.
func extractCopilotSSEUsage(body []byte) (inputTokens, outputTokens int64) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	const maxSSELine = 256 * 1024
	scanner.Buffer(make([]byte, maxSSELine), maxSSELine)

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
			// The terminal usage chunk carries the final cumulative totals; a
			// later chunk (if any) overwrites, which is correct.
			inputTokens = int64(chunk.Usage.PromptTokens)
			outputTokens = int64(chunk.Usage.CompletionTokens)
		}
	}
	return inputTokens, outputTokens
}

// extractCopilotUsage reads token usage from a Copilot completion response body,
// handling both streaming (SSE) and non-streaming (single JSON) responses.
// contentType is the response Content-Type header. Returns (0, 0) when no usage
// is present.
func extractCopilotUsage(contentType string, body []byte) (inputTokens, outputTokens int64) {
	if strings.Contains(contentType, "text/event-stream") {
		return extractCopilotSSEUsage(body)
	}
	return extractOpenAIUsage(body)
}

// extractCopilotResponseModel recovers the concrete model id from a Copilot
// completion response (streaming or not). Used when the request pinned "auto"
// so usage is attributed to the actual model Copilot chose. Returns "" when no
// model is present.
func extractCopilotResponseModel(body []byte, contentType string) string {
	if strings.Contains(contentType, "text/event-stream") {
		scanner := bufio.NewScanner(bytes.NewReader(body))
		const maxSSELine = 256 * 1024
		scanner.Buffer(make([]byte, maxSSELine), maxSSELine)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := line[len("data: "):]
			if data == "[DONE]" {
				break
			}
			var m copilotResponseModel
			if json.Unmarshal([]byte(data), &m) == nil && m.Model != "" {
				return m.Model
			}
		}
		return ""
	}
	var m copilotResponseModel
	if json.Unmarshal(body, &m) == nil {
		return m.Model
	}
	return ""
}
