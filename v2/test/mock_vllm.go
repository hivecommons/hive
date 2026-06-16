// mock_vllm.go — a minimal OpenAI Chat Completions API server for testing
// the Anthropic ↔ OpenAI proxy translation layer.
//
// Usage: go run test/mock_vllm.go [-port 8000] [-stream]
//
// Responds to POST /v1/chat/completions with either a non-streaming JSON
// response or an SSE stream, depending on the request's "stream" field.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

func main() {
	port := flag.Int("port", 8000, "listen port")
	flag.Parse()

	http.HandleFunc("/v1/chat/completions", handleCompletions)
	http.HandleFunc("/v1/models", handleModels)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	})

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("[mock-vllm] listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"data": []map[string]string{
			{"id": "Qwen/Qwen2.5-1.5B-Instruct", "object": "model"},
		},
	})
}

func handleCompletions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Stream    bool `json:"stream"`
		MaxTokens int  `json:"max_tokens"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	log.Printf("[mock-vllm] model=%s stream=%v messages=%d", req.Model, req.Stream, len(req.Messages))
	for i, m := range req.Messages {
		preview := m.Content
		const maxPreview = 100
		if len(preview) > maxPreview {
			preview = preview[:maxPreview] + "..."
		}
		log.Printf("  msg[%d] role=%s: %s", i, m.Role, preview)
	}

	responseText := fmt.Sprintf("Hello from mock vLLM! Model: %s. I received %d messages. "+
		"The last message was from '%s'.",
		req.Model, len(req.Messages),
		req.Messages[len(req.Messages)-1].Role)

	if req.Stream {
		handleStreamingResponse(w, req.Model, responseText)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":      "chatcmpl-mock-001",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]string{
					"role":    "assistant",
					"content": responseText,
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]int{
			"prompt_tokens":     42,
			"completion_tokens": 20,
			"total_tokens":      62,
		},
	})
}

func handleStreamingResponse(w http.ResponseWriter, model, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	words := strings.Fields(text)
	const chunkDelayMs = 50

	for i, word := range words {
		content := word
		if i < len(words)-1 {
			content += " "
		}

		chunk := map[string]interface{}{
			"id":      "chatcmpl-mock-001",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"delta": map[string]string{
						"content": content,
					},
					"finish_reason": nil,
				},
			},
		}

		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
		time.Sleep(chunkDelayMs * time.Millisecond)
	}

	// Final chunk with finish_reason
	finalChunk := map[string]interface{}{
		"id":      "chatcmpl-mock-001",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]string{},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]int{
			"prompt_tokens":     42,
			"completion_tokens": len(words),
			"total_tokens":      42 + len(words),
		},
	}
	data, _ := json.Marshal(finalChunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}
