package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/discord"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/proxy"
	"github.com/hivecommons/hive/pkg/tokens"
	"github.com/hivecommons/hive/pkg/watsonx"
)

func (w *spokeWire) wireSpokeProxyReadyAndLaunch() {
	// Per-agent probe supersedes the backend-level one: under the per-agent-UID
	// layout each agent has its own HOME, so the shared credential path is empty
	// even for authenticated agents (see pkg/agent/authprobe.go).
	dashboard.SetAgentAuthProvider(w.agentMgr.AgentAuthAvailable)

	canaryLeakHandler := func(leak ioscan.CanaryLeak) {
		detail := fmt.Sprintf("rule=%s, agent=%s, source=%s", ioscan.CanaryLeakRule, leak.Agent, leak.Source)
		w.dashSrv.AuditLog(leak.Agent, "ioscan_canary_leak", detail, leak.Agent)
		if store, ok := w.beadStores[leak.Agent]; ok && store != nil {
			if b, berr := store.Create("Canary token leaked via "+leak.Source, beads.TypeAdvisory, beads.PriorityCritical, leak.Agent, ""); berr == nil {
				_ = store.SetMetadata(b.ID, "rule", ioscan.CanaryLeakRule)
				_ = store.SetMetadata(b.ID, "source", leak.Source)
			}
		}
	}
	if w.ghClient != nil {
		w.ghClient.SetCanaryScanner(w.cfg.Ioscan.IsEnabled() && w.cfg.Ioscan.Canaries, w.cfg.Ioscan.FailClosed(), ioscan.DefaultCanaries, canaryLeakHandler)
	}

	var err error
	w.githubProxy, err = proxy.NewGitHubProxy(w.logger, w.cfg.Project.Org, w.cfg.Project.Repos)
	if err != nil {
		w.logger.Error("failed to create github proxy", "error", err)
	} else {
		w.githubProxy.SetCanaryScanner(w.cfg.Ioscan.IsEnabled() && w.cfg.Ioscan.Canaries, w.cfg.Ioscan.FailClosed(), ioscan.DefaultCanaries, canaryLeakHandler)
		// #1861: the proxy resolves an identified agent to its hub-held scoped
		// token via the package-level registry WriteAgentToken feeds (NOT via
		// the w.appAuth instance, which is replaced on key rotation — a closure
		// over it would strand the proxy on the stale instance). Wired
		// unconditionally: with HIVE_PROXY_INJECT_GH_AUTH unset (the default)
		// the proxy never consults the source and the registry stays empty.
		w.githubProxy.SetAgentTokenSource(github.AgentProxyToken)
		dashboard.SetProxyViolationsProvider(w.githubProxy.Violations)
		// Lets the dashboard narrow the LiteLLM model dropdown to the set the
		// configured key is entitled to, learned by the proxy from a key-info
		// probe or a "team not allowed" 403.
		dashboard.SetEntitledModelsProvider(w.githubProxy.EntitledModels)
		// Surface a stale/invalid inference gateway key (repeated 401s on every
		// inference call) as a hive health signal: the proxy latches the failure
		// after several consecutive rejections and clears it on the next success,
		// and the heartbeat builder reports it to the hub (both as an immediate
		// advisory-staleness cause and as a dedicated inference-auth alert).
		dashboard.SetInferenceAuthProvider(w.githubProxy.InferenceAuthError)
		// #4294: the provider spending-limit signal, read by the eval cycle to
		// raise an advisory and stop kicking agents at a gateway that is
		// refusing on a money limit.
		dashboard.SetInferenceBudgetProvider(w.githubProxy.InferenceBudgetExceeded)

		// Wire the inference token sink so the translator records per-agent
		// usage (from the gateway's OpenAI usage block) into the same metrics
		// dir the token collector scans. Without this, bare-mode inference
		// agents (litellm/vllm/llm-d) never write a scannable session file and
		// their consumption reads as zero.
		w.githubProxy.SetTokenSink(tokens.NewInferenceSink(w.cfg.Data.MetricsDir, w.logger))

		// With the sink active, the proxy also MITMs the Copilot completion host
		// (api.githubcopilot.com) to record Copilot token usage live per
		// response — so Copilot cost shows up while an agent runs instead of only
		// tallying at session shutdown. Tell the collector to defer Copilot token
		// accrual to the sink ONLY for sessions active from NOW on (the moment
		// live capture starts). Sessions that ended earlier were never sniffed by
		// the proxy, so the scanner keeps counting their shutdown tokens —
		// otherwise all pre-existing Copilot spend would vanish.
		w.tokenCollector.SetCopilotLiveCapture(time.Now().UnixMilli())

		vllmEndpoints := parseEndpointList(envOrDefault("HIVE_VLLM_ENDPOINT", "http://hive-vllm-svc.hive-inference.svc.cluster.local:8000"))
		llmdEndpoints := parseEndpointList(envOrDefault("HIVE_LLMD_ENDPOINT", "http://hive-llm-d-epp.hive-inference.svc.cluster.local:8000"))
		inferenceEndpoints := map[string][]string{
			"vllm":  vllmEndpoints,
			"llm-d": llmdEndpoints,
		}
		// litellm has no in-cluster default: register it only when an
		// endpoint is configured (yaml or HIVE_LITELLM_ENDPOINT), so an
		// unconfigured backend doesn't show up in model discovery. A URL
		// w.saved later from the governor LiteLLM tab is registered at
		// runtime via w.dashSrv.UpdateInferenceEndpoint.
		if w.cfg.Governor.LiteLLM.LocalProxy {
			inferenceEndpoints["litellm"] = []string{litellmLocalProxyURL()}
		} else if litellmEndpoint := w.cfg.Governor.LiteLLM.ResolveEndpoint(); litellmEndpoint != "" {
			inferenceEndpoints["litellm"] = parseEndpointList(litellmEndpoint)
		}
		// Register every explicitly-configured named gateway's endpoint by
		// gateway NAME so the Model Gateways tab's per-gateway model discovery
		// and per-gateway routing resolve on boot (the legacy "litellm" block
		// is already registered above; ResolvedGateways only synthesizes it
		// when no explicit gateways are set, so this loop never double-adds it).
		for _, gw := range w.cfg.Governor.Gateways {
			if ep := strings.TrimSpace(gw.Endpoint); ep != "" {
				inferenceEndpoints[gw.Name] = parseEndpointList(ep)
			}
		}
		w.dashSrv.SetInferenceEndpoints(inferenceEndpoints)
		// The gateway-name predicate (SetGatewayBackendChecker) is wired right
		// after the manager is constructed — see the comment there (#3961): it
		// must be live before the persisted-state replay re-applies w.saved
		// backend overrides, which happens well before this point.
		w.agentMgr.SetInferenceCallbacks(
			func(agentName, backend, model string) {
				// Named model gateway (OpenRouter, a second LiteLLM, etc.): resolve
				// endpoint/key/model from the gateway and route through it. Built-in
				// backend names (litellm/vllm/llm-d) are handled below; a gateway
				// literally named "litellm" resolves here to the same legacy block
				// via ResolvedGateways, so behavior is identical.
				if gw := w.cfg.Governor.ResolveGateway(backend); gw != nil && !config.IsInferenceBackend(backend) {
					endpoint := gw.Endpoint
					if endpoint == "" {
						w.logger.Warn("gateway backend selected but no endpoint configured",
							"agent", agentName, "gateway", backend, "model", model)
						return
					}
					if model == "" {
						model = gw.DefaultModel
					}
					// watsonx authenticates the OpenAI-compatible model gateway
					// with a short-lived IAM bearer minted from the IBM Cloud API
					// key (NOT the raw key), and scopes billing/limits by a
					// project id sent as X-IBM-Project-ID. Mint (cached) and set
					// both here; every other kind sends the resolved key verbatim.
					apiKey, extraHeaders := resolveGatewayAuth(gw, agentName, backend, w.logger)
					w.githubProxy.SetInferenceRoute(agentName, &proxy.InferenceRoute{
						Backend:      backend,
						Endpoint:     endpoint,
						Model:        model,
						APIKey:       apiKey,
						CABundle:     gw.CABundle,
						ExtraHeaders: extraHeaders,
					})
					return
				}
				if backend == "litellm" {
					// Resolve endpoint/key at call time so a URL w.saved from
					// the governor LiteLLM tab (or a rotated key) takes
					// effect without a hive restart. w.cfg is the live config
					// pointer — the config watcher swaps its contents in
					// place on reload.
					lc := w.cfg.Governor.LiteLLM
					// Endpoint/model resolution lives in a pure function so the
					// decision tree (local proxy / legacy block / explicit-gateway
					// fallback / no route at all) is unit-testable — it is not
					// reachable from a test while inline in main(). See #5460.
					endpoint, resolvedModel, ok := resolveLiteLLMInferenceRoute(w.cfg, backend, model)
					if !ok {
						w.logger.Warn("litellm backend selected but no endpoint configured",
							"agent", agentName, "model", model)
						return
					}
					model = resolvedModel
					// Key source must MATCH the entitlement/probe path (gateways.go,
					// cost.go, openrouter.go), which resolve the key from the gateway
					// via ResolveGateway(backend).ResolveAPIKey(). When an EXPLICIT
					// `gateways:` block names this backend, that gateway carries its
					// own api_key_file (e.g. the key w.saved from the Model Gateways
					// tab). Reading the legacy Governor.LiteLLM key file here instead
					// would send a DIFFERENT (often stale) key than entitlement
					// validated, causing inference 401s after a key rotation done via
					// the Gateways tab. Resolve from the same gateway so inference and
					// entitlement always agree on one key source.
					//
					// Only explicit gateways override: ResolvedGateways synthesizes an
					// implicit "litellm" gateway from the legacy block when no
					// `gateways:` are set, but that synthetic gateway lacks the
					// multi-location file fallback of LiteLLMConfig.ResolveAPIKey
					// (k8s Secret mount + PVC copy). For no-gateway hives we therefore
					// keep the legacy resolver to preserve today's behavior.
					apiKey := w.cfg.Governor.ResolveLiteLLMInferenceKey(backend)
					caBundle := lc.CABundle
					if len(w.cfg.Governor.Gateways) > 0 {
						if gw := w.cfg.Governor.ResolveGateway(backend); gw != nil {
							caBundle = gw.CABundle
						}
					}
					w.githubProxy.SetInferenceRoute(agentName, &proxy.InferenceRoute{
						Backend:  backend,
						Endpoint: endpoint,
						Model:    model,
						APIKey:   apiKey,
						CABundle: caBundle,
					})
					return
				}
				if backend == config.GatewayKindWatsonx {
					// Built-in "watsonx" backend: the operator set
					// `backend: watsonx` without a gateway literally NAMED
					// watsonx (a named one is handled by the gateway branch
					// above). Resolve the watsonx gateway by KIND so the
					// endpoint, IBM Cloud key, project id and region all come
					// from the existing `gateways:` plumbing rather than being
					// re-derived here.
					gw := resolveWatsonxGateway(w.cfg)
					if gw == nil {
						w.logger.Warn("watsonx backend selected but no watsonx gateway is configured; add one under the Model Gateways tab",
							"agent", agentName, "model", model)
						return
					}
					// Region-only gateways are legal (the guided form can save a
					// region without an endpoint), so fall back to the shared
					// region template — the same helper the dashboard preset uses.
					endpoint := strings.TrimSpace(gw.Endpoint)
					if endpoint == "" {
						endpoint = watsonx.EndpointForRegion(gw.Region)
					}
					if model == "" {
						model = gw.DefaultModel
					}
					apiKey, extraHeaders := resolveGatewayAuth(gw, agentName, backend, w.logger)
					w.githubProxy.SetInferenceRoute(agentName, &proxy.InferenceRoute{
						Backend:      backend,
						Endpoint:     endpoint,
						Model:        model,
						APIKey:       apiKey,
						CABundle:     gw.CABundle,
						ExtraHeaders: extraHeaders,
					})
					return
				}
				endpoints := vllmEndpoints
				if backend == "llm-d" {
					endpoints = llmdEndpoints
				}
				// vllm/llm-d endpoints are unauthenticated with a public
				// or in-cluster CA — no bearer key or custom CA bundle.
				endpoint := proxy.FindEndpointForModel(endpoints, model, "", "")
				if endpoint == "" {
					w.logger.Warn("no endpoint serves model, using first endpoint",
						"agent", agentName, "model", model, "backend", backend)
					endpoint = endpoints[0]
				}
				w.githubProxy.SetInferenceRoute(agentName, &proxy.InferenceRoute{
					Backend:  backend,
					Endpoint: endpoint,
					Model:    model,
				})
			},
			func(agentName string) {
				w.githubProxy.ClearInferenceRoute(agentName)
			},
		)

		go func() {
			if err := w.githubProxy.Start(); err != nil {
				w.logger.Error("github proxy failed", "error", err)
			}
		}()
		go func() {
			if err := w.githubProxy.StartInferenceTranslator(); err != nil {
				w.logger.Error("inference translation server failed", "error", err)
			}
		}()
		if w.cfg.Governor.LiteLLM.LocalProxy {
			go superviseLocalLiteLLM(w.ctx, w.logger)
		}
		w.logger.Info("github proxy started", "addr", w.githubProxy.ListenAddr())
	}

	go func() {
		if err := w.dashSrv.Start(); err != nil {
			w.logger.Error("dashboard server failed", "error", err)
		}
	}()

	if w.cfg.Notifications.Discord != nil && w.cfg.Notifications.Discord.BotToken != "" && w.cfg.Notifications.Discord.ChannelID != "" {
		discordBot := discord.NewBot(discord.Config{
			Token:          w.cfg.Notifications.Discord.BotToken,
			ChannelID:      w.cfg.Notifications.Discord.ChannelID,
			DashboardURL:   fmt.Sprintf("http://localhost:%d", w.cfg.Dashboard.Port),
			DashboardToken: os.Getenv("HIVE_DASHBOARD_TOKEN"),
			AllowedUsers:   w.cfg.Notifications.Discord.AllowedUsers,
		}, w.logger)
		var agentNameList []string
		for name := range w.cfg.EnabledAgents() {
			agentNameList = append(agentNameList, name)
		}
		discordBot.SetAgentNames(agentNameList)
		if err := discordBot.Start(w.ctx); err != nil {
			w.logger.Warn("discord bot failed to start", "error", err)
		} else {
			w.logger.Info("discord bot started", "channel", w.cfg.Notifications.Discord.ChannelID)
		}
	}

	w.onDemandFromPack = config.OnDemandAgentsFromPacks()
	if len(w.onDemandFromPack) > 0 {
		w.logger.Info("on-demand agents from pack definitions", "agents", w.onDemandFromPack)
	}
	// One visible "hive restarted" marker per boot, so the audit log shows a
	// restart happened (and at what build) instead of only a burst of
	// per-agent agent_start rows. Include the persisted pauses being restored
	// so the operator can confirm pause state survived the restart — broken
	// down by trigger, and EXCLUDING agents that are startup-paused by design
	// (on-demand agents like brainstorm), whose inclusion turned "restoring 9
	// paused agent(s)" into a false systemic-incident signal on every upgrade
	// restart of a deliberately owner-quiesced fleet (#4041).
	w.dashSrv.AuditLog("system", "hive_restart",
		fmt.Sprintf("build=%s version=%s; %s", gitShort, version,
			pausedRestoreDetail(w.cfg.EnabledAgents(), w.onDemandFromPack, w.agentMgr.AllStatuses())), "")

	// Mark the dashboard READY as soon as the HTTP server can serve requests —
	// which is NOW: config is loaded, GitHub client/App auth are wired, the
	// dashboard deps are set, and the listener (go w.dashSrv.Start() above) is up.
	// None of /api/*, /sso, /open, /api/livez or /api/health depend on the agent
	// fleet being up; the frontend already handles agents appearing over time.
	//
	// This MUST precede the staggered agent-launch loop below. That loop sleeps
	// ~15s per agent (× the whole fleet = several minutes) and previously ran
	// BEFORE MarkReady, so /api/livez returned 503 "starting" for the entire
	// launch window. The liveness probe (period 30s × failureThreshold 3 ≈ 90s)
	// then SIGKILLed the container (exit 137) before readiness was ever reached
	// on cold start, and rolling upgrades left the Service with no Ready endpoint
	// for minutes → 503s on /open and /sso. Flipping ready here makes the pod
	// Ready in seconds and moves the fleet spin-up entirely off the critical path.
	w.dashSrv.MarkReady()

	// Launch the persistent (non-on-demand) agents in the BACKGROUND so the
	// staggered start no longer gates pod readiness. The loop honors w.ctx: on
	// shutdown the w.ctx-aware stagger returns immediately instead of leaking a
	// goroutine parked in a bare time.Sleep.
	go func() {
		const agentLaunchDelaySec = 15
		agentIndex := 0
		for name, ac := range w.cfg.EnabledAgents() {
			isOnDemand := ac.OnDemand || w.onDemandFromPack[name]
			if isOnDemand {
				w.logger.Info("skipping on-demand agent at startup", "name", name)
				continue
			}
			if agentIndex > 0 {
				w.logger.Info("staggering agent launch", "name", name, "delay_sec", agentLaunchDelaySec)
				select {
				case <-time.After(time.Duration(agentLaunchDelaySec) * time.Second):
				case <-w.ctx.Done():
					w.logger.Info("aborting staggered agent launch: shutting down")
					return
				}
			}
			// Bail before starting another agent if we are already shutting down,
			// so a SIGTERM during the launch window doesn't spawn fresh processes.
			if w.ctx.Err() != nil {
				w.logger.Info("aborting staggered agent launch: shutting down")
				return
			}
			w.logger.Info("audit: starting agent", "name", name, "trigger", "startup")
			if err := w.agentMgr.Start(w.ctx, name); err != nil {
				w.logger.Warn("failed to start agent", "name", name, "error", err)
			} else {
				// Surface whether a persisted operator pause was honored on this
				// restart, so the audit log shows pause state survived (or didn't).
				detail := "trigger=startup"
				if ac.Paused {
					detail = "trigger=startup; restored paused (persisted)"
				}
				w.dashSrv.AuditLog("system", "agent_start", detail, name)
			}
			agentIndex++
		}
	}()

}
