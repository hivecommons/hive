package callback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
)

const (
	testToken      = "ghs_minted_secret_do_not_log_0123456789"
	testBearer     = "bearer-shared-secret-ABC"
	testLinearTok  = "lin_oauth_secret_XYZ"
	testLinearKey  = "lin_api_secret_QRS"
	notConfiguredC = "__none__"
)

// fakeCaps is a Capabilities that records what it was asked and answers
// from fields. absent lists capabilities Configured denies.
type fakeCaps struct {
	mu      sync.Mutex
	absent  map[string]bool
	fail    error
	mint    []MintTokenRequest
	rest    []RESTRecord
	usage   []UsageRecord
	canary  []CanaryScanRequest
	text    []TextScanRequest
	conds   []ConditionReport
	pulls   []CreatePRRequest
	linear  LinearCredential
	snap    RateLimitSnapshot
	detect  bool
	blocked bool
}

func (f *fakeCaps) Configured(c string) bool { return !f.absent[c] }

func (f *fakeCaps) MintToken(_ context.Context, req MintTokenRequest) (MintTokenResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mint = append(f.mint, req)
	if f.fail != nil {
		return MintTokenResponse{}, f.fail
	}
	return MintTokenResponse{Token: testToken, ExpiresAt: time.Date(2026, 10, 25, 13, 0, 0, 0, time.UTC)}, nil
}

func (f *fakeCaps) RecordREST(_ context.Context, rec RESTRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rest = append(f.rest, rec)
	return f.fail
}

func (f *fakeCaps) RateLimit(context.Context) (RateLimitSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap, f.fail
}

func (f *fakeCaps) RecordUsage(_ context.Context, rec UsageRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usage = append(f.usage, rec)
	return f.fail
}

func (f *fakeCaps) ScanCanary(_ context.Context, req CanaryScanRequest) (CanaryScanResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canary = append(f.canary, req)
	if f.fail != nil {
		return CanaryScanResponse{}, f.fail
	}
	return CanaryScanResponse{Detected: f.detect, Agent: req.Agent, Source: req.Source}, nil
}

func (f *fakeCaps) ScanText(_ context.Context, req TextScanRequest) (TextScanResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.text = append(f.text, req)
	if f.fail != nil {
		return TextScanResponse{}, f.fail
	}
	if f.blocked {
		return TextScanResponse{Blocked: true, CriticalInjection: true, Findings: []TextFinding{{Kind: "injection", Snippet: "ign***", Rule: "inj-1"}}}, nil
	}
	return TextScanResponse{}, nil
}

func (f *fakeCaps) ReportCondition(_ context.Context, rep ConditionReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conds = append(f.conds, rep)
	return f.fail
}

func (f *fakeCaps) LinearCredential(context.Context) (LinearCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.linear, f.fail
}

func (f *fakeCaps) CreatePR(_ context.Context, req CreatePRRequest) (CreatePRResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls = append(f.pulls, req)
	if f.fail != nil {
		return CreatePRResponse{}, f.fail
	}
	return CreatePRResponse{Number: 42, URL: "https://github.com/o/r/pull/42", Author: "hive[bot]", AlreadyExisted: true, DuplicateTree: true, DuplicateReason: "same-tree"}, nil
}

var _ Capabilities = (*fakeCaps)(nil)

// shortSocketDir returns a private directory whose socket paths fit in
// sun_path on macOS (t.TempDir is too long there).
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cbk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// serveUnix runs srv on a fresh socket and returns a client for it plus the
// captured log output.
func serveUnix(t *testing.T, srv *Server, clientToken string) (*Client, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	srv.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sock := filepath.Join(shortSocketDir(t), SocketName)
	ls, err := Listen(ListenConfig{Socket: sock, Token: srv.Token})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ls) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	c, err := NewClient("unix://"+sock, clientToken)
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, c)
	return c, &logs
}

// waitReady makes one Health call. The socket is bound by Listen before
// Serve starts, so the connection queues in the backlog and the call
// returns once Serve accepts: no polling needed.
func waitReady(t *testing.T, c *Client) {
	t.Helper()
	if _, err := c.Health(context.Background()); err != nil && !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("endpoint never became ready: %v", err)
	}
}

func TestEveryCapabilityOverUnixSocket(t *testing.T) {
	caps := &fakeCaps{detect: true, blocked: true, linear: LinearCredential{AccessToken: testLinearTok}, snap: RateLimitSnapshot{Remaining: 4000, Reset: time.Unix(1_800_000_000, 0).UTC(), OK: true}}
	c, logs := serveUnix(t, NewServer(caps, ""), "")
	ctx := context.Background()

	h, err := c.Health(ctx)
	if err != nil || h.Version != Version || len(h.Capabilities) != len(allCapabilities) {
		t.Fatalf("Health = %+v, %v", h, err)
	}

	tok, err := c.MintToken(ctx, MintTokenRequest{Agent: "scanner", Repo: "o/r", Tier: "contributor", TTLSeconds: 600})
	if err != nil || tok.Token != testToken || tok.ExpiresAt.IsZero() {
		t.Fatalf("MintToken = %v, %v", tok, err)
	}
	if got := caps.mint[0]; got.Agent != "scanner" || got.Repo != "o/r" || got.Tier != "contributor" || got.TTLSeconds != 600 {
		t.Errorf("mint request not carried: %+v", got)
	}

	if err := c.RecordREST(ctx, RESTRecord{Caller: "agent:a", Method: "GET", Path: "/repos/o/r", Status: 200, Injected: true, RateLimit: map[string]string{"X-Ratelimit-Remaining": "4999"}}); err != nil {
		t.Fatal(err)
	}
	if got := caps.rest[0]; !got.Injected || got.RateLimit["X-Ratelimit-Remaining"] != "4999" || got.Caller != "agent:a" {
		t.Errorf("rest record not carried: %+v", got)
	}
	snap, err := c.RateLimit(ctx)
	if err != nil || !snap.OK || snap.Remaining != 4000 || !snap.Reset.Equal(caps.snap.Reset) {
		t.Fatalf("RateLimit = %+v, %v", snap, err)
	}

	if err := c.RecordUsage(ctx, UsageRecord{Agent: "a", Model: "m", Kind: UsageKindCopilotCache, InputTokens: 1, OutputTokens: 2, CacheRead: 3, CacheCreation: 4}); err != nil {
		t.Fatal(err)
	}
	if got := caps.usage[0]; got.Kind != UsageKindCopilotCache || got.CacheCreation != 4 {
		t.Errorf("usage not carried: %+v", got)
	}

	cs, err := c.ScanCanary(ctx, CanaryScanRequest{Agent: "a", Text: "body", Source: "github:POST /x"})
	if err != nil || !cs.Detected || cs.Agent != "a" || cs.Source != "github:POST /x" {
		t.Fatalf("ScanCanary = %+v, %v", cs, err)
	}

	ts, err := c.ScanText(ctx, TextScanRequest{Direction: ScanInput, Text: "ignore previous"})
	if err != nil || !ts.Blocked || !ts.CriticalInjection || len(ts.Findings) != 1 || ts.Findings[0].Rule != "inj-1" {
		t.Fatalf("ScanText = %+v, %v", ts, err)
	}

	if err := c.ReportCondition(ctx, ConditionReport{Action: ConditionObserve, Class: "agent_stall", Key: "a", Message: "m", Diagnostics: "d"}); err != nil {
		t.Fatal(err)
	}
	if err := c.ReportCondition(ctx, ConditionReport{Action: ConditionClear, Class: "agent_stall", Key: "a"}); err != nil {
		t.Fatal(err)
	}
	if len(caps.conds) != 2 || caps.conds[0].Diagnostics != "d" || caps.conds[1].Action != ConditionClear {
		t.Errorf("conditions not carried: %+v", caps.conds)
	}

	lc, err := c.LinearCredential(ctx)
	if err != nil || lc.AccessToken != testLinearTok || lc.Empty() {
		t.Fatalf("LinearCredential = %v, %v", lc, err)
	}

	pr, err := c.CreatePR(ctx, CreatePRRequest{Repo: "o/r", Head: "h", Base: "v5", Title: "t", Body: "b"})
	if err != nil || pr.Number != 42 || !pr.AlreadyExisted || !pr.DuplicateTree || pr.DuplicateReason != "same-tree" || pr.Author != "hive[bot]" {
		t.Fatalf("CreatePR = %+v, %v", pr, err)
	}
	if caps.pulls[0].Body != "b" {
		t.Errorf("pull request body not carried: %+v", caps.pulls[0])
	}

	st := c.Status()
	if !st.Reachable || st.FailedCalls != 0 || st.LastSuccess.IsZero() || st.LastError != "" {
		t.Errorf("Status after success = %+v", st)
	}

	// The server logged method/path/status for every call and nothing
	// sensitive.
	for _, secret := range []string{testToken, testLinearTok, "ignore previous", "body"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("server log contains %q:\n%s", secret, logs.String())
		}
	}
	if !strings.Contains(logs.String(), PathTokens) {
		t.Errorf("server log has no route lines:\n%s", logs.String())
	}
}

func TestTokenNeverPrinted(t *testing.T) {
	r := MintTokenResponse{Token: testToken, ExpiresAt: time.Now()}
	for _, s := range []string{fmt.Sprint(r), fmt.Sprintf("%v", r), fmt.Sprintf("%+v", r), fmt.Sprintf("%#v", r), fmt.Sprintf("%s", r), fmt.Sprint(&r)} {
		if strings.Contains(s, testToken) {
			t.Errorf("formatted response leaks the token: %s", s)
		}
	}
	l := LinearCredential{AccessToken: testLinearTok, APIKey: testLinearKey}
	for _, s := range []string{fmt.Sprint(l), fmt.Sprintf("%+v", l), fmt.Sprintf("%#v", l)} {
		if strings.Contains(s, testLinearTok) || strings.Contains(s, testLinearKey) {
			t.Errorf("formatted credential leaks a secret: %s", s)
		}
	}
	if !strings.Contains(fmt.Sprint(LinearCredential{}), `""`) {
		t.Error("empty credential should print as empty, not redacted")
	}
	// JSON still carries it: that is the wire.
	data, _ := json.Marshal(r)
	if !strings.Contains(string(data), testToken) {
		t.Error("JSON form must carry the token")
	}
}

func TestClientErrorsNeverQuoteBearer(t *testing.T) {
	caps := &fakeCaps{}
	c, logs := serveUnix(t, NewServer(caps, testBearer), "wrong-"+testBearer)
	_, err := c.MintToken(context.Background(), MintTokenRequest{})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if strings.Contains(err.Error(), testBearer) {
		t.Errorf("error quotes the bearer: %v", err)
	}
	if strings.Contains(logs.String(), testBearer) {
		t.Errorf("server log quotes the bearer:\n%s", logs.String())
	}
	if len(caps.mint) != 0 {
		t.Error("an unauthorized request must not reach the capability")
	}
	if st := c.Status(); !st.Reachable {
		t.Errorf("a 401 is hive answering; Status = %+v", st)
	}
}

func TestBearerTokenAccepted(t *testing.T) {
	caps := &fakeCaps{}
	c, _ := serveUnix(t, NewServer(caps, testBearer), testBearer)
	if _, err := c.MintToken(context.Background(), MintTokenRequest{Repo: "o/r"}); err != nil {
		t.Fatal(err)
	}
}

func TestTokenlessServerRefusesOffSocket(t *testing.T) {
	// Handler mounted on a plain TCP httptest server with no unix tag: the
	// token-less rule must refuse, exactly as crux/serve does.
	caps := &fakeCaps{}
	ts := httptest.NewServer(NewServer(caps, "").Handler())
	defer ts.Close()
	c, err := NewClient(ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Health(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("off-socket health = %v, want ErrUnauthorized", err)
	}
	// The same handler with the unix tag admits it.
	ts2 := httptest.NewUnstartedServer(NewServer(caps, "").Handler())
	ts2.Config.BaseContext = func(net.Listener) context.Context { return WithUnixNetwork(context.Background()) }
	ts2.Start()
	defer ts2.Close()
	c2, _ := NewClient(ts2.URL, "")
	if _, err := c2.Health(context.Background()); err != nil {
		t.Fatalf("tagged health = %v", err)
	}
	// With a token, TCP is fine once the token is presented.
	ts3 := httptest.NewServer(NewServer(caps, testBearer).Handler())
	defer ts3.Close()
	c3, _ := NewClient(ts3.URL, testBearer)
	if _, err := c3.Health(context.Background()); err != nil {
		t.Fatalf("tcp+token health = %v", err)
	}
}

func TestUnreachableFailsClosed(t *testing.T) {
	sock := filepath.Join(shortSocketDir(t), "nobody.sock")
	c, err := NewClient("unix://"+sock, "")
	if err != nil {
		t.Fatal(err)
	}
	c.SetTimeout(200 * time.Millisecond)
	ctx := context.Background()

	if _, err := c.MintToken(ctx, MintTokenRequest{Repo: "o/r"}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("MintToken = %v, want ErrUnavailable", err)
	}
	if err := c.RecordREST(ctx, RESTRecord{Method: "GET", Path: "/"}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("RecordREST = %v", err)
	}
	if snap, err := c.RateLimit(ctx); !errors.Is(err, ErrUnavailable) || snap.OK {
		t.Errorf("RateLimit = %+v, %v", snap, err)
	}
	if err := c.RecordUsage(ctx, UsageRecord{Kind: UsageKindGateway}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("RecordUsage = %v", err)
	}
	if _, err := c.ScanCanary(ctx, CanaryScanRequest{}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("ScanCanary = %v", err)
	}
	if _, err := c.ScanText(ctx, TextScanRequest{Direction: ScanInput}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("ScanText = %v", err)
	}
	if err := c.ReportCondition(ctx, ConditionReport{Action: ConditionObserve, Class: "x"}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("ReportCondition = %v", err)
	}
	if lc, err := c.LinearCredential(ctx); !errors.Is(err, ErrUnavailable) || !lc.Empty() {
		t.Errorf("LinearCredential = %v, %v", lc, err)
	}
	if _, err := c.CreatePR(ctx, CreatePRRequest{}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("CreatePR = %v", err)
	}

	st := c.Status()
	if st.Reachable || st.FailedCalls != 9 || st.Since.IsZero() || st.LastError == "" {
		t.Errorf("Status = %+v", st)
	}
	if strings.Contains(st.LastError, "http://") {
		t.Errorf("transport error should not carry the URL: %s", st.LastError)
	}
}

func TestStatusRecoversAfterOutage(t *testing.T) {
	caps := &fakeCaps{}
	sock := filepath.Join(shortSocketDir(t), SocketName)
	c, err := NewClient("unix://"+sock, "")
	if err != nil {
		t.Fatal(err)
	}
	c.SetTimeout(200 * time.Millisecond)
	if _, err := c.Health(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("pre-start health = %v", err)
	}
	if c.Status().Reachable {
		t.Fatal("status should be unreachable")
	}
	ls, err := Listen(ListenConfig{Socket: sock})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- NewServer(caps, "").Serve(ctx, ls) }()
	waitReady(t, c)
	st := c.Status()
	if !st.Reachable || st.FailedCalls != 0 || !st.Since.IsZero() {
		t.Errorf("status after recovery = %+v", st)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket file not removed on shutdown: %v", err)
	}
}

func TestServerErrorsBecomeUnavailableAndUpstream(t *testing.T) {
	caps := &fakeCaps{fail: errors.New("github said no")}
	c, _ := serveUnix(t, NewServer(caps, ""), "")
	ctx := context.Background()
	_, err := c.MintToken(ctx, MintTokenRequest{Repo: "o/r"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("502 should be ErrUnavailable: %v", err)
	}
	var se *api.StatusError
	if !errors.As(err, &se) || se.Status != http.StatusBadGateway || se.Body.Code != CodeUpstream || !strings.Contains(se.Body.Error, "github said no") {
		t.Errorf("status error = %+v", se)
	}
	for _, call := range []func() error{
		func() error { return c.RecordREST(ctx, RESTRecord{Method: "GET", Path: "/"}) },
		func() error { _, e := c.RateLimit(ctx); return e },
		func() error { return c.RecordUsage(ctx, UsageRecord{Kind: UsageKindGateway}) },
		func() error { _, e := c.ScanCanary(ctx, CanaryScanRequest{}); return e },
		func() error { _, e := c.ScanText(ctx, TextScanRequest{Direction: ScanOutput}); return e },
		func() error { return c.ReportCondition(ctx, ConditionReport{Action: ConditionClear, Class: "x"}) },
		func() error { _, e := c.LinearCredential(ctx); return e },
		func() error {
			_, e := c.CreatePR(ctx, CreatePRRequest{Repo: "r", Head: "h", Base: "b", Title: "t"})
			return e
		},
	} {
		if err := call(); !errors.Is(err, ErrUnavailable) {
			t.Errorf("capability failure = %v, want ErrUnavailable", err)
		}
	}
}

func TestNotConfigured(t *testing.T) {
	caps := &fakeCaps{absent: map[string]bool{CapabilityTokens: true, CapabilityCanary: true, CapabilityLinear: true}}
	c, _ := serveUnix(t, NewServer(caps, ""), "")
	ctx := context.Background()
	h, err := c.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range h.Capabilities {
		if caps.absent[name] {
			t.Errorf("health lists absent capability %s", name)
		}
	}
	if len(h.Capabilities) != len(allCapabilities)-3 {
		t.Errorf("health capabilities = %v", h.Capabilities)
	}
	if _, err := c.MintToken(ctx, MintTokenRequest{}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("MintToken = %v, want ErrNotConfigured", err)
	}
	if _, err := c.ScanCanary(ctx, CanaryScanRequest{}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("ScanCanary = %v", err)
	}
	if _, err := c.LinearCredential(ctx); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("LinearCredential = %v", err)
	}
	if len(caps.mint)+len(caps.canary) != 0 {
		t.Error("absent capabilities must not be dispatched")
	}
	if !c.Status().Reachable {
		t.Error("not_configured is hive answering")
	}
	// A capability that reports ErrNotConfigured itself is also a 404.
	caps2 := &fakeCaps{fail: ErrNotConfigured}
	c2, _ := serveUnix(t, NewServer(caps2, ""), "")
	if _, err := c2.CreatePR(ctx, CreatePRRequest{Repo: "r", Head: "h", Base: "b", Title: "t"}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("CreatePR = %v", err)
	}
	// A nil Caps has nothing.
	c3, _ := serveUnix(t, NewServer(nil, ""), "")
	h3, err := c3.Health(ctx)
	if err != nil || len(h3.Capabilities) != 0 {
		t.Errorf("nil caps health = %+v, %v", h3, err)
	}
}

func TestServerValidation(t *testing.T) {
	caps := &fakeCaps{}
	c, _ := serveUnix(t, NewServer(caps, ""), "")
	ctx := context.Background()
	cases := []struct {
		name string
		call func() error
	}{
		{"negative ttl", func() error { _, e := c.MintToken(ctx, MintTokenRequest{TTLSeconds: -1}); return e }},
		{"rest without method", func() error { return c.RecordREST(ctx, RESTRecord{Path: "/"}) }},
		{"usage bad kind", func() error { return c.RecordUsage(ctx, UsageRecord{Kind: "bogus"}) }},
		{"text bad direction", func() error { _, e := c.ScanText(ctx, TextScanRequest{Direction: "sideways"}); return e }},
		{"condition bad action", func() error { return c.ReportCondition(ctx, ConditionReport{Action: "poke", Class: "x"}) }},
		{"condition no class", func() error { return c.ReportCondition(ctx, ConditionReport{Action: ConditionObserve}) }},
		{"pull missing fields", func() error { _, e := c.CreatePR(ctx, CreatePRRequest{Repo: "r"}); return e }},
	}
	for _, tc := range cases {
		err := tc.call()
		var se *api.StatusError
		if !errors.As(err, &se) || se.Status != http.StatusBadRequest || se.Body.Code != CodeBadRequest {
			t.Errorf("%s: err = %v, want 400 bad_request", tc.name, err)
		}
		if errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: a 400 is not an outage", tc.name)
		}
	}
	if n := len(caps.mint) + len(caps.rest) + len(caps.usage) + len(caps.text) + len(caps.conds) + len(caps.pulls); n != 0 {
		t.Errorf("invalid requests reached the capability %d times", n)
	}
	// Malformed JSON.
	req, _ := http.NewRequest(http.MethodPost, c.addr.BaseURL+PathTokens, strings.NewReader("{nope"))
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed JSON status = %d", resp.StatusCode)
	}
	// Unknown route is a plain 404 without CodeNotConfigured: not an outage,
	// not "not configured".
	err = c.do(ctx, http.MethodGet, "/v1/nothing", nil, nil)
	if errors.Is(err, ErrNotConfigured) || errors.Is(err, ErrUnavailable) || !api.IsNotFound(err) {
		t.Errorf("unknown route = %v", err)
	}
}

func TestMintTokenServerFillsExpiryAndRefusesEmpty(t *testing.T) {
	// Expiry defaulted when the capability does not know it.
	srv := NewServer(capsFunc{fakeCaps: &fakeCaps{}, mint: func(MintTokenRequest) (MintTokenResponse, error) {
		return MintTokenResponse{Token: "t"}, nil
	}}, "")
	c, _ := serveUnix(t, srv, "")
	before := time.Now()
	resp, err := c.MintToken(context.Background(), MintTokenRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if d := resp.ExpiresAt.Sub(before); d < installationTokenTTL-time.Minute || d > installationTokenTTL+time.Minute {
		t.Errorf("defaulted expiry %v, want ~%v", d, installationTokenTTL)
	}
	// An empty token from the capability is a 502, never handed out.
	srv2 := NewServer(capsFunc{fakeCaps: &fakeCaps{}, mint: func(MintTokenRequest) (MintTokenResponse, error) {
		return MintTokenResponse{}, nil
	}}, "")
	c2, _ := serveUnix(t, srv2, "")
	if _, err := c2.MintToken(context.Background(), MintTokenRequest{}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("empty token = %v, want ErrUnavailable", err)
	}
}

// capsFunc is a Capabilities with a pluggable MintToken; the rest refuse.
type capsFunc struct {
	*fakeCaps
	mint func(MintTokenRequest) (MintTokenResponse, error)
}

func (c capsFunc) MintToken(_ context.Context, req MintTokenRequest) (MintTokenResponse, error) {
	return c.mint(req)
}

func TestClientEmptyTokenFromWire(t *testing.T) {
	// A server that bypasses the Server type and returns {"token":""}.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"token":"","expires_at":"2026-01-01T00:00:00Z"}`))
	}))
	defer ts.Close()
	c, _ := NewClient(ts.URL, "x")
	if _, err := c.MintToken(context.Background(), MintTokenRequest{}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("empty wire token = %v", err)
	}
	// Garbage JSON from the server is also unavailable.
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`nope`))
	}))
	defer ts2.Close()
	c2, _ := NewClient(ts2.URL, "x")
	if _, err := c2.RateLimit(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("garbage body = %v", err)
	}
}

func TestClientTimeoutAndContext(t *testing.T) {
	block := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer ts.Close()
	defer close(block)
	c, _ := NewClient(ts.URL, "x")
	c.SetTimeout(100 * time.Millisecond)
	start := time.Now()
	if _, err := c.Health(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("timeout = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("default timeout not applied")
	}
	// A caller deadline wins over the default.
	c.SetTimeout(0)
	if c.timeout != DefaultTimeout {
		t.Errorf("SetTimeout(0) = %v, want default", c.timeout)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Health(ctx); !errors.Is(err, ErrUnavailable) {
		t.Errorf("caller deadline = %v", err)
	}
	// nil context is tolerated.
	if _, err := c.Health(nil); !errors.Is(err, ErrUnavailable) { //nolint:staticcheck // deliberate nil
		t.Errorf("nil ctx = %v", err)
	}
}

func TestNewClientAndSetHTTPClient(t *testing.T) {
	if _, err := NewClient("", ""); err == nil {
		t.Error("empty address accepted")
	}
	c, err := NewClient("unix:///tmp/x.sock", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Address().Network != "unix" {
		t.Errorf("address = %+v", c.Address())
	}
	tr := c.http.Transport
	c.SetHTTPClient(&http.Client{})
	if c.http.Transport != tr {
		t.Error("SetHTTPClient dropped the socket transport")
	}
	own := &http.Transport{}
	c.SetHTTPClient(&http.Client{Transport: own})
	if c.http.Transport != own {
		t.Error("SetHTTPClient overrode the caller's transport")
	}
}

func TestListenRefusals(t *testing.T) {
	dir := shortSocketDir(t)
	// TCP without a token.
	cfg := ListenConfig{Socket: filepath.Join(dir, "a.sock"), Listen: "127.0.0.1:0"}
	if _, err := Listen(cfg); err == nil || !strings.Contains(err.Error(), "bearer token") {
		t.Errorf("tcp without token = %v", err)
	}
	// TCP with a token binds both.
	ls, err := Listen(ListenConfig{Socket: filepath.Join(dir, "b.sock"), Listen: "127.0.0.1:0", Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if addrs := ls.Addrs(); len(addrs) != 2 || !strings.HasPrefix(addrs[0], "unix://") || !strings.HasPrefix(addrs[1], "http://") {
		t.Errorf("addrs = %v", addrs)
	}
	fi, err := os.Stat(filepath.Join(dir, "b.sock"))
	if err != nil || fi.Mode().Perm() != SocketMode {
		t.Errorf("socket mode = %v, %v", fi, err)
	}
	// A second listener on a live socket is refused.
	if _, err := Listen(ListenConfig{Socket: filepath.Join(dir, "b.sock")}); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Errorf("double listen = %v", err)
	}
	ls.Close()
	// A stale socket file is replaced.
	if err := os.WriteFile(filepath.Join(dir, "b.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ls2, err := Listen(ListenConfig{Socket: filepath.Join(dir, "b.sock")})
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	ls2.Close()
	// A directory readable by others is refused.
	loose := filepath.Join(dir, "loose")
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(ListenConfig{Socket: filepath.Join(loose, "c.sock")}); err == nil || !strings.Contains(err.Error(), "readable by others") {
		t.Errorf("loose dir = %v", err)
	}
	// A symlinked directory is refused.
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(ListenConfig{Socket: filepath.Join(link, "d.sock")}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("symlink dir = %v", err)
	}
	// A file where the directory should be.
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(ListenConfig{Socket: filepath.Join(file, "e.sock")}); err == nil {
		t.Error("file as dir accepted")
	}
	if err := ownedPrivateDir(file); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("ownedPrivateDir(file) = %v", err)
	}
	if err := ownedPrivateDir(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing dir accepted")
	}
	// Default socket path.
	t.Setenv(SocketEnvVar, "")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/7")
	if got := DefaultSocket(); got != "/run/user/7/crux/hive.sock" {
		t.Errorf("DefaultSocket = %s", got)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if got := DefaultSocket(); !strings.HasSuffix(got, fmt.Sprintf("crux-%d/hive.sock", os.Getuid())) {
		t.Errorf("DefaultSocket fallback = %s", got)
	}
	t.Setenv(SocketEnvVar, "/x/y.sock")
	if got := DefaultSocket(); got != "/x/y.sock" {
		t.Errorf("DefaultSocket env = %s", got)
	}
	// Validate defaults the socket.
	var empty ListenConfig
	if err := empty.Validate(); err != nil || empty.Socket != "/x/y.sock" {
		t.Errorf("Validate = %v, %+v", err, empty)
	}
}

func TestServeReportsListenerError(t *testing.T) {
	// Closing a listener under Serve makes it return the error.
	dir := shortSocketDir(t)
	sock := filepath.Join(dir, "s.sock")
	ls, err := Listen(ListenConfig{Socket: sock})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- NewServer(&fakeCaps{}, "").Serve(context.Background(), ls) }()
	c, err := NewClient("unix://"+sock, "")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, c) // Serve is accepting before the listener is pulled.
	_ = ls.Unix.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Serve returned nil after its listener died")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
}

func TestNotConfiguredSentinelStringAndCodes(t *testing.T) {
	if ConditionClassUnavailable != "hive_callback_unavailable" {
		t.Error("condition class is a wire name; do not change it")
	}
	for _, p := range []string{PathTokens, PathGitHubREST, PathGitHubRateLimit, PathUsage, PathScanCanary, PathScanText, PathConditions, PathLinearCredential, PathPulls, PathHealth} {
		if !strings.HasPrefix(p, "/"+Version+"/") {
			t.Errorf("route %s is not under /%s", p, Version)
		}
	}
}

// bareCaps has no Configured method: the server assumes every capability.
type bareCaps struct{ Capabilities }

func TestCapabilitiesWithoutConfiguredHaveAll(t *testing.T) {
	c, _ := serveUnix(t, NewServer(bareCaps{&fakeCaps{}}, ""), "")
	h, err := c.Health(context.Background())
	if err != nil || len(h.Capabilities) != len(allCapabilities) {
		t.Errorf("health = %+v, %v", h, err)
	}
}

func TestListenUnbindablePath(t *testing.T) {
	dir := shortSocketDir(t)
	long := filepath.Join(dir, strings.Repeat("x", 120)+".sock")
	if _, err := Listen(ListenConfig{Socket: long}); err == nil {
		t.Error("over-long socket path accepted")
	}
	// A directory that cannot be created (parent is a file).
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(ListenConfig{Socket: filepath.Join(file, "sub", "s.sock")}); err == nil {
		t.Error("uncreatable directory accepted")
	}
}
