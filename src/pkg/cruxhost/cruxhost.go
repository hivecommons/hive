// Package cruxhost is hive's end of the Crux callback contract
// (crux/api/callback): it turns hive's existing implementations of the
// capabilities a sidecar runtime needs (App-scoped tokens, REST accounting,
// usage sink, canary and text scanners, condition reporter, Linear
// credential, PR creation) into one callback.Capabilities the runtime can
// call back into, over the hive-served socket or directly in-process
// (hivecommons/hive#14138 stage 2; src/docs/design/crux.md "Credentials and
// hive-provided capabilities").
//
// Deps is deliberately small interfaces, not pkg types, so this package
// stays free of pkg/github, pkg/ioscan, pkg/tokens and pkg/fleetreport and
// cmd/hive can wire whichever it has. A nil Deps field is a capability this
// hive does not provide; the server answers 404 not_configured for it and
// the runtime treats that as the nil seam.
//
// Stage 3 wires it in cmd/hive as, roughly:
//
//	caps := cruxhost.New(cruxhost.Deps{
//		Tokens:     appAuth,                 // *github.AppAuth
//		REST:       githubRESTAccounting{},  // the existing adapter
//		Usage:      inferenceSink,           // *tokens.InferenceSink
//		Canary:     canaryRegistry,          // *ioscan.CanaryRegistry
//		Text:       ioscan.ToolApproveScanner{},
//		Conditions: fleetreport.DefaultReporter{},
//		Linear:     linearResolver,          // func() (accessToken, apiKey string)
//		PullRequests: ghClient,              // *github.Client
//	})
//	ls, _ := callback.Listen(callback.ListenConfig{Socket: callback.DefaultSocket()})
//	go callback.NewServer(caps, "").Serve(ctx, ls)
//
// and, in-process, hands the same caps straight to hivecap.New.
package cruxhost

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api/callback"
	"github.com/hivecommons/hive/crux/effects"
	"github.com/hivecommons/hive/crux/toolapprove"
)

// InstallationTokenTTL is the lifetime GitHub gives an installation access
// token; the mint response reports it when the scoper does not say.
const InstallationTokenTTL = time.Hour

// TokenScoper is *github.AppAuth's ScopedTokenForRepos. repos are bare
// repository names; empty means installation-wide.
type TokenScoper interface {
	ScopedTokenForRepos(ctx context.Context, tier string, repos []string) (string, error)
}

// RESTAccounting is cmd/hive's githubRESTAccounting (and
// proxy.GitHubRESTAccounting), restated here so cruxhost does not import
// the proxy.
type RESTAccounting interface {
	RecordRESTRequest(caller, method, path string, status int, h http.Header)
	RecordInjectedRESTRequest(caller, method, path string, status int, h http.Header)
	CoreRateLimitSnapshot() (remaining int, reset time.Time, ok bool)
}

// UsageSink is *tokens.InferenceSink's three record paths.
type UsageSink interface {
	Record(agent, model string, inputTokens, outputTokens int64)
	RecordCopilot(agent, model string, inputTokens, outputTokens int64)
	RecordCopilotWithCache(agent, model string, inputTokens, outputTokens, cacheRead, cacheCreation int64)
}

// CanaryScanner is *ioscan.CanaryRegistry.Scan.
type CanaryScanner interface {
	Scan(agent, text, source string) (crux.CanaryLeak, bool)
}

// TextScanner is ioscan.ToolApproveScanner.
type TextScanner interface {
	ScanInput(text string) toolapprove.TextVerdict
	ScanOutput(text string) toolapprove.TextVerdict
}

// ConditionReporter is fleetreport.DefaultReporter (crux.ConditionReporter).
type ConditionReporter interface {
	Observe(class, key, message, diagnostics string)
	Clear(class, key string)
}

// LinearResolver returns the Linear credential hive would inject, both
// empty when there is none.
type LinearResolver func() (accessToken, apiKey string)

// PRCreator is *github.Client.CreatePR.
type PRCreator interface {
	CreatePR(ctx context.Context, repo, head, base, title, body string) (crux.PRResult, error)
}

// PolicyResolver returns the crux.Host policy snapshot hive would hand an
// in-process runtime through the policy setters; it is read on every
// GET /v1/policy so a config reload shows on the runtime's next poll.
type PolicyResolver func() callback.Policy

// RepoAutoMergeResolver is the SetRepoAutoMergeEnabledResolver predicate.
type RepoAutoMergeResolver func(repo string) bool

// Deps is what hive has. Nil fields are capabilities this hive does not
// provide.
type Deps struct {
	Tokens       TokenScoper
	REST         RESTAccounting
	Usage        UsageSink
	Canary       CanaryScanner
	Text         TextScanner
	Conditions   ConditionReporter
	Linear       LinearResolver
	PullRequests PRCreator
	// Policy and RepoAutoMerge back the policy routes (#14138 stage 4);
	// Policy nil means the capability is absent. RepoAutoMerge nil with a
	// Policy answers the per-repo call not_configured.
	Policy        PolicyResolver
	RepoAutoMerge RepoAutoMergeResolver
	// Inference is the proxy's route table (SetInferenceCallbacks) and
	// Mutation hive's journaling boundary (SetSandboxMutationBoundary),
	// both #14138 stage 4; nil means the capability is absent. EffectTTL
	// bounds a begun effect without its end (zero: DefaultEffectTTL).
	Inference InferenceRouter
	Mutation  effects.Boundary
	EffectTTL time.Duration
	// Mint is the opt-in agent mint issuer (SetAgentMint) and Bob the Bob
	// key resolvers (SetBobAPIKeyResolver/SetBobKeySourceResolver), both
	// #14138 stage 4 credential writers; nil means the capability is
	// absent. A Mint whose Enabled is false answers an empty token, the
	// in-process "mint off" contract.
	Mint AgentMintIssuer
	Bob  BobResolver
}

// AgentMintIssuer is crux.AgentMintIssuer restated so cruxhost does not
// import crux/agent.
type AgentMintIssuer interface {
	Enabled() bool
	MintAgentToken(agentName, tier string) (string, error)
}

// BobResolver returns the Bob API key and its source ("file:<path>",
// "env:<NAME>", "" when unset).
type BobResolver func() (apiKey, source string)

// Capabilities is callback.Capabilities over Deps. It also implements
// callback.Configured.
type Capabilities struct {
	deps    Deps
	effects *EffectBroker
}

var (
	_ callback.Capabilities    = (*Capabilities)(nil)
	_ callback.Configured      = (*Capabilities)(nil)
	_ callback.PolicySource    = (*Capabilities)(nil)
	_ callback.InferenceRouter = (*Capabilities)(nil)
	_ callback.EffectBoundary  = (*Capabilities)(nil)
	_ callback.AgentMinter     = (*Capabilities)(nil)
	_ callback.BobCredentials  = (*Capabilities)(nil)
)

// New builds the capability set over deps.
func New(deps Deps) *Capabilities {
	return &Capabilities{deps: deps, effects: NewEffectBroker(deps.Mutation, deps.EffectTTL)}
}

// Effects exposes the broker (for Pending in tests and health).
func (c *Capabilities) Effects() *EffectBroker { return c.effects }

// Configured reports whether the named capability has a backing Dep.
func (c *Capabilities) Configured(capability string) bool {
	switch capability {
	case callback.CapabilityTokens:
		return c.deps.Tokens != nil
	case callback.CapabilityGitHubREST:
		return c.deps.REST != nil
	case callback.CapabilityUsage:
		return c.deps.Usage != nil
	case callback.CapabilityCanary:
		return c.deps.Canary != nil
	case callback.CapabilityText:
		return c.deps.Text != nil
	case callback.CapabilityConditions:
		return c.deps.Conditions != nil
	case callback.CapabilityLinear:
		return c.deps.Linear != nil
	case callback.CapabilityPulls:
		return c.deps.PullRequests != nil
	case callback.CapabilityPolicy:
		return c.deps.Policy != nil
	case callback.CapabilityInferenceRoutes:
		return c.deps.Inference != nil
	case callback.CapabilityEffects:
		return c.deps.Mutation != nil
	case callback.CapabilityAgentMint:
		return c.deps.Mint != nil
	case callback.CapabilityBob:
		return c.deps.Bob != nil
	}
	return false
}

// Handler is the callback HTTP handler for deps: the thing hive mounts.
// token empty means the socket's permissions are the auth (callback.Server
// then admits only unix-tagged requests).
func Handler(deps Deps, token string) http.Handler {
	return callback.NewServer(New(deps), token).Handler()
}

// MintToken narrows an App token to req.Repo ∪ req.Repos at req.Tier. The
// repository-scoped token GitHub returns lives InstallationTokenTTL
// regardless of the requested TTL; the response says so. The App private
// key never leaves the scoper.
func (c *Capabilities) MintToken(ctx context.Context, req callback.MintTokenRequest) (callback.MintTokenResponse, error) {
	if c.deps.Tokens == nil {
		return callback.MintTokenResponse{}, callback.ErrNotConfigured
	}
	repos := append([]string(nil), req.Repos...)
	if name := bareRepo(req.Repo); name != "" {
		repos = append(repos, name)
	}
	tok, err := c.deps.Tokens.ScopedTokenForRepos(ctx, req.Tier, repos)
	if err != nil {
		return callback.MintTokenResponse{}, fmt.Errorf("mint token for %q: %w", req.Agent, err)
	}
	if tok == "" {
		return callback.MintTokenResponse{}, fmt.Errorf("mint token for %q: scoper returned no token", req.Agent)
	}
	ttl := InstallationTokenTTL
	if req.TTLSeconds > 0 && time.Duration(req.TTLSeconds)*time.Second < ttl {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	return callback.MintTokenResponse{Token: tok, ExpiresAt: time.Now().Add(ttl)}, nil
}

// bareRepo strips an "owner/" prefix.
func bareRepo(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

// RecordREST forwards one relayed GitHub REST response.
func (c *Capabilities) RecordREST(_ context.Context, rec callback.RESTRecord) error {
	if c.deps.REST == nil {
		return callback.ErrNotConfigured
	}
	h := make(http.Header, len(rec.RateLimit))
	for k, v := range rec.RateLimit {
		h.Set(k, v)
	}
	if rec.Injected {
		c.deps.REST.RecordInjectedRESTRequest(rec.Caller, rec.Method, rec.Path, rec.Status, h)
	} else {
		c.deps.REST.RecordRESTRequest(rec.Caller, rec.Method, rec.Path, rec.Status, h)
	}
	return nil
}

// RateLimit is the shared core-bucket snapshot.
func (c *Capabilities) RateLimit(context.Context) (callback.RateLimitSnapshot, error) {
	if c.deps.REST == nil {
		return callback.RateLimitSnapshot{}, callback.ErrNotConfigured
	}
	remaining, reset, ok := c.deps.REST.CoreRateLimitSnapshot()
	return callback.RateLimitSnapshot{Remaining: remaining, Reset: reset, OK: ok}, nil
}

// RecordUsage forwards one inference usage record by kind.
func (c *Capabilities) RecordUsage(_ context.Context, rec callback.UsageRecord) error {
	if c.deps.Usage == nil {
		return callback.ErrNotConfigured
	}
	switch rec.Kind {
	case callback.UsageKindGateway:
		c.deps.Usage.Record(rec.Agent, rec.Model, rec.InputTokens, rec.OutputTokens)
	case callback.UsageKindCopilot:
		c.deps.Usage.RecordCopilot(rec.Agent, rec.Model, rec.InputTokens, rec.OutputTokens)
	case callback.UsageKindCopilotCache:
		c.deps.Usage.RecordCopilotWithCache(rec.Agent, rec.Model, rec.InputTokens, rec.OutputTokens, rec.CacheRead, rec.CacheCreation)
	default:
		return fmt.Errorf("usage kind %q: %w", rec.Kind, errUnknownKind)
	}
	return nil
}

var errUnknownKind = errors.New("unknown usage kind")

// ScanCanary runs hive's canary registry over the text. The matched token
// stays on this side.
func (c *Capabilities) ScanCanary(_ context.Context, req callback.CanaryScanRequest) (callback.CanaryScanResponse, error) {
	if c.deps.Canary == nil {
		return callback.CanaryScanResponse{}, callback.ErrNotConfigured
	}
	leak, ok := c.deps.Canary.Scan(req.Agent, req.Text, req.Source)
	if !ok {
		return callback.CanaryScanResponse{}, nil
	}
	return callback.CanaryScanResponse{Detected: true, Agent: leak.Agent, Source: leak.Source}, nil
}

// ScanText runs hive's text scanner in the requested direction.
func (c *Capabilities) ScanText(_ context.Context, req callback.TextScanRequest) (callback.TextScanResponse, error) {
	if c.deps.Text == nil {
		return callback.TextScanResponse{}, callback.ErrNotConfigured
	}
	var v toolapprove.TextVerdict
	switch req.Direction {
	case callback.ScanInput:
		v = c.deps.Text.ScanInput(req.Text)
	case callback.ScanOutput:
		v = c.deps.Text.ScanOutput(req.Text)
	default:
		return callback.TextScanResponse{}, fmt.Errorf("scan direction %q: %w", req.Direction, errUnknownDirection)
	}
	resp := callback.TextScanResponse{Blocked: v.Blocked, CriticalInjection: v.CriticalInjection}
	for _, f := range v.Findings {
		resp.Findings = append(resp.Findings, callback.TextFinding{Kind: f.Kind, Snippet: f.Snippet, Rule: f.Rule, High: f.High})
	}
	return resp, nil
}

var errUnknownDirection = errors.New("unknown scan direction")

// ReportCondition applies one Observe or Clear.
func (c *Capabilities) ReportCondition(_ context.Context, rep callback.ConditionReport) error {
	if c.deps.Conditions == nil {
		return callback.ErrNotConfigured
	}
	switch rep.Action {
	case callback.ConditionObserve:
		c.deps.Conditions.Observe(rep.Class, rep.Key, rep.Message, rep.Diagnostics)
	case callback.ConditionClear:
		c.deps.Conditions.Clear(rep.Class, rep.Key)
	default:
		return fmt.Errorf("condition action %q: %w", rep.Action, errUnknownAction)
	}
	return nil
}

var errUnknownAction = errors.New("unknown condition action")

// LinearCredential returns the credential hive would inject.
func (c *Capabilities) LinearCredential(context.Context) (callback.LinearCredential, error) {
	if c.deps.Linear == nil {
		return callback.LinearCredential{}, callback.ErrNotConfigured
	}
	tok, key := c.deps.Linear()
	return callback.LinearCredential{AccessToken: tok, APIKey: key}, nil
}

// MintAgentToken issues req.Agent's mint credential at req.Tier. Mint off
// (Enabled false) is an empty token with no error, as in-process; the
// expiry is advisory (the issuer does not report one) and left zero.
func (c *Capabilities) MintAgentToken(_ context.Context, req callback.AgentMintRequest) (callback.AgentMintResponse, error) {
	if c.deps.Mint == nil {
		return callback.AgentMintResponse{}, callback.ErrNotConfigured
	}
	if !c.deps.Mint.Enabled() {
		return callback.AgentMintResponse{}, nil
	}
	tok, err := c.deps.Mint.MintAgentToken(req.Agent, req.Tier)
	if err != nil {
		return callback.AgentMintResponse{}, fmt.Errorf("mint agent token for %q: %w", req.Agent, err)
	}
	return callback.AgentMintResponse{Token: tok}, nil
}

// BobCredential returns the Bob key hive would inject and where it came from.
func (c *Capabilities) BobCredential(context.Context) (callback.BobCredential, error) {
	if c.deps.Bob == nil {
		return callback.BobCredential{}, callback.ErrNotConfigured
	}
	key, source := c.deps.Bob()
	return callback.BobCredential{APIKey: key, Source: source}, nil
}

// CreatePR opens (or finds) the PR as the App.
func (c *Capabilities) CreatePR(ctx context.Context, req callback.CreatePRRequest) (callback.CreatePRResponse, error) {
	if c.deps.PullRequests == nil {
		return callback.CreatePRResponse{}, callback.ErrNotConfigured
	}
	res, err := c.deps.PullRequests.CreatePR(ctx, req.Repo, req.Head, req.Base, req.Title, req.Body)
	if err != nil {
		return callback.CreatePRResponse{}, err
	}
	return callback.CreatePRResponse{
		Number:          res.Number,
		URL:             res.URL,
		Author:          res.Author,
		AlreadyExisted:  res.AlreadyExisted,
		DuplicateTree:   res.DuplicateTree,
		DuplicateReason: res.DuplicateReason,
	}, nil
}

// Policy returns the policy snapshot. RepoAutoMerge is reported from
// whether the per-repo resolver exists, whatever the resolver says.
func (c *Capabilities) Policy(context.Context) (callback.Policy, error) {
	if c.deps.Policy == nil {
		return callback.Policy{}, callback.ErrNotConfigured
	}
	p := c.deps.Policy()
	p.RepoAutoMerge = c.deps.RepoAutoMerge != nil
	return p, nil
}

// RepoAutoMergeEnabled answers the per-repository auto-merge switch.
func (c *Capabilities) RepoAutoMergeEnabled(_ context.Context, repo string) (bool, error) {
	if c.deps.Policy == nil || c.deps.RepoAutoMerge == nil {
		return false, callback.ErrNotConfigured
	}
	return c.deps.RepoAutoMerge(repo), nil
}

// SetInferenceRoute is callback.InferenceRouter.SetInferenceRoute: the
// sidecar manager's route for agent lands in hive's proxy.
func (c *Capabilities) SetInferenceRoute(_ context.Context, agent string, route callback.InferenceRoute) error {
	if c.deps.Inference == nil {
		return callback.ErrNotConfigured
	}
	c.deps.Inference.SetInferenceRoute(agent, route.Backend, route.Model)
	return nil
}

// ClearInferenceRoute is callback.InferenceRouter.ClearInferenceRoute.
func (c *Capabilities) ClearInferenceRoute(_ context.Context, agent string) error {
	if c.deps.Inference == nil {
		return callback.ErrNotConfigured
	}
	c.deps.Inference.ClearInferenceRoute(agent)
	return nil
}

// BeginEffect is callback.EffectBoundary.BeginEffect over the broker.
func (c *Capabilities) BeginEffect(ctx context.Context, claim callback.EffectClaim) (callback.BeginEffectResponse, error) {
	return c.effects.BeginEffect(ctx, claim)
}

// EndEffect is callback.EffectBoundary.EndEffect over the broker.
func (c *Capabilities) EndEffect(ctx context.Context, id string, req callback.EndEffectRequest) (callback.EndEffectResponse, error) {
	return c.effects.EndEffect(ctx, id, req)
}
