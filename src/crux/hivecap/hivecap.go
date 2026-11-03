// Package hivecap turns a callback.Capabilities (hive's capability set,
// reached over the callback endpoint or called directly) into the seam
// interfaces the Crux ring already consumes: pushbroker.TokenScoper,
// proxy.GitHubRESTAccounting, proxy.UsageSink, proxy.CanaryScanner,
// toolapprove.TextScanner, crux.ConditionReporter and agent.PRCreator
// (hivecommons/hive#14138 stage 2; src/docs/design/crux.md "Credentials and
// hive-provided capabilities"), and caches hive's crux.Host policy snapshot
// for the runtime's policy resolvers (Policy, stage 4).
//
// Every adapter fails closed. The seams that can return an error return one
// wrapping callback.ErrUnavailable or callback.ErrUnauthorized; the seams
// that cannot (the scanners, the sinks, the snapshot) return the value that
// takes the ring's existing refusal path: a canary detection, a Blocked
// verdict, ok=false. A capability hive reports as not configured
// (callback.ErrNotConfigured) is treated as the nil seam it stands in for.
//
// The package is written against callback.Capabilities, not *callback.Client,
// so the in-process mode can hand it hive's direct implementation and the
// sidecar mode a client; the parity test asserts both behave the same.
package hivecap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api/callback"
	"github.com/hivecommons/hive/crux/toolapprove"
)

const (
	// UnavailableSourceSuffix marks a canary "detection" that is really the
	// endpoint being unreachable, so the proxy's audit line says which.
	UnavailableSourceSuffix = " (hive callback unavailable)"
	// TextFindingKind and TextFindingRule name the synthetic finding a
	// Blocked verdict carries when the scan could not run.
	TextFindingKind = "callback"
	TextFindingRule = "hive_unavailable"
)

// Set is every adapter over one Capabilities. Build it with New; the fields
// are the values the runtime's setters take.
type Set struct {
	caps    callback.Capabilities
	timeout time.Duration

	Accounting  GitHubRESTAccounting
	Usage       UsageSink
	Canary      CanaryScanner
	Text        TextScanner
	Conditions  ConditionReporter
	PullRequest PRCreator
	Linear      LinearCredentials
	Policy      *Policy
	Inference   *InferenceRoutes
	Mutation    MutationBoundary
	// AppTokens, Mint and Bob are the stage-4 credential writers: the
	// sidecar's SetAppAuth, SetAgentMint and SetBob*Resolver arguments.
	AppTokens *AppTokenWriter
	Mint      AgentMint
	Bob       *BobCredentials
}

// New builds the adapters over caps. timeout bounds the calls made from
// seams whose callers pass no context (the sinks and scanners); non-positive
// selects callback.DefaultTimeout.
func New(caps callback.Capabilities, timeout time.Duration) *Set {
	if timeout <= 0 {
		timeout = callback.DefaultTimeout
	}
	s := &Set{caps: caps, timeout: timeout}
	s.Accounting = GitHubRESTAccounting{caps: caps, timeout: timeout}
	s.Usage = UsageSink{caps: caps, timeout: timeout}
	s.Canary = CanaryScanner{caps: caps, timeout: timeout}
	s.Text = TextScanner{caps: caps, timeout: timeout}
	s.Conditions = ConditionReporter{caps: caps, timeout: timeout}
	s.PullRequest = PRCreator{caps: caps}
	s.Linear = LinearCredentials{caps: caps, timeout: timeout}
	s.AppTokens = NewAppTokenWriter(caps)
	s.Mint = NewAgentMint(caps, timeout)
	s.Bob = NewBobCredentials(caps, timeout)
	s.Policy = NewPolicy(caps, timeout)
	s.Inference = NewInferenceRoutes(caps, timeout)
	s.Mutation = NewMutationBoundary(caps)
	return s
}

// TokenScoper returns a pushbroker.TokenScoper whose mints are attributed
// to agent. ttl is the lifetime to request; zero lets hive choose.
func (s *Set) TokenScoper(agent string, ttl time.Duration) TokenScoper {
	return TokenScoper{caps: s.caps, Agent: agent, TTL: ttl}
}

func background(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}

// TokenScoper implements pushbroker.TokenScoper: ScopedTokenForRepos mints
// through the callback. The token is returned to the caller and held
// nowhere else; pushbroker keeps it for one push.
type TokenScoper struct {
	caps callback.Capabilities
	// Agent is stamped on every mint request.
	Agent string
	// TTL is the lifetime requested; zero lets hive choose.
	TTL time.Duration
}

// ScopedTokenForRepos mints a token narrowed to tier and, when non-empty,
// to repos (bare repository names). A hive without a GitHub App answers
// callback.ErrNotConfigured, which is the error the nil-App path gives
// today; unreachable is callback.ErrUnavailable. Both fail the push.
func (t TokenScoper) ScopedTokenForRepos(ctx context.Context, tier string, repos []string) (string, error) {
	if t.caps == nil {
		return "", errors.New("hivecap: no capabilities")
	}
	req := callback.MintTokenRequest{Agent: t.Agent, Tier: tier, Repos: repos}
	if t.TTL > 0 {
		req.TTLSeconds = int(t.TTL / time.Second)
	}
	resp, err := t.caps.MintToken(ctx, req)
	if err != nil {
		return "", fmt.Errorf("hivecap: minting scoped token: %w", err)
	}
	if !resp.ExpiresAt.IsZero() && !resp.ExpiresAt.After(time.Now()) {
		return "", fmt.Errorf("hivecap: minted token already expired at %s", resp.ExpiresAt.Format(time.RFC3339))
	}
	return resp.Token, nil
}

// GitHubRESTAccounting implements proxy.GitHubRESTAccounting. Records are
// best effort (a failure is dropped and counted by the client's Status);
// CoreRateLimitSnapshot answers ok=false when hive cannot, so the agent
// GitHub budget applies the per-agent cap alone.
type GitHubRESTAccounting struct {
	caps    callback.Capabilities
	timeout time.Duration
}

func (a GitHubRESTAccounting) RecordRESTRequest(caller, method, path string, status int, h http.Header) {
	a.record(caller, method, path, status, false, h)
}

func (a GitHubRESTAccounting) RecordInjectedRESTRequest(caller, method, path string, status int, h http.Header) {
	a.record(caller, method, path, status, true, h)
}

func (a GitHubRESTAccounting) record(caller, method, path string, status int, injected bool, h http.Header) {
	if a.caps == nil {
		return
	}
	ctx, cancel := background(a.timeout)
	defer cancel()
	_ = a.caps.RecordREST(ctx, callback.RESTRecord{
		Caller: caller, Method: method, Path: path, Status: status, Injected: injected,
		RateLimit: rateLimitHeaders(h),
	})
}

// rateLimitHeaders keeps only the X-RateLimit-* response headers: the
// accounting needs nothing else and the rest of a GitHub response header
// set can carry request ids and cookies that have no business on the wire.
func rateLimitHeaders(h http.Header) map[string]string {
	var out map[string]string
	for k, v := range h {
		if !strings.HasPrefix(http.CanonicalHeaderKey(k), callback.RateLimitHeaderPrefix) || len(v) == 0 {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[http.CanonicalHeaderKey(k)] = v[0]
	}
	return out
}

func (a GitHubRESTAccounting) CoreRateLimitSnapshot() (remaining int, reset time.Time, ok bool) {
	if a.caps == nil {
		return 0, time.Time{}, false
	}
	ctx, cancel := background(a.timeout)
	defer cancel()
	snap, err := a.caps.RateLimit(ctx)
	if err != nil || !snap.OK {
		return 0, time.Time{}, false
	}
	return snap.Remaining, snap.Reset, true
}

// UsageSink implements proxy.UsageSink; records are best effort.
type UsageSink struct {
	caps    callback.Capabilities
	timeout time.Duration
}

func (u UsageSink) Record(agent, model string, in, out int64) {
	u.send(callback.UsageRecord{Agent: agent, Model: model, Kind: callback.UsageKindGateway, InputTokens: in, OutputTokens: out})
}

func (u UsageSink) RecordCopilot(agent, model string, in, out int64) {
	u.send(callback.UsageRecord{Agent: agent, Model: model, Kind: callback.UsageKindCopilot, InputTokens: in, OutputTokens: out})
}

func (u UsageSink) RecordCopilotWithCache(agent, model string, in, out, cacheRead, cacheCreation int64) {
	u.send(callback.UsageRecord{Agent: agent, Model: model, Kind: callback.UsageKindCopilotCache, InputTokens: in, OutputTokens: out, CacheRead: cacheRead, CacheCreation: cacheCreation})
}

func (u UsageSink) send(rec callback.UsageRecord) {
	if u.caps == nil {
		return
	}
	ctx, cancel := background(u.timeout)
	defer cancel()
	_ = u.caps.RecordUsage(ctx, rec)
}

// CanaryScanner implements proxy.CanaryScanner. Scan has no error return,
// so an unreachable hive is reported as a detection with an empty Token and
// Source suffixed UnavailableSourceSuffix: with the proxy's fail-closed
// setting the write is refused, without it the write is audited, which is
// what an unscannable body does today. A hive with no canary registry
// (callback.ErrNotConfigured) scans nothing, like a nil registry.
type CanaryScanner struct {
	caps    callback.Capabilities
	timeout time.Duration
}

func (c CanaryScanner) Scan(agentName, text, source string) (crux.CanaryLeak, bool) {
	if c.caps == nil {
		return crux.CanaryLeak{}, false
	}
	ctx, cancel := background(c.timeout)
	defer cancel()
	resp, err := c.caps.ScanCanary(ctx, callback.CanaryScanRequest{Agent: agentName, Text: text, Source: source})
	if err != nil {
		if errors.Is(err, callback.ErrNotConfigured) {
			return crux.CanaryLeak{}, false
		}
		return crux.CanaryLeak{Agent: agentName, Source: source + UnavailableSourceSuffix}, true
	}
	if !resp.Detected {
		return crux.CanaryLeak{}, false
	}
	leak := crux.CanaryLeak{Agent: resp.Agent, Source: resp.Source}
	if leak.Agent == "" {
		leak.Agent = agentName
	}
	if leak.Source == "" {
		leak.Source = source
	}
	return leak, true
}

// TextScanner implements toolapprove.TextScanner. An unreachable hive is a
// Blocked verdict with one synthetic finding, the denial
// toolapprove.ErrNoTextScanner produces with no scanner installed. There is
// no not-configured case: a hive always has the rule set.
type TextScanner struct {
	caps    callback.Capabilities
	timeout time.Duration
}

func (t TextScanner) ScanInput(text string) toolapprove.TextVerdict {
	return t.scan(callback.ScanInput, text)
}

func (t TextScanner) ScanOutput(text string) toolapprove.TextVerdict {
	return t.scan(callback.ScanOutput, text)
}

func (t TextScanner) scan(direction, text string) toolapprove.TextVerdict {
	if t.caps == nil {
		return unavailableVerdict(errors.New("hivecap: no capabilities"))
	}
	ctx, cancel := background(t.timeout)
	defer cancel()
	resp, err := t.caps.ScanText(ctx, callback.TextScanRequest{Direction: direction, Text: text})
	if err != nil {
		return unavailableVerdict(err)
	}
	v := toolapprove.TextVerdict{Blocked: resp.Blocked, CriticalInjection: resp.CriticalInjection}
	for _, f := range resp.Findings {
		v.Findings = append(v.Findings, toolapprove.TextFinding{Kind: f.Kind, Snippet: f.Snippet, Rule: f.Rule, High: f.High})
	}
	return v
}

func unavailableVerdict(err error) toolapprove.TextVerdict {
	return toolapprove.TextVerdict{
		Blocked:  true,
		Findings: []toolapprove.TextFinding{{Kind: TextFindingKind, Rule: TextFindingRule, Snippet: err.Error(), High: true}},
	}
}

// ConditionReporter implements crux.ConditionReporter; reports are best
// effort.
type ConditionReporter struct {
	caps    callback.Capabilities
	timeout time.Duration
}

func (r ConditionReporter) Observe(class, key, message, diagnostics string) {
	r.send(callback.ConditionReport{Action: callback.ConditionObserve, Class: class, Key: key, Message: message, Diagnostics: diagnostics})
}

func (r ConditionReporter) Clear(class, key string) {
	r.send(callback.ConditionReport{Action: callback.ConditionClear, Class: class, Key: key})
}

func (r ConditionReporter) send(rep callback.ConditionReport) {
	if r.caps == nil {
		return
	}
	ctx, cancel := background(r.timeout)
	defer cancel()
	_ = r.caps.ReportCondition(ctx, rep)
}

// PRCreator implements agent.PRCreator over the callback.
type PRCreator struct {
	caps callback.Capabilities
}

func (p PRCreator) CreatePR(ctx context.Context, repo, head, base, title, body string) (crux.PRResult, error) {
	if p.caps == nil {
		return crux.PRResult{}, errors.New("hivecap: no capabilities")
	}
	resp, err := p.caps.CreatePR(ctx, callback.CreatePRRequest{Repo: repo, Head: head, Base: base, Title: title, Body: body})
	if err != nil {
		return crux.PRResult{}, fmt.Errorf("hivecap: creating pull request: %w", err)
	}
	return crux.PRResult{
		Number: resp.Number, URL: resp.URL, Author: resp.Author,
		AlreadyExisted: resp.AlreadyExisted, DuplicateTree: resp.DuplicateTree, DuplicateReason: resp.DuplicateReason,
	}, nil
}

// LinearCredentials resolves the Linear credential to inject. Resolve has
// the shape of the proxy's resolver minus the agent.LinearCredential type
// (which lives in crux/agent); the host maps the two fields. Unreachable or
// not configured is an empty credential: the proxy injects nothing.
type LinearCredentials struct {
	caps    callback.Capabilities
	timeout time.Duration
}

// Resolve returns the access token and API key, both "" when none.
func (l LinearCredentials) Resolve() (accessToken, apiKey string) {
	if l.caps == nil {
		return "", ""
	}
	ctx, cancel := background(l.timeout)
	defer cancel()
	cred, err := l.caps.LinearCredential(ctx)
	if err != nil {
		return "", ""
	}
	return cred.AccessToken, cred.APIKey
}

// StatusSource is callback.Client.Status: the endpoint's reachability as
// the client last saw it.
type StatusSource func() callback.Status

// Watch turns a StatusSource into the callback.ConditionClassUnavailable
// condition: it polls every interval until ctx ends, reporting Observe on
// the transition to unreachable and Clear on recovery. The reporter is the
// serving process's own (its event stream), not the callback: the condition
// describes the callback being down, so it cannot travel over it. The
// first poll happens at once so a dead endpoint is reported without
// waiting an interval; non-positive interval selects DefaultWatchInterval.
func Watch(ctx context.Context, status StatusSource, reporter crux.ConditionReporter, interval time.Duration) {
	if status == nil || reporter == nil {
		return
	}
	if interval <= 0 {
		interval = DefaultWatchInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	down := false
	for {
		st := status()
		switch {
		case !st.Reachable && !down:
			down = true
			reporter.Observe(callback.ConditionClassUnavailable, WatchConditionKey, "hive callback endpoint unreachable", unavailableDiagnostics(st))
		case st.Reachable && down:
			down = false
			reporter.Clear(callback.ConditionClassUnavailable, WatchConditionKey)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// DefaultWatchInterval is how often Watch samples the status.
const DefaultWatchInterval = 10 * time.Second

// WatchConditionKey is the condition key Watch reports under; one endpoint
// per runtime, so one key.
const WatchConditionKey = "endpoint"

func unavailableDiagnostics(st callback.Status) string {
	parts := []string{"error=" + st.LastError, fmt.Sprintf("failed_calls=%d", st.FailedCalls)}
	if !st.Since.IsZero() {
		parts = append(parts, "since="+st.Since.UTC().Format(time.RFC3339))
	}
	if !st.LastSuccess.IsZero() {
		parts = append(parts, "last_success="+st.LastSuccess.UTC().Format(time.RFC3339))
	}
	return strings.Join(parts, " ")
}

// Policy caches hive's crux.Host policy snapshot (GET /v1/policy) for the
// runtime's policy resolvers. The manager reads several of them while
// holding its own lock, so they never call out: they answer from the last
// snapshot WatchPolicy fetched, and keep answering from it while hive is
// unreachable. The one exception is RepoAutoMergeEnabled, read only on the
// merge-authorisation path, which asks hive per repository and fails closed.
type Policy struct {
	src     callback.PolicySource
	timeout time.Duration

	mu  sync.RWMutex
	cur callback.Policy
	ok  bool
}

// NewPolicy builds the cache over caps; a caps that is not a
// callback.PolicySource never has a snapshot. Non-positive timeout selects
// callback.DefaultTimeout.
func NewPolicy(caps callback.Capabilities, timeout time.Duration) *Policy {
	if timeout <= 0 {
		timeout = callback.DefaultTimeout
	}
	src, _ := caps.(callback.PolicySource)
	return &Policy{src: src, timeout: timeout}
}

// Refresh fetches the snapshot and, on success, makes it current.
func (p *Policy) Refresh(ctx context.Context) (callback.Policy, error) {
	if p == nil || p.src == nil {
		return callback.Policy{}, callback.ErrNotConfigured
	}
	if _, has := ctx.Deadline(); !has {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}
	pol, err := p.src.Policy(ctx)
	if err != nil {
		return callback.Policy{}, err
	}
	p.mu.Lock()
	p.cur, p.ok = pol, true
	p.mu.Unlock()
	return pol, nil
}

// Snapshot is the current snapshot; ok is false before the first
// successful Refresh.
func (p *Policy) Snapshot() (callback.Policy, bool) {
	if p == nil {
		return callback.Policy{}, false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cur, p.ok
}

// BuildInfo is the SetBuildInfo source: hive's version and commit, or
// "unknown" (the manager's own default) without a snapshot.
func (p *Policy) BuildInfo() (version, commit string) {
	if pol, _ := p.Snapshot(); pol.Build != nil {
		return pol.Build.Version, pol.Build.Commit
	}
	return "unknown", "unknown"
}

// ExplainModeDefault is the SetExplainModeDefaultResolver resolver; "" (no
// hive-wide default) without a snapshot.
func (p *Policy) ExplainModeDefault() string {
	if pol, _ := p.Snapshot(); pol.ExplainModeDefault != nil {
		return *pol.ExplainModeDefault
	}
	return ""
}

// KickLogSource is the SetKickLogSourceResolver resolver; "" without a
// snapshot (the manager normalises it to its default source).
func (p *Policy) KickLogSource() string {
	if pol, _ := p.Snapshot(); pol.KickLogSource != nil {
		return *pol.KickLogSource
	}
	return ""
}

// GatewayBackend is the SetGatewayBackendChecker predicate: backend names a
// gateway hive has configured. The empty backend is the default, never a
// named gateway, matching hive's own predicate.
func (p *Policy) GatewayBackend(backend string) bool {
	if backend == "" {
		return false
	}
	pol, _ := p.Snapshot()
	if pol.GatewayBackends == nil {
		return false
	}
	for _, name := range *pol.GatewayBackends {
		if strings.EqualFold(name, backend) {
			return true
		}
	}
	return false
}

// RepoAutoMergeEnabled is the SetRepoAutoMergeEnabledResolver predicate. It
// asks hive; any failure is false, so an unreachable hive refuses the merge
// rather than allowing it.
func (p *Policy) RepoAutoMergeEnabled(repo string) bool {
	if p == nil || p.src == nil {
		return false
	}
	ctx, cancel := background(p.timeout)
	defer cancel()
	ok, err := p.src.RepoAutoMergeEnabled(ctx, repo)
	return err == nil && ok
}

// DefaultPolicyInterval is how often WatchPolicy re-reads the snapshot, so a
// hive config reload reaches the runtime within it.
const DefaultPolicyInterval = 15 * time.Second

// WatchPolicy refreshes p every interval until ctx ends and calls apply with
// each snapshot that differs from the last one applied (the first
// successful one included). A failed refresh keeps the previous snapshot.
func WatchPolicy(ctx context.Context, p *Policy, interval time.Duration, apply func(callback.Policy)) {
	if p == nil || p.src == nil || apply == nil {
		return
	}
	if interval <= 0 {
		interval = DefaultPolicyInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var last []byte
	for {
		if pol, err := p.Refresh(ctx); err == nil {
			if b, err := json.Marshal(pol); err == nil && (last == nil || !bytes.Equal(b, last)) {
				last = b
				apply(pol)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
