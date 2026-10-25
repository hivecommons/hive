package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/api/callback"
	cruxconfig "github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/effects"
	"github.com/hivecommons/hive/crux/pushbroker"

	"github.com/hivecommons/hive/pkg/cruxhost"
	"github.com/hivecommons/hive/pkg/fleetreport"
	"github.com/hivecommons/hive/pkg/ioscan"
)

// eventObservable is the sidecar client's event subscription
// (crux/api/client.Runtime.SetEventObserver): every event on the runtime's
// /v1/events stream, including the crux.Host observer events the server
// relays (#14138 stage 3).
type eventObservable interface {
	SetEventObserver(fn func(api.Event))
}

// sidecarHost adapts the sidecar runtime client to hostedRuntime so the boot
// sequence is the same in both modes. The Host observers are real: they are
// fed from the event stream. The capability setters are no-ops because the
// runtime reaches those capabilities through the hive callback endpoint
// (cruxhost over callback.Listen, started by bootAgents). The policy setters
// the callback carries (#14138 stage 4) record their value here and the
// endpoint serves it as the GET /v1/policy snapshot the runtime polls; the
// rest (inference routes, mutation boundary, span starter) are still no-ops,
// each logged once, and a sidecar runtime runs on its own configuration for
// them.
type sidecarHost struct {
	crux.Runtime
	logger *slog.Logger

	mu           sync.Mutex
	installed    bool
	kickOutcome  func(agentName string, outcome crux.KickOutcome)
	pause        func(crux.PauseTransitionEvent)
	relaunch     func(agentName string)
	persistPause func(name string, paused bool)
	prompt       func(agent, trigger, prompt string)
	sandboxAudit func(agent, action, detail string)
	audit        crux.AuditSink
	conditions   crux.ConditionReporter
	noted        map[string]bool

	// Policy setter values, served to the runtime by policy().
	buildInfo     func() (string, string)
	gatewayCheck  func(string) bool
	explainMode   func() string
	autoMerge     func(string) bool
	kickLogSource func() string
	sandboxConfig *cruxconfig.AgentSandboxConfig
	deadOwner     *bool
}

var _ hostedRuntime = (*sidecarHost)(nil)

func newSidecarHost(rt crux.Runtime, logger *slog.Logger) *sidecarHost {
	if logger == nil {
		logger = slog.Default()
	}
	return &sidecarHost{Runtime: rt, logger: logger, noted: map[string]bool{}}
}

// setLogger replaces the construction-time logger with the boot's.
func (h *sidecarHost) setLogger(l *slog.Logger) {
	if l == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.logger = l
}

// ---- observers: fed from the event stream ----

// install subscribes to the event stream once, on the first observer.
func (h *sidecarHost) install() {
	if h.installed {
		return
	}
	src, ok := h.Runtime.(eventObservable)
	if !ok {
		h.logger.Warn("sidecar runtime has no event stream; host observers will not fire", "runtime", typeName(h.Runtime))
		h.installed = true
		return
	}
	h.installed = true
	src.SetEventObserver(h.dispatch)
}

func (h *sidecarHost) dispatch(ev api.Event) {
	h.mu.Lock()
	kick, pause, relaunch, persist, prompt, sandbox, audit, conds := h.kickOutcome, h.pause, h.relaunch, h.persistPause, h.prompt, h.sandboxAudit, h.audit, h.conditions
	h.mu.Unlock()
	decode := func(v any) bool {
		if err := json.Unmarshal(ev.Data, v); err != nil {
			h.logger.Warn("sidecar runtime: undecodable host event", "type", ev.Type, "agent", ev.Agent, "error", err)
			return false
		}
		return true
	}
	switch ev.Type {
	case api.EventKickOutcome:
		var o api.KickOutcome
		if kick != nil && decode(&o) {
			kick(ev.Agent, crux.KickOutcome{Kind: o.Kind, Reason: o.Reason, At: o.At, KickAt: o.KickAt})
		}
	case api.EventPauseTransition:
		var p api.PauseTransition
		if pause != nil && decode(&p) {
			pause(crux.PauseTransitionEvent{
				Agent: ev.Agent, Paused: p.Paused, Trigger: p.Trigger, Reason: p.Reason, By: p.By, At: p.At,
				Causation: crux.PauseCausation{Depth: p.Causation.Depth, HookName: p.Causation.HookName, OriginTransition: p.Causation.OriginTransition},
			})
		}
	case api.EventRelaunch:
		if relaunch != nil {
			relaunch(ev.Agent)
		}
	case api.EventPausePersist:
		var p api.PausePersist
		if persist != nil && decode(&p) {
			persist(ev.Agent, p.Paused)
		}
	case api.EventPrompt:
		var p api.Prompt
		if prompt != nil && decode(&p) {
			prompt(ev.Agent, p.Trigger, p.Prompt)
		}
	case api.EventSandboxAudit:
		var a api.SandboxAudit
		if sandbox != nil && decode(&a) {
			sandbox(ev.Agent, a.Action, a.Detail)
		}
	case api.EventAudit:
		var a api.Audit
		if audit != nil && decode(&a) {
			audit.Record(a.Actor, a.Action, ev.Agent, a.Fields)
		}
	case api.EventCondition:
		var c api.ConditionEvent
		if conds != nil && decode(&c) {
			if c.Action == api.ConditionClear {
				conds.Clear(c.Class, c.Key)
			} else {
				conds.Observe(c.Class, c.Key, c.Message, c.Diagnostics)
			}
		}
	}
}

func (h *sidecarHost) SetKickOutcomeObserver(fn func(agentName string, outcome crux.KickOutcome)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.kickOutcome = fn
	h.install()
}

func (h *sidecarHost) SetPauseTransitionObserver(fn func(crux.PauseTransitionEvent)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pause = fn
	h.install()
}

func (h *sidecarHost) SetRelaunchObserver(fn func(agentName string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.relaunch = fn
	h.install()
}

func (h *sidecarHost) SetPersistPauseCallback(fn func(name string, paused bool)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.persistPause = fn
	h.install()
}

func (h *sidecarHost) SetRecordPromptCallback(fn func(agent, trigger, prompt string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prompt = fn
	h.install()
}

func (h *sidecarHost) SetSandboxAuditCallback(fn func(agent, action, detail string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sandboxAudit = fn
	h.install()
}

func (h *sidecarHost) SetAuditSink(sink crux.AuditSink) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.audit = sink
	h.install()
}

func (h *sidecarHost) SetConditionReporter(r crux.ConditionReporter) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conditions = r
	h.install()
}

// ---- capabilities: served to the runtime over the hive callback ----

func (h *sidecarHost) SetAppAuth(crux.AppTokenMinter)        { h.callbackCarried("SetAppAuth") }
func (h *sidecarHost) SetAgentMint(crux.AgentMintIssuer)     { h.notCarried("SetAgentMint") }
func (h *sidecarHost) SetBobAPIKeyResolver(func() string)    { h.notCarried("SetBobAPIKeyResolver") }
func (h *sidecarHost) SetBobKeySourceResolver(func() string) { h.notCarried("SetBobKeySourceResolver") }
func (h *sidecarHost) SetLinearCredentialResolver(func() crux.LinearCredential) {
	h.callbackCarried("SetLinearCredentialResolver")
}
func (h *sidecarHost) SetSandboxPushMinter(pushbroker.TokenMinter) {
	h.callbackCarried("SetSandboxPushMinter")
}
func (h *sidecarHost) SetSandboxPRClient(crux.PRCreator) { h.callbackCarried("SetSandboxPRClient") }

// ---- policy: carried to the runtime as the callback's policy snapshot ----

func (h *sidecarHost) SetBuildInfo(fn func() (string, string)) {
	h.setPolicy("SetBuildInfo", func() { h.buildInfo = fn })
}
func (h *sidecarHost) SetGatewayBackendChecker(fn func(string) bool) {
	h.setPolicy("SetGatewayBackendChecker", func() { h.gatewayCheck = fn })
}
func (h *sidecarHost) SetExplainModeDefaultResolver(fn func() string) {
	h.setPolicy("SetExplainModeDefaultResolver", func() { h.explainMode = fn })
}
func (h *sidecarHost) SetRepoAutoMergeEnabledResolver(fn func(string) bool) {
	h.setPolicy("SetRepoAutoMergeEnabledResolver", func() { h.autoMerge = fn })
}
func (h *sidecarHost) SetKickLogSourceResolver(fn func() string) {
	h.setPolicy("SetKickLogSourceResolver", func() { h.kickLogSource = fn })
}
func (h *sidecarHost) SetSandboxConfig(cfg cruxconfig.AgentSandboxConfig) {
	h.setPolicy("SetSandboxConfig", func() { h.sandboxConfig = &cfg })
}
func (h *sidecarHost) SetDeadSessionRecoveryOwner(ownedElsewhere bool) {
	h.setPolicy("SetDeadSessionRecoveryOwner", func() { h.deadOwner = &ownedElsewhere })
}

// Not carried yet: the inference routes configure hive's proxy and move
// with it; the mutation boundary and span starter wrap runtime-side calls
// and have no serialisable form.
func (h *sidecarHost) SetInferenceCallbacks(func(string, string, string), func(string)) {
	h.notCarried("SetInferenceCallbacks")
}
func (h *sidecarHost) SetSandboxMutationBoundary(effects.Boundary) {
	h.notCarried("SetSandboxMutationBoundary")
}
func (h *sidecarHost) SetSpanStarter(crux.SpanStarter) { h.notCarried("SetSpanStarter") }

func (h *sidecarHost) setPolicy(setter string, store func()) {
	h.mu.Lock()
	store()
	h.mu.Unlock()
	h.callbackCarried(setter)
}

// policy is the GET /v1/policy snapshot: one field per policy setter boot
// has called, resolved now. gateways lists the configured gateway names;
// the stored checker decides which of them the runtime may route to.
func (h *sidecarHost) policy(gateways func() []string) callback.Policy {
	h.mu.Lock()
	build, gw, explain, kick, sandbox, owner := h.buildInfo, h.gatewayCheck, h.explainMode, h.kickLogSource, h.sandboxConfig, h.deadOwner
	h.mu.Unlock()
	var p callback.Policy
	if build != nil {
		v, c := build()
		p.Build = &callback.BuildInfo{Version: v, Commit: c}
	}
	if explain != nil {
		v := explain()
		p.ExplainModeDefault = &v
	}
	if kick != nil {
		v := kick()
		p.KickLogSource = &v
	}
	if gw != nil {
		names := []string{}
		if gateways != nil {
			for _, name := range gateways() {
				if gw(name) {
					names = append(names, name)
				}
			}
		}
		p.GatewayBackends = &names
	}
	if owner != nil {
		v := *owner
		p.DeadSessionRecoveryOwnedElsewhere = &v
	}
	if sandbox != nil {
		if b, err := json.Marshal(sandbox); err == nil {
			p.Sandbox = b
		}
	}
	return p
}

// repoAutoMergeEnabled answers the per-repo auto-merge call with the stored
// resolver, default-on (the manager's nil-resolver behaviour) until boot
// sets one.
func (h *sidecarHost) repoAutoMergeEnabled(repo string) bool {
	h.mu.Lock()
	fn := h.autoMerge
	h.mu.Unlock()
	if fn == nil {
		return true
	}
	return fn(repo)
}

func (h *sidecarHost) callbackCarried(setter string) {
	h.note(setter, "sidecar runtime: setter is served by the hive callback endpoint instead")
}

func (h *sidecarHost) notCarried(setter string) {
	h.note(setter, "sidecar runtime: setter not carried; the runtime runs on its own configuration (#14138 stage 4)")
}

func (h *sidecarHost) note(setter, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.noted[setter] {
		return
	}
	h.noted[setter] = true
	h.logger.Info(msg, "setter", setter)
}

func typeName(v any) string {
	if v == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%T", v)
}

// ---- hive side of the callback: the capabilities hive serves the runtime ----

// hiveCallbackCapabilities is the cruxhost capability set hive serves a
// sidecar runtime: the same values the in-process wiring hands the manager
// directly. Values that are not yet constructed when bootAgents runs (the
// Linear resolver, from bootSupervision) are read lazily; nil ones report
// not_configured and the runtime treats them as the nil seam.
func (b *boot) hiveCallbackCapabilities() *cruxhost.Capabilities {
	deps := cruxhost.Deps{
		REST:       githubRESTAccounting{},
		Canary:     ioscan.DefaultCanaries,
		Text:       ioscan.ToolApproveScanner{},
		Conditions: fleetreport.DefaultReporter{},
		Linear: func() (string, string) {
			if b.linearCredentialResolver == nil {
				return "", ""
			}
			c := b.linearCredentialResolver()
			return c.AccessToken, c.APIKey
		},
	}
	if b.appAuth != nil {
		deps.Tokens = b.appAuth
	}
	if b.ghClient != nil {
		deps.PullRequests = b.ghClient
	}
	if sh, ok := b.agentMgr.(*sidecarHost); ok {
		deps.Policy = func() callback.Policy { return sh.policy(b.gatewayNames) }
		deps.RepoAutoMerge = sh.repoAutoMergeEnabled
	}
	return cruxhost.New(deps)
}

// gatewayNames lists the configured model gateway names off the live config.
func (b *boot) gatewayNames() []string {
	if b.cfg == nil {
		return nil
	}
	var names []string
	for _, gw := range b.cfg.Governor.ResolvedGateways() {
		if gw.Name != "" {
			names = append(names, gw.Name)
		}
	}
	return names
}

// startHiveCallback serves caps on the callback socket ($CRUX_HIVE_SOCKET,
// default next to the runtime socket) and, when $CRUX_HIVE_LISTEN is set,
// on TCP with the $CRUX_HIVE_TOKEN bearer. The server runs until ctx ends.
func startHiveCallback(ctx context.Context, caps callback.Capabilities, logger *slog.Logger) error {
	token := os.Getenv(callback.TokenEnvVar)
	ls, err := callback.Listen(callback.ListenConfig{Socket: callback.DefaultSocket(), Listen: os.Getenv(callback.ListenEnvVar), Token: token})
	if err != nil {
		return err
	}
	logger.Info("hive callback endpoint serving", "addrs", ls.Addrs())
	go func() {
		if err := callback.NewServer(caps, token).Serve(ctx, ls); err != nil {
			logger.Error("hive callback endpoint stopped", "error", err)
		}
	}()
	return nil
}
