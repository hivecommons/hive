package main

// Tests for uncovered branches of three cmd/hive boot helpers:
//
//   - writeUpgradeMarker (main.go): the happy path is exercised by
//     upgrade_marker_test.go via recordUpgradeError; the write-failure warn
//     branch had no coverage.
//   - sameUpgradeTarget (main.go): the empty-second-argument guard and the
//     shorter-second-argument truncation were the branches
//     upgrade_marker_test.go's table missed.
//   - startDocsTokenRefresh (main.go): 11% covered — only reachable lines were
//     the DocsInstallationID==0 gate. The init-failure warn branch and the
//     "mint in the BACKGROUND, not on the startup path" contract (the #2439
//     readiness-stall regression guard) were untested.
//
// Deliberately NOT covered here: restartEventsToSnapshot/FromSnapshot
// (claimed by PR #6136), setupLogger/logAgentSandboxPosture (PR #6159), and
// the output-freshness helpers (PR #6127).

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// syncLogBuffer is a goroutine-safe log sink: startDocsTokenRefresh logs from
// a background goroutine, so a bare bytes.Buffer would race under -race.
type syncLogBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncLogBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncLogBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func markerTestLogger(sink io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// --- writeUpgradeMarker ----------------------------------------------------

// A failed marker write is non-fatal by design (the upgrade itself can still
// proceed; only the retry bookkeeping is degraded), so the helper must warn
// and return rather than panic or error out.
func TestWriteUpgradeMarkerWriteFailureWarnsAndDoesNotPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "upgrade-requested")

	var logs syncLogBuffer
	writeUpgradeMarker(path, upgradeMarker{TargetSHA: "abc"}, markerTestLogger(&logs))

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("no marker file should exist, stat err = %v", err)
	}
	if s := logs.String(); !strings.Contains(s, "failed to write upgrade marker") {
		t.Errorf("expected write-failure warn, got: %q", s)
	}
}

// --- sameUpgradeTarget -----------------------------------------------------

// TestSameUpgradeTargetEmptySecondArgAndShorterSecondArg pins the two
// branches upgrade_marker_test.go's table does not reach: an empty SECOND
// side must match nothing (or a zero-length prefix comparison would call
// every target "the same" and let a stale attempt latch swallow a new
// upgrade), and a SHORTER second side must still prefix-match a full SHA.
func TestSameUpgradeTargetEmptySecondArgAndShorterSecondArg(t *testing.T) {
	if sameUpgradeTarget("abc123", "") {
		t.Error(`sameUpgradeTarget("abc123", "") = true, want false`)
	}
	if !sameUpgradeTarget("fc32ae4d9c1b2a3", "fc32ae4") {
		t.Error(`sameUpgradeTarget(full, short) = false, want true`)
	}
	if sameUpgradeTarget("fc32ae4d9c1b2a3", "c11643a") {
		t.Error(`sameUpgradeTarget(full, different short) = true, want false`)
	}
}

// --- startDocsTokenRefresh -------------------------------------------------

// docsTestKeyFile writes a valid PKCS1 RSA private key to a temp file — the
// minimum NewAppAuthWithCache needs to succeed.
func docsTestKeyFile(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	pemData := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, pemData, 0o600); err != nil {
		t.Fatalf("writing test key: %v", err)
	}
	return path
}

// With no docs installation configured the helper must stand down completely:
// no auth init, no goroutine, no log line. The bogus key path makes the
// invariant self-enforcing — touching it past the gate would produce the
// init-failure warn asserted absent below.
func TestStartDocsTokenRefreshWithoutDocsInstallationIsNoOp(t *testing.T) {
	cfg := &config.Config{}
	var logs syncLogBuffer

	startDocsTokenRefresh(t.Context(), cfg, "/nonexistent/app.pem", markerTestLogger(&logs))

	if s := logs.String(); s != "" {
		t.Errorf("no-docs-org boot must be silent, got: %q", s)
	}
}

// An unreadable app key must degrade to a warn, not a failed boot: the docs
// org is an add-on, never this hive's primary auth.
func TestStartDocsTokenRefreshBadKeyFileWarnsAndStandsDown(t *testing.T) {
	cfg := &config.Config{GitHub: config.GitHubConfig{
		AppID:              42,
		DocsInstallationID: 7,
	}}
	var logs syncLogBuffer

	startDocsTokenRefresh(t.Context(), cfg, filepath.Join(t.TempDir(), "missing.pem"), markerTestLogger(&logs))

	if s := logs.String(); !strings.Contains(s, "failed to init docs org token") {
		t.Errorf("expected init-failure warn, got: %q", s)
	}
}

// The initial docs-token mint must happen in the BACKGROUND: blocking startup
// on a hanging docs mint is the #2439 readiness-stall pattern the code
// comment names. The fake GitHub API HOLDS the mint request until the test
// releases it — if the mint were on the startup path, startDocsTokenRefresh
// could not return before the release and the test would time out.
func TestStartDocsTokenRefreshMintsInBackgroundNotOnStartupPath(t *testing.T) {
	mintStarted := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(mintStarted)
		<-release
		// A 5xx keeps the goroutine on its non-fatal warn path and, crucially,
		// keeps AppAuth.Token from ever writing its /var/run cache file.
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	defer close(release)

	cfg := &config.Config{GitHub: config.GitHubConfig{
		AppID:              42,
		DocsInstallationID: 7,
		APIURL:             srv.URL,
	}}

	returned := make(chan struct{})
	go func() {
		startDocsTokenRefresh(t.Context(), cfg, docsTestKeyFile(t), markerTestLogger(io.Discard))
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("startDocsTokenRefresh blocked on the initial mint — the #2439 startup-path stall")
	}

	select {
	case <-mintStarted:
		// The background goroutine really did attempt the mint against the
		// configured (test) API URL — proving init succeeded and the refresh
		// loop is live, not silently skipped.
	case <-time.After(5 * time.Second):
		t.Fatal("background goroutine never attempted the initial docs token mint")
	}
}
