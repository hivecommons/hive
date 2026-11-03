package hivecap

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api/callback"
	"github.com/hivecommons/hive/crux/proxy"
	"github.com/hivecommons/hive/crux/pushbroker"
	"github.com/hivecommons/hive/crux/toolapprove"
)

// The adapters must satisfy the ring's seams without the ring changing.
var (
	_ pushbroker.TokenScoper     = TokenScoper{}
	_ proxy.GitHubRESTAccounting = GitHubRESTAccounting{}
	_ proxy.UsageSink            = UsageSink{}
	_ proxy.CanaryScanner        = CanaryScanner{}
	_ toolapprove.TextScanner    = TextScanner{}
	_ crux.ConditionReporter     = ConditionReporter{}
	// agent.PRCreator's method set, spelled out so this test does not link
	// crux/agent.
	_ interface {
		CreatePR(ctx context.Context, repo, head, base, title, body string) (crux.PRResult, error)
	} = PRCreator{}
)

// direct is a Capabilities implemented in-process, the in-process mode's
// end of the contract.
type direct struct {
	mu     sync.Mutex
	absent map[string]bool
	fail   error
	mints  []callback.MintTokenRequest
	rest   []callback.RESTRecord
	usage  []callback.UsageRecord
	conds  []callback.ConditionReport
	detect bool
	block  bool
	expiry time.Time
}

func (d *direct) Configured(c string) bool { return !d.absent[c] }

func (d *direct) MintToken(_ context.Context, req callback.MintTokenRequest) (callback.MintTokenResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mints = append(d.mints, req)
	if d.fail != nil {
		return callback.MintTokenResponse{}, d.fail
	}
	exp := d.expiry
	if exp.IsZero() {
		exp = time.Now().Add(time.Hour)
	}
	return callback.MintTokenResponse{Token: "ghs_" + req.Tier + "_" + strings.Join(req.Repos, ","), ExpiresAt: exp}, nil
}

func (d *direct) RecordREST(_ context.Context, rec callback.RESTRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rest = append(d.rest, rec)
	return d.fail
}

func (d *direct) RateLimit(context.Context) (callback.RateLimitSnapshot, error) {
	if d.fail != nil {
		return callback.RateLimitSnapshot{}, d.fail
	}
	return callback.RateLimitSnapshot{Remaining: 123, Reset: time.Unix(1_800_000_000, 0).UTC(), OK: true}, nil
}

func (d *direct) RecordUsage(_ context.Context, rec callback.UsageRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.usage = append(d.usage, rec)
	return d.fail
}

func (d *direct) ScanCanary(_ context.Context, req callback.CanaryScanRequest) (callback.CanaryScanResponse, error) {
	if d.fail != nil {
		return callback.CanaryScanResponse{}, d.fail
	}
	return callback.CanaryScanResponse{Detected: d.detect, Agent: req.Agent, Source: req.Source}, nil
}

func (d *direct) ScanText(_ context.Context, req callback.TextScanRequest) (callback.TextScanResponse, error) {
	if d.fail != nil {
		return callback.TextScanResponse{}, d.fail
	}
	if d.block {
		return callback.TextScanResponse{Blocked: true, CriticalInjection: req.Direction == callback.ScanInput, Findings: []callback.TextFinding{{Kind: "secret", Snippet: "gh***", Rule: "sec-1", High: true}}}, nil
	}
	return callback.TextScanResponse{}, nil
}

func (d *direct) ReportCondition(_ context.Context, rep callback.ConditionReport) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.conds = append(d.conds, rep)
	return d.fail
}

func (d *direct) LinearCredential(context.Context) (callback.LinearCredential, error) {
	if d.fail != nil {
		return callback.LinearCredential{}, d.fail
	}
	return callback.LinearCredential{AccessToken: "lin_tok"}, nil
}

func (d *direct) CreatePR(_ context.Context, req callback.CreatePRRequest) (callback.CreatePRResponse, error) {
	if d.fail != nil {
		return callback.CreatePRResponse{}, d.fail
	}
	return callback.CreatePRResponse{Number: 7, URL: "u/" + req.Repo, Author: "bot", AlreadyExisted: true, DuplicateReason: "issue"}, nil
}

// overSocket serves d on a unix socket and returns a client for it: the
// sidecar mode's end of the contract.
func overSocket(t *testing.T, d callback.Capabilities) *callback.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "hcap")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, callback.SocketName)
	ls, err := callback.Listen(callback.ListenConfig{Socket: sock})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- callback.NewServer(d, "").Serve(ctx, ls) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	c, err := callback.NewClient("unix://"+sock, "")
	if err != nil {
		t.Fatal(err)
	}
	// Listen bound the socket before Serve started, so one Health call
	// blocks in the backlog until Serve accepts.
	if _, err := c.Health(context.Background()); err != nil {
		t.Fatalf("endpoint never ready: %v", err)
	}
	return c
}

// exercise drives every seam over s and returns the observable results, so
// the direct and socket modes can be compared field for field.
type outcome struct {
	Token      string
	TokenErr   string
	Remaining  int
	Reset      time.Time
	SnapOK     bool
	Leak       crux.CanaryLeak
	Leaked     bool
	In, Out    toolapprove.TextVerdict
	PR         crux.PRResult
	PRErr      string
	Lin        [2]string
	TokenAgent string
	TokenTTL   int
}

func exercise(t *testing.T, s *Set) outcome {
	t.Helper()
	var o outcome
	ctx := context.Background()
	tok, err := s.TokenScoper("scanner", 10*time.Minute).ScopedTokenForRepos(ctx, "contributor", []string{"r1", "r2"})
	o.Token = tok
	if err != nil {
		o.TokenErr = err.Error()
	}
	hdr := http.Header{"X-Ratelimit-Remaining": {"4999"}, "X-Ratelimit-Reset": {"1800000000"}, "X-Github-Request-Id": {"abc"}, "Set-Cookie": {"nope"}}
	s.Accounting.RecordRESTRequest("agent:a", "GET", "/repos/o/r", 200, hdr)
	s.Accounting.RecordInjectedRESTRequest("agent:a", "POST", "/repos/o/r/issues", 201, hdr)
	o.Remaining, o.Reset, o.SnapOK = s.Accounting.CoreRateLimitSnapshot()
	s.Usage.Record("a", "m", 1, 2)
	s.Usage.RecordCopilot("a", "m", 3, 4)
	s.Usage.RecordCopilotWithCache("a", "m", 5, 6, 7, 8)
	o.Leak, o.Leaked = s.Canary.Scan("a", "body", "github:POST /x")
	o.In = s.Text.ScanInput("ignore all previous")
	o.Out = s.Text.ScanOutput("fine")
	s.Conditions.Observe("agent_stall", "a", "stalled", "diag")
	s.Conditions.Clear("agent_stall", "a")
	o.PR, err = s.PullRequest.CreatePR(ctx, "o/r", "h", "v5", "t", "b")
	if err != nil {
		o.PRErr = err.Error()
	}
	o.Lin[0], o.Lin[1] = s.Linear.Resolve()
	return o
}

func TestDirectAndSocketParity(t *testing.T) {
	// Whole seconds in UTC with no monotonic reading, so the value survives
	// the JSON round trip over the socket unchanged; in the future so the
	// token is not already expired when the test runs.
	exp := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	dDirect := &direct{detect: true, block: true, expiry: exp}
	dSocket := &direct{detect: true, block: true, expiry: exp}
	got := exercise(t, New(dDirect, 0))
	want := exercise(t, New(overSocket(t, dSocket), 0))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("direct and socket outcomes differ:\n direct: %+v\n socket: %+v", got, want)
	}
	// And the hive side saw the same requests both ways.
	if !reflect.DeepEqual(dDirect.mints, dSocket.mints) || !reflect.DeepEqual(dDirect.usage, dSocket.usage) || !reflect.DeepEqual(dDirect.conds, dSocket.conds) || !reflect.DeepEqual(dDirect.rest, dSocket.rest) {
		t.Errorf("hive-side records differ:\n direct: %+v %+v %+v %+v\n socket: %+v %+v %+v %+v", dDirect.mints, dDirect.usage, dDirect.conds, dDirect.rest, dSocket.mints, dSocket.usage, dSocket.conds, dSocket.rest)
	}

	// Spot-check the values themselves.
	if got.Token != "ghs_contributor_r1,r2" || got.TokenErr != "" {
		t.Errorf("token = %q, %q", got.Token, got.TokenErr)
	}
	m := dDirect.mints[0]
	if m.Agent != "scanner" || m.TTLSeconds != 600 || m.Tier != "contributor" {
		t.Errorf("mint request = %+v", m)
	}
	if !got.SnapOK || got.Remaining != 123 || got.Reset.Unix() != 1_800_000_000 {
		t.Errorf("snapshot = %d %v %v", got.Remaining, got.Reset, got.SnapOK)
	}
	if len(dDirect.rest) != 2 || dDirect.rest[0].Injected || !dDirect.rest[1].Injected {
		t.Errorf("rest records = %+v", dDirect.rest)
	}
	// Only X-RateLimit-* headers cross.
	for _, r := range dDirect.rest {
		if len(r.RateLimit) != 2 || r.RateLimit["X-Ratelimit-Remaining"] != "4999" {
			t.Errorf("rate-limit headers = %v", r.RateLimit)
		}
	}
	if len(dDirect.usage) != 3 || dDirect.usage[2].Kind != callback.UsageKindCopilotCache || dDirect.usage[2].CacheCreation != 8 || dDirect.usage[1].Kind != callback.UsageKindCopilot || dDirect.usage[0].Kind != callback.UsageKindGateway {
		t.Errorf("usage = %+v", dDirect.usage)
	}
	if !got.Leaked || got.Leak.Agent != "a" || got.Leak.Source != "github:POST /x" || got.Leak.Token != "" {
		t.Errorf("leak = %+v %v", got.Leak, got.Leaked)
	}
	if !got.In.Blocked || !got.In.CriticalInjection || len(got.In.Findings) != 1 || got.In.Findings[0].Rule != "sec-1" || !got.In.Findings[0].High || got.Out.CriticalInjection {
		t.Errorf("verdicts = %+v / %+v", got.In, got.Out)
	}
	if len(dDirect.conds) != 2 || dDirect.conds[0].Action != callback.ConditionObserve || dDirect.conds[0].Diagnostics != "diag" || dDirect.conds[1].Action != callback.ConditionClear {
		t.Errorf("conditions = %+v", dDirect.conds)
	}
	if got.PR.Number != 7 || got.PR.URL != "u/o/r" || !got.PR.AlreadyExisted || got.PR.DuplicateReason != "issue" || !got.PR.IsDuplicate() {
		t.Errorf("pr = %+v", got.PR)
	}
	if got.Lin != [2]string{"lin_tok", ""} {
		t.Errorf("linear = %v", got.Lin)
	}
}

func TestUnreachableFailsClosed(t *testing.T) {
	dir, err := os.MkdirTemp("", "hcap")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	c, err := callback.NewClient("unix://"+filepath.Join(dir, "nobody.sock"), "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(c, 200*time.Millisecond)
	o := exercise(t, s)

	if o.Token != "" || !strings.Contains(o.TokenErr, callback.ErrUnavailable.Error()) {
		t.Errorf("token must fail closed: %q, %q", o.Token, o.TokenErr)
	}
	if o.SnapOK || o.Remaining != 0 || !o.Reset.IsZero() {
		t.Errorf("snapshot must be ok=false: %d %v %v", o.Remaining, o.Reset, o.SnapOK)
	}
	if !o.Leaked || o.Leak.Agent != "a" || !strings.HasSuffix(o.Leak.Source, UnavailableSourceSuffix) || o.Leak.Token != "" {
		t.Errorf("canary must report a detection: %+v %v", o.Leak, o.Leaked)
	}
	for _, v := range []toolapprove.TextVerdict{o.In, o.Out} {
		if !v.Blocked || len(v.Findings) != 1 || v.Findings[0].Kind != TextFindingKind || v.Findings[0].Rule != TextFindingRule {
			t.Errorf("text verdict must block: %+v", v)
		}
	}
	if o.PR != (crux.PRResult{}) || !strings.Contains(o.PRErr, callback.ErrUnavailable.Error()) {
		t.Errorf("pr must fail: %+v, %q", o.PR, o.PRErr)
	}
	if o.Lin != [2]string{} {
		t.Errorf("linear must be empty: %v", o.Lin)
	}
	st := c.Status()
	if st.Reachable || st.FailedCalls == 0 {
		t.Errorf("client status = %+v", st)
	}
	// Sinks never panic and never block past the timeout.
	start := time.Now()
	s.Usage.Record("a", "m", 1, 1)
	s.Conditions.Observe("c", "k", "m", "d")
	if time.Since(start) > 2*time.Second {
		t.Error("sinks blocked past the timeout")
	}
}

func TestNotConfiguredIsTheNilSeam(t *testing.T) {
	d := &direct{absent: map[string]bool{callback.CapabilityTokens: true, callback.CapabilityCanary: true, callback.CapabilityLinear: true, callback.CapabilityPulls: true}, detect: true}
	for name, caps := range map[string]callback.Capabilities{"direct": d, "socket": overSocket(t, d)} {
		s := New(caps, 0)
		_, err := s.TokenScoper("a", 0).ScopedTokenForRepos(context.Background(), "", nil)
		if name == "socket" && !errors.Is(err, callback.ErrNotConfigured) {
			t.Errorf("%s: no App should be ErrNotConfigured: %v", name, err)
		}
		if name == "socket" {
			// No registry: scans nothing, like a nil registry; NOT a refusal.
			if leak, ok := s.Canary.Scan("a", "t", "s"); ok || leak != (crux.CanaryLeak{}) {
				t.Errorf("%s: absent registry scanned: %+v %v", name, leak, ok)
			}
			if a, k := s.Linear.Resolve(); a != "" || k != "" {
				t.Errorf("%s: absent Linear returned a credential", name)
			}
			if _, err := s.PullRequest.CreatePR(context.Background(), "r", "h", "b", "t", ""); !errors.Is(err, callback.ErrNotConfigured) {
				t.Errorf("%s: absent PR creator = %v", name, err)
			}
		}
	}
	// The direct implementation above has no 404 layer, so its own
	// ErrNotConfigured is the signal.
	d2 := &direct{fail: callback.ErrNotConfigured}
	s := New(d2, 0)
	if leak, ok := s.Canary.Scan("a", "t", "s"); ok || leak != (crux.CanaryLeak{}) {
		t.Errorf("direct ErrNotConfigured scanned: %+v %v", leak, ok)
	}
}

func TestCapabilityErrorsFailClosed(t *testing.T) {
	d := &direct{fail: errors.New("github: 403")}
	s := New(d, 0)
	if tok, err := s.TokenScoper("a", 0).ScopedTokenForRepos(context.Background(), "t", nil); tok != "" || err == nil {
		t.Errorf("token = %q, %v", tok, err)
	}
	if _, _, ok := s.Accounting.CoreRateLimitSnapshot(); ok {
		t.Error("snapshot ok on error")
	}
	if _, ok := s.Canary.Scan("a", "t", "s"); !ok {
		t.Error("canary error must be a detection")
	}
	if v := s.Text.ScanInput("x"); !v.Blocked || !strings.Contains(v.Findings[0].Snippet, "github: 403") {
		t.Errorf("text error must block with the cause: %+v", v)
	}
	if _, err := s.PullRequest.CreatePR(context.Background(), "r", "h", "b", "t", ""); err == nil {
		t.Error("pr error swallowed")
	}
	if a, k := s.Linear.Resolve(); a != "" || k != "" {
		t.Error("linear error returned a credential")
	}
	// Sinks swallow.
	s.Usage.Record("a", "m", 1, 1)
	s.Accounting.RecordRESTRequest("c", "GET", "/", 200, nil)
	s.Conditions.Clear("c", "k")
	if len(d.usage) != 1 || len(d.rest) != 1 || len(d.conds) != 1 {
		t.Error("sinks must still deliver when the capability returns an error")
	}
}

func TestTokenScoperExpiryAndTTL(t *testing.T) {
	d := &direct{expiry: time.Now().Add(-time.Minute)}
	if tok, err := New(d, 0).TokenScoper("a", 0).ScopedTokenForRepos(context.Background(), "t", nil); tok != "" || err == nil || !strings.Contains(err.Error(), "already expired") {
		t.Errorf("expired token accepted: %q, %v", tok, err)
	}
	if d.mints[0].TTLSeconds != 0 || d.mints[0].Agent != "a" {
		t.Errorf("zero ttl must be omitted: %+v", d.mints[0])
	}
	// Snapshot ok=false from hive is ok=false here.
	okFalse := &snapNotOK{direct: &direct{}}
	if _, _, ok := New(okFalse, 0).Accounting.CoreRateLimitSnapshot(); ok {
		t.Error("ok=false from hive reported as ok")
	}
	// A detection with empty agent/source falls back to the request's.
	bare := &bareDetect{direct: &direct{}}
	leak, ok := New(bare, 0).Canary.Scan("agentX", "t", "srcY")
	if !ok || leak.Agent != "agentX" || leak.Source != "srcY" {
		t.Errorf("bare detection = %+v %v", leak, ok)
	}
}

type snapNotOK struct{ *direct }

func (snapNotOK) RateLimit(context.Context) (callback.RateLimitSnapshot, error) {
	return callback.RateLimitSnapshot{Remaining: 5}, nil
}

type bareDetect struct{ *direct }

func (bareDetect) ScanCanary(context.Context, callback.CanaryScanRequest) (callback.CanaryScanResponse, error) {
	return callback.CanaryScanResponse{Detected: true}, nil
}

func TestNilCapabilities(t *testing.T) {
	s := New(nil, -1)
	if s.timeout != callback.DefaultTimeout {
		t.Errorf("timeout = %v", s.timeout)
	}
	if _, err := s.TokenScoper("a", 0).ScopedTokenForRepos(context.Background(), "t", nil); err == nil {
		t.Error("nil caps minted")
	}
	if _, _, ok := s.Accounting.CoreRateLimitSnapshot(); ok {
		t.Error("nil caps snapshot ok")
	}
	if _, ok := s.Canary.Scan("a", "t", "s"); ok {
		t.Error("nil caps is a nil registry: scans nothing")
	}
	if v := s.Text.ScanOutput("x"); !v.Blocked {
		t.Error("nil caps text scan must block")
	}
	if _, err := s.PullRequest.CreatePR(context.Background(), "r", "h", "b", "t", ""); err == nil {
		t.Error("nil caps created a PR")
	}
	if a, k := s.Linear.Resolve(); a != "" || k != "" {
		t.Error("nil caps linear")
	}
	s.Usage.Record("a", "m", 1, 1)
	s.Accounting.RecordRESTRequest("c", "GET", "/", 200, nil)
	s.Conditions.Observe("c", "k", "m", "d")
}

func TestRateLimitHeadersFilter(t *testing.T) {
	if got := rateLimitHeaders(nil); got != nil {
		t.Errorf("nil header = %v", got)
	}
	h := http.Header{"x-ratelimit-limit": {"5000", "ignored"}, "X-Ratelimit-Empty": {}, "Authorization": {"Bearer x"}}
	got := rateLimitHeaders(h)
	if len(got) != 1 || got["X-Ratelimit-Limit"] != "5000" {
		t.Errorf("filtered = %v", got)
	}
}

type watchReporter struct {
	mu       sync.Mutex
	observed []string
	cleared  []string
	seen     chan struct{}
}

func (r *watchReporter) Observe(class, key, message, diagnostics string) {
	r.mu.Lock()
	r.observed = append(r.observed, class+"/"+key+": "+message+" ["+diagnostics+"]")
	r.mu.Unlock()
	r.seen <- struct{}{}
}

func (r *watchReporter) Clear(class, key string) {
	r.mu.Lock()
	r.cleared = append(r.cleared, class+"/"+key)
	r.mu.Unlock()
	r.seen <- struct{}{}
}

func TestWatchReportsTransitionsOnly(t *testing.T) {
	var mu sync.Mutex
	st := callback.Status{Reachable: true}
	set := func(s callback.Status) { mu.Lock(); st = s; mu.Unlock() }
	source := func() callback.Status { mu.Lock(); defer mu.Unlock(); return st }
	rep := &watchReporter{seen: make(chan struct{}, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Watch(ctx, source, rep, time.Millisecond); close(done) }()

	since := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	set(callback.Status{LastError: "dial: refused", Since: since, FailedCalls: 3, LastSuccess: since.Add(-time.Hour)})
	<-rep.seen
	set(callback.Status{LastError: "dial: refused", Since: since, FailedCalls: 9})
	// Still down: a second Observe must not follow; wait a few polls.
	settle := time.NewTimer(20 * time.Millisecond)
	select {
	case <-rep.seen:
		t.Fatal("repeated Observe while still unreachable")
	case <-settle.C:
	}
	set(callback.Status{Reachable: true})
	<-rep.seen
	cancel()
	<-done

	rep.mu.Lock()
	defer rep.mu.Unlock()
	if len(rep.observed) != 1 || len(rep.cleared) != 1 {
		t.Fatalf("observed %v cleared %v", rep.observed, rep.cleared)
	}
	want := callback.ConditionClassUnavailable + "/" + WatchConditionKey + ": hive callback endpoint unreachable [error=dial: refused failed_calls=3 since=2026-01-02T03:04:05Z last_success=2026-01-02T02:04:05Z]"
	if rep.observed[0] != want {
		t.Errorf("observe\n got %s\nwant %s", rep.observed[0], want)
	}
	if rep.cleared[0] != callback.ConditionClassUnavailable+"/"+WatchConditionKey {
		t.Errorf("clear %s", rep.cleared[0])
	}
}

func TestWatchNilArgumentsReturn(t *testing.T) {
	Watch(context.Background(), nil, &watchReporter{}, 0)
	Watch(context.Background(), func() callback.Status { return callback.Status{} }, nil, 0)
	if unavailableDiagnostics(callback.Status{LastError: "x"}) != "error=x failed_calls=0" {
		t.Fatal("diagnostics")
	}
}

// policyDirect is direct plus the policy half of the contract.
type policyDirect struct {
	*direct
	pmu    sync.Mutex
	policy callback.Policy
	pfail  error
}

func (p *policyDirect) Policy(context.Context) (callback.Policy, error) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	return p.policy, p.pfail
}

func (p *policyDirect) RepoAutoMergeEnabled(_ context.Context, repo string) (bool, error) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	return repo == "o/merge", p.pfail
}

func (p *policyDirect) set(pol callback.Policy, fail error) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	p.policy, p.pfail = pol, fail
}

// The policy resolvers answer from the cached snapshot in both modes, keep
// the last snapshot through an outage, and fail the auto-merge question
// closed.
func TestPolicyResolvers(t *testing.T) {
	explain, kick := "on", "transcript"
	gateways := []string{"LiteLLM-EU"}
	pol := callback.Policy{
		Build:              &callback.BuildInfo{Version: "v5.2", Commit: "abc"},
		ExplainModeDefault: &explain,
		KickLogSource:      &kick,
		GatewayBackends:    &gateways,
		RepoAutoMerge:      true,
	}
	d := &policyDirect{direct: &direct{}, policy: pol}
	for name, caps := range map[string]callback.Capabilities{"direct": d, "socket": overSocket(t, d)} {
		p := New(caps, time.Second).Policy
		if v, c := p.BuildInfo(); v != "unknown" || c != "unknown" || p.ExplainModeDefault() != "" || p.KickLogSource() != "" || p.GatewayBackend("litellm-eu") {
			t.Fatalf("%s: resolvers before the first refresh answered from nothing", name)
		}
		if _, err := p.Refresh(context.Background()); err != nil {
			t.Fatalf("%s: Refresh: %v", name, err)
		}
		if v, c := p.BuildInfo(); v != "v5.2" || c != "abc" {
			t.Errorf("%s: BuildInfo = %s %s", name, v, c)
		}
		if p.ExplainModeDefault() != "on" || p.KickLogSource() != "transcript" {
			t.Errorf("%s: explain=%q kick=%q", name, p.ExplainModeDefault(), p.KickLogSource())
		}
		if !p.GatewayBackend("litellm-eu") || p.GatewayBackend("other") || p.GatewayBackend("") {
			t.Errorf("%s: gateway predicate wrong", name)
		}
		if !p.RepoAutoMergeEnabled("o/merge") || p.RepoAutoMergeEnabled("o/other") {
			t.Errorf("%s: auto-merge predicate wrong", name)
		}
		d.set(pol, errors.New("hive down"))
		if _, err := p.Refresh(context.Background()); err == nil {
			t.Errorf("%s: Refresh through an outage succeeded", name)
		}
		if p.ExplainModeDefault() != "on" {
			t.Errorf("%s: outage dropped the last snapshot", name)
		}
		if p.RepoAutoMergeEnabled("o/merge") {
			t.Errorf("%s: auto-merge allowed while hive is down", name)
		}
		d.set(pol, nil)
	}
}

// A Capabilities without the policy half never yields a snapshot, and its
// auto-merge answer is a refusal.
func TestPolicyWithoutSource(t *testing.T) {
	p := New(&direct{}, time.Second).Policy
	if _, err := p.Refresh(context.Background()); !errors.Is(err, callback.ErrNotConfigured) {
		t.Fatalf("Refresh = %v, want ErrNotConfigured", err)
	}
	if _, ok := p.Snapshot(); ok {
		t.Fatal("snapshot without a source")
	}
	if p.RepoAutoMergeEnabled("o/merge") {
		t.Fatal("auto-merge allowed without a source")
	}
	WatchPolicy(context.Background(), p, time.Millisecond, func(callback.Policy) { t.Fatal("applied without a source") })
}

// WatchPolicy applies the first snapshot and each change, not repeats.
func TestWatchPolicyAppliesChanges(t *testing.T) {
	owned := false
	d := &policyDirect{direct: &direct{}, policy: callback.Policy{DeadSessionRecoveryOwnedElsewhere: &owned}}
	p := New(d, time.Second).Policy
	ctx, cancel := context.WithCancel(context.Background())
	applied := make(chan callback.Policy, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		WatchPolicy(ctx, p, time.Millisecond, func(pol callback.Policy) { applied <- pol })
	}()
	first := <-applied
	if first.DeadSessionRecoveryOwnedElsewhere == nil || *first.DeadSessionRecoveryOwnedElsewhere {
		t.Fatalf("first = %+v", first)
	}
	nowOwned := true
	d.set(callback.Policy{DeadSessionRecoveryOwnedElsewhere: &nowOwned}, nil)
	second := <-applied
	if second.DeadSessionRecoveryOwnedElsewhere == nil || !*second.DeadSessionRecoveryOwnedElsewhere {
		t.Fatalf("second = %+v", second)
	}
	cancel()
	<-done
	select {
	case extra := <-applied:
		t.Fatalf("unchanged snapshot re-applied: %+v", extra)
	default:
	}
}

// The zero and nil forms of Policy are safe and answer the defaults.
func TestPolicyEdges(t *testing.T) {
	if p := NewPolicy(nil, 0); p.timeout != callback.DefaultTimeout || p.src != nil {
		t.Fatalf("NewPolicy(nil, 0) = %+v", p)
	}
	var nilPolicy *Policy
	if _, err := nilPolicy.Refresh(context.Background()); !errors.Is(err, callback.ErrNotConfigured) {
		t.Fatalf("nil Refresh = %v", err)
	}
	if _, ok := nilPolicy.Snapshot(); ok || nilPolicy.RepoAutoMergeEnabled("o/merge") || nilPolicy.GatewayBackend("x") {
		t.Fatal("nil policy answered")
	}
	if v, c := nilPolicy.BuildInfo(); v != "unknown" || c != "unknown" {
		t.Fatalf("nil BuildInfo = %s %s", v, c)
	}
	WatchPolicy(context.Background(), nilPolicy, 0, func(callback.Policy) { t.Fatal("nil policy applied") })

	d := &policyDirect{direct: &direct{}}
	p := NewPolicy(d, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := p.Refresh(ctx); err != nil {
		t.Fatalf("Refresh with a deadline: %v", err)
	}
	WatchPolicy(context.Background(), p, 0, nil)
	done, stop := context.WithCancel(context.Background())
	stop()
	applied := 0
	WatchPolicy(done, p, 0, func(callback.Policy) { applied++ })
	if applied != 1 {
		t.Fatalf("applied %d times before a cancelled watch returned, want 1", applied)
	}
}
