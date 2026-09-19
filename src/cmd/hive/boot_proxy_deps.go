package main

import (
	"context"
	"log/slog"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/proxy"
)

// bootProxyDeps are the effects bootProxy performs that a test cannot let run
// for real (#7571, step 2): constructing the proxy (its CA lives under
// /data), binding its two listeners, supervising the local LiteLLM, and the
// per-agent route writes the manager triggers at launch. The route
// resolution — gateway / litellm / watsonx / vllm / llm-d decision tree — is
// captured through installInferenceCallbacks and driven directly, with
// setInferenceRoute / clearInferenceRoute recording what it decided.
type bootProxyDeps struct {
	newGitHubProxy            func(logger *slog.Logger, org string, repos []string) (*proxy.GitHubProxy, error)
	installInferenceCallbacks func(mgr *agent.Manager, setRoute func(agentName, backend, model string), clearRoute func(agentName string))
	setInferenceRoute         func(p *proxy.GitHubProxy, agentName string, route *proxy.InferenceRoute)
	clearInferenceRoute       func(p *proxy.GitHubProxy, agentName string)
	// startProxy binds the GitHub MITM listener and the inference translator.
	startProxy        func(p *proxy.GitHubProxy, logger *slog.Logger)
	startLocalLiteLLM func(ctx context.Context, logger *slog.Logger)
}

func defaultBootProxyDeps() bootProxyDeps {
	return bootProxyDeps{
		newGitHubProxy: proxy.NewGitHubProxy,
		installInferenceCallbacks: func(mgr *agent.Manager, setRoute func(agentName, backend, model string), clearRoute func(agentName string)) {
			mgr.SetInferenceCallbacks(setRoute, clearRoute)
		},
		setInferenceRoute: func(p *proxy.GitHubProxy, agentName string, route *proxy.InferenceRoute) {
			p.SetInferenceRoute(agentName, route)
		},
		clearInferenceRoute: func(p *proxy.GitHubProxy, agentName string) { p.ClearInferenceRoute(agentName) },
		startProxy: func(p *proxy.GitHubProxy, logger *slog.Logger) {
			go func() {
				if err := p.Start(); err != nil {
					logger.Error("github proxy failed", "error", err)
				}
			}()
			go func() {
				if err := p.StartInferenceTranslator(); err != nil {
					logger.Error("inference translation server failed", "error", err)
				}
			}()
		},
		startLocalLiteLLM: func(ctx context.Context, logger *slog.Logger) { go superviseLocalLiteLLM(ctx, logger) },
	}
}
