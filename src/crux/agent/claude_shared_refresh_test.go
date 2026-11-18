package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/claude"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/internal/testutil"
)

// sharedRefreshFixture provisions n bridged Claude agents sharing one login
// that expires at expiresAt, and returns the manager, the audit sink, the
// per-agent link paths and the shared path.
func sharedRefreshFixture(t *testing.T, n int, expiresAt int64) (*Manager, *fakeAuditSink, []string, string) {
	t.Helper()
	shared := withSharedAgentHome(t)
	m, sink := testManagerWithSink(t)
	target := filepath.Join(shared, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, bridgeTestCreds(t, "shared", expiresAt), 0o660); err != nil {
		t.Fatal(err)
	}
	links := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("agent-%d", i)
		ap := &AgentProcess{Name: name, UID: 1001 + i, Config: config.AgentConfig{Backend: "claude"}}
		m.mu.Lock()
		m.agents[name] = ap
		m.mu.Unlock()
		m.setupInteractiveHome(ap, "claude")
		link := filepath.Join(interactiveHomePath(name), ".claude", ".credentials.json")
		assertLinkRestored(t, link, target)
		links = append(links, link)
	}
	return m, sink, links, target
}

// fakeRefreshServer stands in for the authorization server: it honours the
// current refresh token exactly once and rotates it, and rejects any
// presentation of a token it has already rotated away, the way reuse
// detection revokes a grant. It never inspects anything but the refresh
// token. With started and release set, the first request reports on started
// and then blocks until release is closed, so a test can hold one refresh in
// flight deterministically.
type fakeRefreshServer struct {
	mu       sync.Mutex
	calls    atomic.Int32
	rejected atomic.Int32
	live     string
	lifetime time.Duration
	started  chan struct{}
	release  chan struct{}
}

func (s *fakeRefreshServer) refresh(prev *claude.OAuthTokens) (*claude.OAuthTokens, error) {
	if s.calls.Add(1) == 1 && s.started != nil {
		close(s.started)
		<-s.release
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev == nil || prev.RefreshToken != s.live {
		s.rejected.Add(1)
		return nil, fmt.Errorf("%w: invalid_grant", claude.ErrRefreshGrantRejected)
	}
	n := s.calls.Load()
	s.live = fmt.Sprintf("ref-rotated-%d-%s", n, bridgeTestTokenSentinel)
	return &claude.OAuthTokens{
		AccessToken:  fmt.Sprintf("acc-rotated-%d-%s", n, bridgeTestTokenSentinel),
		RefreshToken: s.live,
		ExpiresAt:    time.Now().Add(s.lifetime).UnixMilli(),
		Scopes:       prev.Scopes,
	}, nil
}

func withFakeRefresh(t *testing.T, srv *fakeRefreshServer) {
	t.Helper()
	prev := claudeRefreshTokens
	claudeRefreshTokens = srv.refresh
	t.Cleanup(func() { claudeRefreshTokens = prev })
}

// TestSharedClaudeRefresh_NAgentsOneGrantExactlyOneRequest is the acceptance
// test for #18731: N agents share one grant at expiry, the hive refreshes it
// once, every agent reads the new token through its bridge, and the grant is
// never presented twice (which is what revokes it).
func TestSharedClaudeRefresh_NAgentsOneGrantExactlyOneRequest(t *testing.T) {
	const n = 8
	m, sink, links, target := sharedRefreshFixture(t, n, time.Now().Add(time.Minute).UnixMilli())
	srv := &fakeRefreshServer{
		live: readTokensAt(t, target).RefreshToken, lifetime: 8 * time.Hour,
		started: make(chan struct{}), release: make(chan struct{}),
	}
	withFakeRefresh(t, srv)
	var logBuf bytes.Buffer
	m.logger = slog.New(slog.NewTextHandler(&logBuf, nil))

	// One pass reaches the authorization server and is held there; every
	// other agent's tick lands while it is in flight and must not send a
	// second request for the same grant.
	first := make(chan claudeSharedRefreshOutcome, 1)
	go func() { first <- m.refreshSharedClaudeCredential() }()
	<-srv.started
	var wg sync.WaitGroup
	var skipped atomic.Int32
	for i := 1; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m.refreshSharedClaudeCredential() == claudeSharedRefreshSkipped {
				skipped.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := skipped.Load(); got != n-1 {
		t.Fatalf("%d of %d concurrent passes were skipped while one refresh was in flight, want all", got, n-1)
	}
	close(srv.release)
	if got := <-first; got != claudeSharedRefreshWritten {
		t.Fatalf("the in-flight pass = %v, want written", got)
	}
	// Later ticks find a credential that is not due.
	for i := 0; i < 3; i++ {
		if got := m.refreshSharedClaudeCredential(); got != claudeSharedRefreshSkipped {
			t.Errorf("pass after the refresh = %v, want skipped (not due)", got)
		}
	}

	if got := srv.calls.Load(); got != 1 {
		t.Fatalf("refresh requests = %d, want exactly one for %d agents", got, n)
	}
	if got := srv.rejected.Load(); got != 0 {
		t.Fatalf("the grant was presented after rotation %d times; the server would have revoked it", got)
	}
	shared := readTokensAt(t, target)
	if !strings.HasPrefix(shared.AccessToken, "acc-rotated-1-") || shared.RefreshToken != srv.live {
		t.Fatalf("shared file does not hold the refreshed credential")
	}
	for _, link := range links {
		assertLinkRestored(t, link, target)
		if tok := readTokensAt(t, link); tok.AccessToken != shared.AccessToken || tok.ExpiresAt != shared.ExpiresAt {
			t.Errorf("%s does not read the new token through its bridge", link)
		}
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != claude.CredentialsFileMode {
		t.Errorf("shared credential mode = %v err=%v, want %#o", info.Mode(), err, claude.CredentialsFileMode)
	}
	if got := staleFilesBeside(t, target); len(got) != 0 {
		t.Errorf("a routine refresh must not leave stale copies beside the shared login: %v", got)
	}
	// Audited once, success, no token material anywhere.
	if sink.count() != 1 {
		t.Errorf("audit entries = %d, want one", sink.count())
	}
	rec := sink.find(AuditClaudeCredentialRefreshed)
	if rec == nil || rec.Fields["outcome"] != "success" {
		t.Fatalf("audit entry = %+v", rec)
	}
	if strings.Contains(fmt.Sprint(rec.Fields), bridgeTestTokenSentinel) {
		t.Error("audit fields carry token material")
	}
	if strings.Contains(logBuf.String(), bridgeTestTokenSentinel) {
		t.Error("log carries token material")
	}
	// The cached token for agents that cannot read the file moved too.
	m.mu.RLock()
	cached := m.claudeAuthToken
	m.mu.RUnlock()
	if cached != shared.AccessToken {
		t.Error("manager's cached Claude access token not updated by the refresh")
	}
}

// TestSharedClaudeRefresh_TimerRefreshesBeforeExpiryWithoutRestart: the
// watchdog's own ticker drives the refresh ahead of expiry, and an agent's
// bridged file shows the new expiresAt with no agent restarted or relinked.
func TestSharedClaudeRefresh_TimerRefreshesBeforeExpiryWithoutRestart(t *testing.T) {
	t.Setenv(ClaudeSharedRefreshIntervalEnv, "10ms")
	t.Setenv(CredentialWatchdogIntervalEnv, "1h")
	t.Setenv(ClaudeBridgeIntervalEnv, "1h")
	before := time.Now().Add(90 * time.Minute).UnixMilli() // inside the default 2h lead, not expired
	m, _, links, target := sharedRefreshFixture(t, 2, before)
	srv := &fakeRefreshServer{live: readTokensAt(t, target).RefreshToken, lifetime: 8 * time.Hour}
	withFakeRefresh(t, srv)
	linkInfoBefore, err := os.Lstat(links[0])
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.StartCredentialWatchdog(ctx)
	}()
	stop := func() {
		cancel()
		<-done
	}
	defer stop()

	testutil.Eventually(t, 5*time.Second, func() bool {
		tok := claude.LoadTokens(links[0])
		return tok != nil && tok.ExpiresAt > before
	}, "the timer never refreshed the shared login ahead of expiry")
	stop()
	for _, link := range links {
		if tok := readTokensAt(t, link); tok.ExpiresAt <= before {
			t.Errorf("%s still shows the old expiresAt", link)
		}
		info, err := os.Lstat(link)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s is no longer the bridge symlink: %v %v", link, info, err)
		}
	}
	if info, err := os.Lstat(links[0]); err != nil || !os.SameFile(info, linkInfoBefore) {
		t.Error("the agent's credential link was replaced; the refresh must only rewrite the shared target")
	}
	if got := srv.rejected.Load(); got != 0 {
		t.Errorf("grant presented after rotation %d times", got)
	}
}

func TestSharedClaudeRefresh_NotDueOutsideLead(t *testing.T) {
	m, sink, _, target := sharedRefreshFixture(t, 1, time.Now().Add(6*time.Hour).UnixMilli())
	srv := &fakeRefreshServer{live: readTokensAt(t, target).RefreshToken, lifetime: 8 * time.Hour}
	withFakeRefresh(t, srv)
	if got := m.refreshSharedClaudeCredential(); got != claudeSharedRefreshSkipped {
		t.Errorf("outcome = %v, want skipped", got)
	}
	if srv.calls.Load() != 0 || sink.count() != 0 {
		t.Error("a credential with three quarters of its life left must not be refreshed")
	}
	// An env lead longer than the remaining life makes it due.
	t.Setenv(ClaudeSharedRefreshLeadEnv, "7h")
	if got := m.refreshSharedClaudeCredential(); got != claudeSharedRefreshWritten {
		t.Errorf("outcome with a 7h lead = %v, want written", got)
	}
}

// An already-expired access token with a live refresh grant is refreshed at
// once; that is exactly the moment every CLI would otherwise refresh.
func TestSharedClaudeRefresh_ExpiredAccessTokenIsRefreshedImmediately(t *testing.T) {
	m, _, _, target := sharedRefreshFixture(t, 1, time.Now().Add(-time.Hour).UnixMilli())
	srv := &fakeRefreshServer{live: readTokensAt(t, target).RefreshToken, lifetime: 8 * time.Hour}
	withFakeRefresh(t, srv)
	if got := m.refreshSharedClaudeCredential(); got != claudeSharedRefreshWritten {
		t.Fatalf("outcome = %v, want written", got)
	}
	if !claude.HasValidToken(target) {
		t.Error("shared file should now hold a live access token")
	}
}

// A rejected grant is reported once with the operator recovery and is not
// presented again; a sign-in (a changed file) rearms the refresher.
func TestSharedClaudeRefresh_RejectedGrantIsNotRetriedUntilTheFileChanges(t *testing.T) {
	m, sink, _, target := sharedRefreshFixture(t, 2, time.Now().Add(time.Minute).UnixMilli())
	srv := &fakeRefreshServer{live: "something-else", lifetime: 8 * time.Hour} // every presentation is rejected
	withFakeRefresh(t, srv)
	var logBuf bytes.Buffer
	m.logger = slog.New(slog.NewTextHandler(&logBuf, nil))

	for i := 0; i < 3; i++ {
		m.refreshSharedClaudeCredential()
	}
	if got := srv.calls.Load(); got != 1 {
		t.Fatalf("a rejected grant was presented %d times, want once", got)
	}
	rec := sink.find(AuditClaudeCredentialRefreshed)
	if rec == nil || rec.Fields["outcome"] != "failed" || rec.Fields["recovery"] != defaultCredentialRecovery {
		t.Fatalf("audit entry = %+v", rec)
	}
	if sink.count() != 1 {
		t.Errorf("audit entries = %d, want one", sink.count())
	}
	if !strings.Contains(logBuf.String(), "rejected the shared refresh grant") {
		t.Error("rejection not logged")
	}
	if strings.Contains(logBuf.String(), bridgeTestTokenSentinel) || strings.Contains(fmt.Sprint(rec.Fields), bridgeTestTokenSentinel) {
		t.Error("token material leaked")
	}
	// The shared file is untouched: the dead credential is still what the
	// operator's sign-in will replace.
	if tok := readTokensAt(t, target); !strings.HasPrefix(tok.AccessToken, "acc-shared-") {
		t.Error("shared file was rewritten after a rejected refresh")
	}

	// Operator signs in: a new credential version. The refresher tries again.
	if err := os.WriteFile(target, bridgeTestCreds(t, "signed-in", time.Now().Add(2*time.Minute).UnixMilli()), 0o660); err != nil {
		t.Fatal(err)
	}
	srv.live = readTokensAt(t, target).RefreshToken
	if got := m.refreshSharedClaudeCredential(); got != claudeSharedRefreshWritten {
		t.Errorf("outcome after sign-in = %v, want written", got)
	}
}

// A transient failure backs off instead of hammering the endpoint, then
// retries after the backoff.
func TestSharedClaudeRefresh_TransientFailureBacksOff(t *testing.T) {
	m, sink, _, target := sharedRefreshFixture(t, 1, time.Now().Add(time.Minute).UnixMilli())
	var calls atomic.Int32
	prev := claudeRefreshTokens
	claudeRefreshTokens = func(*claude.OAuthTokens) (*claude.OAuthTokens, error) {
		calls.Add(1)
		return nil, errors.New("token exchange request: dial tcp: i/o timeout")
	}
	t.Cleanup(func() { claudeRefreshTokens = prev })
	base := time.Now()
	clock := base
	prevNow := claudeSharedRefreshNow
	claudeSharedRefreshNow = func() time.Time { return clock }
	t.Cleanup(func() { claudeSharedRefreshNow = prevNow })

	for i := 0; i < 3; i++ {
		m.refreshSharedClaudeCredential()
	}
	if calls.Load() != 1 {
		t.Fatalf("transient failure retried %d times inside the backoff, want one attempt", calls.Load())
	}
	rec := sink.find(AuditClaudeCredentialRefreshed)
	if rec == nil || rec.Fields["outcome"] != "failed" || rec.Fields["reason"] != "transient" {
		t.Fatalf("audit entry = %+v", rec)
	}
	clock = base.Add(claudeSharedRefreshRetryBackoff + time.Second)
	m.refreshSharedClaudeCredential()
	if calls.Load() != 2 {
		t.Errorf("no retry after the backoff: calls = %d", calls.Load())
	}
	if tok := readTokensAt(t, target); !strings.HasPrefix(tok.AccessToken, "acc-shared-") {
		t.Error("shared file rewritten after failed refreshes")
	}
}

// A CLI that refreshed first and was promoted by the bridge while the hive's
// request was in flight expires later; the hive keeps it rather than writing
// its own, older result over it.
func TestSharedClaudeRefresh_FresherPromotedCredentialIsKept(t *testing.T) {
	m, _, _, target := sharedRefreshFixture(t, 1, time.Now().Add(time.Minute).UnixMilli())
	later := time.Now().Add(9 * time.Hour).UnixMilli()
	prev := claudeRefreshTokens
	claudeRefreshTokens = func(p *claude.OAuthTokens) (*claude.OAuthTokens, error) {
		// Meanwhile, the bridge promotes a CLI's own refresh.
		if err := os.WriteFile(target, bridgeTestCreds(t, "cli", later), 0o660); err != nil {
			t.Fatal(err)
		}
		return &claude.OAuthTokens{
			AccessToken: "acc-hive-" + bridgeTestTokenSentinel, RefreshToken: "ref-hive-" + bridgeTestTokenSentinel,
			ExpiresAt: time.Now().Add(8 * time.Hour).UnixMilli(),
		}, nil
	}
	t.Cleanup(func() { claudeRefreshTokens = prev })

	if got := m.refreshSharedClaudeCredential(); got != claudeSharedRefreshSuperseded {
		t.Fatalf("outcome = %v, want superseded", got)
	}
	if tok := readTokensAt(t, target); !strings.HasPrefix(tok.AccessToken, "acc-cli-") || tok.ExpiresAt != later {
		t.Error("the fresher promoted credential was overwritten")
	}
}

func TestSharedClaudeRefresh_SkipsWhenDisabledOrOperatorTokenMode(t *testing.T) {
	m, _, _, target := sharedRefreshFixture(t, 1, time.Now().Add(-time.Hour).UnixMilli())
	srv := &fakeRefreshServer{live: readTokensAt(t, target).RefreshToken, lifetime: 8 * time.Hour}
	withFakeRefresh(t, srv)

	t.Setenv(ClaudeSharedRefreshEnv, "off")
	if got := m.refreshSharedClaudeCredential(); got != claudeSharedRefreshSkipped {
		t.Errorf("disabled: outcome = %v, want skipped", got)
	}
	t.Setenv(ClaudeSharedRefreshEnv, "")
	m.claudeOperatorToken = "operator-token"
	if got := m.refreshSharedClaudeCredential(); got != claudeSharedRefreshSkipped {
		t.Errorf("operator-token mode: outcome = %v, want skipped", got)
	}
	m.claudeOperatorToken = ""
	if srv.calls.Load() != 0 {
		t.Error("a refresh request was sent while the refresher should have been idle")
	}
	if got := m.refreshSharedClaudeCredential(); got != claudeSharedRefreshWritten {
		t.Errorf("re-enabled: outcome = %v, want written", got)
	}
}

func TestSharedClaudeRefresh_NothingToRefresh(t *testing.T) {
	withSharedAgentHome(t)
	m, sink := testManagerWithSink(t)
	srv := &fakeRefreshServer{lifetime: 8 * time.Hour}
	withFakeRefresh(t, srv)
	// Absent file.
	if got := m.refreshSharedClaudeCredential(); got != claudeSharedRefreshSkipped {
		t.Errorf("absent file: outcome = %v", got)
	}
	// No refresh grant (a failed CLI refresh leaves empty tokens).
	target := sharedClaudeBridgeTarget()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`), 0o660); err != nil {
		t.Fatal(err)
	}
	if got := m.refreshSharedClaudeCredential(); got != claudeSharedRefreshSkipped {
		t.Errorf("no grant: outcome = %v", got)
	}
	// Refresh grant itself expired: an operator sign-in is the only way.
	if err := os.WriteFile(target, []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":%d,"refreshTokenExpiresAt":%d}}`,
		time.Now().Add(-time.Hour).UnixMilli(), time.Now().Add(-time.Minute).UnixMilli())), 0o660); err != nil {
		t.Fatal(err)
	}
	if got := m.refreshSharedClaudeCredential(); got != claudeSharedRefreshSkipped {
		t.Errorf("expired grant: outcome = %v", got)
	}
	if srv.calls.Load() != 0 || sink.count() != 0 {
		t.Error("nothing should have been requested or audited")
	}
}

func TestClaudeSharedRefreshLead(t *testing.T) {
	t.Setenv(ClaudeSharedRefreshLeadEnv, "")
	if got := claudeSharedRefreshLead(0); got != 2*time.Hour {
		t.Errorf("default lead = %v, want 2h (a quarter of the 8h lifetime)", got)
	}
	if got := claudeSharedRefreshLead(4 * time.Hour); got != time.Hour {
		t.Errorf("lead for a 4h lifetime = %v, want 1h", got)
	}
	t.Setenv(ClaudeSharedRefreshLeadEnv, "45m")
	if got := claudeSharedRefreshLead(0); got != 45*time.Minute {
		t.Errorf("env lead = %v, want 45m", got)
	}
	t.Setenv(ClaudeSharedRefreshLeadEnv, "nonsense")
	if got := claudeSharedRefreshLead(0); got != 2*time.Hour {
		t.Errorf("invalid env lead = %v, want the default", got)
	}
}

func TestClaudeSharedRefreshEnabled(t *testing.T) {
	for v, want := range map[string]bool{"": true, "1": true, "true": true, "0": false, "false": false, "off": false, "OFF": false, "maybe": true} {
		t.Setenv(ClaudeSharedRefreshEnv, v)
		if got := claudeSharedRefreshEnabled(); got != want {
			t.Errorf("%q: enabled = %v, want %v", v, got, want)
		}
	}
}
