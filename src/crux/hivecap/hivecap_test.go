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
func overSocket(t *testing.T, d *direct) *callback.Client {
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
	exp := time.Date(2026, 10, 25, 14, 0, 0, 0, time.UTC)
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
