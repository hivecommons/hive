package main

import (
	"context"
	"log/slog"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/inference"
)

// This file is the thin wiring layer between the hive binary and the inference
// routing policy, which now lives in pkg/inference (#7238 stage 3).

func litellmLocalProxyURL() string {
	return inference.LocalProxyURL()
}

func resolveLiteLLMInferenceRoute(cfg *config.Config, backend, requestedModel string) (endpoint, model string, ok bool) {
	return inference.ResolveLiteLLMRoute(cfg, backend, requestedModel)
}

func resolveWatsonxGateway(cfg *config.Config) *config.GatewayConfig {
	return inference.ResolveWatsonxGateway(cfg)
}

func resolveGatewayAuth(gw *config.GatewayConfig, agentName, backend string, logger *slog.Logger) (string, map[string]string) {
	return inference.ResolveGatewayAuth(gw, agentName, backend, logger)
}

func superviseLocalLiteLLM(ctx context.Context, logger *slog.Logger) {
	inference.SuperviseLocalLiteLLM(ctx, logger)
}

func parseEndpointList(raw string) []string {
	return inference.ParseEndpointList(raw)
}
