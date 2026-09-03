package dashboard

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// newSucceedingAppAuth builds a real *ghpkg.AppAuth whose JWT generation succeeds
// (valid RSA key) and whose CreateInstallationToken call hits a stub server that
// returns the given token, so ScopedToken — and therefore mintScopedToken —
// yields it deterministically and offline. This is the succeeding counterpart to
// newFailingAppAuth, used to exercise the mint+send paths (e.g. the #2610 finding
// 3 resume re-arm) without any real GitHub App.
func newSucceedingAppAuth(t *testing.T, token string) *ghpkg.AppAuth {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	keyFile := filepath.Join(t.TempDir(), "app-key.pem")
	if err := os.WriteFile(keyFile, pemBytes, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// POST /app/installations/{id}/access_tokens → an installation token.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		exp := time.Now().Add(wsTokenTTL).UTC().Format(time.RFC3339)
		fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, token, exp)
	}))
	t.Cleanup(srv.Close)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	auth, err := ghpkg.NewAppAuthWithCache(1, 2, keyFile,
		filepath.Join(t.TempDir(), "token.cache"), logger, srv.URL)
	if err != nil {
		t.Fatalf("NewAppAuthWithCache: %v", err)
	}
	return auth
}

// TestTokenRefreshDue covers the timing decision that gates the heartbeat's
// token re-mint: only an active task whose token was minted at least
// wsTokenRefreshPeriod ago is due. See #2393 item 2.
func TestTokenRefreshDue(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		task     *WSTaskAssign
		mintedAt time.Time
		now      time.Time
		wantDue  bool
		wantTier string
	}{
		{
			name:     "no active task",
			task:     nil,
			mintedAt: base,
			now:      base.Add(wsTokenRefreshPeriod + time.Minute),
			wantDue:  false,
		},
		{
			name:     "active task but never minted",
			task:     &WSTaskAssign{TaskID: "t1"},
			mintedAt: time.Time{},
			now:      base.Add(time.Hour),
			wantDue:  false,
		},
		{
			name:     "active task, not yet due",
			task:     &WSTaskAssign{TaskID: "t1"},
			mintedAt: base,
			now:      base.Add(wsTokenRefreshPeriod - time.Minute),
			wantDue:  false,
		},
		{
			name:     "active task, past refresh period",
			task:     &WSTaskAssign{TaskID: "t1"},
			mintedAt: base,
			now:      base.Add(wsTokenRefreshPeriod + time.Second),
			wantDue:  true,
			wantTier: "contributor",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &ContributorConnection{
				profile:       &ContributorProfile{TrustTier: "contributor"},
				currentTask:   tc.task,
				tokenMintedAt: tc.mintedAt,
			}
			tier, _, due := tokenRefreshDue(c, tc.now)
			if due != tc.wantDue {
				t.Fatalf("due = %v, want %v", due, tc.wantDue)
			}
			if due && tier != tc.wantTier {
				t.Fatalf("tier = %q, want %q", tier, tc.wantTier)
			}
		})
	}

	// wsTokenRefreshPeriod must leave headroom before wsTokenTTL, or a refresh
	// would race the expiry it is meant to beat.
	if wsTokenRefreshPeriod >= wsTokenTTL {
		t.Fatalf("wsTokenRefreshPeriod (%s) must be < wsTokenTTL (%s)", wsTokenRefreshPeriod, wsTokenTTL)
	}
}

// TestSendTokenRefreshShape verifies that a token_refresh emitted for an active
// task carries github_token + token_expires_at with the exact field names the
// relay's token_refresh handler consumes (bin/contributor-relay.sh), and that
// the mint timestamp is advanced so the next refresh is scheduled correctly.
func TestSendTokenRefreshShape(t *testing.T) {
	hub := &ContributeWSHub{logger: slog.Default()}

	// Stand up a real websocket pair so sendJSON writes an actual frame the
	// client can read back and inspect.
	var serverConn *websocket.Conn
	connReady := make(chan struct{})
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		serverConn = c
		close(connReady)
		// Keep the handler alive so the conn stays open for the read below.
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	<-connReady

	before := time.Now()
	conn := &ContributorConnection{
		ws:            serverConn,
		profile:       &ContributorProfile{TrustTier: "contributor", GitHubUsername: "refresh-user"},
		currentTask:   &WSTaskAssign{TaskID: "t-refresh"},
		tokenMintedAt: before.Add(-wsTokenRefreshPeriod - time.Minute),
	}

	const newToken = "ghs_new_scoped_token"
	if err := hub.sendTokenRefresh(conn, newToken); err != nil {
		t.Fatalf("sendTokenRefresh: %v", err)
	}

	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("client read: %v", err)
	}

	// Decode into a bare map to assert the ON-THE-WIRE field names the relay
	// reads (msg.github_token / msg.token_expires_at), not the Go struct names.
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if wire["type"] != "token_refresh" {
		t.Fatalf("type = %v, want token_refresh", wire["type"])
	}
	if wire["github_token"] != newToken {
		t.Fatalf("github_token = %v, want %q", wire["github_token"], newToken)
	}
	expStr, ok := wire["token_expires_at"].(string)
	if !ok || expStr == "" {
		t.Fatalf("token_expires_at missing/empty: %v", wire["token_expires_at"])
	}
	exp, err := time.Parse(time.RFC3339, expStr)
	if err != nil {
		t.Fatalf("token_expires_at not RFC3339: %v", err)
	}
	// Expiry must be roughly one TTL out from now, i.e. in the future.
	if !exp.After(before.Add(wsTokenTTL - time.Minute)) {
		t.Fatalf("token_expires_at %s not ~wsTokenTTL out from %s", exp, before)
	}

	// The mint timestamp must have advanced so the next refresh is scheduled
	// off the new token, not the stale one.
	conn.mu.Lock()
	minted := conn.tokenMintedAt
	conn.mu.Unlock()
	if !minted.After(before.Add(-time.Second)) {
		t.Fatalf("tokenMintedAt not advanced: %s", minted)
	}
}

// TestResumeTaskTokenReArmsRefresh reproduces #2610 finding 3 and verifies the
// fix: after a reconnect the disconnect defer has cleared tokenMintedAt to zero,
// so tokenRefreshDue is a permanent no-op for the resumed task. resumeTaskToken
// must re-mint, push the token, and re-arm tokenMintedAt so the very next
// heartbeat sees refresh as due again.
func TestResumeTaskTokenReArmsRefresh(t *testing.T) {
	hub := &ContributeWSHub{logger: slog.Default()}

	// Real websocket pair so resumeTaskToken -> sendTokenRefresh writes a frame.
	var serverConn *websocket.Conn
	connReady := make(chan struct{})
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		serverConn = c
		close(connReady)
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	<-connReady

	// Give the hub a real, succeeding App auth so mintScopedToken returns a
	// non-empty token deterministically and offline (the resume path only re-arms
	// on a successful mint+send).
	s := NewServer(0, slog.Default())
	s.deps = &Dependencies{GHAppAuth: newSucceedingAppAuth(t, "ghs_resume_scoped_token")}
	hub.server = s

	// State exactly as task_progress leaves it on a reconnect resume: currentTask
	// rebuilt, tokenMintedAt zeroed by the disconnect defer.
	conn := &ContributorConnection{
		ws:            serverConn,
		profile:       &ContributorProfile{TrustTier: "contributor", GitHubUsername: "resume-user"},
		currentTask:   &WSTaskAssign{TaskID: "t-resume", Repo: "o/r", Number: 7},
		tokenMintedAt: time.Time{},
	}

	// Precondition: with a zero mint time, refresh is NOT due — the bug's steady
	// state where the resumed session never refreshes again.
	if _, _, due := tokenRefreshDue(conn, time.Now().Add(2*wsTokenTTL)); due {
		t.Fatalf("precondition: refresh should not be due with zero tokenMintedAt")
	}

	before := time.Now()
	// C4: resume mints for the SERVER-issued lease's tier + repo.
	hub.resumeTaskToken(conn, &taskLease{taskID: "t-resume", repo: "o/r", number: 7, tier: "contributor", gen: 1})

	// The relay must receive a token_refresh carrying the fresh token.
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if wire["type"] != "token_refresh" {
		t.Fatalf("type = %v, want token_refresh", wire["type"])
	}
	if wire["github_token"] != "ghs_resume_scoped_token" {
		t.Fatalf("github_token = %v, want the resumed scoped token", wire["github_token"])
	}

	// Postcondition: tokenMintedAt is re-armed, so a later heartbeat sees refresh
	// as due again — the resumed task no longer silently loses its token.
	conn.mu.Lock()
	minted := conn.tokenMintedAt
	conn.mu.Unlock()
	if minted.IsZero() || minted.Before(before.Add(-time.Second)) {
		t.Fatalf("tokenMintedAt not re-armed on resume: %s", minted)
	}
	if _, _, due := tokenRefreshDue(conn, minted.Add(wsTokenRefreshPeriod+time.Second)); !due {
		t.Fatalf("after re-arm, refresh should be due again past the refresh period")
	}
}
