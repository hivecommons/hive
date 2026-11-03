package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/inferencehealth"
)

// inferenceAPIKeyPrefix is what the manager prepends to the agent's name to
// form the ANTHROPIC_API_KEY it hands each inference-routed agent
// (agent/manager_env.go). The suffix is the agent's NAME, not a secret.
const inferenceAPIKeyPrefix = "sk-hive-"

// translatorAgentName names the caller of the inference translator.
//
// The x-api-key header the Claude CLI sends is "sk-hive-<agent>", and the
// agent name is public inside the pod: every agent knows every other agent's
// name. Trusting the header alone lets any process on the loopback borrow
// another agent's inference route — its backend, model, upstream credential
// and usage attribution — which is exactly the self-asserted-identity hole
// closed for the GitHub path in fallbackAgentName and for the decision
// endpoint in IdentifyAgentByUID. So the socket owner's UID decides:
//
//   - a UID mapped to an agent is authoritative; a header naming someone else
//     is logged and overridden;
//   - the root control plane keeps the header name — the hive is the party
//     that issues these keys; the proxy UID cannot assert a name because its
//     upstream sockets can relay agent-controlled requests;
//   - an unmapped UID, or no UID map at all, accepts the header only under
//     HIVE_PROXY_ADVISORY_OK, the same operator escape hatch the GitHub path
//     honours for deployments without per-agent UID separation.
//
// The second return is false when the caller could not be identified and the
// request must be refused.
func (p *GitHubProxy) translatorAgentName(r *http.Request) (string, bool) {
	claimed := strings.TrimPrefix(r.Header.Get("x-api-key"), inferenceAPIKeyPrefix)
	if p.uidMap == nil {
		if p.proxyAdvisoryOK {
			return claimed, true
		}
		return "", false
	}
	uid, err := uidForRemoteAddr(r.RemoteAddr)
	if err == nil {
		if name := p.uidMap.LookupByUID(uid); name != "" {
			if claimed != "" && claimed != name {
				p.logger.Warn("inference identity: x-api-key names another agent; using the socket owner",
					"agent", name, "claimed", claimed, "uid", uid)
			}
			return name, true
		}
		if uid == 0 && p.uidMap.IsInternalUID(uid) {
			return claimed, true
		}
	}
	if p.proxyAdvisoryOK {
		return claimed, true
	}
	return "", false
}

// SetInferenceRoute configures an agent to use a self-hosted inference backend.
//
// The route is stored immediately and the caller returns without blocking. The
// model's max context length is queried in the background because this function
// is invoked from callers that hold the agent-manager mutex (agent launch,
// SetModelOverride, SetBackendOverride). A synchronous endpoint probe here
// (up to maxModelLenQueryTimeout) would pin that mutex for its whole duration,
// stalling /api/status and other dashboard handlers — which on a single-replica
// pod reads to the OpenShift router as "Application is not available". The
// max-context-len only feeds token capping and defaults gracefully to 0
// (no cap) until the async probe fills it in.
func (p *GitHubProxy) SetInferenceRoute(agentName string, route *InferenceRoute) {
	p.inference.Set(agentName, route)
	keySHA256 := config.APIKeySHA256(route.APIKey)
	if keySHA256 != "" {
		p.logger.Info("inference route set", "agent", agentName, "backend", route.Backend, "endpoint", route.Endpoint, "model", route.Model, "maxContextLen", route.MaxContextLen, "api_key_sha256", keySHA256)
	} else {
		p.logger.Info("inference route set", "agent", agentName, "backend", route.Backend, "endpoint", route.Endpoint, "model", route.Model, "maxContextLen", route.MaxContextLen, "api_key_sha256", "")
	}

	if route.MaxContextLen == 0 {
		endpoint, model, apiKey, caBundle := route.Endpoint, route.Model, route.APIKey, route.CABundle
		go func() {
			if maxLen := queryMaxModelLen(endpoint, model, apiKey, caBundle); maxLen > 0 {
				if p.inference.UpdateMaxContextLen(agentName, endpoint, model, maxLen) {
					p.logger.Info("inference route max context length resolved", "agent", agentName, "model", model, "maxContextLen", maxLen)
				}
			}
		}()
	}

	// Best-effort: learn the key's entitled model set from a LiteLLM
	// key-info endpoint before any 403 happens, so the dashboard can offer
	// only usable models up front. Only litellm gateways entitlement-filter
	// per key; vllm/llm-d do not. The 403-parse path (recordInferenceError)
	// remains the authoritative fallback when no key-info endpoint responds.
	if route.Backend == "litellm" && route.APIKey != "" {
		p.entitlements.rememberProbe(route.Endpoint, route.APIKey, route.CABundle)
		go p.probeEntitlements(route.Endpoint, route.APIKey, route.CABundle)
	}
}

// probeEntitlements queries a LiteLLM gateway's per-key info endpoints for the
// key's entitled model set and caches it. A nil result (no key-info endpoint,
// or an unrestricted key) leaves any existing 403-derived entry untouched.
func (p *GitHubProxy) probeEntitlements(endpoint, apiKey, caBundle string) {
	if !p.entitlements.beginProbe(endpoint) {
		return // another probe for this endpoint is already in flight
	}
	defer p.entitlements.endProbe(endpoint)
	transport, err := inferenceTransport(caBundle)
	if err != nil {
		transport = nil
	}
	if models := probeEntitledModels(endpoint, apiKey, transport); len(models) > 0 {
		p.entitlements.set(endpoint, apiKey, models, "key-info")
		p.logger.Info("litellm entitled models discovered via key-info",
			"endpoint", endpoint, "count", len(models))
	}
}

// EntitledModels returns the entitled model set known for a LiteLLM endpoint
// (from a key-info probe or a "team not allowed" 403), the signal source, and
// whether the set is known. It is the accessor the dashboard uses to narrow
// model dropdowns to what the configured key can actually use.
//
// A stale entry (older than entitlementTTL) is still returned so the served
// model list stays STABLE for an unchanged key — a filtered list that flapped
// back to the full catalog between probes would make the dashboard's
// auto-heal (see reconcileModelsAfterDiscovery, #1848) see models reappear
// and vanish across refreshes. Staleness instead triggers a bounded
// background re-probe that overwrites the entry only on a fresh signal.
func (p *GitHubProxy) EntitledModels(endpoint string) (models []string, source string, known bool) {
	models, source, known, stale := p.entitlements.get(endpoint)
	if stale {
		if ep, apiKey, caBundle, ok := p.entitlements.probeParams(endpoint); ok {
			go p.probeEntitlements(ep, apiKey, caBundle)
		}
	}
	return models, source, known
}

// recordInferenceError inspects an upstream inference error body for a
// LiteLLM "team not allowed to access model" 403 and, when found, caches the
// entitled model set it lists (keyed by the route's endpoint) so the
// dashboard's model list narrows to the usable set. Only a 403 carrying the
// gateway's team-scope marker is treated as an entitlement signal —
// provider-side failures relayed by the gateway (upstream 404 not-found, 400
// bad-request, 5xx) say nothing about the key's scope and are ignored, as is
// any other 403 (bad key, rate limit).
//
// It deliberately does NOT switch the agent's route itself: automatic model
// switching is governed by the dashboard's reconcile policy (#1848 — unpinned
// + governor suggestion, or confirmed genuine unavailability, at most once
// per list change). Once the entitled set is cached here, the dashboard's
// filtered discovery list drives that heal path for any agent left on a
// non-entitled model.
func (p *GitHubProxy) recordInferenceError(route *InferenceRoute, agentName string, status int, body []byte) {
	if route == nil {
		return
	}
	if p.gatewayHealth != nil {
		p.gatewayHealth.RecordEndpointHTTPError(route.Backend, route.Endpoint, status, string(truncateBytes(body, 200)), time.Now())
	}

	// Inference-backend AUTH failure (a stale/invalid gateway key returning 401
	// on every call). Tracked for ANY inference backend, not just litellm — a
	// self-hosted vLLM/llm-d fronted by auth can 401 too. Latches only after
	// inferenceAuthFailureThreshold consecutive failures and clears on the next
	// success, so a single transient blip never alarms. The stored cause is
	// log-safe: the gateway's 401 body describes the rejection without echoing
	// the presented token, and inferenceAuthErrorMessage never reads the key.
	if isInferenceAuthStatus(status) && p.inferenceAuth != nil {
		msg := inferenceAuthErrorMessage(route.Backend, status, truncateBytes(body, 200))
		p.inferenceAuth.recordFailure(msg, time.Now())
		p.logger.Warn("inference backend auth failure",
			"agent", agentName, "backend", route.Backend, "endpoint", route.Endpoint, "status", status)
	}

	// PROVIDER SPEND REBUFF (#4294). Checked before the 403/entitlement branch
	// below because that branch returns early for every non-403, which would
	// skip the 429/400 statuses a budget refusal actually arrives on.
	//
	// Requires the body to name spend, so an ordinary 429 throttle keeps
	// flowing to the existing transient-retry path untouched.
	if p.inferenceBudget != nil && isInferenceBudgetRebuff(status, body) {
		msg := inferenceBudgetMessage(route.Backend, status, truncateBytes(body, 200))
		p.inferenceBudget.recordRebuff(msg, time.Now())
		p.logger.Error("inference backend refused on a spending limit",
			"agent", agentName, "backend", route.Backend, "endpoint", route.Endpoint,
			"status", status, "cause", msg)
	}

	if route.Backend != "litellm" || status != http.StatusForbidden {
		return
	}
	entitled := parseTeam403Models(string(body))
	if len(entitled) == 0 {
		return
	}
	p.entitlements.set(route.Endpoint, route.APIKey, entitled, "403")
	p.logger.Info("litellm entitled models learned from 403",
		"agent", agentName, "endpoint", route.Endpoint, "count", len(entitled), "model", route.Model)
}

// routeWithEntitledModel returns the route to actually forward with: when the
// gateway's entitled model set is known (key-info probe or a prior team-scope
// 403) and the route's stored model differs from an entitled id only by
// provider-prefix / dot-hyphen spelling drift, a COPY of the route carrying
// the exact entitled id is returned. LiteLLM matches the request model string
// verbatim, so without this a stored copilot-spelled id (e.g.
// "claude-sonnet-4.6" vs entitled "aws/claude-sonnet-4-6") 403s on every call
// forever (#4400) — while the dashboard's tolerant preselect matcher shows the
// agent on a perfectly valid model. Self-healing: the first 403 teaches the
// entitled set (recordInferenceError), so the next request resolves.
//
// When the stored model is NOT entitled at all — verbatim or by drift — and
// the backend's DefaultModel IS, the request goes out for the default instead
// (hivecommons/hive#9945). The FMA hive's agents carried a `model:
// claude-sonnet-4-6` written for their original copilot backend into a
// switch to the litellm gateway whose default_model was the only model the
// team could use; the per-agent value won, and every call 403'd forever with
// "team not allowed to access model" while the operator stared at a gateway
// card showing the right default. The gateway's own entitlement signal (the
// key-info probe or that very 403) is what authorises the substitution, so a
// model the team can't use is never sent when one it can use is configured.
// The stored route is still untouched: a key/team change that re-entitles the
// per-agent model takes effect on the next request.
//
// The stored route is never mutated: the rewrite is per-request, so a
// key/team change that re-entitles the verbatim id takes effect immediately,
// and concurrent requests race on nothing. Unknown entitlements, verbatim
// matches, and ambiguous candidates all pass the route through unchanged.
func (p *GitHubProxy) routeWithEntitledModel(route *InferenceRoute, agentName string) *InferenceRoute {
	// A blank per-agent selection inherits the configured backend default,
	// even before an entitlement probe has completed. Keep the stored snapshot
	// untouched, as with entitlement rewrites below.
	if route != nil && strings.TrimSpace(route.Model) == "" && strings.TrimSpace(route.DefaultModel) != "" {
		rewritten := *route
		rewritten.Model = route.DefaultModel
		route = &rewritten
	}
	if route == nil || route.Backend != "litellm" || p.entitlements == nil {
		return route
	}
	entitled, _, known := p.EntitledModels(route.Endpoint)
	if !known {
		return route
	}
	if entitledModelListed(entitled, route.Model) {
		return route
	}
	if resolved, ok := resolveEntitledModelID(entitled, route.Model); ok {
		p.logger.Info("inference model resolved to entitled gateway id",
			"agent", agentName, "endpoint", route.Endpoint,
			"configured", route.Model, "resolved", resolved)
		rewritten := *route
		rewritten.Model = resolved
		return &rewritten
	}
	fallback := strings.TrimSpace(route.DefaultModel)
	if fallback == "" || fallback == route.Model {
		return route
	}
	if !entitledModelListed(entitled, fallback) {
		resolved, ok := resolveEntitledModelID(entitled, fallback)
		if !ok {
			return route
		}
		fallback = resolved
	}
	p.logger.Warn("inference model not entitled on gateway; sending the backend default model instead",
		"agent", agentName, "backend", route.Backend, "endpoint", route.Endpoint,
		"configured", route.Model, "default", fallback,
		"remedy", "clear the agent's model (or pick one of the gateway's entitled models) so the agent uses the gateway default")
	rewritten := *route
	rewritten.Model = fallback
	return &rewritten
}

// entitledModelListed reports whether model appears verbatim in entitled.
func entitledModelListed(entitled []string, model string) bool {
	for _, id := range entitled {
		if id == model {
			return true
		}
	}
	return false
}

// recordInferenceSuccess records a successful inference call so a previously
// latched auth-failure signal clears — the self-heal for the inference-auth
// health signal. Called from every inference forward path on a 2xx response.
// A nil tracker (impossible after NewGitHubProxy, but cheap to guard) is a
// no-op.
func (p *GitHubProxy) recordInferenceSuccessFor(route *InferenceRoute) {
	if route != nil && p.gatewayHealth != nil {
		p.gatewayHealth.Clear(route.Backend)
	}
	p.recordInferenceSuccess()
}

func (p *GitHubProxy) recordInferenceSuccess() {
	if p.inferenceAuth != nil {
		p.inferenceAuth.recordSuccess()
	}
	// Self-heal for the spend rebuff (#4294): whenever the provider's window
	// resets — hive neither knows nor assumes the schedule; the field report's
	// gateway rolled over at the key's creation time of day, not at midnight
	// (see #4294) — the first call that succeeds takes the hive out of the
	// suppressed state with no operator action.
	if p.inferenceBudget != nil {
		p.inferenceBudget.recordSuccess()
	}
}

// GatewayHealth reports currently failing inference gateways.
func (p *GitHubProxy) GatewayHealth() []inferencehealth.GatewayStatus {
	if p == nil || p.gatewayHealth == nil {
		return nil
	}
	return p.gatewayHealth.Snapshot()
}

// InferenceAuthError reports the current inference-backend auth-failure signal:
// a non-empty, log-safe cause string (and the time it first latched) ONLY while
// the backend has been auth-failing for inferenceAuthFailureThreshold
// consecutive calls, empty otherwise. Wired to the dashboard/heartbeat so the
// hub can flag a hive that is silently 401'ing on every inference call. Safe on
// a nil tracker.
func (p *GitHubProxy) InferenceAuthError() (errMsg string, since time.Time) {
	if p == nil || p.inferenceAuth == nil {
		return "", time.Time{}
	}
	return p.inferenceAuth.snapshot()
}

// InferenceBudgetExceeded reports the current provider spending-limit signal:
// a non-empty log-safe cause, when it first latched, when the most recent
// rebuff arrived, and how many rebuffs have been observed since — all
// zero-valued while the provider is serving normally.
//
// Callers use a non-empty cause as "this hive cannot buy inference right now",
// which is a reason to stop kicking agents and to tell the operator, NOT a
// reason to retry in a few minutes. lastRebuff is what bounds that: because
// withholding kicks also withholds the calls that would clear the latch,
// callers suppress only while lastRebuff is fresh and send a probe once it is
// stale (see inference_budget.go). Safe on a nil receiver/tracker.
func (p *GitHubProxy) InferenceBudgetExceeded() (errMsg string, since, lastRebuff time.Time, rebuffs int) {
	if p == nil || p.inferenceBudget == nil {
		return "", time.Time{}, time.Time{}, 0
	}
	return p.inferenceBudget.snapshot()
}

// ClearInferenceRoute removes an agent's inference backend override.
func (p *GitHubProxy) ClearInferenceRoute(agentName string) {
	p.inference.Clear(agentName)
	p.logger.Info("inference route cleared", "agent", agentName)
}

// StartInferenceTranslator runs a plain HTTP server that accepts Anthropic
// Messages API requests and translates+forwards them to the configured
// inference backend. Agents use ANTHROPIC_BASE_URL=http://127.0.0.1:18444
// to reach this server instead of api.anthropic.com.
func (p *GitHubProxy) inferenceTranslatorHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		agentName, ok := p.translatorAgentName(r)
		if !ok {
			p.logger.Warn("inference request from unidentified caller refused",
				"remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path)
			http.Error(w, `{"type":"error","error":{"type":"authentication_error","message":"caller could not be identified"}}`, http.StatusUnauthorized)
			return
		}

		p.logger.Info("inference request",
			"agent", agentName,
			"method", r.Method,
			"path", r.URL.Path,
			"content-type", r.Header.Get("Content-Type"),
			"anthropic-version", r.Header.Get("anthropic-version"),
		)

		route := p.inference.Get(agentName)
		if route == nil {
			p.logger.Warn("inference no route", "agent", agentName)
			http.Error(w, `{"type":"error","error":{"type":"api_error","message":"no inference route for agent"}}`, http.StatusBadGateway)
			return
		}
		route = p.routeWithEntitledModel(route, agentName)

		body, err := io.ReadAll(r.Body)
		if r.Body != nil {
			_ = r.Body.Close()
		}
		if err != nil {
			http.Error(w, `{"type":"error","error":{"type":"api_error","message":"failed to read request"}}`, http.StatusBadRequest)
			return
		}

		// Same path gate as the MITM reroute: only POST /v1/messages is
		// translated and forwarded; CLI housekeeping is answered here.
		if kind := classifyInferencePath(r.Method, r.URL.Path); kind != inferencePathMessages {
			status, respBody := p.localInferenceResponse(kind, r.Method, r.URL.Path, body, agentName)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, respBody)
			return
		}

		// Diagnostic: count tools in original Anthropic request.
		var toolDiag struct {
			Tools json.RawMessage `json:"tools"`
		}
		toolCount := -1 // -1 = no tools key
		if json.Unmarshal(body, &toolDiag) == nil && toolDiag.Tools != nil {
			var arr []json.RawMessage
			if json.Unmarshal(toolDiag.Tools, &arr) == nil {
				toolCount = len(arr)
			}
		}
		p.logger.Info("inference request body", "agent", agentName, "len", len(body), "tool_count", toolCount, "preview", truncateBytes(body, 200))

		openaiBody, err := translateAnthropicToOpenAI(body, route.Model, route.MaxContextLen, resolveInferencePreamble(route, agentName))
		if err != nil {
			p.logger.Error("inference translate request failed", "agent", agentName, "error", err)
			p.recordAgentInferenceError(agentName, "translation error: "+err.Error(), 0)
			http.Error(w, fmt.Sprintf(`{"type":"error","error":{"type":"api_error","message":"translation error: %s"}}`, jsonEscape(err.Error())), http.StatusBadGateway)
			return
		}

		upstreamURL := openAIChatCompletionsURL(route.Endpoint)
		upstreamReq, err := http.NewRequestWithContext(r.Context(), "POST", upstreamURL, bytes.NewReader(openaiBody))
		if err != nil {
			http.Error(w, `{"type":"error","error":{"type":"api_error","message":"failed to create upstream request"}}`, http.StatusBadGateway)
			return
		}
		upstreamReq.Header.Set("Content-Type", "application/json")
		applyInferenceAuth(upstreamReq, route)

		p.logger.Info("inference forward",
			"agent", agentName,
			"backend", route.Backend,
			"model", route.Model,
			"url", upstreamURL,
			"openai_body", truncateBytes(openaiBody, 300),
		)

		client, err := inferenceHTTPClient(route)
		if err != nil {
			p.logger.Error("inference client setup failed", "agent", agentName, "error", err)
			http.Error(w, fmt.Sprintf(`{"type":"error","error":{"type":"api_error","message":"inference client setup failed: %s"}}`, err.Error()), http.StatusBadGateway)
			return
		}
		resp, err := client.Do(upstreamReq)
		if err != nil {
			if errors.Is(err, context.Canceled) && errors.Is(r.Context().Err(), context.Canceled) {
				p.logger.Debug("inference request canceled", "agent", agentName)
				return
			}
			p.logger.Error("inference upstream failed", "agent", agentName, "url", upstreamURL, "error", err)
			if p.gatewayHealth != nil {
				p.gatewayHealth.RecordEndpointError(route.Backend, route.Endpoint, err, time.Now())
			}
			unreachableMsg := inferenceBackendUnreachableMessage(err, upstreamURL)
			p.recordAgentInferenceError(agentName, unreachableMsg, 0)
			http.Error(w, fmt.Sprintf(`{"type":"error","error":{"type":"api_error","message":"%s"}}`, jsonEscape(unreachableMsg)), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		p.logger.Info("inference upstream response",
			"agent", agentName,
			"status", resp.StatusCode,
			"content-type", resp.Header.Get("Content-Type"),
		)

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			errBody, _ := io.ReadAll(resp.Body)
			p.logger.Error("inference upstream error",
				"agent", agentName,
				"status", resp.StatusCode,
				"url", upstreamURL,
				"server", resp.Header.Get("Server"),
				"body", truncateBytes(errBody, 500),
			)
			// A LiteLLM "team not allowed to access model" 403 carries the
			// entitled set — cache it so the dashboard's model list narrows
			// and its reconcile policy (#1848) can heal the agent.
			p.recordInferenceError(route, agentName, resp.StatusCode, errBody)
			backendMsg := inferenceBackendErrorMessage(resp.StatusCode, errBody, upstreamURL, resp.Header.Get("Server"))
			p.recordAgentInferenceError(agentName, backendMsg, resp.StatusCode)
			anthropicErr := fmt.Sprintf(`{"type":"error","error":{"type":"api_error","message":"%s"}}`,
				jsonEscape(backendMsg))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write([]byte(anthropicErr))
			return
		}

		// A 2xx means the gateway accepted the key — clear any latched
		// inference-auth failure so a hive whose key was fixed self-heals.
		p.recordInferenceSuccessFor(route)
		p.clearAgentInferenceError(agentName)

		isStreaming := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

		if isStreaming {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			in, out, err := translateOpenAISSEToAnthropic(resp.Body, w, route.Model)
			if err != nil {
				p.logger.Error("inference SSE translation failed", "agent", agentName, "error", err)
			}
			if in > 0 || out > 0 {
				p.recordUsage(agentName, route.Model, in, out)
			}
			return
		}

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(w, `{"type":"error","error":{"type":"api_error","message":"failed to read inference response"}}`, http.StatusBadGateway)
			return
		}

		p.logger.Info("inference upstream raw response",
			"agent", agentName,
			"len", len(respBody),
			"preview", truncateBytes(respBody, 500),
		)

		if in, out := extractOpenAIUsage(respBody); in > 0 || out > 0 {
			p.recordUsage(agentName, route.Model, in, out)
		}

		translated, err := translateOpenAIResponseToAnthropic(respBody, route.Model)
		if err != nil {
			p.logger.Error("inference translate response failed", "agent", agentName, "error", err)
			http.Error(w, `{"type":"error","error":{"type":"api_error","message":"response translation error"}}`, http.StatusBadGateway)
			return
		}

		p.logger.Info("inference translated response",
			"agent", agentName,
			"len", len(translated),
			"preview", truncateBytes(translated, 500),
		)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(translated)
	})

	return mux
}

// StartInferenceTranslator runs the Anthropic-compatible inference translator
// on the fixed local port used by agents.
func (p *GitHubProxy) StartInferenceTranslator() error {
	addr := fmt.Sprintf("127.0.0.1:%d", InferenceTranslatePort)
	p.logger.Info("inference translation server starting", "addr", addr)
	server := &http.Server{
		Addr:              addr,
		Handler:           p.inferenceTranslatorHandler(),
		ReadHeaderTimeout: httpReadTimeout,
	}
	return server.ListenAndServe()
}

// handleAnthropicReroute performs MITM on an Anthropic API connection and
// reroutes requests to a self-hosted vLLM/llm-d endpoint, translating
// between Anthropic and OpenAI API formats.
func (p *GitHubProxy) handleAnthropicReroute(conn net.Conn, r *http.Request, host, agentName string, route *InferenceRoute) {
	p.logger.Info("inference reroute", "agent", agentName, "backend", route.Backend, "host", host)

	if _, err := fmt.Fprintf(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		p.logger.Error("inference reroute: CONNECT response failed", "error", err)
		return
	}

	tlsCert, err := p.forgeCert(host)
	if err != nil {
		p.logger.Error("inference reroute: forge cert failed", "host", host, "error", err)
		return
	}

	tlsConn := tls.Server(conn, &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
	})
	_ = conn.SetDeadline(time.Now().Add(clientTLSHandshakeTimeout))
	handshakeErr := tlsConn.Handshake()
	_ = conn.SetDeadline(time.Time{})
	if handshakeErr != nil {
		p.logger.Warn("inference reroute: TLS handshake failed", "error", handshakeErr)
		return
	}
	defer func() { _ = tlsConn.Close() }()

	clientBuf := bufio.NewReader(tlsConn)
	for {
		_ = tlsConn.SetReadDeadline(time.Now().Add(httpReadTimeout))
		req, err := http.ReadRequest(clientBuf)
		_ = tlsConn.SetReadDeadline(time.Time{})
		if err != nil {
			p.logTimeout("inference reroute: client request read timed out", err, "agent", agentName)
			return
		}

		p.handleInferenceRequest(tlsConn, req, agentName, route)
	}
}

// handleInferenceRequest translates a single Anthropic API request and
// forwards it to the inference backend.
func (p *GitHubProxy) handleInferenceRequest(conn net.Conn, req *http.Request, agentName string, route *InferenceRoute) {
	route = p.routeWithEntitledModel(route, agentName)
	_ = conn.SetReadDeadline(time.Now().Add(httpReadTimeout))
	body, err := io.ReadAll(req.Body)
	_ = conn.SetReadDeadline(time.Time{})
	if req.Body != nil {
		_ = req.Body.Close()
	}
	if err != nil {
		p.logTimeout("inference reroute: request body read timed out", err, "agent", agentName)
		p.writeHTTPError(conn, http.StatusBadGateway, "failed to read request body")
		return
	}

	// Only POST /v1/messages is an inference call. The Claude Code CLI also
	// sends telemetry batches, error reports, and count_tokens to this host;
	// translating those bodies produced {"messages": null} and a gateway 400
	// per call, charged against the provider's request rate limit.
	if kind := classifyInferencePath(req.Method, req.URL.Path); kind != inferencePathMessages {
		status, respBody := p.localInferenceResponse(kind, req.Method, req.URL.Path, body, agentName)
		writeLocalInferenceResponse(conn, status, respBody)
		return
	}

	openaiBody, err := translateAnthropicToOpenAI(body, route.Model, route.MaxContextLen, resolveInferencePreamble(route, agentName))
	if err != nil {
		p.logger.Error("inference translate request failed", "agent", agentName, "error", err)
		p.recordAgentInferenceError(agentName, "translation error: "+err.Error(), 0)
		p.writeHTTPError(conn, http.StatusBadGateway, "translation error: "+err.Error())
		return
	}

	upstreamURL := openAIChatCompletionsURL(route.Endpoint)
	upstreamReq, err := http.NewRequestWithContext(req.Context(), "POST", upstreamURL, bytes.NewReader(openaiBody))
	if err != nil {
		p.writeHTTPError(conn, http.StatusBadGateway, "failed to create upstream request")
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	applyInferenceAuth(upstreamReq, route)

	p.logger.Info("inference forward", "agent", agentName, "backend", route.Backend, "model", route.Model, "url", upstreamURL)

	client, err := inferenceHTTPClient(route)
	if err != nil {
		p.logger.Error("inference client setup failed", "agent", agentName, "error", err)
		p.writeHTTPError(conn, http.StatusBadGateway, "inference client setup failed: "+err.Error())
		return
	}
	resp, err := client.Do(upstreamReq)
	if err != nil {
		p.logger.Error("inference upstream failed", "agent", agentName, "url", upstreamURL, "error", err)
		if p.gatewayHealth != nil {
			p.gatewayHealth.RecordEndpointError(route.Backend, route.Endpoint, err, time.Now())
		}
		unreachableMsg := inferenceBackendUnreachableMessage(err, upstreamURL)
		p.recordAgentInferenceError(agentName, unreachableMsg, 0)
		p.writeHTTPError(conn, http.StatusBadGateway, unreachableMsg)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		p.logger.Error("inference upstream error", "agent", agentName, "status", resp.StatusCode, "url", upstreamURL, "server", resp.Header.Get("Server"), "body", truncateBytes(errBody, 500))
		// Same entitlement capture as the HTTP translator path.
		p.recordInferenceError(route, agentName, resp.StatusCode, errBody)
		backendMsg := inferenceBackendErrorMessage(resp.StatusCode, errBody, upstreamURL, resp.Header.Get("Server"))
		p.recordAgentInferenceError(agentName, backendMsg, resp.StatusCode)
		anthropicErr := &http.Response{
			StatusCode: resp.StatusCode,
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(fmt.Sprintf(
				`{"type":"error","error":{"type":"api_error","message":"%s"}}`,
				jsonEscape(backendMsg)))),
		}
		_ = anthropicErr.Write(conn)
		return
	}

	// A 2xx means the gateway accepted the key — clear any latched
	// inference-auth failure so a hive whose key was fixed self-heals.
	p.recordInferenceSuccessFor(route)
	p.clearAgentInferenceError(agentName)

	isStreaming := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

	if isStreaming {
		hdr := "HTTP/1.1 200 OK\r\n" +
			"Content-Type: text/event-stream; charset=utf-8\r\n" +
			"Cache-Control: no-cache\r\n" +
			"Connection: keep-alive\r\n\r\n"
		if _, err := conn.Write([]byte(hdr)); err != nil {
			return
		}
		in, out, err := translateOpenAISSEToAnthropic(resp.Body, conn, route.Model)
		if err != nil {
			p.logger.Error("inference SSE translation failed", "agent", agentName, "error", err)
		}
		if in > 0 || out > 0 {
			p.recordUsage(agentName, route.Model, in, out)
		}
		return
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		p.writeHTTPError(conn, http.StatusBadGateway, "failed to read inference response")
		return
	}

	if in, out := extractOpenAIUsage(respBody); in > 0 || out > 0 {
		p.recordUsage(agentName, route.Model, in, out)
	}

	translated, err := translateOpenAIResponseToAnthropic(respBody, route.Model)
	if err != nil {
		p.logger.Error("inference translate response failed", "agent", agentName, "error", err)
		p.writeHTTPError(conn, http.StatusBadGateway, "response translation error")
		return
	}

	httpResp := &http.Response{
		StatusCode: http.StatusOK,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(translated)),
	}
	_ = httpResp.Write(conn)
}
