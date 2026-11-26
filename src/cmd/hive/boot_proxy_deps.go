package main

import (
	"context"
	"log/slog"

	"github.com/hivecommons/hive/crux"

	"github.com/hivecommons/hive/crux/proxy"
	"github.com/hivecommons/hive/pkg/jev"
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
	installInferenceCallbacks func(mgr crux.Host, setRoute func(agentName, backend, model string), clearRoute func(agentName string))
	setInferenceRoute         func(p *proxy.GitHubProxy, agentName string, route *proxy.InferenceRoute)
	clearInferenceRoute       func(p *proxy.GitHubProxy, agentName string)
	// installContextWindowLookup lets the manager read the context window
	// the proxy knows for each agent's route, to pass to the Claude CLI at
	// launch (hivecommons/hive#19913).
	installContextWindowLookup func(mgr crux.Host, p *proxy.GitHubProxy)
	// startProxy binds the GitHub MITM listener and the inference translator.
	startProxy        func(p *proxy.GitHubProxy, logger *slog.Logger)
	startLocalLiteLLM func(ctx context.Context, logger *slog.Logger)
	// startJev binds the loopback Jev decision endpoint agents with
	// jev_mode: assist call (hivecommons/hive#8939).
	startJev func(srv *jev.Server, logger *slog.Logger)
}

func defaultBootProxyDeps() bootProxyDeps {
	return bootProxyDeps{
		newGitHubProxy: proxy.NewGitHubProxy,
		installInferenceCallbacks: func(mgr crux.Host, setRoute func(agentName, backend, model string), clearRoute func(agentName string)) {
			mgr.SetInferenceCallbacks(setRoute, clearRoute)
		},
		setInferenceRoute: func(p *proxy.GitHubProxy, agentName string, route *proxy.InferenceRoute) {
			p.SetInferenceRoute(agentName, route)
		},
		clearInferenceRoute:        func(p *proxy.GitHubProxy, agentName string) { p.ClearInferenceRoute(agentName) },
		installContextWindowLookup: installInferenceContextWindowLookup,
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
		startJev: func(srv *jev.Server, logger *slog.Logger) {
			go func() {
				if err := srv.ListenAndServe(); err != nil {
					logger.Error("jev decision endpoint failed", "error", err)
				}
			}()
		},
	}
}

// inferenceContextWindowSetter is the manager method that takes the proxy's
// per-agent context-window lookup. The in-process agent manager has it; a
// manager reached across the agent-runtime sidecar boundary does not, and
// there only the agent's max_context_tokens setting reaches the CLI.
type inferenceContextWindowSetter interface {
	SetInferenceContextWindowLookup(fn func(agentName string) (window int, model string))
}

// installInferenceContextWindowLookup wires p's lookup into mgr when mgr
// takes one (two-value assertion: a host without the method is left alone).
func installInferenceContextWindowLookup(mgr crux.Host, p *proxy.GitHubProxy) {
	if p == nil {
		return
	}
	if s, ok := mgr.(inferenceContextWindowSetter); ok {
		s.SetInferenceContextWindowLookup(p.InferenceContextWindow)
	}
}
