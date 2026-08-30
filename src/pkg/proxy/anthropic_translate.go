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
// maxContextLen is the model's max context window (0 = no cap).
// preamble, when non-empty, is prepended to the system message to steer
// open-source models toward agentic tool-calling behaviour. Pass "" to
// leave the system prompt unchanged (used by tests and non-inference paths).
func translateAnthropicToOpenAI(body []byte, targetModel string, maxContextLen int, preamble string) ([]byte, error) {
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("unmarshal anthropic request: %w", err)
	}

	openaiReq := openaiRequest{
		Model:  targetModel,
		Stream: req.Stream,
	}
	if req.Stream {
		// Request the terminal usage chunk so streamed inference responses
		// carry token counts we can attribute to the agent.
		openaiReq.StreamOptions = &openaiStreamOptions{IncludeUsage: true}
	}

	var totalChars int

	if len(req.System) > 0 {
		systemText := extractSystemText(req.System)
		if systemText != "" {
			if preamble != "" {
				systemText = preamble + systemText
			}
			totalChars += len(systemText)
			openaiReq.Messages = append(openaiReq.Messages, openaiMessage{
				Role:    "system",
				Content: systemText,
			})
		}
	} else if preamble != "" {
		// No system prompt from the CLI — inject the preamble as the system message.
		totalChars += len(preamble)
		openaiReq.Messages = append(openaiReq.Messages, openaiMessage{
			Role:    "system",
			Content: preamble,
		})
	}

	for _, msg := range req.Messages {
		translated := translateAnthropicMessage(msg)
		for _, tm := range translated {
			totalChars += len(tm.Content)
			if tm.ToolCallID != "" {
				totalChars += len(tm.ToolCallID)
			}
			for _, tc := range tm.ToolCalls {
				totalChars += len(tc.Function.Arguments)
			}
		}
		openaiReq.Messages = append(openaiReq.Messages, translated...)
	}

	if len(req.Tools) > 0 {
		openaiReq.Tools = translateAnthropicTools(req.Tools)
		openaiReq.ToolChoice = translateAnthropicToolChoice(req.ToolChoice)
	} else if preamble != "" {
		// CLI sent no tools (common with Claude CLI in bare mode).
		// Inject default tools so the model can make structured tool calls
		// that the CLI knows how to execute.
		openaiReq.Tools = DefaultInferenceTools
		autoJSON, _ := json.Marshal("auto")
		openaiReq.ToolChoice = autoJSON
	}

	openaiReq.MaxTokens = capMaxTokensForInput(req.MaxTokens, maxContextLen, totalChars)

	return json.Marshal(openaiReq)
}

// translateAnthropicMessage converts a single Anthropic message into one or
// more OpenAI messages. An Anthropic assistant message can contain both text
// and tool_use blocks; a user message can contain tool_result blocks.
func translateAnthropicMessage(msg anthropicMessage) []openaiMessage {
	var str string
	if json.Unmarshal(msg.Content, &str) == nil {
		return []openaiMessage{{Role: msg.Role, Content: str}}
	}

	var blocks []anthropicContentBlockRaw
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return []openaiMessage{{Role: msg.Role, Content: string(msg.Content)}}
	}

	if msg.Role == "assistant" {
		return translateAssistantBlocks(blocks)
	}

	var toolResults []openaiMessage
	var textParts []string

	for _, block := range blocks {
		switch block.Type {
		case "tool_result":
			content := ""
			if block.Content != nil {
				var s string
				if json.Unmarshal(block.Content, &s) == nil {
					content = s
				} else {
					var innerBlocks []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					}
					if json.Unmarshal(block.Content, &innerBlocks) == nil {
						var b strings.Builder
						for _, ib := range innerBlocks {
							if ib.Type == "text" {
								b.WriteString(ib.Text)
							}
						}
						content = b.String()
					} else {
						content = string(block.Content)
					}
				}
			}
			toolResults = append(toolResults, openaiMessage{
				Role:       "tool",
				Content:    content,
				ToolCallID: block.ToolUseID,
			})
		case "text":
			if block.Text != "" {
				textParts = append(textParts, block.Text)
			}
		}
	}

	var msgs []openaiMessage
	if len(textParts) > 0 {
		msgs = append(msgs, openaiMessage{
			Role:    "user",
			Content: strings.Join(textParts, "\n"),
		})
	}
	msgs = append(msgs, toolResults...)
	return msgs
}

// translateAssistantBlocks converts an assistant message's content blocks
// (text + tool_use) into a single OpenAI assistant message with tool_calls.
func translateAssistantBlocks(blocks []anthropicContentBlockRaw) []openaiMessage {
	var text strings.Builder
	var toolCalls []openaiToolCall

	for _, block := range blocks {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			args, _ := json.Marshal(block.Input)
			toolCalls = append(toolCalls, openaiToolCall{
				ID:   block.ID,
				Type: "function",
				Function: openaiFunction{
					Name:      block.Name,
					Arguments: string(args),
				},
			})
		}
	}

	msg := openaiMessage{
		Role:    "assistant",
		Content: text.String(),
	}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}
	return []openaiMessage{msg}
}

// translateAnthropicTools converts Anthropic tool definitions to OpenAI format.
func translateAnthropicTools(tools []anthropicTool) []openaiTool {
	result := make([]openaiTool, 0, len(tools))
	for _, t := range tools {
		result = append(result, openaiTool{
			Type: "function",
			Function: openaiToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}
	return result
}

// translateAnthropicToolChoice maps Anthropic tool_choice to OpenAI format.
// Anthropic: {"type":"auto"}, {"type":"any"}, {"type":"tool","name":"X"}
// OpenAI:    "auto",           "required",    {"type":"function","function":{"name":"X"}}
func translateAnthropicToolChoice(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		autoJSON, _ := json.Marshal("auto")
		return autoJSON
	}

	var choice anthropicToolChoice
	if err := json.Unmarshal(raw, &choice); err != nil {
		autoJSON, _ := json.Marshal("auto")
		return autoJSON
	}

	switch choice.Type {
	case "any":
		result, _ := json.Marshal("required")
		return result
	case "tool":
		result, _ := json.Marshal(openaiToolChoiceNamed{
			Type:     "function",
			Function: openaiToolChoiceFunction{Name: choice.Name},
		})
		return result
	default:
		result, _ := json.Marshal("auto")
		return result
	}
}

// extractOpenAIUsage pulls the prompt/completion token counts from an upstream
// OpenAI (non-streaming) chat completion response body. Returns (0, 0) when no
// usage block is present. Used to attribute inference-agent token consumption
// into the token store.
func extractOpenAIUsage(body []byte) (inputTokens, outputTokens int64) {
	var resp openaiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, 0
	}
	if resp.Usage == nil {
		return 0, 0
	}
	return int64(resp.Usage.PromptTokens), int64(resp.Usage.CompletionTokens)
}

// translateOpenAIResponseToAnthropic converts a non-streaming OpenAI Chat
// Completions response into an Anthropic Messages response.
func translateOpenAIResponseToAnthropic(body []byte, model string) ([]byte, error) {
	var resp openaiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal openai response: %w", err)
	}

	anthropicResp := anthropicResponse{
		ID:    resp.ID,
		Type:  "message",
		Role:  "assistant",
		Model: model,
	}

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		anthropicResp.StopReason = mapFinishReason(choice.FinishReason)

		if choice.Message.Content != "" {
			anthropicResp.Content = append(anthropicResp.Content, anthropicContentBlock{
				Type: "text",
				Text: choice.Message.Content,
			})
		}

		for _, tc := range choice.Message.ToolCalls {
			var input json.RawMessage
			if tc.Function.Arguments != "" {
				input = json.RawMessage(tc.Function.Arguments)
			} else {
				input = json.RawMessage("{}")
			}
			anthropicResp.Content = append(anthropicResp.Content, anthropicContentBlock{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: input,
			})
		}

		if len(anthropicResp.Content) == 0 {
			anthropicResp.Content = []anthropicContentBlock{{Type: "text", Text: ""}}
		}
	} else {
		anthropicResp.StopReason = "end_turn"
		anthropicResp.Content = []anthropicContentBlock{{Type: "text", Text: ""}}
	}

	if resp.Usage != nil {
		anthropicResp.Usage = &anthropicUsage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		}
	} else {
		anthropicResp.Usage = &anthropicUsage{}
	}

	return json.Marshal(anthropicResp)
}

// translateOpenAISSEToAnthropic reads an OpenAI SSE stream and writes the
// equivalent Anthropic SSE events to the writer. The caller must close the
// reader when done. It returns the token usage reported in the stream's
// terminal usage chunk (present when the outbound request set
// stream_options.include_usage). Returns (0, 0) when the gateway did not emit
// usage.
func translateOpenAISSEToAnthropic(r io.Reader, w io.Writer, model string) (inputTokens, outputTokens int64, err error) {
	msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())

	writeSSE(w, "message_start", anthropicSSEMessageStart{
		Type: "message_start",
		Message: anthropicSSEMessage{
			ID:      msgID,
			Type:    "message",
			Role:    "assistant",
			Model:   model,
			Content: []anthropicContentBlock{},
			Usage:   &anthropicUsage{},
		},
	})

	scanner := bufio.NewScanner(r)
	const maxSSELine = 256 * 1024
	scanner.Buffer(make([]byte, maxSSELine), maxSSELine)

	var totalInputTokens int
	var totalOutputTokens int
	var contentBlockIndex int
	textBlockStarted := false
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

		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta

			if delta.Content != "" {
				if !textBlockStarted {
					writeSSE(w, "content_block_start", map[string]interface{}{
						"type":          "content_block_start",
						"index":         contentBlockIndex,
						"content_block": map[string]string{"type": "text", "text": ""},
					})
					textBlockStarted = true
				}
				writeSSE(w, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": contentBlockIndex,
					"delta": map[string]string{
						"type": "text_delta",
						"text": delta.Content,
					},
				})
			}

			for _, tc := range delta.ToolCalls {
				acc, exists := toolCallState[tc.Index]
				if !exists {
					if textBlockStarted {
						writeSSE(w, "content_block_stop", map[string]interface{}{
							"type":  "content_block_stop",
							"index": contentBlockIndex,
						})
						contentBlockIndex++
						textBlockStarted = false
					}

					acc = &sseToolCallAccumulator{
						id:         tc.ID,
						name:       tc.Function.Name,
						blockIndex: contentBlockIndex,
					}
					toolCallState[tc.Index] = acc

					writeSSE(w, "content_block_start", map[string]interface{}{
						"type":  "content_block_start",
						"index": contentBlockIndex,
						"content_block": map[string]interface{}{
							"type":  "tool_use",
							"id":    tc.ID,
							"name":  tc.Function.Name,
							"input": map[string]interface{}{},
						},
					})
					contentBlockIndex++
				}

				if tc.Function.Arguments != "" {
					acc.args.WriteString(tc.Function.Arguments)
					writeSSE(w, "content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": acc.blockIndex,
						"delta": map[string]interface{}{
							"type":         "input_json_delta",
							"partial_json": tc.Function.Arguments,
						},
					})
				}
			}

			if chunk.Choices[0].FinishReason != "" {
				if textBlockStarted {
					writeSSE(w, "content_block_stop", map[string]interface{}{
						"type":  "content_block_stop",
						"index": contentBlockIndex,
					})
				}
				for _, acc := range toolCallState {
					writeSSE(w, "content_block_stop", map[string]interface{}{
						"type":  "content_block_stop",
						"index": acc.blockIndex,
					})
				}

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
			if chunk.Usage.PromptTokens > 0 {
				totalInputTokens = chunk.Usage.PromptTokens
			}
		}
	}

	writeSSE(w, "message_stop", map[string]string{"type": "message_stop"})
	return int64(totalInputTokens), int64(totalOutputTokens), scanner.Err()
}

type sseToolCallAccumulator struct {
	id         string
	name       string
	blockIndex int
	args       strings.Builder
}

func writeSSE(w io.Writer, eventType string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	// Best effort: the client may have disconnected mid-stream; the request
	// was already authorized, so a broken pipe here is not a policy concern.
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, jsonData)
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
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// forwardToInference sends an Anthropic Messages API request to a vLLM/llm-d
// endpoint, translating the request and response formats. It writes the
// translated response directly to the provided http.ResponseWriter.
func forwardToInference(clientReq *http.Request, clientBody []byte, w http.ResponseWriter, route *InferenceRoute, agentName string) error {
	openaiBody, err := translateAnthropicToOpenAI(clientBody, route.Model, route.MaxContextLen, resolveInferencePreamble(route, agentName))
	if err != nil {
		return fmt.Errorf("translate request: %w", err)
	}

	upstreamURL := openAIChatCompletionsURL(route.Endpoint)
	upstreamReq, err := http.NewRequestWithContext(
		clientReq.Context(), "POST", upstreamURL, bytes.NewReader(openaiBody))
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
		anthropicErr := map[string]interface{}{
			"type": "error",
			"error": map[string]interface{}{
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

	isStreaming := resp.Header.Get("Content-Type") == "text/event-stream" ||
		strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

	if isStreaming {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		if f, ok := w.(http.Flusher); ok {
			flushWriter := &flushResponseWriter{w: w, f: f}
			_, _, err := translateOpenAISSEToAnthropic(resp.Body, flushWriter, route.Model)
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

func (fw *flushResponseWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.f.Flush()
	return n, err
}

// --- Request/response types ---

type anthropicRequest struct {
	Model      string             `json:"model"`
	MaxTokens  int                `json:"max_tokens"`
	Messages   []anthropicMessage `json:"messages"`
	System     json.RawMessage    `json:"system,omitempty"`
	Stream     bool               `json:"stream,omitempty"`
	Tools      []anthropicTool    `json:"tools,omitempty"`
	ToolChoice json.RawMessage    `json:"tool_choice,omitempty"`
}

type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicContentBlockRaw is used to parse content blocks that may contain
// text, tool_use, or tool_result entries.
type anthropicContentBlockRaw struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type anthropicResponse struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"`
	Role         string                  `json:"role"`
	Model        string                  `json:"model"`
	Content      []anthropicContentBlock `json:"content"`
	StopReason   string                  `json:"stop_reason"`
	StopSequence *string                 `json:"stop_sequence"`
	Usage        *anthropicUsage         `json:"usage,omitempty"`
}

type anthropicContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type anthropicSSEMessageStart struct {
	Type    string              `json:"type"`
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
	Model         string               `json:"model"`
	Messages      []openaiMessage      `json:"messages"`
	MaxTokens     int                  `json:"max_tokens,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	StreamOptions *openaiStreamOptions `json:"stream_options,omitempty"`
	Tools         []openaiTool         `json:"tools,omitempty"`
	ToolChoice    json.RawMessage      `json:"tool_choice,omitempty"`
}

// openaiStreamOptions asks the gateway to emit a terminal usage chunk on
// streamed responses. Without include_usage most OpenAI-compatible gateways
// omit token counts from SSE responses, so inference-agent streaming usage
// would be uncountable.
type openaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openaiToolChoiceNamed struct {
	Type     string                   `json:"type"`
	Function openaiToolChoiceFunction `json:"function"`
}

type openaiToolChoiceFunction struct {
	Name string `json:"name"`
}

type openaiTool struct {
	Type     string             `json:"type"`
	Function openaiToolFunction `json:"function"`
}

type openaiToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type openaiMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCalls  []openaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openaiToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function openaiFunction `json:"function"`
}

type openaiFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openaiResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Message struct {
			Content   string           `json:"content"`
			ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openaiUsage `json:"usage,omitempty"`
}

type openaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type openaiSSEChunk struct {
	Choices []struct {
		Delta struct {
			Content   string              `json:"content"`
			ToolCalls []openaiSSEToolCall `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openaiUsage `json:"usage,omitempty"`
}

type openaiSSEToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}
