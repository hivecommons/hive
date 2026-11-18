package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/claude"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/internal/testutil"
	"github.com/hivecommons/hive/crux/sharedfile"
)

// Every credential in this file is synthetic (bridgeTestCreds): each token
// string carries bridgeTestTokenSentinel, so a test can prove that no log line
// and no audit field ever holds token material.

// lockedBuffer is a log sink safe for the watchdog goroutine and the test.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type sessionLoginFixture struct {
	r      *UserSessionRuntime
	p      *fakePriv
	m      *Manager
	sink   *fakeAuditSink
	logs   *lockedBuffer
	target string
}

// newSessionLoginFixture builds a session runtime and a manager over one
// temporary shared home and sandbox root, both logging (at debug) into the
// same buffer and auditing into the same sink.
func newSessionLoginFixture(t *testing.T) *sessionLoginFixture {
	t.Helper()
	r, p := newTestSessionRuntimeWithPriv(t, sessionLifecycleTmux(""), nil)
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r.logger = logger
	m := NewManager(map[string]config.AgentConfig{}, logger, ProjectContext{})
	sink := &fakeAuditSink{}
	m.SetAuditSink(sink)
	r.SetAuditSink(sink)
	useSessionLoginPriv(t, p.run)
	target := sharedClaudeBridgeTarget()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	return &sessionLoginFixture{r: r, p: p, m: m, sink: sink, logs: logs, target: target}
}

// useSessionLoginPriv points the manager-side seams at a fake for one test.
func useSessionLoginPriv(t *testing.T, priv privRunFunc) {
	t.Helper()
	oldPriv, oldStat := sessionLoginPriv, sessionLoginStatUID
	sessionLoginPriv = priv
	sessionLoginStatUID = func(string) (int, error) { return testSessionUID, nil }
	t.Cleanup(func() { sessionLoginPriv, sessionLoginStatUID = oldPriv, oldStat })
}

// writeLoginAt writes a credential file with an explicit modification time,
// so "written after" never depends on the filesystem's timestamp resolution.
func writeLoginAt(t *testing.T, path string, data []byte, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := path + ".tmp.test"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func sessionLoginPath(id string) string {
	return filepath.Join(userSessionHome(id), userSessionClaudeLoginRel)
}

func (f *sessionLoginFixture) start(t *testing.T) {
	t.Helper()
	if err := f.r.Start(crux.UserSessionSpec{ID: testSessionID, Vendor: "claude", UIDSlot: testSessionSlot}); err != nil {
		t.Fatal(err)
	}
}

// assertNoTokenMaterial fails when a log line or an audit field holds any
// part of a synthetic token.
func (f *sessionLoginFixture) assertNoTokenMaterial(t *testing.T) {
	t.Helper()
	if strings.Contains(f.logs.String(), bridgeTestTokenSentinel) {
		t.Error("token material reached the log")
	}
	f.sink.mu.Lock()
	defer f.sink.mu.Unlock()
	for _, e := range f.sink.entries {
		if strings.Contains(fmt.Sprintf("%v %v %v", e.Action, e.Agent, e.Fields), bridgeTestTokenSentinel) {
			t.Errorf("token material reached the audit entry %s", e.Action)
		}
	}
}

func (f *sessionLoginFixture) adoptions() int {
	f.sink.mu.Lock()
	defer f.sink.mu.Unlock()
	n := 0
	for _, e := range f.sink.entries {
		if e.Action == AuditUserSessionLoginAdopted {
			n++
		}
	}
	return n
}

// assertRefreshless fails unless the session's copy is a credential with the
// given access token and no refresh token.
func assertRefreshless(t *testing.T, path, wantAccess string) {
	t.Helper()
	raw := mustRead(t, path)
	tok := claude.ParseTokens(raw)
	if tok == nil {
		t.Fatalf("session copy at %s is not a credential", path)
	}
	if tok.RefreshToken != "" || tok.RefreshTokenExpiresAt != 0 || bytes.Contains(raw, []byte("refreshToken")) {
		t.Error("the session's copy carries a refresh token: the session could rotate the fleet's login")
	}
	if tok.AccessToken != wantAccess {
		t.Error("the session's copy does not hold the shared login's access token")
	}
}

func TestSessionClaudeLogin_DropsRefreshTokenAndEverythingElse(t *testing.T) {
	exp := time.Now().Add(time.Hour).UnixMilli()
	full := []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"acc-%[1]s","refreshToken":"ref-%[1]s","expiresAt":%[2]d,"refreshTokenExpiresAt":%[2]d,"scopes":["user:inference"],"subscriptionType":"max","futureField":7},"mcpOAuth":{"srv":{"accessToken":"mcp-%[1]s"}}}`, bridgeTestTokenSentinel, exp))
	out, ok := sessionClaudeLogin(full)
	if !ok {
		t.Fatal("a complete credential was refused")
	}
	for _, gone := range []string{"refreshToken", "refreshTokenExpiresAt", "ref-", "mcpOAuth", "mcp-"} {
		if bytes.Contains(out, []byte(gone)) {
			t.Errorf("the session copy still holds %q", gone)
		}
	}
	tok := claude.ParseTokens(out)
	if tok == nil || tok.AccessToken != "acc-"+bridgeTestTokenSentinel || tok.ExpiresAt != exp || tok.SubscriptionType != "max" || len(tok.Scopes) != 1 {
		t.Errorf("the session copy lost what the CLI needs to stay signed in")
	}
	if !bytes.Contains(out, []byte("futureField")) {
		t.Error("an unknown token-block field was dropped")
	}
	// A refresh-less credential can never be promoted over the shared login.
	if claudeBridgePromotable(tok, time.Now()) {
		t.Error("a refresh-less session copy counts as promotable")
	}
	for name, bad := range map[string]string{
		"not json":        "login-file",
		"no token block":  `{"other":{}}`,
		"null block":      `{"claudeAiOauth":null}`,
		"no access token": `{"claudeAiOauth":{"refreshToken":"r","expiresAt":1}}`,
		"empty access":    `{"claudeAiOauth":{"accessToken":"","refreshToken":"r"}}`,
		"oversized":       `{"claudeAiOauth":{"accessToken":"` + strings.Repeat("a", claudeBridgeMaxCredentialBytes) + `"}}`,
	} {
		if _, ok := sessionClaudeLogin([]byte(bad)); ok {
			t.Errorf("%s: handed out as a session login", name)
		}
	}
}

// A session is never a refresh holder: what it is given has no refresh token,
// and handing it out leaves the shared login byte-for-byte as it was. With
// one class of holder (the agents' bridge and the dashboard sign-in) two
// holders cannot refresh the same grant into divergent tokens.
func TestUserSessionStart_ClaudeLoginCopyCannotRefresh(t *testing.T) {
	f := newSessionLoginFixture(t)
	shared := bridgeTestCreds(t, "shared", time.Now().Add(time.Hour).UnixMilli())
	writeLoginAt(t, f.target, shared, time.Now().Add(-time.Hour))
	f.start(t)

	path := sessionLoginPath(testSessionID)
	assertRefreshless(t, path, "acc-shared-"+bridgeTestTokenSentinel)
	if st, err := os.Lstat(path); err != nil || st.Mode().Perm() != 0o400 || !st.Mode().IsRegular() {
		t.Errorf("session copy: %v, %v; want a regular 0400 file", st, err)
	}
	if !bytes.Equal(mustRead(t, f.target), shared) {
		t.Error("starting a session changed the shared login")
	}
	if f.adoptions() != 0 {
		t.Error("a plain start audited an adoption")
	}
	f.assertNoTokenMaterial(t)
}

// The incident: the CLI in a session refreshed its own copy of the login,
// which rotated the grant and left the shared file every agent reads holding a
// dead refresh token. The newer login must reach the shared file, the session
// must go back to a refresh-less copy, and the change must be audited.
func TestSessionRefresh_NeverLeavesTheSharedLoginStale(t *testing.T) {
	f := newSessionLoginFixture(t)
	now := time.Now()
	writeLoginAt(t, f.target, bridgeTestCreds(t, "shared", now.Add(10*time.Minute).UnixMilli()), now.Add(-time.Hour))
	f.start(t)
	path := sessionLoginPath(testSessionID)
	// What a CLI holding a full copy does on refresh: a new file, a later
	// expiry, a rotated refresh token.
	writeLoginAt(t, path, bridgeTestCreds(t, "rotated", now.Add(8*time.Hour).UnixMilli()), now)

	adopted := f.m.syncUserSessionClaudeLogins(sessionLoginTriggerWatchdog)
	if len(adopted) != 1 || adopted[0] != testSessionID {
		t.Fatalf("adopted = %v, want the session", adopted)
	}
	got := readTokensAt(t, f.target)
	if got.AccessToken != "acc-rotated-"+bridgeTestTokenSentinel || got.RefreshToken != "ref-rotated-"+bridgeTestTokenSentinel {
		t.Error("the shared login does not hold the session's refreshed credential")
	}
	if n := len(staleFilesBeside(t, f.target)); n != 1 {
		t.Errorf("replaced shared login preserved %d times, want 1", n)
	}
	assertRefreshless(t, path, "acc-rotated-"+bridgeTestTokenSentinel)

	rec := f.sink.find(AuditUserSessionLoginAdopted)
	if rec == nil || rec.Fields["session"] != testSessionID || rec.Fields["trigger"] != sessionLoginTriggerWatchdog || rec.Fields["outcome"] != "success" {
		t.Errorf("audit = %+v", rec)
	}
	// Converged: further polls change nothing and audit nothing.
	before := mustRead(t, f.target)
	for i := 0; i < 3; i++ {
		if again := f.m.syncUserSessionClaudeLogins(sessionLoginTriggerSignIn); len(again) != 0 {
			t.Errorf("poll %d adopted again: %v", i, again)
		}
	}
	if !bytes.Equal(mustRead(t, f.target), before) || f.adoptions() != 1 {
		t.Error("the login did not settle after one adoption")
	}
	f.assertNoTokenMaterial(t)
}

// Deleting a session must never delete the only valid login: the newest login
// is adopted before the sandbox goes, and when it cannot be adopted the
// sandbox stays.
func TestSessionRemove_NeverDestroysTheOnlyValidLogin(t *testing.T) {
	t.Run("adopted before removal", func(t *testing.T) {
		f := newSessionLoginFixture(t)
		now := time.Now()
		writeLoginAt(t, f.target, bridgeTestCreds(t, "shared", now.Add(-time.Minute).UnixMilli()), now.Add(-time.Hour))
		f.start(t)
		writeLoginAt(t, sessionLoginPath(testSessionID), bridgeTestCreds(t, "rotated", now.Add(8*time.Hour).UnixMilli()), now)

		if err := f.r.Remove(testSessionID); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(userSessionSandboxDir(testSessionID)); err == nil {
			t.Error("the sandbox survived Remove")
		}
		if got := readTokensAt(t, f.target); got.RefreshToken != "ref-rotated-"+bridgeTestTokenSentinel {
			t.Error("the session's login was deleted with the session instead of being adopted")
		}
		rec := f.sink.find(AuditUserSessionLoginAdopted)
		if rec == nil || rec.Fields["trigger"] != sessionLoginTriggerRemove {
			t.Errorf("audit = %+v", rec)
		}
		f.assertNoTokenMaterial(t)
	})
	t.Run("kept when it cannot be adopted", func(t *testing.T) {
		f := newSessionLoginFixture(t)
		now := time.Now()
		f.start(t)
		path := sessionLoginPath(testSessionID)
		rotated := bridgeTestCreds(t, "rotated", now.Add(8*time.Hour).UnixMilli())
		writeLoginAt(t, path, rotated, now)
		// The shared location is not a regular file: sharedfile refuses it.
		if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), f.target); err != nil {
			t.Fatal(err)
		}
		err := f.r.Remove(testSessionID)
		if err == nil || !errors.Is(err, sharedfile.ErrUnsafeLocation) {
			t.Fatalf("Remove = %v, want a refusal that keeps the session", err)
		}
		if !bytes.Equal(mustRead(t, path), rotated) {
			t.Error("the session's login is gone although it was never adopted")
		}
		if strings.Contains(err.Error(), bridgeTestTokenSentinel) {
			t.Error("token material in the error")
		}
		f.assertNoTokenMaterial(t)
	})
	t.Run("a refresh-less copy is just removed", func(t *testing.T) {
		f := newSessionLoginFixture(t)
		shared := bridgeTestCreds(t, "shared", time.Now().Add(time.Hour).UnixMilli())
		writeLoginAt(t, f.target, shared, time.Now().Add(-time.Hour))
		f.start(t)
		if err := f.r.Remove(testSessionID); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(mustRead(t, f.target), shared) || f.adoptions() != 0 {
			t.Error("removing an ordinary session touched the shared login")
		}
	})
}

func TestSessionClaudeLoginNewer(t *testing.T) {
	now := time.Now()
	tok := func(access, refresh string, exp time.Time) *claude.OAuthTokens {
		return &claude.OAuthTokens{AccessToken: access, RefreshToken: refresh, ExpiresAt: exp.UnixMilli()}
	}
	sharedLive := tok("a", "r", now.Add(time.Hour))
	old, recent := now.Add(-time.Hour), now
	cases := []struct {
		name               string
		sess, shared       *claude.OAuthTokens
		sessMod, sharedMod time.Time
		want               bool
	}{
		{"no session credential", nil, sharedLive, recent, old, false},
		{"failed refresh (empty tokens)", &claude.OAuthTokens{}, sharedLive, recent, old, false},
		{"refresh-less copy, later expiry", tok("b", "", now.Add(8*time.Hour)), sharedLive, recent, old, false},
		{"no recorded expiry", &claude.OAuthTokens{AccessToken: "b", RefreshToken: "r2"}, sharedLive, recent, old, false},
		{"same expiry", tok("b", "r2", now.Add(time.Hour)), sharedLive, recent, old, false},
		{"earlier expiry", tok("b", "r2", now.Add(time.Minute)), sharedLive, recent, old, false},
		{"later expiry but written before the shared file", tok("b", "r2", now.Add(8*time.Hour)), sharedLive, old, recent, false},
		{"later expiry and written after", tok("b", "r2", now.Add(8*time.Hour)), sharedLive, recent, old, true},
		{"shared login absent", tok("b", "r2", now.Add(time.Hour)), nil, old, recent, true},
		{"shared login signed out", tok("b", "r2", now.Add(time.Hour)), &claude.OAuthTokens{}, old, recent, true},
	}
	for _, c := range cases {
		if got := sessionClaudeLoginNewer(c.sess, c.sessMod, c.shared, c.sharedMod, now); got != c.want {
			t.Errorf("%s: newer = %v, want %v", c.name, got, c.want)
		}
	}
}

// The session's copy follows the shared login (an agent refreshed, an
// operator signed in); a full copy handed out by an earlier release is
// reduced to a refresh-less one; a login the session obtained itself and that
// is not newer is left exactly as it is.
func TestSessionLogin_FollowsTheSharedLogin(t *testing.T) {
	f := newSessionLoginFixture(t)
	now := time.Now()
	writeLoginAt(t, f.target, bridgeTestCreds(t, "first", now.Add(10*time.Minute).UnixMilli()), now.Add(-2*time.Hour))
	f.start(t)
	path := sessionLoginPath(testSessionID)
	assertRefreshless(t, path, "acc-first-"+bridgeTestTokenSentinel)

	// An agent's refresh is published to the shared file.
	writeLoginAt(t, f.target, bridgeTestCreds(t, "second", now.Add(8*time.Hour).UnixMilli()), now.Add(time.Minute))
	if adopted := f.m.syncUserSessionClaudeLogins(sessionLoginTriggerWatchdog); len(adopted) != 0 {
		t.Fatalf("a refresh-less copy was adopted: %v", adopted)
	}
	assertRefreshless(t, path, "acc-second-"+bridgeTestTokenSentinel)

	// A full copy of the shared login (what releases before this one handed
	// out) loses its refresh token and nothing is adopted.
	writeLoginAt(t, path, mustRead(t, f.target), now.Add(2*time.Minute))
	shared := mustRead(t, f.target)
	f.m.syncUserSessionClaudeLogins(sessionLoginTriggerWatchdog)
	assertRefreshless(t, path, "acc-second-"+bridgeTestTokenSentinel)
	if !bytes.Equal(mustRead(t, f.target), shared) {
		t.Error("reducing a legacy copy changed the shared login")
	}

	// A different login that is not newer is not hive's to replace.
	own := bridgeTestCreds(t, "own", now.Add(time.Hour).UnixMilli())
	writeLoginAt(t, path, own, now.Add(3*time.Minute))
	f.m.syncUserSessionClaudeLogins(sessionLoginTriggerSignIn)
	if !bytes.Equal(mustRead(t, path), own) || !bytes.Equal(mustRead(t, f.target), shared) {
		t.Error("an older login the session obtained itself was replaced or adopted")
	}
	if f.adoptions() != 0 {
		t.Error("nothing was newer than the shared login, yet an adoption was audited")
	}
	f.assertNoTokenMaterial(t)
}

// A failed refresh leaves an empty credential behind; it is never adopted and
// the session is put back on the shared login.
func TestSessionLogin_FailedRefreshIsNeverAdopted(t *testing.T) {
	f := newSessionLoginFixture(t)
	now := time.Now()
	shared := bridgeTestCreds(t, "shared", now.Add(time.Hour).UnixMilli())
	writeLoginAt(t, f.target, shared, now.Add(-time.Hour))
	f.start(t)
	path := sessionLoginPath(testSessionID)
	writeLoginAt(t, path, []byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`), now.Add(-time.Minute))

	if adopted := f.m.syncUserSessionClaudeLogins(sessionLoginTriggerWatchdog); len(adopted) != 0 {
		t.Fatalf("a signed-out credential was adopted: %v", adopted)
	}
	if !bytes.Equal(mustRead(t, f.target), shared) {
		t.Error("a failed refresh in a session changed the shared login")
	}
	// The shared file is older than the session's empty file but holds a
	// later expiry, which is enough to put the session back on it.
	assertRefreshless(t, path, "acc-shared-"+bridgeTestTokenSentinel)
}

// Recovery of a hive already locked out: at boot, before any tick, the
// credential watchdog adopts the newer login a session HOME still holds.
func TestCredentialWatchdog_BootAdoptsANewerSessionLogin(t *testing.T) {
	t.Setenv(ClaudeBridgeIntervalEnv, "1h")
	t.Setenv(CredentialWatchdogIntervalEnv, "1h")
	f := newSessionLoginFixture(t)
	now := time.Now()
	f.start(t)
	// The stuck state: the shared access token is spent and its refresh
	// token was rotated away; the session holds the live login.
	writeLoginAt(t, f.target, bridgeTestCreds(t, "stale", now.Add(-3*time.Hour).UnixMilli()), now.Add(-12*time.Hour))
	writeLoginAt(t, sessionLoginPath(testSessionID), bridgeTestCreds(t, "live", now.Add(5*time.Hour).UnixMilli()), now.Add(-3*time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { f.m.StartCredentialWatchdog(ctx); close(done) }()
	testutil.Eventually(t, 5*time.Second, func() bool {
		tok := claude.LoadTokens(f.target)
		return tok != nil && tok.RefreshToken == "ref-live-"+bridgeTestTokenSentinel
	}, "the watchdog did not adopt the session's login at boot")
	cancel()
	<-done
	rec := f.sink.find(AuditUserSessionLoginAdopted)
	if rec == nil || rec.Fields["trigger"] != sessionLoginTriggerBoot {
		t.Errorf("audit = %+v", rec)
	}
	assertRefreshless(t, sessionLoginPath(testSessionID), "acc-live-"+bridgeTestTokenSentinel)
	f.assertNoTokenMaterial(t)
}

// A session-owned 0400/0600 login the hive cannot open is read as the
// session's own uid, bounded; when that fails too, nothing is changed and the
// error carries no file content.
func TestSessionLogin_ReadsAsTheSessionUID(t *testing.T) {
	f := newSessionLoginFixture(t)
	now := time.Now()
	writeLoginAt(t, f.target, bridgeTestCreds(t, "shared", now.Add(10*time.Minute).UnixMilli()), now.Add(-time.Hour))
	f.start(t)
	path := sessionLoginPath(testSessionID)
	writeLoginAt(t, path, bridgeTestCreds(t, "rotated", now.Add(8*time.Hour).UnixMilli()), now)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	asOwner := 0
	failRead := true
	f.p.handler = func(c fakePrivCall) (bool, []byte, error) {
		if len(c.argv) == 0 || c.argv[0] != "head" {
			return false, nil, nil
		}
		asOwner++
		if c.user != testSessionSpecUser {
			t.Errorf("the owner read ran as %q", c.user)
		}
		if failRead {
			// A helper that dies mid-read has credential bytes on its output.
			return true, []byte("partial-" + bridgeTestTokenSentinel), errors.New("exit status 1")
		}
		_ = os.Chmod(path, 0o600)
		data, err := os.ReadFile(path)
		_ = os.Chmod(path, 0)
		return true, data, err
	}
	shared := mustRead(t, f.target)
	f.m.syncUserSessionClaudeLogins(sessionLoginTriggerSignIn)
	if os.Getuid() != 0 {
		if asOwner == 0 {
			t.Fatal("an unreadable session login was not read as its owner")
		}
		if !bytes.Equal(mustRead(t, f.target), shared) {
			t.Error("the shared login changed although the session's login could not be read")
		}
		if !strings.Contains(f.logs.String(), "check failed") {
			t.Error("a failed read was not reported")
		}
	}
	f.assertNoTokenMaterial(t)

	failRead = false
	f.m.syncUserSessionClaudeLogins(sessionLoginTriggerSignIn)
	if got := readTokensAt(t, f.target); got.RefreshToken != "ref-rotated-"+bridgeTestTokenSentinel {
		t.Error("the session's login was not adopted once it could be read")
	}
	f.assertNoTokenMaterial(t)
}

// Many newer logins judged at once, from the watchdog sweep and from
// per-session checks together: every read-compare-write of the shared login
// is serialised, so the outcome is one login (the latest), never a mix, and no
// session is left holding a login newer than the shared one.
func TestSessionLogin_ConcurrentChecksConvergeOnOneLogin(t *testing.T) {
	const sessions = 8
	shared := withSharedAgentHome(t)
	t.Setenv(userSessionSandboxDirEnv, filepath.Join(t.TempDir(), "sandbox"))
	t.Setenv(userSessionUIDBaseEnv, "")
	t.Setenv(userSessionUIDSlotsEnv, "")
	t.Setenv(userSessionGIDEnv, "")
	var privMu sync.Mutex
	priv := func(_ context.Context, _ string, stdin io.Reader, argv ...string) ([]byte, error) {
		privMu.Lock()
		defer privMu.Unlock()
		if len(argv) < 5 || argv[2] != userSessionCopyLoginScript {
			return nil, errors.New("unexpected helper")
		}
		data, err := io.ReadAll(stdin)
		if err != nil {
			return nil, err
		}
		tmp := argv[4] + ".hive-new"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return nil, err
		}
		return nil, os.Rename(tmp, argv[4])
	}
	useSessionLoginPriv(t, priv)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewManager(map[string]config.AgentConfig{}, logger, ProjectContext{})
	sink := &fakeAuditSink{}
	m.SetAuditSink(sink)

	now := time.Now()
	target := filepath.Join(shared, userSessionClaudeLoginRel)
	writeLoginAt(t, target, bridgeTestCreds(t, "shared", now.Add(time.Minute).UnixMilli()), now.Add(-time.Hour))
	ids := make([]string, sessions)
	for i := range ids {
		ids[i] = fmt.Sprintf("%012x", i+1)
		// Later sessions hold later logins, written later.
		writeLoginAt(t, sessionLoginPath(ids[i]), bridgeTestCreds(t, fmt.Sprintf("s%d", i), now.Add(time.Duration(i+1)*time.Hour).UnixMilli()),
			now.Add(time.Duration(i+1)*time.Hour))
	}
	ops := sessionLoginOps{priv: priv, statUID: func(string) (int, error) { return testSessionUID, nil }, now: time.Now}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(2)
		go func(id string) {
			defer wg.Done()
			_, _ = ops.checkSessionClaudeLogin(id, sessionLoginModeWatch)
		}(id)
		go func() {
			defer wg.Done()
			m.syncUserSessionClaudeLogins(sessionLoginTriggerSignIn)
		}()
	}
	wg.Wait()
	m.syncUserSessionClaudeLogins(sessionLoginTriggerSignIn)

	last := fmt.Sprintf("s%d", sessions-1)
	got := readTokensAt(t, target)
	if got.AccessToken != "acc-"+last+"-"+bridgeTestTokenSentinel || got.RefreshToken != "ref-"+last+"-"+bridgeTestTokenSentinel {
		t.Error("the shared login is not the latest credential, whole")
	}
	assertRefreshless(t, sessionLoginPath(ids[sessions-1]), got.AccessToken)
	for _, id := range ids {
		tok := claude.LoadTokens(sessionLoginPath(id))
		if tok == nil {
			t.Errorf("session %s lost its login file", id)
			continue
		}
		if tok.ExpiresAt > got.ExpiresAt {
			t.Errorf("session %s still holds a login newer than the shared one", id)
		}
	}
}

// Logins that do not rotate (Copilot, Gemini) are still plain copies, but a
// resume no longer replaces a file the session's CLI rewrote after the shared
// one was written with the older shared file.
func TestUserSessionLoginCopy_KeepsASessionFileNewerThanTheSharedOne(t *testing.T) {
	f := newSessionLoginFixture(t)
	const rel = ".copilot/config.json"
	src := filepath.Join(sharedAgentHome, rel)
	now := time.Now()
	writeLoginAt(t, src, []byte("fleet-login-1"), now.Add(-time.Hour))
	f.start(t)
	dest := filepath.Join(userSessionHome(testSessionID), rel)
	if got := string(mustRead(t, dest)); got != "fleet-login-1" {
		t.Fatalf("first copy = %q", got)
	}
	// The CLI in the session rewrites its login.
	writeLoginAt(t, dest, []byte("session-refreshed"), now)
	f.r.copyLoginFiles(testSessionID, testSessionUID)
	if got := string(mustRead(t, dest)); got != "session-refreshed" {
		t.Errorf("a resume replaced the session's newer login with the older shared one: %q", got)
	}
	// A new fleet sign-in is newer than the session's file and is copied.
	writeLoginAt(t, src, []byte("fleet-login-2"), now.Add(time.Hour))
	f.r.copyLoginFiles(testSessionID, testSessionUID)
	if got := string(mustRead(t, dest)); got != "fleet-login-2" {
		t.Errorf("a newer fleet login did not reach the session: %q", got)
	}
}

func TestSessionLoginCache(t *testing.T) {
	var c sessionLoginCache
	now := time.Now()
	st := sessionLoginStamp{sessMod: 1, sharedMod: 2, sessSize: 3, sharedSize: 4}
	if !c.due("a", st, now, false) {
		t.Error("an unseen session is not due")
	}
	c.record("a", st, now)
	if c.due("a", st, now.Add(time.Minute), false) {
		t.Error("an unchanged pair is judged again at once")
	}
	if !c.due("a", st, now.Add(time.Minute), true) {
		t.Error("force is ignored")
	}
	changed := st
	changed.sharedMod++
	if !c.due("a", changed, now.Add(time.Minute), false) {
		t.Error("a changed shared login is not due")
	}
	if !c.due("a", st, now.Add(sessionLoginRecheckInterval), false) {
		t.Error("the recheck interval does not expire the entry")
	}
	c.forget(map[string]bool{})
	if !c.due("a", st, now, false) {
		t.Error("a removed session is remembered")
	}
}
