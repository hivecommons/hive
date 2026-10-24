// Package inference resolves where an agent's inference calls go and how they
// authenticate: the LiteLLM endpoint/model route, the watsonx model gateway,
// the bearer token and extra headers a gateway expects, and the supervision of
// the optional bundled local LiteLLM proxy.
//
// Extracted from cmd/hive's package main (#7238 stage 3). These are pure
// resolvers over the governor's AgentRuntimeGovernorConfig (the leaf type the
// host passes as &cfg.Governor.AgentRuntimeGovernorConfig) plus one supervisor
// loop; none of them needs
// anything from package main, and keeping them there meant the route
// resolution that decides whether every agent call 502s could only be tested
// from inside the binary.
package inference

import (
	"context"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/watsonx"
)

// ResolveLiteLLMRoute resolves the endpoint and model an agent's
// inference route should use for the built-in "litellm" backend. It is the
// whole route-install decision tree for that backend, lifted out of main() so
// it can be unit-tested (#5460); main() calls it and keeps ownership of key,
// CA bundle and logging.
//
// requestedModel is the model the agent asked for ("" when it named none). The
// returned model is that request when non-empty, otherwise the default
// inherited from whichever source supplied the endpoint.
//
// local_proxy wins outright: the Go translator forwards to the bundled litellm
// proxy on loopback, overriding any configured remote endpoint. Otherwise the
// decision is AgentRuntimeGovernorConfig.ResolveLiteLLMRemoteRoute — explicit
// gateway named by the backend first, legacy governor.litellm block second —
// which lives in pkg/config so the dashboard's Test Connection can print the
// very same route without importing this package (hivecommons/hive#9945).
//
// ok is false when no source yields an endpoint: the caller must warn and
// install NO route. It never invents an endpoint, and never returns a route
// with an empty endpoint — a silently empty endpoint is the 502 this whole
// path exists to prevent.
func ResolveLiteLLMRoute(gov *config.AgentRuntimeGovernorConfig, backend, requestedModel string) (endpoint, model string, ok bool) {
	lc := gov.LiteLLM
	if lc.LocalProxy {
		model = requestedModel
		if model == "" {
			model = lc.DefaultModel
		}
		return LocalLiteLLMProxyURL(), model, true
	}
	endpoint, model, source := gov.ResolveLiteLLMRemoteRoute(backend, requestedModel)
	if source == config.LiteLLMRouteNone || endpoint == "" {
		return "", requestedModel, false
	}
	return endpoint, model, true
}

// ExplicitLiteLLMGateway is AgentRuntimeGovernorConfig.ExplicitLiteLLMGateway
// behind a nil-safe pointer.
func ExplicitLiteLLMGateway(gov *config.AgentRuntimeGovernorConfig, backend string) *config.GatewayConfig {
	if gov == nil {
		return nil
	}
	return gov.ExplicitLiteLLMGateway(backend)
}

// ChatCompletionsURL is config.LiteLLMChatCompletionsURL: the exact URL the
// translator POSTs to for an endpoint.
func ChatCompletionsURL(endpoint string) string {
	return config.LiteLLMChatCompletionsURL(endpoint)
}

// ResolveWatsonxGateway finds the gateway backing the built-in "watsonx" agent
// backend. It prefers a gateway explicitly NAMED watsonx, then falls back to
// the first gateway of KIND watsonx — so `backend: watsonx` works whether the
// operator named their gateway "watsonx" or something descriptive like
// "ibm-granite-prod". Returns nil when no watsonx gateway is configured.
func ResolveWatsonxGateway(gov *config.AgentRuntimeGovernorConfig) *config.GatewayConfig {
	gws := gov.ResolvedGateways()
	for i := range gws {
		if strings.EqualFold(gws[i].Name, config.GatewayKindWatsonx) &&
			strings.EqualFold(gws[i].Kind, config.GatewayKindWatsonx) {
			gw := gws[i]
			return &gw
		}
	}
	for i := range gws {
		if strings.EqualFold(gws[i].Kind, config.GatewayKindWatsonx) {
			gw := gws[i]
			return &gw
		}
	}
	return nil
}

// ResolveGatewayAuth resolves the bearer token and non-secret extra headers an
// agent's inference route should present for a gateway.
//
// For every kind except watsonx this is the resolved API key verbatim and no
// extra headers. watsonx authenticates its OpenAI-compatible model gateway with
// a SHORT-LIVED IAM bearer minted from the IBM Cloud API key (not the raw key)
// and scopes billing/limits by a project id sent as X-IBM-Project-ID, so both
// are set here via the shared process-wide minter (pkg/watsonx.DefaultMinter),
// whose cache means one token is reused across inference, probes and discovery.
//
// Shared by the named-gateway branch and the built-in "watsonx" backend branch
// so the two cannot authenticate differently. Never logs the key or the token.
func ResolveGatewayAuth(gw *config.GatewayConfig, agentName, backend string, logger *slog.Logger) (string, map[string]string) {
	apiKey := gw.ResolveAPIKey()
	if !strings.EqualFold(gw.Kind, config.GatewayKindWatsonx) {
		return apiKey, nil
	}
	if token, err := watsonx.DefaultMinter.Token(context.Background(), apiKey); err != nil {
		logger.Warn("watsonx IAM token mint failed; agent inference will fail until the key/project are valid",
			"agent", agentName, "gateway", backend, "error", err.Error())
		// Leave apiKey as-is (the raw key). watsonx will reject it, surfacing a
		// clear upstream 401 rather than a silent success — better than dropping
		// the route entirely.
	} else {
		apiKey = token
	}
	var extraHeaders map[string]string
	if gw.ProjectID != "" {
		extraHeaders = map[string]string{watsonx.ProjectIDHeader: gw.ProjectID}
	}
	return apiKey, extraHeaders
}

const (
	// LocalLiteLLMProxyPort is the loopback port the bundled litellm proxy
	// listens on when governor.litellm.local_proxy is enabled. Distinct from
	// proxy.InferenceTranslatePort (18444): agents always talk to the Go
	// translator, which forwards to this local litellm instance. The value
	// lives in pkg/config so the dashboard can print the same URL.
	LocalLiteLLMProxyPort = config.LocalLiteLLMProxyPort
	// localLiteLLMConfigPath is the user-provided litellm proxy config
	// (model list, upstream keys) on the /data volume.
	localLiteLLMConfigPath = "/data/litellm/config.yaml"
	// localLiteLLMRestartDelay is the pause before restarting a crashed local
	// litellm proxy, to avoid a tight crash loop.
	localLiteLLMRestartDelay = 5 * time.Second
)

// LocalLiteLLMProxyURL is the endpoint the Go inference translator forwards
// to when the local litellm proxy fallback is enabled.
func LocalLiteLLMProxyURL() string {
	return config.LocalLiteLLMProxyURL()
}

// SuperviseLocalLiteLLM runs the bundled litellm binary as a local
// Anthropic-compat translator fallback (governor.litellm.local_proxy: true),
// restarting it on exit like StartInferenceTranslator's supervision.
// Agents never talk to it directly — the Go translator stays in front so
// per-agent attribution, mode enforcement, and the MITM proxy path are
// preserved.
func SuperviseLocalLiteLLM(ctx context.Context, logger *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		cmd := exec.CommandContext(ctx, "litellm",
			"--host", "127.0.0.1",
			"--port", strconv.Itoa(LocalLiteLLMProxyPort),
			"--config", localLiteLLMConfigPath)
		logger.Info("starting local litellm proxy",
			"port", LocalLiteLLMProxyPort, "config", localLiteLLMConfigPath)
		if err := cmd.Run(); err != nil {
			logger.Warn("local litellm proxy exited", "error", err)
		} else {
			logger.Warn("local litellm proxy exited cleanly; restarting")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(localLiteLLMRestartDelay):
		}
	}
}
