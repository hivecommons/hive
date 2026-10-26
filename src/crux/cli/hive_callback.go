package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agent"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/api/callback"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/effects"
	"github.com/hivecommons/hive/crux/hivecap"
	"github.com/hivecommons/hive/crux/pushbroker"
	"github.com/hivecommons/hive/crux/serve"
	"github.com/hivecommons/hive/crux/toolapprove"
)

// hiveCapTarget is the subset of *agent.Manager setters the hive callback
// feeds in sidecar mode: the seams hive provides in-process through
// pkg/cruxhost (hivecommons/hive#14138 stage 2). A runtime that is not the
// manager (a test fake) is left alone.
type hiveCapTarget interface {
	SetSandboxPRClient(agent.PRCreator)
	SetLinearCredentialResolver(func() agent.LinearCredential)
	SetSandboxPushMinter(pushbroker.TokenMinter)
	SetInferenceCallbacks(setRoute func(agentName, backend, model string), clearRoute func(agentName string))
	SetSandboxMutationBoundary(effects.Boundary)
	// Credential writers (hivecommons/hive#14138 stage 4).
	SetAppAuth(agent.AppTokenMinter)
	SetAgentMint(agent.AgentMintIssuer)
	SetBobAPIKeyResolver(func() string)
	SetBobKeySourceResolver(func() string)
}

// hivePolicyTarget is the subset of crux.Host policy setters the hive
// callback's policy snapshot feeds (hivecommons/hive#14138 stage 4): the
// values an in-process hive hands the manager directly.
type hivePolicyTarget interface {
	SetBuildInfo(func() (version, commit string))
	SetGatewayBackendChecker(func(backend string) bool)
	SetExplainModeDefaultResolver(func() string)
	SetRepoAutoMergeEnabledResolver(func(repo string) bool)
	SetKickLogSourceResolver(func() string)
	SetSandboxConfig(config.AgentSandboxConfig)
	SetDeadSessionRecoveryOwner(ownedElsewhere bool)
}

// policyApplier turns each new policy snapshot into setter calls. Resolvers
// are installed once, the first time hive reports the value, and then read
// the live snapshot; the pushed values (sandbox config, dead-session owner)
// are set again whenever they change. A value hive does not report leaves
// the runtime's own default in place.
type policyApplier struct {
	target hivePolicyTarget
	policy *hivecap.Policy
	errw   io.Writer

	mu        sync.Mutex
	installed map[string]bool
	sandbox   []byte
	owner     *bool
}

func newPolicyApplier(target hivePolicyTarget, policy *hivecap.Policy, errw io.Writer) *policyApplier {
	return &policyApplier{target: target, policy: policy, errw: errw, installed: map[string]bool{}}
}

func (a *policyApplier) apply(pol callback.Policy) {
	a.mu.Lock()
	defer a.mu.Unlock()
	once := func(name string, present bool, install func()) {
		if present && !a.installed[name] {
			a.installed[name] = true
			install()
		}
	}
	p := a.policy
	once("build", pol.Build != nil, func() { a.target.SetBuildInfo(p.BuildInfo) })
	once("explain_mode_default", pol.ExplainModeDefault != nil, func() { a.target.SetExplainModeDefaultResolver(p.ExplainModeDefault) })
	once("kick_log_source", pol.KickLogSource != nil, func() { a.target.SetKickLogSourceResolver(p.KickLogSource) })
	once("gateway_backends", pol.GatewayBackends != nil, func() { a.target.SetGatewayBackendChecker(p.GatewayBackend) })
	once("repo_auto_merge", pol.RepoAutoMerge, func() { a.target.SetRepoAutoMergeEnabledResolver(p.RepoAutoMergeEnabled) })
	if v := pol.DeadSessionRecoveryOwnedElsewhere; v != nil && (a.owner == nil || *a.owner != *v) {
		owned := *v
		a.owner = &owned
		a.target.SetDeadSessionRecoveryOwner(owned)
	}
	if len(pol.Sandbox) > 0 && !bytes.Equal(pol.Sandbox, a.sandbox) {
		var cfg config.AgentSandboxConfig
		if err := json.Unmarshal(pol.Sandbox, &cfg); err != nil {
			fmt.Fprintf(a.errw, "crux: hive policy: undecodable sandbox config, keeping the previous one: %v\n", err)
			return
		}
		a.sandbox = append([]byte(nil), pol.Sandbox...)
		a.target.SetSandboxConfig(cfg)
	}
}

// hiveCallback is `crux serve --hive-callback`: the hive capability endpoint
// the runtime dials for the capabilities it does not own.
type hiveCallback struct {
	client *callback.Client
	set    *hivecap.Set
}

// dialHiveCallback builds the client and adapters for addr (a form
// api.ParseAddress accepts), presenting token when non-empty. Nothing is
// called yet: every adapter fails closed until hive answers, and
// hivecap.Watch reports the outage.
func dialHiveCallback(addr, token string) (*hiveCallback, error) {
	c, err := callback.NewClient(addr, token)
	if err != nil {
		return nil, fmt.Errorf("hive callback %s: %w", addr, err)
	}
	return &hiveCallback{client: c, set: hivecap.New(c, 0)}, nil
}

// install feeds the manager and process-wide seams from the adapters,
// reports the endpoint on the server's health route, and starts the
// condition watch, which ends with ctx. It reports to w which seams were
// wired so an operator can see the difference between a runtime that is a
// manager and one that is not.
func (h *hiveCallback) install(ctx context.Context, rt crux.Runtime, srv *serve.Server, w io.Writer) {
	srv.HiveCallback = h.health
	// Conditions go through the server's tee, not the manager setter: Serve
	// installs its own reporter on a Host runtime (so conditions reach the
	// event stream) and forwards to srv.ConditionReporter; setting the
	// manager directly would be overwritten when Serve starts.
	srv.ConditionReporter = h.set.Conditions
	toolapprove.SetDefaultTextScanner(h.set.Text)
	seams := "text scanner, conditions"
	if m, ok := rt.(hiveCapTarget); ok {
		m.SetSandboxPRClient(h.set.PullRequest)
		linear := h.set.Linear
		m.SetLinearCredentialResolver(func() agent.LinearCredential {
			tok, key := linear.Resolve()
			return agent.LinearCredential{AccessToken: tok, APIKey: key}
		})
		m.SetSandboxPushMinter(pushbroker.GitHubAppMinter{Auth: h.set.TokenScoper("", 0)})
		// Routes are pushed by a worker so the manager's launch path never
		// waits on hive; the boundary fails closed when hive is away.
		m.SetInferenceCallbacks(h.set.Inference.Set, h.set.Inference.Clear)
		go h.set.Inference.Run(ctx, 0)
		m.SetSandboxMutationBoundary(h.set.Mutation)
		// Credential writers: App tokens and the opt-in mint are minted by
		// hive and only cached here; Bob's key is read through hive. All
		// fail closed (no credential) when hive is away.
		m.SetAppAuth(h.set.AppTokens)
		m.SetAgentMint(h.set.Mint)
		m.SetBobAPIKeyResolver(h.set.Bob.Key)
		m.SetBobKeySourceResolver(h.set.Bob.Source)
		seams += ", pull requests, Linear credential, push tokens, inference routes, mutation boundary, App tokens, agent mint, Bob credential"
	}
	fmt.Fprintf(w, "hive callback %s: %s\n", addressString(h.client.Address()), seams)
	// hive_callback_unavailable describes the callback being down, so it
	// goes to the stream only and never over the callback.
	go hivecap.Watch(ctx, h.client.Status, srv.StreamConditions(), 0)
}

// watchPolicy polls hive's policy snapshot (GET /v1/policy) until ctx ends
// and feeds it to rt's crux.Host policy setters (#14138 stage 4). A runtime
// without them is left alone; a hive without the policy capability answers
// not_configured and the runtime keeps its own configuration.
func (h *hiveCallback) watchPolicy(ctx context.Context, rt crux.Runtime, w, errw io.Writer) {
	m, ok := rt.(hivePolicyTarget)
	if !ok {
		return
	}
	go hivecap.WatchPolicy(ctx, h.set.Policy, 0, newPolicyApplier(m, h.set.Policy, errw).apply)
	fmt.Fprintf(w, "hive callback %s: host policy\n", addressString(h.client.Address()))
}

// health is the status Health reports.
func (h *hiveCallback) health() api.HiveCallback {
	st := h.client.Status()
	out := api.HiveCallback{Address: addressString(h.client.Address()), Reachable: st.Reachable, LastError: st.LastError, FailedCalls: st.FailedCalls}
	if !st.Since.IsZero() {
		since := st.Since
		out.Since = &since
	}
	if !st.LastSuccess.IsZero() {
		last := st.LastSuccess
		out.LastSuccess = &last
	}
	return out
}

// addressString renders an address the way serve.Listeners.Addrs does.
func addressString(a api.Address) string {
	if a.Network == "unix" {
		return "unix://" + a.Path
	}
	return a.BaseURL
}
