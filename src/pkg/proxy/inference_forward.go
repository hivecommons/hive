package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// forwardToInference sends an Anthropic Messages API request to an
// OpenAI-compatible endpoint, translating the request and response formats.
func forwardToInference(clientReq *http.Request, clientBody []byte, w http.ResponseWriter, route *InferenceRoute, agentName string) error {
	openaiBody, err := translateAnthropicToOpenAI(clientBody, route.Model, route.MaxContextLen, resolveInferencePreamble(route, agentName))
	if err != nil {
		return fmt.Errorf("translate request: %w", err)
	}

	upstreamReq, err := http.NewRequestWithContext(
		clientReq.Context(), http.MethodPost, openAIChatCompletionsURL(route.Endpoint), bytes.NewReader(openaiBody))
	if err != nil {
		return fmt.Errorf("create upstream request: %w", err)
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	applyInferenceAuth(upstreamReq, route)

	client, err := inferenceHTTPClient(route)
	if err != nil {
		return fmt.Errorf("inference client setup: %w", err)
	}
	resp, err := client.Do(upstreamReq)
	if err != nil {
		return fmt.Errorf("upstream request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		anthropicErr := map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "api_error",
				"message": fmt.Sprintf("inference backend returned %d: %s", resp.StatusCode, string(errBody)),
			},
		}
		errJSON, _ := json.Marshal(anthropicErr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, writeErr := w.Write(errJSON)
		return writeErr
	}

	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			_, _, err := translateOpenAISSEToAnthropic(resp.Body, flushResponseWriter{w: w, f: f}, route.Model)
			return err
		}
		_, _, err := translateOpenAISSEToAnthropic(resp.Body, w, route.Model)
		return err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read upstream response: %w", err)
	}
	translated, err := translateOpenAIResponseToAnthropic(body, route.Model)
	if err != nil {
		return fmt.Errorf("translate response: %w", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(translated)
	return err
}

func openAIChatCompletionsURL(endpoint string) string {
	base := strings.TrimRight(endpoint, "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

type flushResponseWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw flushResponseWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.f.Flush()
	return n, err
}
