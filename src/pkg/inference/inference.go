// Package inference resolves how an agent reaches a model: which endpoint and
// model an inference route should target, which gateway backs a given backend,
// and what credentials that gateway expects. It also supervises the optional
// bundled litellm proxy.
//
// This logic was extracted from cmd/hive's package main (#7238) so it is
// testable and reusable outside the hive binary.
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

const (
	// LocalProxyPort is the loopback port the bundled litellm proxy
	// listens on when governor.litellm.local_proxy is enabled. Distinct from
	// proxy.InferenceTranslatePort (18444): agents always talk to the Go
	// translator, which forwards to this local litellm instance.
	LocalProxyPort = 18445
	// LocalConfigPath is the user-provided litellm proxy config
	// (model list, upstream keys) on the /data volume.
	LocalConfigPath = "/data/litellm/config.yaml"
	// RestartDelay is the pause before restarting a crashed local
	// litellm proxy, to avoid a tight crash loop.
	RestartDelay = 5 * time.Second
)

// LocalProxyURL is the endpoint the Go inference translator forwards
// to when the local litellm proxy fallback is enabled.
func LocalProxyURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", LocalProxyPort)
}

// ResolveLiteLLMRoute resolves the endpoint and model an agent's
// inference route should target.
//
// The local proxy overrides the configured endpoint when enabled. When no
// litellm endpoint resolves at all, the named gateway for the agent's backend
// is consulted: configuring inference purely through the Model Gateways tab
// leaves the legacy block empty, and the key and CA bundle already resolve
// from that gateway, so the endpoint must too — or NO route is installed and
// every agent call dies "502 no inference route" while the Gateways tab Test
// button happily passes (ains-validation/pocketmini, 2026-08-31 — #5393).
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
		endpoint = LocalProxyURL()
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
			"--port", strconv.Itoa(LocalProxyPort),
			"--config", LocalConfigPath)
		logger.Info("starting local litellm proxy",
			"port", LocalProxyPort, "config", LocalConfigPath)
		if err := cmd.Run(); err != nil {
			logger.Warn("local litellm proxy exited", "error", err)
		} else {
			logger.Warn("local litellm proxy exited cleanly; restarting")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(RestartDelay):
		}
	}
}

// ParseEndpointList splits a comma-separated list of URLs into a slice.
// A single URL is returned as a one-element slice.
func ParseEndpointList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
