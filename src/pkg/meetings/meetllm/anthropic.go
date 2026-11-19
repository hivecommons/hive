package meetllm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Anthropic Messages API (hivecommons/hive#18393): the meeting persona's
// answer path on hives configured with only an Anthropic key.
const (
	// ProviderAnthropic selects the Anthropic Messages API.
	ProviderAnthropic = "anthropic"
	// AnthropicEndpoint is the default API root.
	AnthropicEndpoint = "https://api.anthropic.com"
	// anthropicVersion is the anthropic-version header value.
	anthropicVersion = "2023-06-01"
	// stopRefusal is the stop reason of a declined request.
	stopRefusal = "refusal"
	// noThinkingModelPrefix marks the model family that takes thinking
	// disabled: replies must arrive within the persona's 2.5 to 4 second
	// budget, and Haiku 5.x thinks by default.
	noThinkingModelPrefix = "claude-haiku-5"
)

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []chatMessage      `json:"messages"`
	Thinking  *anthropicThinking `json:"thinking,omitempty"`
}

type anthropicThinking struct {
	Type string `json:"type"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func anthropicURL(endpoint string) string {
	if endpoint == "" {
		endpoint = AnthropicEndpoint
	}
	endpoint = strings.TrimRight(endpoint, "/")
	switch {
	case strings.HasSuffix(endpoint, "/v1/messages"):
		return endpoint
	case strings.HasSuffix(endpoint, "/v1"):
		return endpoint + "/messages"
	}
	return endpoint + "/v1/messages"
}

// completeAnthropic makes one Messages API call and records its usage; the
// caller holds the budget check, timeout and slot.
func completeAnthropic(ctx context.Context, c Config, hc *http.Client, prompt string) (string, error) {
	if c.APIKey == "" || c.Model == "" {
		return "", errors.New("meeting-llm: the Anthropic path needs a key and a model")
	}
	ar := anthropicRequest{
		Model: c.Model, MaxTokens: maxReplyTokens, System: strings.TrimSpace(c.System),
		Messages: []chatMessage{{Role: "user", Content: prompt}},
	}
	if strings.HasPrefix(c.Model, noThinkingModelPrefix) {
		ar.Thinking = &anthropicThinking{Type: "disabled"}
	}
	body, _ := json.Marshal(ar)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicURL(c.Endpoint), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("meeting-llm: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("meeting-llm: model returned %d", resp.StatusCode)
	}
	var mr anthropicResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&mr); err != nil {
		return "", fmt.Errorf("meeting-llm: decoding response: %w", err)
	}
	if err := c.charge(mr.Usage.InputTokens+mr.Usage.OutputTokens, time.Now()); err != nil {
		return "", fmt.Errorf("meeting-llm: recording usage: %w", err)
	}
	if mr.StopReason == stopRefusal {
		return "", errors.New("meeting-llm: the model declined the request")
	}
	var b strings.Builder
	for _, block := range mr.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	if strings.TrimSpace(b.String()) == "" {
		return "", errors.New("meeting-llm: model returned no text")
	}
	return b.String(), nil
}
