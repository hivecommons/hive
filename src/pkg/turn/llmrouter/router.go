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
// Providers are Hive Model Gateways: FromGateway builds one from the same
// crux/config.GatewayConfig the governor's gateway path uses, authenticates
// through crux/inference.ResolveGatewayAuth (so watsonx IAM minting and key
// files behave identically), and POSTs to config.LiteLLMChatCompletionsURL,
// the URL rule the inference translator and Test Connection share. Attempt
// outcomes can be fed to crux/inferencehealth (HealthHook) so free-tier
// throttling shows up in the existing gateway-health surface.
//
// Configuration never carries a credential. A provider whose configured key
// source resolves empty is skipped, not an error; a provider with no key
// source at all is keyless (e.g. a local vLLM).
package llmrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	cruxconfig "github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/inference"
	"github.com/hivecommons/hive/crux/toolapprove"
	"github.com/hivecommons/hive/pkg/turn"
)

// Provider is one OpenAI-compatible endpoint and model. Order in the Router's
// list is the failover order. Build it with FromGateway in production; the
// literal form exists for tests and embedders without a hive config.
type Provider struct {
	// Name identifies the provider in the ledger, attempts and health store;
	// for a gateway it is the gateway name.
	Name string
	// BaseURL is the gateway endpoint. The request URL is
	// config.LiteLLMChatCompletionsURL(BaseURL), exactly as the inference
	// translator builds it.
	BaseURL string
	// Model is sent verbatim as the request's model field.
	Model string
	// APIKeyEnv / APIKeyFile name where the bearer token lives (never the
	// value). Ignored when Auth is set.
	APIKeyEnv  string
	APIKeyFile string
	// Auth, when set, resolves the bearer and extra headers per request
	// (FromGateway wires crux/inference.ResolveGatewayAuth here).
	Auth func() (token string, headers map[string]string)
	// DailyRequestCap is the coordinator-side request budget per UTC day;
	// zero means no local cap (the provider's 429s still apply).
	DailyRequestCap int
	// Headers are extra non-secret request headers (e.g. OpenRouter's
	// HTTP-Referer / X-Title attribution).
	Headers map[string]string
	// MaxTokens, when positive, is sent as max_tokens. Some free endpoints
	// default low enough to cut a long tool call mid-argument.
	MaxTokens int
	// TrainsOnInputs marks a provider (typically a free tier) whose terms
	// let it train on request content. A router serving a private repository
	// skips it unless WithAllowTrainingProvidersForPrivate opts in.
	TrainsOnInputs bool `yaml:"trains_on_inputs" json:"trains_on_inputs,omitempty"`
}

// FromGateway builds a Provider from a configured Hive Model Gateway. model
// overrides the gateway's DefaultModel; one of them must be set.
func FromGateway(gw cruxconfig.GatewayConfig, model string, dailyCap int) (Provider, error) {
	if strings.TrimSpace(model) == "" {
		model = gw.DefaultModel
	}
	gwCopy := gw
	p := Provider{
		Name:            gw.Name,
		BaseURL:         strings.TrimSpace(gw.Endpoint),
		Model:           strings.TrimSpace(model),
		APIKeyEnv:       gw.APIKeyEnv,
		APIKeyFile:      gw.APIKeyFile,
		DailyRequestCap: dailyCap,
		TrainsOnInputs:  gw.TrainsOnInputs,
	}
	// A gateway with no key source is keyless (some vLLM); only gateways
	// that name one authenticate, through the gateway path's own resolver.
	if strings.TrimSpace(gw.APIKeyEnv) != "" || strings.TrimSpace(gw.APIKeyFile) != "" {
		p.Auth = func() (string, map[string]string) {
			return inference.ResolveGatewayAuth(&gwCopy, "llmrouter", gwCopy.Name, slog.Default())
		}
	}
	return p, p.Validate()
}

// Validate reports a configuration error for p.
func (p Provider) Validate() error {
	switch {
	case strings.TrimSpace(p.Name) == "":
		return errors.New("llmrouter: provider name is required")
	case !strings.HasPrefix(p.BaseURL, "https://") && !strings.HasPrefix(p.BaseURL, "http://"):
		return fmt.Errorf("llmrouter: provider %s: endpoint must be http(s)", p.Name)
	case strings.TrimSpace(p.Model) == "":
		return fmt.Errorf("llmrouter: provider %s: model is required (set one or a gateway default_model)", p.Name)
	case p.DailyRequestCap < 0:
		return fmt.Errorf("llmrouter: provider %s: daily cap must be >= 0", p.Name)
	}
	return nil
}

// hasKeySource reports whether p is configured to authenticate at all.
func (p Provider) hasKeySource() bool {
	return p.Auth != nil || strings.TrimSpace(p.APIKeyEnv) != "" || strings.TrimSpace(p.APIKeyFile) != ""
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
	providers  []Provider
	ledger     Ledger
	client     *http.Client
	getenv     func(string) string
	now        func() time.Time
	onAttempt  func(Attempt)
	toolChoice string
	// private marks the router as serving a private repository; with it,
	// providers that train on inputs are skipped unless allowTraining.
	private       bool
	allowTraining bool
}

// Option configures a Router.
type Option func(*Router)

// WithLedger replaces the in-memory ledger (e.g. with a store-backed one).
func WithLedger(l Ledger) Option { return func(r *Router) { r.ledger = l } }

// WithHTTPClient sets the HTTP client; the default has a 120s timeout.
func WithHTTPClient(c *http.Client) Option { return func(r *Router) { r.client = c } }

// WithAttemptHook receives every attempt, for metrics and inferencehealth.
func WithAttemptHook(f func(Attempt)) Option { return func(r *Router) { r.onAttempt = f } }

// WithToolChoice sets the OpenAI tool_choice ("required", "auto", "none")
// sent whenever tools are offered. "required" forces a call on providers
// that honour it; measured 2026-11-16 on Workers AI: llama-4-scout and
// glm-4.7-flash honour it, gpt-oss-120b ignores it (and still reports
// finish_reason "tool_calls"), so callers must not rely on it alone.
func WithToolChoice(choice string) Option { return func(r *Router) { r.toolChoice = choice } }

// WithPrivateRepo marks the content this router sends as coming from a
// private repository. Providers with TrainsOnInputs are then skipped on
// every call (reported as OutcomeSkipped) unless
// WithAllowTrainingProvidersForPrivate(true) is also given. Callers that
// cannot determine visibility should pass true.
func WithPrivateRepo(private bool) Option { return func(r *Router) { r.private = private } }

// WithAllowTrainingProvidersForPrivate is the explicit opt-in
// (allow_training_providers_for_private) that lets providers with
// TrainsOnInputs serve a private repository. It has no effect on public
// repositories, where every provider is eligible.
func WithAllowTrainingProvidersForPrivate(allow bool) Option {
	return func(r *Router) { r.allowTraining = allow }
}

// ErrOnlyTrainingProviders is returned by New when the repository is
// private, training providers are not allowed for it, and every provider
// trains on inputs: such a router could never serve, and reporting that as
// a capacity pause would retry forever.
var ErrOnlyTrainingProviders = errors.New("llmrouter: every provider trains on inputs and the repository is private (set allow_training_providers_for_private to opt in)")

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
	eligible := false
	for _, p := range r.providers {
		if !r.excluded(p) {
			eligible = true
			break
		}
	}
	if !eligible {
		return nil, ErrOnlyTrainingProviders
	}
	return r, nil
}

// excluded reports whether p may not see this router's content: it trains
// on inputs, the repository is private, and there is no opt-in.
func (r *Router) excluded(p Provider) bool {
	return p.TrainsOnInputs && r.private && !r.allowTraining
}

var _ turn.LLMClient = (*Router)(nil)

// Generate sends the conversation to the first provider with capacity and
// fails over on 429, 5xx, transport errors, auth failures, and retired
// models. A request the provider rejects as malformed (other 4xx) is returned
// immediately: resending it elsewhere would only spend more budget.
func (r *Router) Generate(ctx context.Context, messages []turn.Message, tools []toolapprove.ToolRequest) (turn.LLMResponse, error) {
	names, err := newToolNames(tools, messages)
	if err != nil {
		return turn.LLMResponse{}, err
	}
	wireBody, err := buildRequestBody(messages, tools, names, r.toolChoice)
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
		if r.excluded(p) {
			attempts = append(attempts, r.report(Attempt{Provider: p.Name, Outcome: OutcomeSkipped, Detail: "trains on inputs; private repository and no allow_training_providers_for_private opt-in"}))
			continue
		}
		now := r.now()
		key, extra := r.resolveAuth(p)
		if key == "" && p.hasKeySource() {
			attempts = append(attempts, r.report(Attempt{Provider: p.Name, Outcome: OutcomeSkipped, Detail: "configured api key resolved empty"}))
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
		resp, a, cooldown := r.try(ctx, p, key, extra, wireBody, names)
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
			until := r.now().Add(cooldown)
			if err := r.ledger.Cooldown(p.Name, until); err != nil {
				return turn.LLMResponse{}, fmt.Errorf("llmrouter: ledger: %w", err)
			}
			noteRetry(until)
		}
	}
	return turn.LLMResponse{}, &CapacityError{RetryAt: retryAt, Attempts: attempts}
}

func (r *Router) resolveAuth(p Provider) (string, map[string]string) {
	if p.Auth != nil {
		return p.Auth()
	}
	if p.APIKeyEnv != "" {
		if v := strings.TrimSpace(r.getenv(p.APIKeyEnv)); v != "" {
			return v, nil
		}
	}
	if p.APIKeyFile != "" {
		if b, err := os.ReadFile(p.APIKeyFile); err == nil {
			return strings.TrimSpace(string(b)), nil
		}
	}
	return "", nil
}

func (r *Router) report(a Attempt) Attempt {
	if r.onAttempt != nil {
		r.onAttempt(a)
	}
	return a
}

// try performs one HTTP attempt and returns the response, the attempt record,
// and the cooldown to apply on failure.
func (r *Router) try(ctx context.Context, p Provider, key string, extra map[string]string, body []byte, names toolNames) (turn.LLMResponse, Attempt, time.Duration) {
	a := Attempt{Provider: p.Name}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cruxconfig.LiteLLMChatCompletionsURL(p.BaseURL), bytes.NewReader(withModel(body, p.Model, p.MaxTokens)))
	if err != nil {
		a.Outcome, a.Detail = OutcomeRejected, err.Error()
		return turn.LLMResponse{}, a, 0
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	for k, v := range extra {
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
		out, err := parseResponse(raw, names)
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
		if retry == 0 && dailyQuotaBody(a.Detail) {
			// A per-day cap (Groq RPD/TPD, OpenRouter :free daily requests,
			// Workers AI neurons) will not lift in a minute; retrying every
			// 60s would only burn the other providers' minute budgets.
			a.Detail = "daily quota: " + a.Detail
			return turn.LLMResponse{}, a, untilUTCMidnight(r.now())
		}
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
	case resp.StatusCode == http.StatusBadRequest && modelOutputFault(a.Detail):
		// The provider failed to parse the model's own generation (seen live:
		// Workers AI code 3030, glm-4.7-flash emitted "<tool_call>…" that the
		// server's tool parser rejected as json_invalid). The request was
		// fine; another provider or a retry can serve it.
		a.Outcome = OutcomeMalformed
		return turn.LLMResponse{}, a, DefaultUnavailableCooldown
	default:
		a.Outcome = OutcomeRejected
		return turn.LLMResponse{}, a, 0
	}
}

// modelOutputFault reports a 400 body that blames the model's generation
// (a server-side tool-call parse failure) rather than the request.
func modelOutputFault(body string) bool {
	b := strings.ToLower(body)
	return strings.Contains(b, "<tool_call>") ||
		(strings.Contains(b, "json_invalid") && strings.Contains(b, "tool")) ||
		strings.Contains(b, "failed to parse tool call") ||
		strings.Contains(b, "tool_use_failed")
}

// dailyQuotaBody reports whether a 429 body names a per-day limit.
func dailyQuotaBody(body string) bool {
	b := strings.ToLower(body)
	for _, marker := range []string{"per day", "per-day", "daily", "requests per day", "(rpd)", "(tpd)", "free-models-per-day", "neurons"} {
		if strings.Contains(b, marker) {
			return true
		}
	}
	return false
}

func untilUTCMidnight(now time.Time) time.Duration {
	y, m, d := now.UTC().Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC).Sub(now)
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
	Model      string        `json:"model"`
	Messages   []wireMessage `json:"messages"`
	Tools      []wireTool    `json:"tools,omitempty"`
	ToolChoice string        `json:"tool_choice,omitempty"`
}

// buildRequestBody renders everything except the model, which differs per
// provider and is spliced in by withModel.
func buildRequestBody(messages []turn.Message, tools []toolapprove.ToolRequest, names toolNames, toolChoice string) ([]byte, error) {
	req := wireRequest{Messages: make([]wireMessage, 0, len(messages))}
	for _, m := range messages {
		wm := wireMessage{Role: string(m.Role)}
		content := m.Content
		switch m.Role {
		case turn.RoleSystem, turn.RoleUser:
		case turn.RoleAssistant:
			for _, c := range m.ToolCalls {
				wm.ToolCalls = append(wm.ToolCalls, wireToolCall{ID: c.ID, Type: "function", Function: wireFunctionCall{Name: names.wire(c.Name), Arguments: historyArguments(c.Arguments)}})
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
		// Always a string, never null: the OpenAI spec allows null content on
		// an assistant tool-call message, but Workers AI's gpt-oss-120b
		// rejects it (HTTP 400 "'string' not in 'null'", measured
		// 2026-11-16) while every provider tested accepts "".
		c := content
		wm.Content = &c
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
		req.Tools = append(req.Tools, wireTool{Type: "function", Function: wireFunctionDef{Name: names.wire(t.Tool), Description: t.Content, Parameters: params}})
	}
	if len(req.Tools) > 0 && toolChoice != "" {
		req.ToolChoice = toolChoice
	}
	return json.Marshal(req)
}

func withModel(body []byte, model string, maxTokens int) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	mb, _ := json.Marshal(model)
	m["model"] = mb
	if maxTokens > 0 {
		m["max_tokens"] = json.RawMessage(strconv.Itoa(maxTokens))
	}
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

func parseResponse(raw []byte, names toolNames) (turn.LLMResponse, error) {
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
		args := normalizeArguments(tc.Function.Arguments)
		if c.FinishReason == "length" && !json.Valid([]byte(args)) {
			// Cut off mid-argument by the output limit: the same request
			// would be cut off again, so fail over instead of executing.
			return turn.LLMResponse{}, fmt.Errorf("tool call %d arguments truncated at the output limit (finish_reason length)", i)
		}
		out.ToolCalls = append(out.ToolCalls, turn.ToolCall{ID: id, Name: names.hive(tc.Function.Name), Arguments: args})
	}
	if out.Content == "" && len(out.ToolCalls) == 0 {
		return turn.LLMResponse{}, errors.New("response has neither content nor tool calls")
	}
	if len(out.ToolCalls) == 0 && degenerate(out.Content) {
		return turn.LLMResponse{}, errors.New("degenerate completion (one repeated character)")
	}
	return out, nil
}

// degenerate reports a long completion made almost entirely of one rune.
// Observed live 2026-11-16: Workers AI gpt-oss-120b returned HTTP 200 whose
// content was ~300 '!' at ~4k tokens of context. Treating it as malformed
// fails the turn over to the next provider instead of persisting garbage
// into the envelope.
func degenerate(s string) bool {
	if len(s) < 64 {
		return false
	}
	counts := map[rune]int{}
	total, maxCount := 0, 0
	for _, r := range s {
		if r == ' ' || r == '\n' || r == '\t' {
			continue
		}
		total++
		counts[r]++
		if counts[r] > maxCount {
			maxCount = counts[r]
		}
	}
	return total >= 64 && maxCount*10 >= total*9
}

// historyArguments makes a past tool call's arguments safe to resend.
// Observed live 2026-11-16: gpt-oss emitted a task.complete cut off
// mid-string; the executor refused it, but resending those bytes in history
// made Workers AI reject every later request (HTTP 400 code 3030,
// "Unterminated string"). Invalid arguments are wrapped in a valid object
// so the model still sees what it sent.
func historyArguments(args string) string {
	if strings.TrimSpace(args) == "" {
		return "{}"
	}
	if json.Valid([]byte(args)) {
		return args
	}
	b, _ := json.Marshal(map[string]string{"invalid_arguments": args})
	return string(b)
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
