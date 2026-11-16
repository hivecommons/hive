// Package llmrouter is a turn.LLMClient that drives an ordered list of
// OpenAI-compatible chat-completions providers and fails over between them
// on rate limits, outages, and retired models (RFC: non-CLI backends /
// hive-lite, Family C).
//
// The router holds no conversation state: every call carries the full message
// array from the caller's turn.SessionEnvelope (RFC #4002), so any provider in
// the list can continue any session. Provider availability (cooldowns and
// daily request caps) lives behind the Ledger interface so a stateless
// coordinator can keep it in its own store across passes; MemoryLedger is the
// in-process default.
//
// Configuration never carries a credential. A Provider names the environment
// variable that holds its API key; a provider whose variable is unset is
// skipped, not an error.
package llmrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/toolapprove"
	"github.com/hivecommons/hive/pkg/turn"
)

// Provider is one OpenAI-compatible endpoint and model. Order in the Router's
// list is the failover order.
type Provider struct {
	// Name identifies the provider in the ledger and in attempts, e.g. "groq".
	Name string `json:"name" yaml:"name"`
	// BaseURL is the OpenAI-compatible root, e.g.
	// "https://api.groq.com/openai/v1". "/chat/completions" is appended.
	BaseURL string `json:"base_url" yaml:"base_url"`
	// Model is sent verbatim as the request's model field.
	Model string `json:"model" yaml:"model"`
	// APIKeyEnv names the environment variable holding the bearer token.
	APIKeyEnv string `json:"api_key_env" yaml:"api_key_env"`
	// DailyRequestCap is the coordinator-side request budget per UTC day;
	// zero means no local cap (the provider's 429s still apply).
	DailyRequestCap int `json:"daily_request_cap,omitempty" yaml:"daily_request_cap,omitempty"`
	// Headers are extra non-secret request headers (e.g. OpenRouter's
	// HTTP-Referer / X-Title attribution).
	Headers map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
}

// Validate reports a configuration error for p.
func (p Provider) Validate() error {
	switch {
	case strings.TrimSpace(p.Name) == "":
		return errors.New("llmrouter: provider name is required")
	case !strings.HasPrefix(p.BaseURL, "https://") && !strings.HasPrefix(p.BaseURL, "http://"):
		return fmt.Errorf("llmrouter: provider %s: base_url must be http(s)", p.Name)
	case strings.TrimSpace(p.Model) == "":
		return fmt.Errorf("llmrouter: provider %s: model is required", p.Name)
	case strings.TrimSpace(p.APIKeyEnv) == "":
		return fmt.Errorf("llmrouter: provider %s: api_key_env is required", p.Name)
	case p.DailyRequestCap < 0:
		return fmt.Errorf("llmrouter: provider %s: daily_request_cap must be >= 0", p.Name)
	}
	return nil
}

// Outcome classifies one provider attempt.
type Outcome string

const (
	OutcomeServed      Outcome = "served"
	OutcomeRateLimited Outcome = "rate_limited" // 429
	OutcomeUnavailable Outcome = "unavailable"  // 5xx, 408, transport
	OutcomeAuth        Outcome = "auth"         // 401/403
	OutcomeGone        Outcome = "gone"         // 404/410: model retired
	OutcomeSkipped     Outcome = "skipped"      // cooling down, capped, or no key
	OutcomeRejected    Outcome = "rejected"     // other 4xx: request itself is bad
	OutcomeMalformed   Outcome = "malformed"    // 2xx with an unusable body
)

// Attempt is reported once per provider considered for a Generate call.
type Attempt struct {
	Provider string
	Outcome  Outcome
	Status   int
	Detail   string
}

// Default cooldowns applied when the provider gives no Retry-After.
const (
	DefaultRateLimitCooldown   = 60 * time.Second
	DefaultUnavailableCooldown = 30 * time.Second
	DefaultAuthCooldown        = 6 * time.Hour
	DefaultGoneCooldown        = 24 * time.Hour
	maxErrorBody               = 2048
	maxResponseBody            = 8 << 20
)

// ErrCapacityExhausted is wrapped by the error Generate returns when every
// provider is cooling down, capped, keyless, or failed over. It also matches
// turn.ErrInferenceCapacity so the Runner parks the session instead of
// failing it.
var ErrCapacityExhausted = fmt.Errorf("llmrouter: no provider capacity: %w", turn.ErrInferenceCapacity)

// CapacityError carries the earliest time any provider may be retried.
type CapacityError struct {
	RetryAt  time.Time
	Attempts []Attempt
}

func (e *CapacityError) Error() string {
	parts := make([]string, 0, len(e.Attempts))
	for _, a := range e.Attempts {
		parts = append(parts, a.Provider+"="+string(a.Outcome))
	}
	msg := "llmrouter: no provider capacity [" + strings.Join(parts, " ") + "]"
	if !e.RetryAt.IsZero() {
		msg += " retry at " + e.RetryAt.UTC().Format(time.RFC3339)
	}
	return msg
}

// Unwrap lets errors.Is match ErrCapacityExhausted and turn.ErrInferenceCapacity.
func (e *CapacityError) Unwrap() error { return ErrCapacityExhausted }

// Router implements turn.LLMClient over an ordered provider list.
type Router struct {
	providers []Provider
	ledger    Ledger
	client    *http.Client
	getenv    func(string) string
	now       func() time.Time
	onAttempt func(Attempt)
}

// Option configures a Router.
type Option func(*Router)

// WithLedger replaces the in-memory ledger (e.g. with a store-backed one).
func WithLedger(l Ledger) Option { return func(r *Router) { r.ledger = l } }

// WithHTTPClient sets the HTTP client; the default has a 120s timeout.
func WithHTTPClient(c *http.Client) Option { return func(r *Router) { r.client = c } }

// WithAttemptHook receives every attempt, for metrics and inferencehealth.
func WithAttemptHook(f func(Attempt)) Option { return func(r *Router) { r.onAttempt = f } }

// WithEnv overrides the key lookup (tests).
func WithEnv(getenv func(string) string) Option { return func(r *Router) { r.getenv = getenv } }

// WithClock overrides the clock (tests).
func WithClock(now func() time.Time) Option { return func(r *Router) { r.now = now } }

// New validates providers and builds a Router.
func New(providers []Provider, opts ...Option) (*Router, error) {
	if len(providers) == 0 {
		return nil, errors.New("llmrouter: at least one provider is required")
	}
	seen := map[string]bool{}
	for _, p := range providers {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if seen[p.Name] {
			return nil, fmt.Errorf("llmrouter: duplicate provider name %q", p.Name)
		}
		seen[p.Name] = true
	}
	r := &Router{
		providers: append([]Provider(nil), providers...),
		ledger:    NewMemoryLedger(),
		client:    &http.Client{Timeout: 120 * time.Second},
		getenv:    os.Getenv,
		now:       time.Now,
	}
	for _, o := range opts {
		o(r)
	}
	return r, nil
}

var _ turn.LLMClient = (*Router)(nil)

// Generate sends the conversation to the first provider with capacity and
// fails over on 429, 5xx, transport errors, auth failures, and retired
// models. A request the provider rejects as malformed (other 4xx) is returned
// immediately: resending it elsewhere would only spend more budget.
func (r *Router) Generate(ctx context.Context, messages []turn.Message, tools []toolapprove.ToolRequest) (turn.LLMResponse, error) {
	body, err := buildRequestBody(messages, tools)
	if err != nil {
		return turn.LLMResponse{}, err
	}
	var attempts []Attempt
	var retryAt time.Time
	noteRetry := func(t time.Time) {
		if !t.IsZero() && (retryAt.IsZero() || t.Before(retryAt)) {
			retryAt = t
		}
	}
	for _, p := range r.providers {
		if err := ctx.Err(); err != nil {
			return turn.LLMResponse{}, err
		}
		now := r.now()
		key := r.getenv(p.APIKeyEnv)
		if key == "" {
			attempts = append(attempts, r.report(Attempt{Provider: p.Name, Outcome: OutcomeSkipped, Detail: "no api key in $" + p.APIKeyEnv}))
			continue
		}
		ok, until, err := r.ledger.Acquire(p.Name, p.DailyRequestCap, now)
		if err != nil {
			return turn.LLMResponse{}, fmt.Errorf("llmrouter: ledger: %w", err)
		}
		if !ok {
			noteRetry(until)
			attempts = append(attempts, r.report(Attempt{Provider: p.Name, Outcome: OutcomeSkipped, Detail: "unavailable until " + until.UTC().Format(time.RFC3339)}))
			continue
		}
		resp, a, cooldown := r.try(ctx, p, key, body)
		attempts = append(attempts, r.report(a))
		switch a.Outcome {
		case OutcomeServed:
			return resp, nil
		case OutcomeRejected:
			return turn.LLMResponse{}, fmt.Errorf("llmrouter: %s rejected request (HTTP %d): %s", p.Name, a.Status, a.Detail)
		}
		if ctx.Err() != nil {
			return turn.LLMResponse{}, ctx.Err()
		}
		if cooldown > 0 {
			until := now.Add(cooldown)
			if err := r.ledger.Cooldown(p.Name, until); err != nil {
				return turn.LLMResponse{}, fmt.Errorf("llmrouter: ledger: %w", err)
			}
			noteRetry(until)
		}
	}
	return turn.LLMResponse{}, &CapacityError{RetryAt: retryAt, Attempts: attempts}
}

func (r *Router) report(a Attempt) Attempt {
	if r.onAttempt != nil {
		r.onAttempt(a)
	}
	return a
}

// try performs one HTTP attempt and returns the response, the attempt record,
// and the cooldown to apply on failure.
func (r *Router) try(ctx context.Context, p Provider, key string, body []byte) (turn.LLMResponse, Attempt, time.Duration) {
	a := Attempt{Provider: p.Name}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.BaseURL, "/")+"/chat/completions", bytes.NewReader(withModel(body, p.Model)))
	if err != nil {
		a.Outcome, a.Detail = OutcomeRejected, err.Error()
		return turn.LLMResponse{}, a, 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		a.Outcome, a.Detail = OutcomeUnavailable, err.Error()
		return turn.LLMResponse{}, a, DefaultUnavailableCooldown
	}
	defer resp.Body.Close()
	a.Status = resp.StatusCode
	if resp.StatusCode/100 == 2 {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		if err != nil {
			a.Outcome, a.Detail = OutcomeUnavailable, err.Error()
			return turn.LLMResponse{}, a, DefaultUnavailableCooldown
		}
		out, err := parseResponse(raw)
		if err != nil {
			// Some free routes return 200 with an error object or no
			// choices when an upstream is overloaded; treat as unavailable.
			a.Outcome, a.Detail = OutcomeMalformed, err.Error()
			return turn.LLMResponse{}, a, DefaultUnavailableCooldown
		}
		a.Outcome = OutcomeServed
		return out, a, 0
	}
	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	a.Detail = strings.TrimSpace(string(errBody))
	retry := parseRetryAfter(resp.Header.Get("Retry-After"), r.now())
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		a.Outcome = OutcomeRateLimited
		return turn.LLMResponse{}, a, orDefault(retry, DefaultRateLimitCooldown)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		a.Outcome = OutcomeAuth
		return turn.LLMResponse{}, a, DefaultAuthCooldown
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		a.Outcome = OutcomeGone
		return turn.LLMResponse{}, a, DefaultGoneCooldown
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500:
		a.Outcome = OutcomeUnavailable
		return turn.LLMResponse{}, a, orDefault(retry, DefaultUnavailableCooldown)
	default:
		a.Outcome = OutcomeRejected
		return turn.LLMResponse{}, a, 0
	}
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// parseRetryAfter accepts delta-seconds or an HTTP date.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

// ---- wire format -----------------------------------------------------------

type wireFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function wireFunctionCall `json:"function"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	Name       string         `json:"name,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireTool struct {
	Type     string          `json:"type"`
	Function wireFunctionDef `json:"function"`
}

type wireFunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type wireRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	Tools    []wireTool    `json:"tools,omitempty"`
}

// buildRequestBody renders everything except the model, which differs per
// provider and is spliced in by withModel.
func buildRequestBody(messages []turn.Message, tools []toolapprove.ToolRequest) ([]byte, error) {
	req := wireRequest{Messages: make([]wireMessage, 0, len(messages))}
	for _, m := range messages {
		wm := wireMessage{Role: string(m.Role)}
		content := m.Content
		switch m.Role {
		case turn.RoleSystem, turn.RoleUser:
		case turn.RoleAssistant:
			for _, c := range m.ToolCalls {
				wm.ToolCalls = append(wm.ToolCalls, wireToolCall{ID: c.ID, Type: "function", Function: wireFunctionCall{Name: c.Name, Arguments: c.Arguments}})
			}
		case turn.RoleTool:
			wm.ToolCallID = m.ToolCallID
		case turn.RoleSubagent:
			// Not an OpenAI role: deliver subagent output as attributed user input.
			wm.Role = string(turn.RoleUser)
			if m.Name != "" {
				content = "[subagent " + m.Name + "]\n" + content
			}
		default:
			return nil, fmt.Errorf("llmrouter: unsupported message role %q", m.Role)
		}
		if !(m.Role == turn.RoleAssistant && content == "" && len(wm.ToolCalls) > 0) {
			c := content
			wm.Content = &c
		}
		req.Messages = append(req.Messages, wm)
	}
	// Tool definitions ride on toolapprove.ToolRequest (the Runner's type):
	// Tool is the function name, Content its description, and Arguments the
	// JSON Schema for its parameters.
	for _, t := range tools {
		if strings.TrimSpace(t.Tool) == "" {
			return nil, errors.New("llmrouter: tool definition without a name")
		}
		params := t.Arguments
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		req.Tools = append(req.Tools, wireTool{Type: "function", Function: wireFunctionDef{Name: t.Tool, Description: t.Content, Parameters: params}})
	}
	return json.Marshal(req)
}

func withModel(body []byte, model string) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	mb, _ := json.Marshal(model)
	m["model"] = mb
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

type wireResponse struct {
	Choices []struct {
		Message struct {
			Content   *string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func parseResponse(raw []byte) (turn.LLMResponse, error) {
	var w wireResponse
	if err := json.Unmarshal(raw, &w); err != nil {
		return turn.LLMResponse{}, fmt.Errorf("decode response: %w", err)
	}
	if w.Error != nil {
		return turn.LLMResponse{}, fmt.Errorf("provider error: %s", w.Error.Message)
	}
	if len(w.Choices) == 0 {
		return turn.LLMResponse{}, errors.New("response has no choices")
	}
	c := w.Choices[0]
	out := turn.LLMResponse{StopReason: c.FinishReason}
	if c.Message.Content != nil {
		out.Content = *c.Message.Content
	}
	for i, tc := range c.Message.ToolCalls {
		if tc.Function.Name == "" {
			return turn.LLMResponse{}, fmt.Errorf("tool call %d has no function name", i)
		}
		id := tc.ID
		if id == "" {
			// Some OpenAI-compatible servers omit ids; the Runner pairs
			// results by id, so mint a positional one.
			id = "call_" + strconv.Itoa(i)
		}
		out.ToolCalls = append(out.ToolCalls, turn.ToolCall{ID: id, Name: tc.Function.Name, Arguments: normalizeArguments(tc.Function.Arguments)})
	}
	if out.Content == "" && len(out.ToolCalls) == 0 {
		return turn.LLMResponse{}, errors.New("response has neither content nor tool calls")
	}
	return out, nil
}

// normalizeArguments returns the arguments as a JSON string. The OpenAI shape
// is a JSON-encoded string; some compatible servers return a bare object.
func normalizeArguments(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "{}"
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			if strings.TrimSpace(s) == "" {
				return "{}"
			}
			return s
		}
	}
	return string(trimmed)
}
