package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/hivecommons/hive/crux/config"
)

// forwardToInference sends an Anthropic Messages API request to an
// OpenAI-compatible endpoint, translating the request and response formats.
func forwardToInference(clientReq *http.Request, clientBody []byte, w http.ResponseWriter, route *InferenceRoute, agentName string) error {
	openaiBody, _, err := translateAnthropicToOpenAI(clientBody, route.Model, route.MaxContextLen, resolveInferencePreamble(route, agentName))
	if err != nil {
		return fmt.Errorf("translate request: %w", err)
	}

	upstreamURL := config.LiteLLMChatCompletionsURL(route.Endpoint)
	upstreamReq, err := http.NewRequestWithContext(
		clientReq.Context(), http.MethodPost, upstreamURL, bytes.NewReader(openaiBody))
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
				"message": inferenceBackendErrorMessage(resp.StatusCode, errBody, upstreamURL, resp.Header.Get("Server")),
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
