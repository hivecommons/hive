package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/claude"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/internal/testutil"
	"github.com/hivecommons/hive/crux/sharedfile"
)

// Fake tokens. Every token string carries this sentinel so a test can prove
// no log line or audit field ever leaks token material.
const bridgeTestTokenSentinel = "SENTINEL-TOKEN-MATERIAL"

func bridgeTestCreds(t *testing.T, tag string, expiresAt int64) []byte {
	t.Helper()
	data, err := json.Marshal(claude.Credentials{ClaudeAIOAuth: &claude.OAuthTokens{
		AccessToken:  "acc-" + tag + "-" + bridgeTestTokenSentinel,
		RefreshToken: "ref-" + tag + "-" + bridgeTestTokenSentinel,
		ExpiresAt:    expiresAt,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// bridgeTestFixture provisions a per-agent home for "scanner" with the shared
// login in place and the credential symlink bridged, and returns the manager,
// the audit sink, the link path and the shared path.
func bridgeTestFixture(t *testing.T) (*Manager, *fakeAuditSink, *AgentProcess, string, string) {
	t.Helper()
	shared := withSharedAgentHome(t)
	m, sink := testManagerWithSink(t)
	ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}
	m.mu.Lock()
	m.agents["scanner"] = ap
	m.mu.Unlock()
	target := filepath.Join(shared, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	m.setupInteractiveHome(ap, "claude")
	link := filepath.Join(interactiveHomePath("scanner"), ".claude", ".credentials.json")
	if got, err := os.Readlink(link); err != nil || got != target {
		t.Fatalf("fixture: credential bridge not provisioned: target=%q err=%v", got, err)
	}
	return m, sink, ap, link, target
}

// simulateClaudeRefresh does what Claude Code's credential store does on a
// token refresh: write a temp file in the agent's .claude and rename it over
// .credentials.json, then chmod 0600. The rename replaces the symlink.
func simulateClaudeRefresh(t *testing.T, link string, data []byte) {
	t.Helper()
	tmp := link + ".tmp.12345"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(link, 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("simulated refresh did not replace the link: info=%v err=%v", info, err)
	}
}

func readTokensAt(t *testing.T, path string) *claude.OAuthTokens {
	t.Helper()
	tok := claude.LoadTokens(path)
	if tok == nil {
		t.Fatalf("no tokens at %s", path)
	}
	return tok
}

func staleFilesBeside(t *testing.T, path string) []string {
	t.Helper()
	matches, err := filepath.Glob(path + claudeCredentialStaleInfix + "*")
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func assertLinkRestored(t *testing.T, link, target string) {
	t.Helper()
	got, err := os.Readlink(link)
	if err != nil || got != target {
		t.Fatalf("credential link not restored: target=%q err=%v", got, err)
	}
}

// TestSetupInteractiveHome_RefreshReplacedLinkIsPublishedToSharedLogin is the
// reported failure (Discord, 2026-11-11): on a per-UID hive one agent's token
// refresh replaced its credential link, the refresh never reached the shared
// file, and the fleet logged out when the shared token expired. The refreshed
// token must reach the shared login and the agent must be re-linked, with
// memory untouched.
func TestSetupInteractiveHome_RefreshReplacedLinkIsPublishedToSharedLogin(t *testing.T) {
	m, sink, ap, link, target := bridgeTestFixture(t)
	now := time.Now()
	if err := os.WriteFile(target, bridgeTestCreds(t, "old", now.Add(10*time.Minute).UnixMilli()), 0o660); err != nil {
		t.Fatal(err)
	}
	memory := filepath.Join(filepath.Dir(link), "projects", "-data-agents-scanner", "memory", "MEMORY.md")
	if err := os.MkdirAll(filepath.Dir(memory), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memory, []byte("# mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	fresh := bridgeTestCreds(t, "fresh", now.Add(8*time.Hour).UnixMilli())
	simulateClaudeRefresh(t, link, fresh)
	// The defect: the shared file did not move.
	if tok := readTokensAt(t, target); !strings.HasPrefix(tok.AccessToken, "acc-old-") {
		t.Fatalf("precondition: shared file should still hold the old token, got %q", tok.AccessToken)
	}

	m.setupInteractiveHome(ap, "claude")

	if tok := readTokensAt(t, target); !strings.HasPrefix(tok.AccessToken, "acc-fresh-") {
		t.Errorf("refresh not published to the shared login: shared access token = %q", tok.AccessToken)
	}
	assertLinkRestored(t, link, target)
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != claudeSharedCredentialMode {
		t.Errorf("shared credential mode = %v err=%v, want %#o", info.Mode(), err, claudeSharedCredentialMode)
	}
	// Private copy moved aside, not deleted, still owner-only.
	stale := staleFilesBeside(t, link)
	if len(stale) != 1 {
		t.Fatalf("stale agent files = %v, want exactly one", stale)
	}
	if info, err := os.Stat(stale[0]); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("stale private copy mode = %v err=%v, want 0600", info.Mode(), err)
	}
	// Old shared file preserved beside the shared credential.
	if got := staleFilesBeside(t, target); len(got) != 1 || !bytes.Contains(mustRead(t, got[0]), []byte("acc-old-")) {
		t.Errorf("replaced shared file not preserved: %v", got)
	}
	if got, err := os.ReadFile(memory); err != nil || string(got) != "# mine" {
		t.Errorf("per-agent memory disturbed: %q err=%v", got, err)
	}
	// Audited, kept=agent, no token material.
	rec := sink.find(AuditClaudeCredentialRebridged)
	if rec == nil {
		t.Fatal("no claude_credential_rebridged audit entry")
	}
	if rec.Agent != "scanner" || rec.Fields["kept"] != "agent" || rec.Fields["trigger"] != claudeBridgeTriggerLaunch {
		t.Errorf("audit entry = %+v", rec)
	}
	if strings.Contains(fmt.Sprint(rec.Fields), bridgeTestTokenSentinel) {
		t.Error("audit fields carry token material")
	}
	// Idempotent: a second launch finds a healthy link and changes nothing.
	m.setupInteractiveHome(ap, "claude")
	if sink.count() != 1 {
		t.Errorf("second launch audited again: %d entries", sink.count())
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestRepairClaudeCredentialBridge_KeepsNewerShared: a dashboard sign-in after
// the refresh wrote a newer shared credential; the agent's stale private copy
// is moved aside and the agent re-linked, the shared file untouched.
func TestRepairClaudeCredentialBridge_KeepsNewerShared(t *testing.T) {
	m, sink, _, link, target := bridgeTestFixture(t)
	now := time.Now()
	simulateClaudeRefresh(t, link, bridgeTestCreds(t, "agent", now.Add(1*time.Hour).UnixMilli()))
	sharedBytes := bridgeTestCreds(t, "signin", now.Add(8*time.Hour).UnixMilli())
	if err := os.WriteFile(target, sharedBytes, 0o660); err != nil {
		t.Fatal(err)
	}

	rep := m.rebridgeClaudeCredential("scanner", link, target, claudeBridgeTriggerWatchdog)
	if rep == nil || rep.kept != "shared" || rep.staleSharedPath != "" {
		t.Fatalf("repair = %+v, want kept=shared with no preserved shared file", rep)
	}
	if !bytes.Equal(mustRead(t, target), sharedBytes) {
		t.Error("shared credential was rewritten")
	}
	assertLinkRestored(t, link, target)
	if got := staleFilesBeside(t, link); len(got) != 1 || !bytes.Contains(mustRead(t, got[0]), []byte("acc-agent-")) {
		t.Errorf("private copy not moved aside: %v", got)
	}
	if rec := sink.find(AuditClaudeCredentialRebridged); rec == nil || rec.Fields["kept"] != "shared" {
		t.Errorf("audit = %+v", rec)
	}
}

// TestRepairClaudeCredentialBridge_FailedRefreshNeverWins: an agent whose
// refresh failed on a rotated grant writes an empty-token, expiresAt-0 file.
// It must never replace the shared login, usable or not.
func TestRepairClaudeCredentialBridge_FailedRefreshNeverWins(t *testing.T) {
	for _, sharedUsable := range []bool{true, false} {
		t.Run(fmt.Sprintf("sharedUsable=%v", sharedUsable), func(t *testing.T) {
			m, _, _, link, target := bridgeTestFixture(t)
			exp := time.Now().Add(-time.Hour).UnixMilli()
			if sharedUsable {
				exp = time.Now().Add(time.Hour).UnixMilli()
			}
			sharedBytes := bridgeTestCreds(t, "shared", exp)
			if err := os.WriteFile(target, sharedBytes, 0o660); err != nil {
				t.Fatal(err)
			}
			simulateClaudeRefresh(t, link, []byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`))

			rep := m.rebridgeClaudeCredential("scanner", link, target, claudeBridgeTriggerWatchdog)
			if rep == nil || rep.kept != "shared" {
				t.Fatalf("repair = %+v, want kept=shared", rep)
			}
			if !bytes.Equal(mustRead(t, target), sharedBytes) {
				t.Error("empty-token file replaced the shared login")
			}
			assertLinkRestored(t, link, target)
		})
	}
}

// TestRepairClaudeCredentialBridge_ReadsAgentOwnedFileAsOwner is the per-UID
// case: the private file is the agent's, 0600, and the hive process cannot
// open it. The read must go through the su-exec seam as the file's owner, and
// the fresh token must still be promoted.
func TestRepairClaudeCredentialBridge_ReadsAgentOwnedFileAsOwner(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read a mode-000 file; the permission fallback cannot be exercised")
	}
	m, _, _, link, target := bridgeTestFixture(t)
	now := time.Now()
	if err := os.WriteFile(target, bridgeTestCreds(t, "old", now.Add(10*time.Minute).UnixMilli()), 0o660); err != nil {
		t.Fatal(err)
	}
	fresh := bridgeTestCreds(t, "fresh", now.Add(8*time.Hour).UnixMilli())
	simulateClaudeRefresh(t, link, fresh)
	if err := os.Chmod(link, 0); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var calls []string
	old := runAsAgentUser
	runAsAgentUser = func(spec string, stdin []byte, argv ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, spec+" "+strings.Join(argv, " "))
		if len(argv) != 2 || argv[0] != "cat" {
			return nil, fmt.Errorf("unexpected argv %v", argv)
		}
		// Stand in for the owner: the file is ours, only its mode blocks us.
		if err := os.Chmod(argv[1], 0o600); err != nil {
			return nil, err
		}
		defer func() { _ = os.Chmod(argv[1], 0) }()
		return os.ReadFile(argv[1])
	}
	t.Cleanup(func() { runAsAgentUser = old })

	rep := m.rebridgeClaudeCredential("scanner", link, target, claudeBridgeTriggerWatchdog)
	if rep == nil || rep.kept != "agent" {
		t.Fatalf("repair = %+v, want kept=agent", rep)
	}
	if !bytes.Equal(mustRead(t, target), fresh) {
		t.Error("agent-owned refresh was not promoted to the shared login")
	}
	assertLinkRestored(t, link, target)
	mu.Lock()
	defer mu.Unlock()
	wantSpec := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	if len(calls) < 1 || !strings.HasPrefix(calls[0], wantSpec+" cat "+link) {
		t.Errorf("su-exec calls = %v, want first read as %s of %s", calls, wantSpec, link)
	}
}

// TestRepairClaudeCredentialBridge_UnreadablePrivateLeftInPlace: when neither
// a direct read nor the owner helper can read the private file, nothing is
// moved or relinked (the agent keeps the token it has), nothing is audited,
// and the ERROR is logged once per file version.
func TestRepairClaudeCredentialBridge_UnreadablePrivateLeftInPlace(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read a mode-000 file; the permission fallback cannot be exercised")
	}
	m, sink, _, link, target := bridgeTestFixture(t)
	var logBuf bytes.Buffer
	m.logger = slog.New(slog.NewTextHandler(&logBuf, nil))
	sharedBytes := bridgeTestCreds(t, "shared", time.Now().Add(time.Hour).UnixMilli())
	if err := os.WriteFile(target, sharedBytes, 0o660); err != nil {
		t.Fatal(err)
	}
	private := bridgeTestCreds(t, "private", time.Now().Add(8*time.Hour).UnixMilli())
	simulateClaudeRefresh(t, link, private)
	if err := os.Chmod(link, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(link, 0o600) })
	old := runAsAgentUser
	runAsAgentUser = func(string, []byte, ...string) ([]byte, error) {
		return nil, errors.New("su-exec unavailable")
	}
	t.Cleanup(func() { runAsAgentUser = old })

	for i := 0; i < 3; i++ {
		if rep := m.rebridgeClaudeCredential("scanner", link, target, claudeBridgeTriggerWatchdog); rep != nil {
			t.Fatalf("cycle %d: repair = %+v, want none", i, rep)
		}
	}
	if info, err := os.Lstat(link); err != nil || !info.Mode().IsRegular() {
		t.Errorf("private file was moved or relinked: info=%v err=%v", info, err)
	}
	if got := staleFilesBeside(t, link); len(got) != 0 {
		t.Errorf("stale files created: %v", got)
	}
	if !bytes.Equal(mustRead(t, target), sharedBytes) {
		t.Error("shared credential changed")
	}
	if sink.count() != 0 {
		t.Errorf("audited %d entries, want none (nothing changed)", sink.count())
	}
	logs := logBuf.String()
	if n := strings.Count(logs, "cannot be judged"); n != 1 {
		t.Errorf("unreadable ERROR logged %d times over three cycles, want once:\n%s", n, logs)
	}
	if strings.Contains(logs, bridgeTestTokenSentinel) {
		t.Error("log carries token material")
	}
	if !strings.Contains(logs, link) {
		t.Error("log does not name the path")
	}
	// A new file version is logged again.
	if err := os.Chmod(link, 0o600); err != nil {
		t.Fatal(err)
	}
	simulateClaudeRefresh(t, link, private)
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(link, future, future); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(link, 0); err != nil {
		t.Fatal(err)
	}
	m.rebridgeClaudeCredential("scanner", link, target, claudeBridgeTriggerWatchdog)
	if n := strings.Count(logBuf.String(), "cannot be judged"); n != 2 {
		t.Errorf("new file version not logged again: %d", n)
	}
}

// TestRepairClaudeCredentialBridge_LegacyLayoutIsNeverTouched: an agent whose
// whole ~/.claude is still the legacy directory symlink has the SHARED login
// at its credential path. Moving it aside would log the fleet out.
func TestRepairClaudeCredentialBridge_LegacyLayoutIsNeverTouched(t *testing.T) {
	shared := withSharedAgentHome(t)
	m, sink := testManagerWithSink(t)
	var logBuf bytes.Buffer
	m.logger = slog.New(slog.NewTextHandler(&logBuf, nil))
	target := filepath.Join(shared, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	sharedBytes := bridgeTestCreds(t, "shared", time.Now().Add(time.Hour).UnixMilli())
	if err := os.WriteFile(target, sharedBytes, 0o660); err != nil {
		t.Fatal(err)
	}
	home := interactiveHomePath("scanner")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(shared, ".claude"), filepath.Join(home, ".claude")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".claude", ".credentials.json")

	for i := 0; i < 2; i++ {
		if rep := m.rebridgeClaudeCredential("scanner", link, target, claudeBridgeTriggerWatchdog); rep != nil {
			t.Fatalf("repair = %+v, want none", rep)
		}
	}
	if !bytes.Equal(mustRead(t, target), sharedBytes) {
		t.Error("shared credential disturbed")
	}
	if got := staleFilesBeside(t, target); len(got) != 0 {
		t.Errorf("shared login moved aside: %v", got)
	}
	if sink.count() != 0 {
		t.Error("legacy skip must not audit")
	}
	if n := strings.Count(logBuf.String(), "legacy whole-directory symlink"); n != 1 {
		t.Errorf("legacy warning logged %d times, want once", n)
	}
}

// TestRepairClaudeCredentialBridge_NothingToRepair: a healthy link, an absent
// link and a directory are not this failure.
func TestRepairClaudeCredentialBridge_NothingToRepair(t *testing.T) {
	m, sink, _, link, target := bridgeTestFixture(t)
	if rep := m.rebridgeClaudeCredential("scanner", link, target, claudeBridgeTriggerWatchdog); rep != nil {
		t.Errorf("healthy link: repair = %+v", rep)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if rep := m.rebridgeClaudeCredential("scanner", link, target, claudeBridgeTriggerWatchdog); rep != nil {
		t.Errorf("absent link: repair = %+v", rep)
	}
	if err := os.Mkdir(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if rep := m.rebridgeClaudeCredential("scanner", link, target, claudeBridgeTriggerWatchdog); rep != nil {
		t.Errorf("directory: repair = %+v", rep)
	}
	if sink.count() != 0 {
		t.Error("nothing to repair must not audit")
	}
	// No per-agent home at all: no paths, no repair.
	if _, _, ok := claudeCredentialBridgePaths("scanner", 0, "claude"); ok {
		t.Error("uid 0 must have no bridge")
	}
	if _, _, ok := claudeCredentialBridgePaths("scanner", 1001, "vllm"); ok {
		t.Error("inference backend must have no bridge")
	}
	t.Setenv("HIVE_SHARED_AGENT_HOME", "1")
	if _, _, ok := claudeCredentialBridgePaths("scanner", 1001, "claude"); ok {
		t.Error("forced shared home must have no bridge")
	}
}

// TestRepairClaudeCredentialBridge_StaleNamesNeverCollide: two repairs within
// the same second get distinct stale names on both sides.
func TestRepairClaudeCredentialBridge_StaleNamesNeverCollide(t *testing.T) {
	m, _, _, link, target := bridgeTestFixture(t)
	fixed := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	oldNow := claudeBridgeNow
	claudeBridgeNow = func() time.Time { return fixed }
	t.Cleanup(func() { claudeBridgeNow = oldNow })
	if err := os.WriteFile(target, bridgeTestCreds(t, "s0", fixed.Add(time.Hour).UnixMilli()), 0o660); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		simulateClaudeRefresh(t, link, bridgeTestCreds(t, fmt.Sprintf("a%d", i), fixed.Add(time.Duration(i+1)*time.Hour).UnixMilli()))
		if rep := m.rebridgeClaudeCredential("scanner", link, target, claudeBridgeTriggerWatchdog); rep == nil || rep.kept != "agent" {
			t.Fatalf("repair %d = %+v", i, rep)
		}
	}
	if got := staleFilesBeside(t, link); len(got) != 2 {
		t.Errorf("agent stale files = %v, want 2 distinct", got)
	}
	got := staleFilesBeside(t, target)
	if len(got) != 2 {
		t.Fatalf("shared stale files = %v, want 2 distinct", got)
	}
	if !strings.HasSuffix(got[1], "-1") {
		t.Errorf("second stale name = %q, want -1 suffix", got[1])
	}
	if tok := readTokensAt(t, target); !strings.HasPrefix(tok.AccessToken, "acc-a2-") {
		t.Errorf("shared token = %q, want the latest refresh", tok.AccessToken)
	}
}

func TestPreferAgentClaudeCredential(t *testing.T) {
	now := time.Now()
	live := func(tag string, d time.Duration) *claude.OAuthTokens {
		return &claude.OAuthTokens{AccessToken: "a" + tag, RefreshToken: "r" + tag, ExpiresAt: now.Add(d).UnixMilli()}
	}
	cases := []struct {
		name          string
		agent, shared *claude.OAuthTokens
		want          bool
	}{
		{"nil agent", nil, live("s", time.Hour), false},
		{"agent newer", live("a", 2*time.Hour), live("s", time.Hour), true},
		{"agent older", live("a", time.Hour), live("s", 2*time.Hour), false},
		{"tie keeps shared", live("a", time.Hour), live("a", time.Hour), false},
		{"shared unusable", live("a", time.Hour), &claude.OAuthTokens{AccessToken: "x", ExpiresAt: now.Add(-time.Hour).UnixMilli()}, true},
		{"shared absent", live("a", time.Hour), nil, true},
		{"agent no refresh token", &claude.OAuthTokens{AccessToken: "a", ExpiresAt: now.Add(time.Hour).UnixMilli()}, nil, false},
		{"agent no expiry", &claude.OAuthTokens{AccessToken: "a", RefreshToken: "r"}, nil, false},
		{"agent expired, no refresh expiry", &claude.OAuthTokens{AccessToken: "a", RefreshToken: "r", ExpiresAt: now.Add(-time.Hour).UnixMilli()}, nil, true},
		{"agent empty", &claude.OAuthTokens{}, nil, false},
	}
	for _, tc := range cases {
		if got := preferAgentClaudeCredential(tc.agent, tc.shared, now); got != tc.want {
			t.Errorf("%s: prefer agent = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPromoteClaudeCredential_RefusesUnusableBytes(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, ".credentials.json")
	sharedBytes := bridgeTestCreds(t, "shared", time.Now().Add(time.Hour).UnixMilli())
	if err := os.WriteFile(target, sharedBytes, 0o660); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{[]byte("not json"), []byte(`{}`), []byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`)} {
		stale, err := promoteClaudeCredential(bad, target, time.Now())
		if !errors.Is(err, errClaudeBridgeNotPromotable) || stale != "" {
			t.Errorf("promote(%q) = (%q, %v), want errClaudeBridgeNotPromotable", bad, stale, err)
		}
	}
	if !bytes.Equal(mustRead(t, target), sharedBytes) {
		t.Error("shared credential changed")
	}
	if got := staleFilesBeside(t, target); len(got) != 0 {
		t.Errorf("preserved copies made for refused promotions: %v", got)
	}
	// Promotion with no shared file: nothing to preserve, chown seam called
	// only when an owner was read.
	fresh := filepath.Join(dir, "fresh", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(fresh), 0o755); err != nil {
		t.Fatal(err)
	}
	stale, err := promoteClaudeCredential(sharedBytes, fresh, time.Now())
	if err != nil || stale != "" {
		t.Fatalf("promote to absent target = (%q, %v)", stale, err)
	}
	if info, err := os.Stat(fresh); err != nil || info.Mode().Perm() != claudeSharedCredentialMode {
		t.Errorf("promoted mode = %v err=%v", info.Mode(), err)
	}
}

func TestPromoteClaudeCredential_WritesOnlyTheTokenBlock(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(target, bridgeTestCreds(t, "shared", time.Now().Add(time.Hour).UnixMilli()), 0o660); err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(8 * time.Hour).UnixMilli()
	// A credential with extra material around and inside the token block.
	raw := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"acc-%s","refreshToken":"ref-%s","expiresAt":%d,"scopes":["user:inference"],"extraField":{"deep":"EXTRA-MATERIAL"}},"otherTopLevel":"EXTRA-MATERIAL"}`,
		bridgeTestTokenSentinel, bridgeTestTokenSentinel, exp)
	stale, err := promoteClaudeCredential([]byte(raw), target, time.Now())
	if err != nil || stale == "" {
		t.Fatalf("promote = (%q, %v)", stale, err)
	}
	got := mustRead(t, target)
	if bytes.Contains(got, []byte("EXTRA-MATERIAL")) {
		t.Errorf("shared file carries bytes outside the typed token block: %s", got)
	}
	want, _ := json.Marshal(claude.Credentials{ClaudeAIOAuth: &claude.OAuthTokens{
		AccessToken: "acc-" + bridgeTestTokenSentinel, RefreshToken: "ref-" + bridgeTestTokenSentinel,
		ExpiresAt: exp, Scopes: []string{"user:inference"},
	}})
	if !bytes.Equal(got, want) {
		t.Errorf("shared file = %s, want canonical %s", got, want)
	}
	if info, err := os.Lstat(target); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != claudeSharedCredentialMode {
		t.Errorf("shared file mode = %v err=%v", info.Mode(), err)
	}
	if info, err := os.Stat(stale); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("preserved shared copy mode = %v err=%v, want 0600", info.Mode(), err)
	}
	if _, err := os.Lstat(target + ".tmp-rebridge"); !errors.Is(err, fs.ErrNotExist) {
		t.Error("a predictable temporary name was used")
	}
	if left, _ := filepath.Glob(target + ".*.tmp"); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
	if _, _, err := canonicalClaudeCredential(bytes.Repeat([]byte("x"), claudeBridgeMaxCredentialBytes+1), time.Now()); !errors.Is(err, errClaudeBridgeNotPromotable) {
		t.Errorf("oversized input: err = %v", err)
	}
}

// TestRepairClaudeCredentialBridge_SharedTargetSymlinkIsRefused: the shared
// credential path holds a symlink. Nothing is written through it, the agent
// keeps its private copy in place, and the refusal is logged once.
func TestRepairClaudeCredentialBridge_SharedTargetSymlinkIsRefused(t *testing.T) {
	m, sink, _, link, target := bridgeTestFixture(t)
	var logBuf bytes.Buffer
	m.logger = slog.New(slog.NewTextHandler(&logBuf, nil))
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, target); err != nil {
		t.Fatal(err)
	}
	simulateClaudeRefresh(t, link, bridgeTestCreds(t, "fresh", time.Now().Add(8*time.Hour).UnixMilli()))

	for i := 0; i < 2; i++ {
		if rep := m.rebridgeClaudeCredential("scanner", link, target, claudeBridgeTriggerWatchdog); rep != nil {
			t.Fatalf("repair = %+v, want refusal", rep)
		}
	}
	if got, _ := os.ReadFile(victim); string(got) != "keep" {
		t.Errorf("victim rewritten: %q", got)
	}
	if info, _ := os.Lstat(victim); info.Mode().Perm() != 0o600 {
		t.Errorf("victim mode changed: %v", info.Mode())
	}
	if info, err := os.Lstat(target); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("symlink at the shared path was replaced: %v %v", info, err)
	}
	if info, err := os.Lstat(link); err != nil || !info.Mode().IsRegular() {
		t.Errorf("agent's private file not left in place: %v %v", info, err)
	}
	if got := staleFilesBeside(t, link); len(got) != 0 {
		t.Errorf("private copy left aside: %v", got)
	}
	if sink.count() != 0 {
		t.Error("a refusal must not audit")
	}
	if n := strings.Count(logBuf.String(), "location was refused"); n != 1 {
		t.Errorf("refusal logged %d times over two cycles, want once:\n%s", n, logBuf.String())
	}
	if strings.Contains(logBuf.String(), bridgeTestTokenSentinel) {
		t.Error("log carries token material")
	}
}

// TestRepairClaudeCredentialBridge_SharedDirSymlinkIsRefused: the shared
// .claude directory itself is a symlink. Nothing is written into what it
// points at.
func TestRepairClaudeCredentialBridge_SharedDirSymlinkIsRefused(t *testing.T) {
	m, _, _, link, target := bridgeTestFixture(t)
	sharedBytes := bridgeTestCreds(t, "shared", time.Now().Add(time.Hour).UnixMilli())
	if err := os.WriteFile(target, sharedBytes, 0o660); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(target)
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Rename(dir, elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, dir); err != nil {
		t.Fatal(err)
	}
	simulateClaudeRefresh(t, link, bridgeTestCreds(t, "fresh", time.Now().Add(8*time.Hour).UnixMilli()))

	if rep := m.rebridgeClaudeCredential("scanner", link, target, claudeBridgeTriggerWatchdog); rep != nil {
		t.Fatalf("repair = %+v, want refusal", rep)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(elsewhere, ".credentials.json")), sharedBytes) {
		t.Error("wrote through the directory symlink")
	}
	entries, _ := os.ReadDir(elsewhere)
	for _, e := range entries {
		if e.Name() != ".credentials.json" {
			t.Errorf("unexpected entry behind the directory symlink: %s", e.Name())
		}
	}
	if info, err := os.Lstat(link); err != nil || !info.Mode().IsRegular() {
		t.Errorf("agent's private file not left in place: %v %v", info, err)
	}
}

// TestMoveAsideClaudeCredential_NonRegularSourceIsRefused: a symlink where the
// private file should be is neither moved nor read.
func TestMoveAsideClaudeCredential_NonRegularSourceIsRefused(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, bridgeTestCreds(t, "v", time.Now().Add(time.Hour).UnixMilli()), 0o600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, ".credentials.json")
	if err := os.Symlink(victim, src); err != nil {
		t.Fatal(err)
	}
	if _, err := moveAsideClaudeCredential(src, time.Now()); !errors.Is(err, errClaudeBridgeSourceChanged) {
		t.Errorf("err = %v, want errClaudeBridgeSourceChanged", err)
	}
	if info, err := os.Lstat(src); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("symlink moved: %v %v", info, err)
	}
	if got := staleFilesBeside(t, src); len(got) != 0 {
		t.Errorf("stale files created: %v", got)
	}
	// A directory is refused the same way.
	d := filepath.Join(dir, "d")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := moveAsideClaudeCredential(d, time.Now()); !errors.Is(err, errClaudeBridgeSourceChanged) {
		t.Errorf("directory: err = %v", err)
	}
	// The owner-read path refuses a symlink too, even one to a readable file.
	if _, err := readClaudeCredentialAsOwner(src); !errors.Is(err, sharedfile.ErrNotRegular) {
		t.Errorf("read through symlink: err = %v, want ErrNotRegular", err)
	}
}

// TestRepairClaudeCredentialBridge_UnreadableIsRelinkedOnlyAfterSignIn: an
// unreadable private file stays put on the watchdog, but a dashboard sign-in
// (a shared login newer than any private copy) moves it aside unread and
// relinks the agent.
func TestRepairClaudeCredentialBridge_UnreadableIsRelinkedOnlyAfterSignIn(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read a mode-000 file; the permission fallback cannot be exercised")
	}
	m, sink, _, link, target := bridgeTestFixture(t)
	simulateClaudeRefresh(t, link, bridgeTestCreds(t, "private", time.Now().Add(8*time.Hour).UnixMilli()))
	if err := os.Chmod(link, 0); err != nil {
		t.Fatal(err)
	}
	old := runAsAgentUser
	runAsAgentUser = func(string, []byte, ...string) ([]byte, error) { return nil, errors.New("no su-exec") }
	t.Cleanup(func() { runAsAgentUser = old })

	// No shared login yet: even the sign-in trigger moves nothing.
	if rep := m.rebridgeClaudeCredential("scanner", link, target, claudeBridgeTriggerSignIn); rep != nil {
		t.Fatalf("sign-in without a shared login: repair = %+v", rep)
	}
	sharedBytes := bridgeTestCreds(t, "signin", time.Now().Add(8*time.Hour).UnixMilli())
	if err := os.WriteFile(target, sharedBytes, 0o660); err != nil {
		t.Fatal(err)
	}
	if rep := m.rebridgeClaudeCredential("scanner", link, target, claudeBridgeTriggerWatchdog); rep != nil {
		t.Fatalf("watchdog: repair = %+v, want none", rep)
	}
	if info, err := os.Lstat(link); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("watchdog moved the unreadable file: %v %v", info, err)
	}

	m.ReloadClaudeToken()
	assertLinkRestored(t, link, target)
	stale := staleFilesBeside(t, link)
	if len(stale) != 1 {
		t.Fatalf("stale files = %v, want the unreadable copy moved aside", stale)
	}
	if info, err := os.Lstat(stale[0]); err != nil || info.Mode().Perm() != 0 {
		t.Errorf("moved-aside copy mode = %v err=%v, want unchanged (000)", info.Mode(), err)
	}
	_ = os.Chmod(stale[0], 0o600)
	if !bytes.Equal(mustRead(t, target), sharedBytes) {
		t.Error("shared login changed")
	}
	rec := sink.find(AuditClaudeCredentialRebridged)
	if rec == nil || rec.Fields["kept"] != "shared" || rec.Fields["trigger"] != claudeBridgeTriggerSignIn {
		t.Errorf("audit = %+v", rec)
	}
}

// TestCredentialWatchdog_RebridgesWithinOneCycle: the bridge poll repairs a
// replaced link without waiting for the 5-minute probe.
func TestCredentialWatchdog_RebridgesWithinOneCycle(t *testing.T) {
	t.Setenv(ClaudeBridgeIntervalEnv, "20ms")
	t.Setenv(CredentialWatchdogIntervalEnv, "1h")
	m, _, _, link, target := bridgeTestFixture(t)
	now := time.Now()
	if err := os.WriteFile(target, bridgeTestCreds(t, "old", now.Add(10*time.Minute).UnixMilli()), 0o660); err != nil {
		t.Fatal(err)
	}
	fresh := bridgeTestCreds(t, "fresh", now.Add(8*time.Hour).UnixMilli())
	simulateClaudeRefresh(t, link, fresh)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { m.StartCredentialWatchdog(ctx); close(done) }()

	testutil.Eventually(t, 5*time.Second, func() bool {
		got, err := os.Readlink(link)
		return err == nil && got == target
	}, "watchdog did not re-bridge the credential within 5s")
	cancel()
	<-done
	if !bytes.Equal(mustRead(t, target), fresh) {
		t.Error("watchdog did not publish the refresh to the shared login")
	}
}

func TestClaudeBridgeInterval(t *testing.T) {
	t.Setenv(ClaudeBridgeIntervalEnv, "")
	if got := claudeBridgeInterval(); got != defaultClaudeBridgeInterval {
		t.Errorf("default = %v", got)
	}
	t.Setenv(ClaudeBridgeIntervalEnv, "250ms")
	if got := claudeBridgeInterval(); got != 250*time.Millisecond {
		t.Errorf("override = %v", got)
	}
	for _, bad := range []string{"nope", "-1s", "0"} {
		t.Setenv(ClaudeBridgeIntervalEnv, bad)
		if got := claudeBridgeInterval(); got != defaultClaudeBridgeInterval {
			t.Errorf("%q: = %v, want default", bad, got)
		}
	}
}

// TestReloadClaudeToken_RebridgesAfterSignIn: a dashboard sign-in reaches an
// agent whose link a refresh had replaced, at once rather than on the next
// poll; a logout (nothing usable) moves nothing.
func TestReloadClaudeToken_RebridgesAfterSignIn(t *testing.T) {
	m, sink, _, link, target := bridgeTestFixture(t)
	simulateClaudeRefresh(t, link, bridgeTestCreds(t, "agent", time.Now().Add(-time.Hour).UnixMilli()))

	// Logout: no shared file. Nothing moves.
	m.ReloadClaudeToken()
	if info, err := os.Lstat(link); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("logout must not touch the private file: info=%v err=%v", info, err)
	}
	if sink.count() != 0 {
		t.Fatal("logout audited a rebridge")
	}

	sharedBytes := bridgeTestCreds(t, "signin", time.Now().Add(8*time.Hour).UnixMilli())
	if err := os.WriteFile(target, sharedBytes, 0o660); err != nil {
		t.Fatal(err)
	}
	m.ReloadClaudeToken()
	assertLinkRestored(t, link, target)
	rec := sink.find(AuditClaudeCredentialRebridged)
	if rec == nil || rec.Fields["trigger"] != claudeBridgeTriggerSignIn || rec.Fields["kept"] != "shared" {
		t.Errorf("audit = %+v", rec)
	}
}

// TestRepairClaudeCredentialBridge_MidRenameIsNextCycle: the link vanishes
// between the Lstat and the read (the CLI is mid-rename). Nothing to do.
func TestRepairClaudeCredentialBridge_MidRenameIsNextCycle(t *testing.T) {
	m, _, _, link, target := bridgeTestFixture(t)
	simulateClaudeRefresh(t, link, bridgeTestCreds(t, "agent", time.Now().Add(time.Hour).UnixMilli()))
	if err := os.Chmod(link, 0); err != nil {
		t.Fatal(err)
	}
	old := runAsAgentUser
	runAsAgentUser = func(spec string, stdin []byte, argv ...string) ([]byte, error) {
		_ = os.Remove(link)
		return nil, &fs.PathError{Op: "open", Path: link, Err: fs.ErrNotExist}
	}
	t.Cleanup(func() { runAsAgentUser = old })
	if os.Getuid() == 0 {
		// Root reads the file directly; remove it from under the Lstat another way.
		runAsAgentUser = old
		t.Skip("root cannot exercise the owner-read path")
	}
	rep, err := m.repairClaudeCredentialBridge("scanner", link, target, claudeBridgeTriggerWatchdog)
	if rep != nil || err != nil {
		t.Errorf("mid-rename: (%+v, %v), want nothing", rep, err)
	}
}
