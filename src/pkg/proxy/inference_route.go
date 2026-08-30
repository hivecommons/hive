package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/pkg/config"
)

// InferenceRoute defines where to send an agent's API traffic when using
// a self-hosted inference backend instead of the cloud API.
type InferenceRoute struct {
	Backend       string // "vllm", "llm-d", or "litellm"
	Endpoint      string // e.g. "http://vllm-svc.hive.svc.cluster.local:8000"
	Model         string // e.g. "Qwen/Qwen2.5-1.5B-Instruct"
	MaxContextLen int    // from vLLM /v1/models max_model_len; 0 = unknown
	Preamble      string // prepended to the system prompt for inference backends; empty = use DefaultInferencePreamble
	APIKey        string // optional bearer token for the upstream (litellm requires auth); never logged
	CABundle      string // optional PEM path for a private CA; verification is never disabled
	// ExtraHeaders are additional headers set on every upstream request for
	// this route. Used for watsonx, which needs X-IBM-Project-ID (an OpenAI
	// client would not send it). Empty for all other backends. Header VALUES
	// here are identifiers, not secrets (the bearer stays in APIKey).
	ExtraHeaders map[string]string
}

// DefaultInferencePreamble is injected at the top of the system prompt when
// requests are routed to a self-hosted inference backend (vLLM / llm-d).
// It makes open-source models behave more agentically — calling tools
// proactively, iterating autonomously, and never asking for permission.
// CLI backends (claude, copilot, goose, etc.) are unaffected.
const DefaultInferencePreamble = `You are an autonomous AI coding agent. You have access to tools (Bash, Read, Write/Edit, etc.) and MUST use them to accomplish every task.

CRITICAL RULES:
- ALWAYS call tools to gather information and take action. Never describe what you would do — actually do it.
- NEVER ask for permission or clarification. Execute immediately with the information available.
- After each tool result, decide your next action and call the next tool. Continue until the task is fully complete.
- If a tool call fails, try an alternative approach. Do not stop or ask the user.
- Produce concrete artifacts (files, commands, analysis) — not plans or proposals.
- When you have completed the task, summarize what you did.

ENVIRONMENT:
- The target repository must be cloned first: use "gh repo clone $HIVE_REPO" before analyzing code.
- Go is at /usr/local/go/bin/go (already on PATH).
- Use "bd create --title '...' --type advisory --priority 0-4 --actor $HIVE_PROXY_AGENT" to record findings as beads.
- Use "gh" for all GitHub operations (issues, PRs, repo access). Authentication is pre-configured.

`

// SupervisorInferencePreamble extends the default preamble with supervisor-only
// tooling (hive-panes). Applied only when the agent name is "supervisor".
const SupervisorInferencePreamble = DefaultInferencePreamble + `SUPERVISOR TOOLS:
- Run "hive-panes" to see what ALL other agents are doing (their tmux pane output). Use this to monitor agent health and detect stuck agents. It automatically excludes your own session.
- Run "hive-panes 50" to see the last 50 lines per agent (default is 30).

`

// DefaultInferenceTools are injected into inference-backed requests when the
// CLI sends no tools (Claude CLI in bare mode omits tool definitions).
// These match the built-in Claude Code tools that the CLI knows how to execute.
var DefaultInferenceTools = []openaiTool{
	{
		Type: "function",
		Function: openaiToolFunction{
			Name:        "Bash",
			Description: "Run a bash command in the user's shell. Use for system commands, file operations, builds, tests, and any task requiring shell access. Commands run in the agent's working directory.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"The bash command to execute"},"timeout":{"type":"integer","description":"Optional timeout in milliseconds (max 600000)"}},"required":["command"]}`),
		},
	},
	{
		Type: "function",
		Function: openaiToolFunction{
			Name:        "Read",
			Description: "Read the contents of a file. Use to examine source code, config files, logs, or any text file. Returns the file content with line numbers. For large files, use offset and limit to read specific sections.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string","description":"Absolute path to the file to read"},"offset":{"type":"integer","description":"Line number to start reading from (0-indexed)"},"limit":{"type":"integer","description":"Maximum number of lines to read"}},"required":["file_path"]}`),
		},
	},
	{
		Type: "function",
		Function: openaiToolFunction{
			Name:        "Write",
			Description: "Write content to a file. Creates the file if it doesn't exist, or overwrites it if it does. Use for creating new files or completely rewriting existing ones. For partial edits, prefer the Edit tool.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string","description":"Absolute path to the file to write"},"content":{"type":"string","description":"The full content to write to the file"}},"required":["file_path","content"]}`),
		},
	},
	{
		Type: "function",
		Function: openaiToolFunction{
			Name:        "Edit",
			Description: "Make a targeted edit to a file by replacing a specific string with new content. The old_string must match exactly one location in the file. Include enough surrounding context to make the match unique.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string","description":"Absolute path to the file to edit"},"old_string":{"type":"string","description":"The exact string to find and replace (must match exactly one location)"},"new_string":{"type":"string","description":"The replacement string"}},"required":["file_path","old_string","new_string"]}`),
		},
	},
	{
		Type: "function",
		Function: openaiToolFunction{
			Name:        "Glob",
			Description: "Find files matching a glob pattern. Use to discover files in the codebase. Returns matching file paths.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"Glob pattern to match files (e.g. '**/*.go', 'src/**/*.ts')"},"path":{"type":"string","description":"Base directory for the search (defaults to working directory)"}},"required":["pattern"]}`),
		},
	},
	{
		Type: "function",
		Function: openaiToolFunction{
			Name:        "Grep",
			Description: "Search file contents using a regular expression pattern. Returns matching lines with file paths and line numbers.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"Regular expression pattern to search for"},"path":{"type":"string","description":"Directory or file to search in (defaults to working directory)"},"include":{"type":"string","description":"Glob pattern to filter files (e.g. '*.go', '*.ts')"}},"required":["pattern"]}`),
		},
	},
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

// UpdateMaxContextLen sets the max context length on an agent's stored route,
// but only if the route still points at the same endpoint and model that the
// (asynchronous) query was issued for. This prevents a slow probe from an old
// model switch from clobbering a route the user changed again in the meantime.
// Returns true if the update was applied.
//
// The update is copy-on-write: it stores a NEW *InferenceRoute rather than
// mutating the existing struct in place. Get() hands its caller a bare pointer
// and releases the lock, so any concurrent reader of route.MaxContextLen (e.g.
// the token-capping path in the inference request handlers) would otherwise
// race this write. Replacing the map entry means existing readers keep a
// consistent snapshot and only subsequent Get() calls observe the new value.
func (ir *inferenceRouter) UpdateMaxContextLen(agentName, endpoint, model string, maxLen int) bool {
	ir.mu.Lock()
	defer ir.mu.Unlock()
	route, ok := ir.routes[agentName]
	if !ok || route.Endpoint != endpoint || route.Model != model {
		return false
	}
	updated := *route // shallow copy; InferenceRoute has no reference-type fields that alias mutably
	updated.MaxContextLen = maxLen
	ir.routes[agentName] = &updated
	return true
}

// anthropicHosts are hosts that should be intercepted when an agent has
// an inference route configured.
var anthropicHostsMu sync.RWMutex
var anthropicHosts = map[string]bool{
	"api.anthropic.com": true,
}

// RegisterAnthropicHost adds a custom hostname to the Anthropic intercept map.
func RegisterAnthropicHost(host string) {
	if host == "" {
		return
	}
	anthropicHostsMu.Lock()
	anthropicHosts[host] = true
	anthropicHostsMu.Unlock()
}

// unregisterAnthropicHost removes a hostname from the Anthropic intercept map (test cleanup).
func unregisterAnthropicHost(host string) {
	anthropicHostsMu.Lock()
	delete(anthropicHosts, host)
	anthropicHostsMu.Unlock()
}

// IsAnthropicHost returns true if the host is an Anthropic API endpoint.
func IsAnthropicHost(host string) bool {
	anthropicHostsMu.RLock()
	defer anthropicHostsMu.RUnlock()
	return anthropicHosts[host]
}

// InferenceBackends lists the valid self-hosted inference backend IDs.
// The canonical list lives in the config package (see
// config.InferenceBackends) so agent and proxy share it without an
// import cycle.
var InferenceBackends = config.InferenceBackends

// IsInferenceBackend returns true if the backend name is a self-hosted
// inference backend rather than a CLI tool.
func IsInferenceBackend(backend string) bool {
	return config.IsInferenceBackend(backend)
}

// applyInferenceAuth sets the upstream Authorization header when the route
// has an API key (litellm endpoints require bearer auth; vllm/llm-d
// typically do not).
func applyInferenceAuth(req *http.Request, route *InferenceRoute) {
	if route.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+route.APIKey)
	}
	// Non-secret per-route headers (watsonx X-IBM-Project-ID). Set after auth so
	// a route never overrides the Authorization header via ExtraHeaders.
	for k, v := range route.ExtraHeaders {
		if k == "" || strings.EqualFold(k, "Authorization") || v == "" {
			continue
		}
		req.Header.Set(k, v)
	}
}

// caTransportCache caches one transport per CA bundle path so TLS session
// state and connection pools are reused across requests.
var (
	caTransportMu    sync.Mutex
	caTransportCache = map[string]*http.Transport{}
)

// inferenceTransport returns the transport for the given CA bundle path.
// An empty path returns the default transport (system trust store).
// Certificate verification is NEVER disabled — a private CA is trusted by
// adding its PEM to the root pool, not by skipping verification.
func inferenceTransport(caBundle string) (http.RoundTripper, error) {
	if caBundle == "" {
		return http.DefaultTransport, nil
	}
	caTransportMu.Lock()
	defer caTransportMu.Unlock()
	if t, ok := caTransportCache[caBundle]; ok {
		return t, nil
	}
	pem, err := os.ReadFile(caBundle)
	if err != nil {
		return nil, fmt.Errorf("read ca bundle %s: %w", caBundle, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificates parsed from ca bundle %s", caBundle)
	}
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("default transport is not *http.Transport")
	}
	t = t.Clone()
	t.TLSClientConfig = &tls.Config{RootCAs: pool}
	caTransportCache[caBundle] = t
	return t, nil
}

// inferenceHTTPClient returns the client for forwarding translated requests
// to the route's endpoint. Like http.DefaultClient it has no overall
// timeout — streaming completions can legitimately run for minutes.
func inferenceHTTPClient(route *InferenceRoute) (*http.Client, error) {
	rt, err := inferenceTransport(route.CABundle)
	if err != nil {
		return nil, err
	}
	if rt == http.DefaultTransport {
		return http.DefaultClient, nil
	}
	return &http.Client{Transport: rt}, nil
}

// newModelsRequest builds an authenticated GET {endpoint}/v1/models request.
func newModelsRequest(endpoint, apiKey string) (*http.Request, error) {
	req, err := http.NewRequest("GET", strings.TrimRight(endpoint, "/")+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return req, nil
}

// FindEndpointForModel queries /v1/models on each endpoint and returns the
// first one that serves the requested model. Returns "" if none match.
// apiKey and caBundle are optional (litellm endpoints require bearer auth
// and may be signed by a private CA).
func FindEndpointForModel(endpoints []string, model, apiKey, caBundle string) string {
	rt, err := inferenceTransport(caBundle)
	if err != nil {
		return ""
	}
	client := &http.Client{Timeout: endpointQueryTimeout, Transport: rt}
	for _, ep := range endpoints {
		req, err := newModelsRequest(ep, apiKey)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || err != nil {
			continue
		}
		var result struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &result) != nil {
			continue
		}
		for _, m := range result.Data {
			if m.ID == model {
				return ep
			}
		}
	}
	return ""
}

const endpointQueryTimeout = 3 * time.Second

const maxModelLenQueryTimeout = 3 * time.Second

// queryMaxModelLen queries the /v1/models endpoint and returns the
// max_model_len for the given model. Returns 0 if the query fails.
// apiKey and caBundle are optional (see FindEndpointForModel).
func queryMaxModelLen(endpoint, model, apiKey, caBundle string) int {
	rt, rtErr := inferenceTransport(caBundle)
	if rtErr != nil {
		return 0
	}
	client := &http.Client{Timeout: maxModelLenQueryTimeout, Transport: rt}
	req, err := newModelsRequest(endpoint, apiKey)
	if err != nil {
		return 0
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
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

// charsPerToken is a conservative estimate for tokenization overhead.
// Tokenizers average ~3-4 chars/token, but system prompts, tool schemas,
// and structured content tokenize more densely. Using 2 overestimates
// input token count to avoid exceeding the context window.
const charsPerToken = 2

// safetyMarginPercent is reserved from the context window as headroom
// for tokenizer variance, special tokens, and tool schema overhead that
// may not be fully captured in the character count.
const safetyMarginPercent = 15

// capMaxTokensForInput adjusts max_tokens based on estimated input size.
// totalInputChars is the total character count of all messages + system prompt.
func capMaxTokensForInput(maxTokens, maxContextLen, totalInputChars int) int {
	if maxContextLen <= 0 || maxTokens <= 0 {
		return maxTokens
	}
	safetyMargin := maxContextLen * safetyMarginPercent / 100
	estimatedInputTokens := totalInputChars / charsPerToken
	available := maxContextLen - estimatedInputTokens - safetyMargin
	if available < 1 {
		available = 1
	}
	if maxTokens > available {
		return available
	}
	return maxTokens
}

// resolveInferencePreamble returns the preamble string to inject into the
// system prompt for inference-backed requests. If the route has an explicit
// Preamble it is used; the supervisor agent gets SupervisorInferencePreamble;
// otherwise DefaultInferencePreamble is returned.
func resolveInferencePreamble(route *InferenceRoute, agentName string) string {
	if route == nil {
		return ""
	}
	if route.Preamble != "" {
		return route.Preamble
	}
	// litellm may front frontier models that don't need the vLLM agentic
	// preamble — opt out unless a preamble is explicitly configured.
	if route.Backend == "litellm" {
		return ""
	}
	if agentName == "supervisor" {
		return SupervisorInferencePreamble
	}
	return DefaultInferencePreamble
}
