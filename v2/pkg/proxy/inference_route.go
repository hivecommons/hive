package proxy

import "sync"

// InferenceRoute defines where to send an agent's API traffic when using
// a self-hosted inference backend instead of the cloud API.
type InferenceRoute struct {
	Backend  string // "vllm" or "llm-d"
	Endpoint string // e.g. "http://vllm-svc.hive.svc.cluster.local:8000"
	Model    string // e.g. "Qwen/Qwen2.5-1.5B-Instruct"
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
