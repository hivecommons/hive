package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// inferencePathKind classifies a request the Claude Code CLI sends to what it
// believes is api.anthropic.com (either the MITM'd host or the local
// ANTHROPIC_BASE_URL translator). Only the Messages endpoint is an inference
// call; everything else is CLI housekeeping that must never reach the
// OpenAI-compatible backend.
type inferencePathKind int

const (
	// inferencePathMessages is POST /v1/messages — the only path that is
	// translated and forwarded to the inference backend.
	inferencePathMessages inferencePathKind = iota
	// inferencePathCountTokens is POST /v1/messages/count_tokens, answered
	// locally with an estimate.
	inferencePathCountTokens
	// inferencePathTelemetry is anything under /api/ — event-logging batches,
	// error reports, feedback — answered locally with an empty object.
	inferencePathTelemetry
	// inferencePathUnknown is any other method/path, answered locally 404.
	inferencePathUnknown
)

// classifyInferencePath decides how the inference reroute handles a request.
// A query string on the Messages path (e.g. ?beta=true) is fine: callers pass
// the URL path, not the raw request URI. GET /v1/messages is not an inference
// call.
func classifyInferencePath(method, path string) inferencePathKind {
	path = strings.TrimRight(path, "/")
	if method == http.MethodPost {
		switch path {
		case "/v1/messages":
			return inferencePathMessages
		case "/v1/messages/count_tokens":
			return inferencePathCountTokens
		}
	}
	if strings.HasPrefix(path, "/api/") {
		return inferencePathTelemetry
	}
	return inferencePathUnknown
}

// estimateAnthropicInputTokens estimates the prompt size of an Anthropic
// Messages-shaped request body using the same chars-per-token heuristic the
// translator uses to cap max_tokens, so count_tokens answers and the
// context-window guard agree with each other. The count is over the serialized
// system, messages, and tools fields — a deliberate over-estimate (JSON framing
// counts too) so the CLI compacts a little early rather than a little late.
// Always returns at least 1: the CLI treats 0 as "no context used".
func estimateAnthropicInputTokens(body []byte) int {
	var req struct {
		System   json.RawMessage `json:"system"`
		Messages json.RawMessage `json:"messages"`
		Tools    json.RawMessage `json:"tools"`
	}
	chars := 0
	if err := json.Unmarshal(body, &req); err == nil {
		chars = len(req.System) + len(req.Messages) + len(req.Tools)
	} else {
		chars = len(body)
	}
	n := chars / charsPerToken
	if n < 1 {
		n = 1
	}
	return n
}

// localInferenceResponse builds the answer for a request the reroute handles
// itself instead of forwarding. It is only valid for kinds other than
// inferencePathMessages. The body is always JSON.
func (p *GitHubProxy) localInferenceResponse(kind inferencePathKind, method, path string, body []byte, agentName string) (status int, respBody string) {
	switch kind {
	case inferencePathCountTokens:
		return http.StatusOK, fmt.Sprintf(`{"input_tokens":%d}`, estimateAnthropicInputTokens(body))
	case inferencePathTelemetry:
		// Once per path: the whole point is to keep these off the gateway's
		// request budget, not to move the noise into the hive log.
		if _, seen := p.localInferencePaths.LoadOrStore(path, struct{}{}); !seen {
			p.logger.Debug("inference reroute: answering CLI telemetry locally", "agent", agentName, "method", method, "path", path)
		}
		return http.StatusOK, `{}`
	default:
		// A new CLI endpoint should surface here, in the hive log, rather
		// than as an opaque 400 from the inference gateway.
		p.logger.Warn("inference reroute: unknown Anthropic API path not forwarded", "agent", agentName, "method", method, "path", path)
		return http.StatusNotFound, fmt.Sprintf(
			`{"type":"error","error":{"type":"not_found_error","message":"%s"}}`,
			jsonEscape(fmt.Sprintf("hive inference reroute: %s %s is not an inference endpoint and was not forwarded", method, path)))
	}
}

// writeLocalInferenceResponse writes a locally-generated response to a raw
// MITM connection (the CONNECT reroute path).
func writeLocalInferenceResponse(conn net.Conn, status int, body string) {
	resp := &http.Response{
		StatusCode:    status,
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
	}
	// Best effort: conn is a raw MITM connection and may already be gone;
	// the policy decision was already made before this write.
	_ = resp.Write(conn)
}
