package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/proxy"
	"github.com/hivecommons/hive/pkg/tokens"
)

// bootProxyFake records what bootProxyWith asked for. The route callbacks
// are captured so a test drives the resolution tree directly; every route
// write lands in routes / cleared instead of on the proxy.
type bootProxyFake struct {
	deps bootProxyDeps
	log  bytes.Buffer

	proxy      *proxy.GitHubProxy
	newErr     error
	setRoute   func(agentName, backend, model string)
	clearRoute func(agentName string)
	routes     map[string]*proxy.InferenceRoute
	cleared    []string
	started    *proxy.GitHubProxy
	litellmSup int
}

func newBootProxyFake() *bootProxyFake {
	f := &bootProxyFake{routes: map[string]*proxy.InferenceRoute{}}
	f.deps = bootProxyDeps{
		newGitHubProxy: func(logger *slog.Logger, org string, repos []string) (*proxy.GitHubProxy, error) {
			if f.newErr != nil {
				return nil, f.newErr
			}
			p, err := proxy.NewGitHubProxyEphemeral(logger, org, repos)
			f.proxy = p
			return p, err
		},
		installInferenceCallbacks: func(_ *agent.Manager, set func(string, string, string), clear func(string)) {
			f.setRoute, f.clearRoute = set, clear
		},
		setInferenceRoute: func(_ *proxy.GitHubProxy, agentName string, r *proxy.InferenceRoute) {
			f.routes[agentName] = r
		},
		clearInferenceRoute: func(_ *proxy.GitHubProxy, agentName string) { f.cleared = append(f.cleared, agentName) },
		startProxy:          func(p *proxy.GitHubProxy, _ *slog.Logger) { f.started = p },
		startLocalLiteLLM:   func(context.Context, *slog.Logger) { f.litellmSup++ },
	}
	return f
}

func bootProxyConfig() *config.Config {
	cfg := &config.Config{}
	cfg.HiveID = "boot-proxy-test"
	cfg.Project.Org = "acme"
	cfg.Project.Repos = []string{"widgets"}
	cfg.Agents = map[string]config.AgentConfig{}
	return cfg
}

func newBootProxyBoot(t *testing.T, f *bootProxyFake, cfg *config.Config) *boot {
	t.Helper()
	t.Setenv("HIVE_VLLM_ENDPOINT", "")
	t.Setenv("HIVE_LLMD_ENDPOINT", "")
	logger := slog.New(slog.NewTextHandler(&f.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg.Data.MetricsDir = t.TempDir()
	return &boot{
		ctx:                      ctx,
		cfg:                      cfg,
		logger:                   logger,
		agentMgr:                 agent.NewManager(cfg.Agents, logger, agent.ProjectContext{}),
		dashSrv:                  dashboard.NewServer(0, logger),
		tokenCollector:           tokens.NewCollector(t.TempDir(), logger),
		linearCredentialResolver: func() agent.LinearCredential { return agent.LinearCredential{} },
	}
}

func TestBootProxyWith_ConstructionFailureIsLoggedNotFatal(t *testing.T) {
	f := newBootProxyFake()
	f.newErr = errors.New("CA setup: /data is read-only")
	b := newBootProxyBoot(t, f, bootProxyConfig())

	b.bootProxyWith(f.deps)

	if f.started != nil || f.setRoute != nil {
		t.Fatal("proxy started or callbacks installed although construction failed")
	}
	if !strings.Contains(f.log.String(), "failed to create github proxy") {
		t.Fatalf("construction failure not reported:\n%s", f.log.String())
	}
}

func TestBootProxyWith_StartsTheProxyItBuiltAndSupervisesLiteLLMOnlyWhenLocal(t *testing.T) {
	t.Run("remote litellm", func(t *testing.T) {
		f := newBootProxyFake()
		b := newBootProxyBoot(t, f, bootProxyConfig())

		b.bootProxyWith(f.deps)

		if f.proxy == nil || f.started != f.proxy {
			t.Fatal("the proxy that was built is not the one that was started")
		}
		if f.litellmSup != 0 {
			t.Fatal("local LiteLLM supervisor started without local_proxy")
		}
		if f.setRoute == nil || f.clearRoute == nil {
			t.Fatal("inference route callbacks not installed on the manager")
		}
		if !strings.Contains(f.log.String(), "github proxy started") {
			t.Fatalf("start not logged:\n%s", f.log.String())
		}
	})
	t.Run("local litellm", func(t *testing.T) {
		f := newBootProxyFake()
		cfg := bootProxyConfig()
		cfg.Governor.LiteLLM.LocalProxy = true
		b := newBootProxyBoot(t, f, cfg)

		b.bootProxyWith(f.deps)

		if f.litellmSup != 1 {
			t.Fatalf("local LiteLLM supervisor started %d times, want 1", f.litellmSup)
		}
	})
}

func TestBootProxyWith_RouteResolution(t *testing.T) {
	cfg := bootProxyConfig()
	cfg.Governor.Gateways = []config.GatewayConfig{
		{Name: "openrouter", Kind: "openrouter", Endpoint: "https://openrouter.ai/api/v1", DefaultModel: "or/default"},
		{Name: "bare", Kind: "litellm"},
	}
	f := newBootProxyFake()
	b := newBootProxyBoot(t, f, cfg)
	// Unroutable loopback ports so the model probe fails fast instead of
	// resolving hostnames.
	t.Setenv("HIVE_VLLM_ENDPOINT", "http://127.0.0.1:1, http://127.0.0.1:2")
	t.Setenv("HIVE_LLMD_ENDPOINT", "http://127.0.0.1:3")
	b.bootProxyWith(f.deps)

	t.Run("named gateway routes through it and fills the default model", func(t *testing.T) {
		f.setRoute("scanner", "openrouter", "")
		r := f.routes["scanner"]
		if r == nil || r.Backend != "openrouter" || r.Endpoint != "https://openrouter.ai/api/v1" || r.Model != "or/default" {
			t.Fatalf("route = %+v, want the openrouter gateway with its default model", r)
		}
	})
	t.Run("gateway without endpoint sets no route", func(t *testing.T) {
		f.setRoute("reviewer", "bare", "m")
		if _, ok := f.routes["reviewer"]; ok {
			t.Fatal("route set through a gateway that has no endpoint")
		}
		if !strings.Contains(f.log.String(), "gateway backend selected but no endpoint configured") {
			t.Fatal("missing-endpoint gateway not reported")
		}
	})
	t.Run("litellm with nothing configured sets no route", func(t *testing.T) {
		f.setRoute("triage", "litellm", "m")
		if _, ok := f.routes["triage"]; ok {
			t.Fatal("litellm route set with no endpoint anywhere")
		}
		if !strings.Contains(f.log.String(), "litellm backend selected but no endpoint configured") {
			t.Fatal("missing litellm endpoint not reported")
		}
	})
	t.Run("watsonx without a watsonx gateway sets no route", func(t *testing.T) {
		f.setRoute("ibm", config.GatewayKindWatsonx, "m")
		if _, ok := f.routes["ibm"]; ok {
			t.Fatal("watsonx route set with no watsonx gateway")
		}
		if !strings.Contains(f.log.String(), "watsonx backend selected but no watsonx gateway is configured") {
			t.Fatal("missing watsonx gateway not reported")
		}
	})
	t.Run("vllm picks an endpoint from HIVE_VLLM_ENDPOINT", func(t *testing.T) {
		f.setRoute("fast", "vllm", "some-model")
		r := f.routes["fast"]
		if r == nil || r.Backend != "vllm" || !strings.HasPrefix(r.Endpoint, "http://127.0.0.1:") || r.Model != "some-model" {
			t.Fatalf("route = %+v, want a vllm endpoint from the env list", r)
		}
		if r.APIKey != "" || r.CABundle != "" {
			t.Fatal("vllm routes are unauthenticated: no key or CA bundle")
		}
	})
	t.Run("llm-d falls back to its first endpoint when none serves the model", func(t *testing.T) {
		f.setRoute("dist", "llm-d", "m")
		r := f.routes["dist"]
		if r == nil || r.Backend != "llm-d" || r.Endpoint != "http://127.0.0.1:3" {
			t.Fatalf("route = %+v, want the HIVE_LLMD_ENDPOINT fallback", r)
		}
		if !strings.Contains(f.log.String(), "no endpoint serves model, using first endpoint") {
			t.Fatal("fallback not reported")
		}
	})
	t.Run("clear callback clears", func(t *testing.T) {
		f.clearRoute("scanner")
		if f.cleared[len(f.cleared)-1] != "scanner" {
			t.Fatalf("cleared = %v, want scanner last", f.cleared)
		}
	})
}

func TestBootProxyWith_LiteLLMRoutesThroughTheLegacyBlock(t *testing.T) {
	cfg := bootProxyConfig()
	cfg.Governor.LiteLLM.Endpoint = "https://litellm.example.test"
	cfg.Governor.LiteLLM.CABundle = "/etc/ssl/corp.pem"
	f := newBootProxyFake()
	b := newBootProxyBoot(t, f, cfg)
	b.bootProxyWith(f.deps)

	f.setRoute("triage", "litellm", "gpt-x")
	r := f.routes["triage"]
	if r == nil || r.Backend != "litellm" || r.Endpoint != "https://litellm.example.test" || r.Model != "gpt-x" {
		t.Fatalf("route = %+v, want the legacy litellm block", r)
	}
	if r.CABundle != "/etc/ssl/corp.pem" {
		t.Fatalf("CABundle = %q, want the legacy block's bundle when no explicit gateways exist", r.CABundle)
	}
}

func TestBootProxyWith_VLLMWithoutEndpointsClearsTheRoute(t *testing.T) {
	f := newBootProxyFake()
	b := newBootProxyBoot(t, f, bootProxyConfig())
	b.bootProxyWith(f.deps)

	f.setRoute("fast", "vllm", "m")
	if _, ok := f.routes["fast"]; ok {
		t.Fatal("vllm route set with HIVE_VLLM_ENDPOINT unset")
	}
	if len(f.cleared) != 1 || f.cleared[0] != "fast" {
		t.Fatalf("cleared = %v, want fast cleared so a stale route cannot linger", f.cleared)
	}
	if !strings.Contains(f.log.String(), "inference backend selected but no endpoint configured") {
		t.Fatal("missing vllm endpoint not reported")
	}
}
