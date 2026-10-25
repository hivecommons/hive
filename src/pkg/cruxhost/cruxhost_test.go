package cruxhost

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api/callback"
	"github.com/hivecommons/hive/crux/toolapprove"
)

const secret = "ghs_private_token_value"

type fakeDeps struct {
	mu        sync.Mutex
	mintTier  string
	mintRepos []string
	mintErr   error
	mintEmpty bool
	rest      []string
	headers   http.Header
	usage     []string
	conds     []string
	prErr     error
}

func (f *fakeDeps) ScopedTokenForRepos(_ context.Context, tier string, repos []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mintTier, f.mintRepos = tier, repos
	if f.mintErr != nil {
		return "", f.mintErr
	}
	if f.mintEmpty {
		return "", nil
	}
	return secret, nil
}

func (f *fakeDeps) RecordRESTRequest(caller, method, path string, status int, h http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rest = append(f.rest, "plain "+caller+" "+method+" "+path)
	f.headers = h
}

func (f *fakeDeps) RecordInjectedRESTRequest(caller, method, path string, status int, h http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rest = append(f.rest, "injected "+caller+" "+method+" "+path)
	f.headers = h
}

func (f *fakeDeps) CoreRateLimitSnapshot() (int, time.Time, bool) {
	return 42, time.Unix(1_800_000_000, 0).UTC(), true
}

func (f *fakeDeps) Record(agent, model string, in, out int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usage = append(f.usage, "gateway")
}

func (f *fakeDeps) RecordCopilot(agent, model string, in, out int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usage = append(f.usage, "copilot")
}

func (f *fakeDeps) RecordCopilotWithCache(agent, model string, in, out, cr, cc int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usage = append(f.usage, "cache")
}

func (f *fakeDeps) Scan(agent, text, source string) (crux.CanaryLeak, bool) {
	if strings.Contains(text, "CANARY") {
		return crux.CanaryLeak{Agent: agent, Token: "CANARY-1", Source: source}, true
	}
	return crux.CanaryLeak{}, false
}

func (f *fakeDeps) ScanInput(text string) toolapprove.TextVerdict {
	if strings.Contains(text, "ignore") {
		return toolapprove.TextVerdict{Blocked: true, CriticalInjection: true, Findings: []toolapprove.TextFinding{{Kind: "injection", Snippet: "ign***", Rule: "inj-1", High: true}}}
	}
	return toolapprove.TextVerdict{}
}

func (f *fakeDeps) ScanOutput(text string) toolapprove.TextVerdict {
	return toolapprove.TextVerdict{Blocked: strings.Contains(text, "secret")}
}

func (f *fakeDeps) Observe(class, key, message, diagnostics string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conds = append(f.conds, "observe "+class+"/"+key)
}

func (f *fakeDeps) Clear(class, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conds = append(f.conds, "clear "+class+"/"+key)
}

func (f *fakeDeps) CreatePR(_ context.Context, repo, head, base, title, body string) (crux.PRResult, error) {
	if f.prErr != nil {
		return crux.PRResult{}, f.prErr
	}
	return crux.PRResult{Number: 9, URL: "https://x/" + repo + "/pull/9", Author: "bot", AlreadyExisted: true, DuplicateTree: true, DuplicateReason: "tree"}, nil
}

func allDeps(f *fakeDeps) Deps {
	return Deps{Tokens: f, REST: f, Usage: f, Canary: f, Text: f, Conditions: f, Linear: func() (string, string) { return "lin", "key" }, PullRequests: f}
}

// serve mounts deps on a unix socket exactly as stage 3 will and returns a
// runtime-side client plus the server log.
func serve(t *testing.T, deps Deps) (*callback.Client, *bytes.Buffer) {
	t.Helper()
	dir, err := os.MkdirTemp("", "cruxhost")
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
	var logBuf bytes.Buffer
	srv := callback.NewServer(New(deps), "")
	srv.Logger = slog.New(slog.NewTextHandler(&logBuf, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ls) }()
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
	return c, &logBuf
}

func TestEveryCapabilityOverSocket(t *testing.T) {
	f := &fakeDeps{}
	c, logBuf := serve(t, allDeps(f))
	ctx := context.Background()

	resp, err := c.MintToken(ctx, callback.MintTokenRequest{Agent: "scanner", Repo: "org/repo", Repos: []string{"other"}, Tier: "contributor", TTLSeconds: 600})
	if err != nil || resp.Token != secret {
		t.Fatalf("mint = %v, %v", resp, err)
	}
	if ttl := time.Until(resp.ExpiresAt); ttl > 10*time.Minute || ttl < 9*time.Minute {
		t.Errorf("expiry should honour the shorter requested ttl: %v", ttl)
	}
	if f.mintTier != "contributor" || strings.Join(f.mintRepos, ",") != "other,repo" {
		t.Errorf("scoper got tier=%q repos=%v", f.mintTier, f.mintRepos)
	}

	if err := c.RecordREST(ctx, callback.RESTRecord{Caller: "a", Method: "GET", Path: "/p", Status: 200, RateLimit: map[string]string{"X-Ratelimit-Remaining": "7"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.RecordREST(ctx, callback.RESTRecord{Caller: "a", Method: "POST", Path: "/q", Status: 201, Injected: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.rest, ";") != "plain a GET /p;injected a POST /q" {
		t.Errorf("rest = %v", f.rest)
	}
	snap, err := c.RateLimit(ctx)
	if err != nil || !snap.OK || snap.Remaining != 42 || snap.Reset.Unix() != 1_800_000_000 {
		t.Errorf("snapshot = %+v, %v", snap, err)
	}

	for _, kind := range []string{callback.UsageKindGateway, callback.UsageKindCopilot, callback.UsageKindCopilotCache} {
		if err := c.RecordUsage(ctx, callback.UsageRecord{Agent: "a", Model: "m", Kind: kind, InputTokens: 1, OutputTokens: 2}); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(f.usage, ",") != "gateway,copilot,cache" {
		t.Errorf("usage = %v", f.usage)
	}

	leak, err := c.ScanCanary(ctx, callback.CanaryScanRequest{Agent: "a", Text: "has CANARY-1 inside", Source: "src"})
	if err != nil || !leak.Detected || leak.Agent != "a" || leak.Source != "src" {
		t.Errorf("canary = %+v, %v", leak, err)
	}
	if clean, err := c.ScanCanary(ctx, callback.CanaryScanRequest{Agent: "a", Text: "clean"}); err != nil || clean.Detected {
		t.Errorf("clean canary = %+v, %v", clean, err)
	}

	v, err := c.ScanText(ctx, callback.TextScanRequest{Direction: callback.ScanInput, Text: "ignore all"})
	if err != nil || !v.Blocked || !v.CriticalInjection || len(v.Findings) != 1 || !v.Findings[0].High || v.Findings[0].Rule != "inj-1" {
		t.Errorf("input verdict = %+v, %v", v, err)
	}
	if v, err := c.ScanText(ctx, callback.TextScanRequest{Direction: callback.ScanOutput, Text: "a secret"}); err != nil || !v.Blocked || v.CriticalInjection {
		t.Errorf("output verdict = %+v, %v", v, err)
	}

	if err := c.ReportCondition(ctx, callback.ConditionReport{Action: callback.ConditionObserve, Class: "c", Key: "k", Message: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := c.ReportCondition(ctx, callback.ConditionReport{Action: callback.ConditionClear, Class: "c", Key: "k"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.conds, ";") != "observe c/k;clear c/k" {
		t.Errorf("conds = %v", f.conds)
	}

	lin, err := c.LinearCredential(ctx)
	if err != nil || lin.AccessToken != "lin" || lin.APIKey != "key" {
		t.Errorf("linear = %+v, %v", lin, err)
	}

	pr, err := c.CreatePR(ctx, callback.CreatePRRequest{Repo: "o/r", Head: "h", Base: "b", Title: "t"})
	if err != nil || pr.Number != 9 || pr.Author != "bot" || !pr.AlreadyExisted || !pr.DuplicateTree || pr.DuplicateReason != "tree" {
		t.Errorf("pr = %+v, %v", pr, err)
	}

	for _, s := range []string{secret, "lin", "key", "CANARY-1"} {
		if strings.Contains(logBuf.String(), s) {
			t.Errorf("server log leaked %q:\n%s", s, logBuf.String())
		}
	}
}

func TestNilDepsAreNotConfigured(t *testing.T) {
	caps := New(Deps{})
	for _, name := range []string{callback.CapabilityTokens, callback.CapabilityGitHubREST, callback.CapabilityUsage, callback.CapabilityCanary, callback.CapabilityText, callback.CapabilityConditions, callback.CapabilityLinear, callback.CapabilityPulls, "bogus"} {
		if caps.Configured(name) {
			t.Errorf("%s configured with no deps", name)
		}
	}
	ctx := context.Background()
	checks := map[string]error{}
	_, checks["mint"] = caps.MintToken(ctx, callback.MintTokenRequest{})
	checks["rest"] = caps.RecordREST(ctx, callback.RESTRecord{})
	_, checks["ratelimit"] = caps.RateLimit(ctx)
	checks["usage"] = caps.RecordUsage(ctx, callback.UsageRecord{})
	_, checks["canary"] = caps.ScanCanary(ctx, callback.CanaryScanRequest{})
	_, checks["text"] = caps.ScanText(ctx, callback.TextScanRequest{})
	checks["cond"] = caps.ReportCondition(ctx, callback.ConditionReport{})
	_, checks["linear"] = caps.LinearCredential(ctx)
	_, checks["pr"] = caps.CreatePR(ctx, callback.CreatePRRequest{})
	for name, err := range checks {
		if !errors.Is(err, callback.ErrNotConfigured) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// Over the socket the same shows as 404 not_configured → ErrNotConfigured.
	c, _ := serve(t, Deps{})
	if _, err := c.MintToken(ctx, callback.MintTokenRequest{Agent: "a"}); !errors.Is(err, callback.ErrNotConfigured) {
		t.Errorf("socket mint = %v", err)
	}
	if _, err := c.ScanCanary(ctx, callback.CanaryScanRequest{Agent: "a", Text: "x", Source: "s"}); !errors.Is(err, callback.ErrNotConfigured) {
		t.Errorf("socket canary = %v", err)
	}
	f := &fakeDeps{}
	partial := New(Deps{Tokens: f})
	if !partial.Configured(callback.CapabilityTokens) || partial.Configured(callback.CapabilityPulls) {
		t.Error("partial deps misreported")
	}
}

// The policy capability serves the resolver's snapshot live, stamps
// RepoAutoMerge from whether a per-repo resolver exists, and is absent
// without a Policy resolver.
func TestPolicyOverSocket(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	explain, calls := "off", 0
	deps := Deps{
		Policy: func() callback.Policy {
			mu.Lock()
			defer mu.Unlock()
			calls++
			e := explain
			return callback.Policy{Build: &callback.BuildInfo{Version: "v5", Commit: "c"}, ExplainModeDefault: &e}
		},
		RepoAutoMerge: func(repo string) bool { return repo == "o/yes" },
	}
	c, _ := serve(t, deps)
	p, err := c.Policy(ctx)
	if err != nil || p.Build == nil || p.Build.Version != "v5" || p.ExplainModeDefault == nil || *p.ExplainModeDefault != "off" || !p.RepoAutoMerge {
		t.Fatalf("Policy = %+v, %v", p, err)
	}
	mu.Lock()
	explain = "on"
	mu.Unlock()
	p, err = c.Policy(ctx)
	mu.Lock()
	n := calls
	mu.Unlock()
	if err != nil || *p.ExplainModeDefault != "on" || n != 2 {
		t.Fatalf("Policy not live: %+v, %v (calls %d)", p, err, n)
	}
	if ok, err := c.RepoAutoMergeEnabled(ctx, "o/yes"); err != nil || !ok {
		t.Fatalf("RepoAutoMergeEnabled(o/yes) = %v, %v", ok, err)
	}
	if ok, err := c.RepoAutoMergeEnabled(ctx, "o/no"); err != nil || ok {
		t.Fatalf("RepoAutoMergeEnabled(o/no) = %v, %v", ok, err)
	}

	noMerge := New(Deps{Policy: deps.Policy})
	if p, err := noMerge.Policy(ctx); err != nil || p.RepoAutoMerge {
		t.Fatalf("Policy without RepoAutoMerge = %+v, %v", p, err)
	}
	if _, err := noMerge.RepoAutoMergeEnabled(ctx, "o/yes"); !errors.Is(err, callback.ErrNotConfigured) {
		t.Fatalf("RepoAutoMergeEnabled without resolver = %v", err)
	}
	none := New(Deps{})
	if none.Configured(callback.CapabilityPolicy) {
		t.Error("policy configured with no deps")
	}
	if _, err := none.Policy(ctx); !errors.Is(err, callback.ErrNotConfigured) {
		t.Errorf("Policy with no deps = %v", err)
	}
	if _, err := none.RepoAutoMergeEnabled(ctx, "o/r"); !errors.Is(err, callback.ErrNotConfigured) {
		t.Errorf("RepoAutoMergeEnabled with no deps = %v", err)
	}
}

func TestMintFailures(t *testing.T) {
	ctx := context.Background()
	f := &fakeDeps{mintErr: errors.New("app not installed")}
	if _, err := New(Deps{Tokens: f}).MintToken(ctx, callback.MintTokenRequest{Agent: "a"}); err == nil || !strings.Contains(err.Error(), "app not installed") || !strings.Contains(err.Error(), `"a"`) {
		t.Errorf("mint error = %v", err)
	}
	f = &fakeDeps{mintEmpty: true}
	if _, err := New(Deps{Tokens: f}).MintToken(ctx, callback.MintTokenRequest{}); err == nil || !strings.Contains(err.Error(), "no token") {
		t.Errorf("empty token = %v", err)
	}
	// Over the socket a scoper error is 502 → ErrUnavailable: fail closed.
	c, logBuf := serve(t, Deps{Tokens: &fakeDeps{mintErr: errors.New("private failure detail")}})
	if _, err := c.MintToken(ctx, callback.MintTokenRequest{Agent: "a"}); !errors.Is(err, callback.ErrUnavailable) {
		t.Errorf("socket mint error = %v", err)
	}
	if strings.Contains(logBuf.String(), "private failure detail") {
		t.Error("server log carried the upstream error text")
	}
	// Default TTL when none or a longer one is requested.
	f = &fakeDeps{}
	for _, ttl := range []int{0, 7200} {
		resp, err := New(Deps{Tokens: f}).MintToken(ctx, callback.MintTokenRequest{TTLSeconds: ttl, Repo: "bare"})
		if err != nil {
			t.Fatal(err)
		}
		if d := time.Until(resp.ExpiresAt); d > InstallationTokenTTL || d < InstallationTokenTTL-time.Minute {
			t.Errorf("ttl %d: expiry %v", ttl, d)
		}
		if strings.Join(f.mintRepos, ",") != "bare" {
			t.Errorf("repos = %v", f.mintRepos)
		}
	}
	if _, err := New(Deps{Tokens: f}).MintToken(ctx, callback.MintTokenRequest{}); err != nil || f.mintRepos != nil {
		t.Errorf("empty request should scope installation-wide: %v %v", f.mintRepos, err)
	}
}

func TestBadKindsAreErrors(t *testing.T) {
	f := &fakeDeps{}
	caps := New(allDeps(f))
	ctx := context.Background()
	if err := caps.RecordUsage(ctx, callback.UsageRecord{Kind: "weird"}); !errors.Is(err, errUnknownKind) {
		t.Errorf("usage kind = %v", err)
	}
	if _, err := caps.ScanText(ctx, callback.TextScanRequest{Direction: "sideways"}); !errors.Is(err, errUnknownDirection) {
		t.Errorf("direction = %v", err)
	}
	if err := caps.ReportCondition(ctx, callback.ConditionReport{Action: "poke"}); !errors.Is(err, errUnknownAction) {
		t.Errorf("action = %v", err)
	}
	if len(f.usage)+len(f.conds) != 0 {
		t.Error("bad kinds reached the deps")
	}
	// Headers rebuilt on the hive side are canonical.
	if err := caps.RecordREST(ctx, callback.RESTRecord{RateLimit: map[string]string{"x-ratelimit-reset": "1"}}); err != nil {
		t.Fatal(err)
	}
	if f.headers.Get("X-RateLimit-Reset") != "1" {
		t.Errorf("headers = %v", f.headers)
	}
	f.prErr = errors.New("forge down")
	if _, err := caps.CreatePR(ctx, callback.CreatePRRequest{}); err == nil || err.Error() != "forge down" {
		t.Errorf("pr err = %v", err)
	}
}

func TestHandlerRefusesWithoutAuth(t *testing.T) {
	f := &fakeDeps{}
	h := Handler(allDeps(f), "shared-secret")
	rr := &recorder{header: http.Header{}}
	req, _ := http.NewRequest(http.MethodGet, callback.PathHealth, nil)
	h.ServeHTTP(rr, req)
	if rr.status != http.StatusUnauthorized {
		t.Errorf("no bearer: %d", rr.status)
	}
	rr = &recorder{header: http.Header{}}
	req.Header.Set("Authorization", "Bearer shared-secret")
	h.ServeHTTP(rr, req)
	if rr.status != http.StatusOK {
		t.Errorf("bearer: %d %s", rr.status, rr.body.String())
	}
	if !strings.Contains(rr.body.String(), callback.CapabilityTokens) {
		t.Errorf("health should list configured capabilities: %s", rr.body.String())
	}
}

type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(s int)           { r.status = s }
