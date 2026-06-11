package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// translateAnthropicToOpenAI converts an Anthropic Messages API request body
// into an OpenAI Chat Completions API request body, overriding the model.
func translateAnthropicToOpenAI(body []byte, targetModel string) ([]byte, error) {
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("unmarshal anthropic request: %w", err)
	}

	openaiReq := openaiRequest{
		Model:     targetModel,
		MaxTokens: req.MaxTokens,
		Stream:    req.Stream,
	}

	if len(req.System) > 0 {
		systemText := extractSystemText(req.System)
		if systemText != "" {
			openaiReq.Messages = append(openaiReq.Messages, openaiMessage{
				Role:    "system",
				Content: systemText,
			})
		}
	}

	for _, msg := range req.Messages {
		text := extractTextFromContent(msg.Content)
		openaiReq.Messages = append(openaiReq.Messages, openaiMessage{
			Role:    msg.Role,
			Content: text,
		})
	}

	return json.Marshal(openaiReq)
}

// translateOpenAIResponseToAnthropic converts a non-streaming OpenAI Chat
// Completions response into an Anthropic Messages response.
func translateOpenAIResponseToAnthropic(body []byte, model string) ([]byte, error) {
	var resp openaiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal openai response: %w", err)
	}

	anthropicResp := anthropicResponse{
		ID:         resp.ID,
		Type:       "message",
		Role:       "assistant",
		Model:      model,
		StopReason: mapFinishReason(resp.Choices[0].FinishReason),
	}

	if len(resp.Choices) > 0 {
		anthropicResp.Content = []anthropicContentBlock{{
			Type: "text",
			Text: resp.Choices[0].Message.Content,
		}}
	}

	if resp.Usage != nil {
		anthropicResp.Usage = &anthropicUsage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		}
	}

	return json.Marshal(anthropicResp)
}

// translateOpenAISSEToAnthropic reads an OpenAI SSE stream and writes the
// equivalent Anthropic SSE events to the writer. The caller must close the
// reader when done.
func translateOpenAISSEToAnthropic(r io.Reader, w io.Writer, model string) error {
	msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())

	writeSSE(w, "message_start", anthropicSSEMessageStart{
		Type: "message_start",
		Message: anthropicSSEMessage{
			ID:    msgID,
			Type:  "message",
			Role:  "assistant",
			Model: model,
			Content: []anthropicContentBlock{},
			Usage: &anthropicUsage{InputTokens: 0, OutputTokens: 0},
		},
	})

	writeSSE(w, "content_block_start", map[string]interface{}{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]string{"type": "text", "text": ""},
	})

	scanner := bufio.NewScanner(r)
	const maxSSELine = 256 * 1024
	scanner.Buffer(make([]byte, maxSSELine), maxSSELine)

	var totalOutputTokens int

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

		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta
			if delta.Content != "" {
				writeSSE(w, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": 0,
					"delta": map[string]string{
						"type": "text_delta",
						"text": delta.Content,
					},
				})
			}

			if chunk.Choices[0].FinishReason != "" {
				writeSSE(w, "content_block_stop", map[string]interface{}{
					"type":  "content_block_stop",
					"index": 0,
				})

				writeSSE(w, "message_delta", map[string]interface{}{
					"type": "message_delta",
					"delta": map[string]string{
						"stop_reason": mapFinishReason(chunk.Choices[0].FinishReason),
					},
					"usage": map[string]int{
						"output_tokens": totalOutputTokens,
					},
				})
			}
		}

		if chunk.Usage != nil {
			totalOutputTokens = chunk.Usage.CompletionTokens
		}
	}

	writeSSE(w, "message_stop", map[string]string{"type": "message_stop"})
	return scanner.Err()
}

func writeSSE(w io.Writer, eventType string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, jsonData)
}

func extractSystemText(raw json.RawMessage) string {
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, block := range blocks {
			if block.Type == "text" {
				b.WriteString(block.Text)
			}
		}
		return b.String()
	}
	return ""
}

func extractTextFromContent(content json.RawMessage) string {
	var str string
	if json.Unmarshal(content, &str) == nil {
		return str
	}

	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) == nil {
		var b strings.Builder
		for _, block := range blocks {
			if block.Type == "text" {
				b.WriteString(block.Text)
			}
		}
		return b.String()
	}
	return string(content)
}

func mapFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

// forwardToInference sends an Anthropic Messages API request to a vLLM/llm-d
// endpoint, translating the request and response formats. It writes the
// translated response directly to the provided http.ResponseWriter.
func forwardToInference(clientReq *http.Request, clientBody []byte, w http.ResponseWriter, route *InferenceRoute) error {
	openaiBody, err := translateAnthropicToOpenAI(clientBody, route.Model)
	if err != nil {
		return fmt.Errorf("translate request: %w", err)
	}

	upstreamURL := strings.TrimRight(route.Endpoint, "/") + "/v1/chat/completions"
	upstreamReq, err := http.NewRequestWithContext(
		clientReq.Context(), "POST", upstreamURL, bytes.NewReader(openaiBody))
	if err != nil {
		return fmt.Errorf("create upstream request: %w", err)
	}
	upstreamReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(upstreamReq)
	if err != nil {
		return fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()

	isStreaming := resp.Header.Get("Content-Type") == "text/event-stream" ||
		strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

	if isStreaming {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		if f, ok := w.(http.Flusher); ok {
			flushWriter := &flushResponseWriter{w: w, f: f}
			return translateOpenAISSEToAnthropic(resp.Body, flushWriter, route.Model)
		}
		return translateOpenAISSEToAnthropic(resp.Body, w, route.Model)
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

type flushResponseWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw *flushResponseWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.f.Flush()
	return n, err
}

// --- Request/response types ---

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Messages  []anthropicMessage `json:"messages"`
	System    json.RawMessage    `json:"system,omitempty"`
	Stream    bool               `json:"stream,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicResponse struct {
	ID         string                  `json:"id"`
	Type       string                  `json:"type"`
	Role       string                  `json:"role"`
	Model      string                  `json:"model"`
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
	Usage      *anthropicUsage         `json:"usage,omitempty"`
}

type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type anthropicSSEMessageStart struct {
	Type    string            `json:"type"`
	Message anthropicSSEMessage `json:"message"`
}

type anthropicSSEMessage struct {
	ID      string                  `json:"id"`
	Type    string                  `json:"type"`
	Role    string                  `json:"role"`
	Model   string                  `json:"model"`
	Content []anthropicContentBlock `json:"content"`
	Usage   *anthropicUsage         `json:"usage,omitempty"`
}

type openaiRequest struct {
	Model     string          `json:"model"`
	Messages  []openaiMessage `json:"messages"`
	MaxTokens int             `json:"max_tokens,omitempty"`
	Stream    bool            `json:"stream,omitempty"`
}

type openaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openaiUsage `json:"usage,omitempty"`
}

type openaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type openaiSSEChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openaiUsage `json:"usage,omitempty"`
}
