package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// InferenceRoute defines where to send an agent's API traffic when using
// a self-hosted inference backend instead of the cloud API.
type InferenceRoute struct {
	Backend       string // "vllm" or "llm-d"
	Endpoint      string // e.g. "http://vllm-svc.hive.svc.cluster.local:8000"
	Model         string // e.g. "Qwen/Qwen2.5-1.5B-Instruct"
	MaxContextLen int    // from vLLM /v1/models max_model_len; 0 = unknown
}

// inferenceRouter manages per-agent inference backend routing.
type inferenceRouter struct {
	mu     sync.RWMutex
	routes map[string]*InferenceRoute // agent name → route
}

func newInferenceRouter() *inferenceRouter {
	return &inferenceRouter{
		routes: make(map[string]*InferenceRoute),
	}
}

func (ir *inferenceRouter) Set(agentName string, route *InferenceRoute) {
	ir.mu.Lock()
	defer ir.mu.Unlock()
	ir.routes[agentName] = route
}

func (ir *inferenceRouter) Clear(agentName string) {
	ir.mu.Lock()
	defer ir.mu.Unlock()
	delete(ir.routes, agentName)
}

func (ir *inferenceRouter) Get(agentName string) *InferenceRoute {
	ir.mu.RLock()
	defer ir.mu.RUnlock()
	return ir.routes[agentName]
}

// anthropicHosts are hosts that should be intercepted when an agent has
// an inference route configured.
var anthropicHosts = map[string]bool{
	"api.anthropic.com": true,
}

// IsAnthropicHost returns true if the host is an Anthropic API endpoint.
func IsAnthropicHost(host string) bool {
	return anthropicHosts[host]
}

// InferenceBackends lists the valid self-hosted inference backend IDs.
var InferenceBackends = []string{"vllm", "llm-d"}

// IsInferenceBackend returns true if the backend name is a self-hosted
// inference backend rather than a CLI tool.
func IsInferenceBackend(backend string) bool {
	for _, b := range InferenceBackends {
		if b == backend {
			return true
		}
	}
	return false
}

const maxModelLenQueryTimeout = 3 * time.Second

// queryMaxModelLen queries the /v1/models endpoint and returns the
// max_model_len for the given model. Returns 0 if the query fails.
func queryMaxModelLen(endpoint, model string) int {
	url := strings.TrimRight(endpoint, "/") + "/v1/models"
	client := &http.Client{Timeout: maxModelLenQueryTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0
	}
	var result struct {
		Data []struct {
			ID          string `json:"id"`
			MaxModelLen int    `json:"max_model_len"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0
	}
	for _, m := range result.Data {
		if m.ID == model {
			return m.MaxModelLen
		}
	}
	if len(result.Data) > 0 {
		return result.Data[0].MaxModelLen
	}
	return 0
}

// capMaxTokens adjusts max_tokens to fit within the model's context window.
// It reserves space for input tokens by capping output to contextLen - inputEstimate.
func capMaxTokens(maxTokens, maxContextLen int) int {
	if maxContextLen <= 0 || maxTokens <= 0 {
		return maxTokens
	}
	inputReserve := maxContextLen / 4
	limit := maxContextLen - inputReserve
	if limit < 1 {
		limit = 1
	}
	if maxTokens > limit {
		return limit
	}
	return maxTokens
}

