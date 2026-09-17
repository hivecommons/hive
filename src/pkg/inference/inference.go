// Package inference resolves where an agent's inference calls go and how they
// authenticate: the LiteLLM endpoint/model route, the watsonx model gateway,
// the bearer token and extra headers a gateway expects, and the supervision of
// the optional bundled local LiteLLM proxy.
//
// Extracted from cmd/hive's package main (#7238 stage 3). These are pure
// resolvers over *config.Config plus one supervisor loop; none of them needs
// anything from package main, and keeping them there meant the route
// resolution that decides whether every agent call 502s could only be tested
// from inside the binary.
package inference

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/watsonx"
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
// Resolution order — each step matches the behavior shipped in 231ca4b:
//
//  1. local_proxy: the Go translator forwards to the bundled litellm proxy on
//     loopback, overriding any configured remote endpoint.
//  2. the legacy governor.litellm block (HIVE_LITELLM_ENDPOINT or yaml), whose
//     default_model supplies the model.
//  3. the EXPLICIT gateway named by this backend. A hive configured only
//     through the Model Gateways tab leaves the legacy block empty; the key
//     and CA bundle already resolve from that gateway, so the endpoint must
//     too, or NO route is installed and every agent call dies "502 no
//     inference route" while the Gateways tab Test button happily passes
//     (ains-validation/pocketmini, 2026-08-31 — #5393).
//
// ok is false when no source yields an endpoint: the caller must warn and
// install NO route. It never invents an endpoint, and never returns a route
// with an empty endpoint — a silently empty endpoint is the 502 this whole
// path exists to prevent.
func ResolveLiteLLMRoute(cfg *config.Config, backend, requestedModel string) (endpoint, model string, ok bool) {
	lc := cfg.Governor.LiteLLM
	model = requestedModel
	endpoint = lc.ResolveEndpoint()
	if lc.LocalProxy {
		endpoint = LocalLiteLLMProxyURL()
	}
	if endpoint == "" {
		if gw := cfg.Governor.ResolveGateway(backend); gw != nil && gw.Endpoint != "" {
			endpoint = gw.Endpoint
			if model == "" {
				model = gw.DefaultModel
			}
		}
	}
	if endpoint == "" {
		return "", requestedModel, false
	}
	if model == "" {
		model = lc.DefaultModel
	}
	return endpoint, model, true
}

// ResolveWatsonxGateway finds the gateway backing the built-in "watsonx" agent
// backend. It prefers a gateway explicitly NAMED watsonx, then falls back to
// the first gateway of KIND watsonx — so `backend: watsonx` works whether the
// operator named their gateway "watsonx" or something descriptive like
// "ibm-granite-prod". Returns nil when no watsonx gateway is configured.
func ResolveWatsonxGateway(cfg *config.Config) *config.GatewayConfig {
	gws := cfg.Governor.ResolvedGateways()
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
	// translator, which forwards to this local litellm instance.
	LocalLiteLLMProxyPort = 18445
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
	return fmt.Sprintf("http://127.0.0.1:%d", LocalLiteLLMProxyPort)
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
