package main

import (
	"strings"

	"github.com/hivecommons/hive/crux/proxy"
	"github.com/hivecommons/hive/pkg/config"
)

// litellmInferenceKeyResolver returns the resolver the inference proxy uses to
// re-read a litellm route's key at request time (hive#18532). It runs the same
// resolution as the route's initial key (ResolveLiteLLMInferenceKeyWithSource)
// against the LIVE config, so a rotated Secret, a re-saved key file or a config
// reload all reach the next request without a restart.
func litellmInferenceKeyResolver(cfg *config.Config, backend string) *proxy.InferenceKeyResolver {
	return proxy.NewInferenceKeyResolver(backend, func() (string, string) {
		return cfg.Governor.ResolveLiteLLMInferenceKeyWithSource(backend)
	})
}

// gatewayInferenceKeyResolver is the named-gateway twin of
// litellmInferenceKeyResolver. It returns nil for watsonx: that route carries a
// short-lived IAM bearer minted from the key, not the key, so it keeps its
// existing minting path.
func gatewayInferenceKeyResolver(cfg *config.Config, gw *config.GatewayConfig, backend string) *proxy.InferenceKeyResolver {
	if gw == nil || strings.EqualFold(gw.Kind, config.GatewayKindWatsonx) {
		return nil
	}
	return proxy.NewInferenceKeyResolver(backend, func() (string, string) {
		if cur := cfg.Governor.ResolveGateway(backend); cur != nil {
			return cur.ResolveAPIKeyWithSource()
		}
		return "", ""
	})
}
