package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/claude"
)

// All credentials here are synthetic and the token endpoint is
// fakeRefreshServer; nothing real is read, written or contacted.

// grantClock replaces the refresher's clock with one the test advances.
func grantClock(t *testing.T) *time.Time {
	t.Helper()
	clock := time.Now()
	prev := claudeSharedRefreshNow
	claudeSharedRefreshNow = func() time.Time { return clock }
	t.Cleanup(func() { claudeSharedRefreshNow = prev })
	return &clock
}

// grantWriteSeams swaps the shared-file and pending-file writers for ones
// that fail while *failMain / *failPending are true.
func grantWriteSeams(t *testing.T, failMain, failPending *bool) {
	t.Helper()
	realMain, realPending := claudeSharedWrite, claudeSharedWritePending
	claudeSharedWrite = func(c *claude.OAuthTokens, target string, now time.Time, preserve bool) (string, error) {
		if *failMain {
			return "", errors.New("write credentials: no space left on device")
		}
		return realMain(c, target, now, preserve)
	}
	claudeSharedWritePending = func(c *claude.OAuthTokens, target string) error {
		if *failPending {
			return errors.New("write pending: no space left on device")
		}
		return realPending(c, target)
	}
	t.Cleanup(func() { claudeSharedWrite, claudeSharedWritePending = realMain, realPending })
}

func grantLogs(m *Manager) *bytes.Buffer {
	var buf bytes.Buffer
	m.logger = slog.New(slog.NewTextHandler(&buf, nil))
	return &buf
}

func assertNoSentinel(t *testing.T, what, s string) {
	t.Helper()
	if strings.Contains(s, bridgeTestTokenSentinel) {
		t.Errorf("token material reached %s", what)
	}
}

func assertNoTokenInAudit(t *testing.T, sink *fakeAuditSink) {
	t.Helper()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, e := range sink.entries {
		assertNoSentinel(t, "the audit log", fmt.Sprint(e.Fields))
	}
}

// The new grant is written, the endpoint is called exactly once, and a write
// that fails once does not cost the login.
func TestSharedClaudeGrant_WriteFailsOnceThenSucceeds(t *testing.T) {
	m, sink, _, target := sharedRefreshFixture(t, 1, time.Now().Add(time.Minute).UnixMilli())
	srv := &fakeRefreshServer{live: readTokensAt(t, target).RefreshToken, lifetime: 8 * time.Hour}
	withFakeRefresh(t, srv)
	logs := grantLogs(m)
	clock := grantClock(t)
	failMain, failPending := true, false
	grantWriteSeams(t, &failMain, &failPending)

	if got := m.refreshSharedClaudeCredential(); got != claudeSharedRefreshFailed {
		t.Fatalf("first pass = %v, want failed", got)
	}
	if tok := readTokensAt(t, target); !strings.HasPrefix(tok.AccessToken, "acc-shared-") {
		t.Fatal("shared file changed although the write failed")
	}
	pend := readSharedClaudePending(target)
	if pend == nil || !strings.HasPrefix(pend.AccessToken, "acc-rotated-") {
		t.Fatal("the new grant was not saved to the pending file before the write")
	}
	if info, err := os.Stat(sharedClaudePendingPath(target)); err != nil || info.Mode().Perm() != claudeSharedPendingMode {
		t.Errorf("pending file mode = %v err=%v, want 0600", info.Mode(), err)
	}
	if m.ClaudeSharedLoginUnsavedAlert() == "" {
		t.Error("no alert while the new grant is unsaved")
	}

	// Inside the backoff nothing is attempted, and never the endpoint.
	if got := m.refreshSharedClaudeCredential(); got != claudeSharedRefreshSkipped {
		t.Errorf("pass inside the backoff = %v, want skipped", got)
	}
	failMain = false
	*clock = clock.Add(claudeSharedWriteBackoffBase + time.Second)
	if got := m.refreshSharedClaudeCredential(); got != claudeSharedRefreshWritten {
		t.Fatalf("retry = %v, want written", got)
	}
	if srv.calls.Load() != 1 {
		t.Errorf("token endpoint called %d times, want exactly once", srv.calls.Load())
	}
	if tok := readTokensAt(t, target); !strings.HasPrefix(tok.AccessToken, "acc-rotated-") {
		t.Error("the new grant never reached the shared file")
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != claudeSharedCredentialMode {
		t.Errorf("shared file mode = %v err=%v", info.Mode(), err)
	}
	if readSharedClaudePending(target) != nil {
		t.Error("pending file left behind after the write succeeded")
	}
	if m.ClaudeSharedLoginUnsavedAlert() != "" {
		t.Error("alert still raised after the write succeeded")
	}
	assertNoSentinel(t, "the log", logs.String())
	assertNoTokenInAudit(t, sink)
}

// The process "restarts" after the refresh and before the write: the pending
// grant is adopted by the next process without another refresh.
func TestSharedClaudeGrant_PendingAdoptedAfterRestart(t *testing.T) {
	m1, _, _, target := sharedRefreshFixture(t, 1, time.Now().Add(time.Minute).UnixMilli())
	srv := &fakeRefreshServer{live: readTokensAt(t, target).RefreshToken, lifetime: 8 * time.Hour}
	withFakeRefresh(t, srv)
	failMain, failPending := true, false
	grantWriteSeams(t, &failMain, &failPending)
	grantClock(t)
	m1.refreshSharedClaudeCredential() // refresh succeeds, write fails, "crash"

	failMain = false
	m2, sink := testManagerWithSink(t)
	logs := grantLogs(m2)
	if got := m2.refreshSharedClaudeCredential(); got != claudeSharedRefreshWritten {
		t.Fatalf("pass after restart = %v, want written", got)
	}
	if srv.calls.Load() != 1 {
		t.Errorf("token endpoint called %d times across the restart, want once", srv.calls.Load())
	}
	if tok := readTokensAt(t, target); !strings.HasPrefix(tok.AccessToken, "acc-rotated-") {
		t.Error("the pending grant was not adopted")
	}
	if readSharedClaudePending(target) != nil {
		t.Error("pending file left behind after adoption")
	}
	assertNoSentinel(t, "the log", logs.String())
	assertNoTokenInAudit(t, sink)
}

// A pending grant that is not newer than the shared login is dropped, never
// written over it.
func TestSharedClaudeGrant_StalePendingIsDroppedNotWritten(t *testing.T) {
	m, _, _, target := sharedRefreshFixture(t, 1, time.Now().Add(8*time.Hour).UnixMilli())
	older := &claude.OAuthTokens{AccessToken: "acc-old-" + bridgeTestTokenSentinel, RefreshToken: "ref-old-" + bridgeTestTokenSentinel,
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	if err := writeSharedClaudePending(older, target); err != nil {
		t.Fatal(err)
	}
	srv := &fakeRefreshServer{live: "unused", lifetime: 8 * time.Hour}
	withFakeRefresh(t, srv)
	m.settlePendingSharedClaudeGrant(time.Now())
	if tok := readTokensAt(t, target); !strings.HasPrefix(tok.AccessToken, "acc-shared-") {
		t.Error("an older pending grant overwrote the shared login")
	}
	if readSharedClaudePending(target) != nil {
		t.Error("stale pending file not removed")
	}
	if srv.calls.Load() != 0 {
		t.Error("token endpoint called")
	}
}

// The write keeps failing: the endpoint is never called again, the alert is
// raised, and nothing secret is logged, audited or put in the banner.
func TestSharedClaudeGrant_WriteKeepsFailing_NoSecondRefreshAndAlert(t *testing.T) {
	m, sink, _, target := sharedRefreshFixture(t, 1, time.Now().Add(time.Minute).UnixMilli())
	srv := &fakeRefreshServer{live: readTokensAt(t, target).RefreshToken, lifetime: 8 * time.Hour}
	withFakeRefresh(t, srv)
	logs := grantLogs(m)
	clock := grantClock(t)
	failMain, failPending := true, false
	grantWriteSeams(t, &failMain, &failPending)

	for i := 0; i < 8; i++ {
		m.refreshSharedClaudeCredential()
		*clock = clock.Add(claudeSharedWriteBackoffMax + time.Second) // past any backoff, and past the old credential's expiry
	}
	if srv.calls.Load() != 1 {
		t.Fatalf("token endpoint called %d times while a newer grant was unwritten, want once", srv.calls.Load())
	}
	alert := m.ClaudeSharedLoginUnsavedAlert()
	if alert == "" || !strings.Contains(alert, "no space left on device") || !strings.Contains(alert, ".credentials.pending") {
		t.Errorf("alert = %q", alert)
	}
	if !strings.Contains(logs.String(), "could NOT save the new one") {
		t.Error("no loud log line")
	}
	rec := sink.find(AuditClaudeCredentialRefreshed)
	if rec == nil || rec.Fields["reason"] != "new grant could not be saved" {
		t.Errorf("audit entry = %+v", rec)
	}
	assertNoSentinel(t, "the log", logs.String())
	assertNoSentinel(t, "the alert", alert)
	assertNoTokenInAudit(t, sink)

	// Both locations failing is said plainly.
	failPending = true
	m2, _ := testManagerWithSink(t)
	m2.holdSharedClaudeGrant(&claude.OAuthTokens{AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, target, time.Hour)
	m2.recordSharedClaudeWriteFailure(errors.New("boom"), time.Now(), target)
	if a := m2.ClaudeSharedLoginUnsavedAlert(); !strings.Contains(a, "NOT saved to its backup file") {
		t.Errorf("alert with both writes failing = %q", a)
	}
}

// invalid_grant while a newer grant has reached an agent's home: reconciled,
// not marked rejected.
func TestSharedClaudeGrant_InvalidGrantWithNewerGrantIsReconciled(t *testing.T) {
	m, sink, links, target := sharedRefreshFixture(t, 1, time.Now().Add(time.Minute).UnixMilli())
	srv := &fakeRefreshServer{live: "something-else", lifetime: 8 * time.Hour} // refuses the shared refresh token
	withFakeRefresh(t, srv)
	logs := grantLogs(m)
	later := time.Now().Add(9 * time.Hour).UnixMilli()
	simulateClaudeRefresh(t, links[0], bridgeTestCreds(t, "agent", later))

	if got := m.refreshSharedClaudeCredential(); got != claudeSharedRefreshSuperseded {
		t.Fatalf("outcome = %v, want superseded (reconciled)", got)
	}
	if tok := readTokensAt(t, target); !strings.HasPrefix(tok.AccessToken, "acc-agent-") {
		t.Error("the newer agent grant was not promoted before judging the refusal")
	}
	m.claudeSharedRefresh.mu.Lock()
	rejected := m.claudeSharedRefresh.rejectedExpiresAt
	m.claudeSharedRefresh.mu.Unlock()
	if rejected != 0 {
		t.Error("credential marked rejected although a newer grant exists")
	}
	if rec := sink.find(AuditClaudeCredentialRefreshed); rec != nil && rec.Fields["reason"] == "refresh grant rejected" {
		t.Error("a rejection was audited")
	}
	assertNoSentinel(t, "the log", logs.String())
	assertNoTokenInAudit(t, sink)
}

// The central refresher, the bridge's promote-on-race and the session
// adopt-back all end in replaceSharedClaudeLoginIfNewer under claudeBridgeMu:
// whatever the arrival order, the newest grant wins.
func TestSharedClaudeGrant_ThreeWritersNewestAlwaysWins(t *testing.T) {
	shared := withSharedAgentHome(t)
	target := sharedClaudeBridgeTarget()
	if err := os.MkdirAll(shared+"/.claude", 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	grant := func(tag string, d time.Duration) *claude.OAuthTokens {
		return &claude.OAuthTokens{AccessToken: "acc-" + tag + "-" + bridgeTestTokenSentinel,
			RefreshToken: "ref-" + tag + "-" + bridgeTestTokenSentinel, ExpiresAt: now.Add(d).UnixMilli()}
	}
	grants := []*claude.OAuthTokens{grant("w1", 1*time.Hour), grant("w2", 2*time.Hour), grant("w3", 3*time.Hour)}
	for iter := 0; iter < 12; iter++ {
		if err := os.WriteFile(target, bridgeTestCreds(t, "base", now.Add(30*time.Minute).UnixMilli()), 0o660); err != nil {
			t.Fatal(err)
		}
		m, _ := testManagerWithSink(t)
		g := func(i int) *claude.OAuthTokens { return grants[(i+iter)%3] }
		m.claudeSharedRefresh.pending = g(0)
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); m.flushPendingSharedClaudeGrant(now) }() // the refresher
		go func() {                                                           // the agents' bridge promote-on-race
			defer wg.Done()
			data := mustMarshalCreds(t, g(1))
			claudeBridgeMu.Lock()
			defer claudeBridgeMu.Unlock()
			_, _ = promoteClaudeCredential(data, target, now)
		}()
		go func() { // the session adopt-back
			defer wg.Done()
			data := mustMarshalCreds(t, g(2))
			claudeBridgeMu.Lock()
			defer claudeBridgeMu.Unlock()
			_, _ = promoteClaudeCredential(data, target, now)
		}()
		wg.Wait()
		if got := readTokensAt(t, target); got.ExpiresAt != grants[2].ExpiresAt {
			t.Fatalf("iteration %d: shared login expires %d, want the newest %d", iter, got.ExpiresAt, grants[2].ExpiresAt)
		}
	}
}

func TestReplaceSharedClaudeLoginIfNewer_Rule(t *testing.T) {
	shared := withSharedAgentHome(t)
	target := sharedClaudeBridgeTarget()
	if err := os.MkdirAll(shared+"/.claude", 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cur := now.Add(2 * time.Hour)
	if err := os.WriteFile(target, bridgeTestCreds(t, "cur", cur.UnixMilli()), 0o660); err != nil {
		t.Fatal(err)
	}
	mk := func(exp time.Time) *claude.OAuthTokens {
		return &claude.OAuthTokens{AccessToken: "a", RefreshToken: "r", ExpiresAt: exp.UnixMilli()}
	}
	for _, c := range []*claude.OAuthTokens{mk(cur.Add(-time.Minute)), mk(cur)} {
		if _, err := replaceSharedClaudeLoginIfNewer(c, target, now, false); !errors.Is(err, errClaudeSharedNotNewer) {
			t.Errorf("older or equal grant: err = %v, want errClaudeSharedNotNewer", err)
		}
	}
	if _, err := replaceSharedClaudeLoginIfNewer(&claude.OAuthTokens{AccessToken: "a", ExpiresAt: 1}, target, now, false); !errors.Is(err, errClaudeBridgeNotPromotable) {
		t.Errorf("incomplete grant: err = %v", err)
	}
	if _, err := replaceSharedClaudeLoginIfNewer(mk(cur.Add(time.Minute)), target, now, false); err != nil {
		t.Fatalf("newer grant: %v", err)
	}
	if got := readTokensAt(t, target); got.ExpiresAt != cur.Add(time.Minute).UnixMilli() {
		t.Error("newer grant not written")
	}
}

func mustMarshalCreds(t *testing.T, tok *claude.OAuthTokens) []byte {
	t.Helper()
	data, err := json.Marshal(claude.Credentials{ClaudeAIOAuth: tok})
	if err != nil {
		t.Fatal(err)
	}
	return data
}
