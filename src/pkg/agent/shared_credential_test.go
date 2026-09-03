package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claude"
	"github.com/hivecommons/hive/pkg/config"
)

// Regression coverage for kubestellar/hive#5730.
//
// Hive lets one operator login authenticate a whole fleet: every agent runs as
// its own Unix uid, but their ~/.claude all symlink to one shared
// /data/home/.claude (#4619). Claude Code refreshes that OAuth credential
// roughly every 8 hours and rewrites it as whichever agent refreshed it, mode
// 0600 — so the file the fleet shares becomes readable by exactly one of them.
//
// Measured on a standalone rootless-Podman hive, 2026-09-02: five of six claude
// agents went auth-required within 30 minutes of a refresh, the survivor being
// the agent that owned the file. The credential held a live access token AND a
// valid refresh grant throughout. The watchdog nonetheless reported "login
// expired (no usable refresh grant)" and prescribed an operator device-flow
// login, which fixes nothing — the next refresh re-tightens the file.
//
// Two invariants are pinned here:
//   - the permissions watcher recognises a shared credential and reopens it to
//     the node group instead of skipping it as another uid's file;
//   - the credential watchdog reports an unreadable credential as unreadable,
//     naming the chmod, rather than blaming an expired login.

// fixOne runs the watcher's per-entry repair over a single path, the way
// fixPermissions does during a walk.
func fixOne(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	fixEntry(path, fi, discardLogger())
}

// TestFixEntry_ReopensSharedClaudeCredential is the core repair: a 0600
// credential is brought back to group-readable so every agent uid sharing the
// login can read it again.
func TestFixEntry_ReopensSharedClaudeCredential(t *testing.T) {
	resetPermWarnDedupe()
	path := filepath.Join(t.TempDir(), ".credentials.json")
	if err := os.WriteFile(path, []byte(`{"claudeAiOauth":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	fixOne(t, path)

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o040 == 0 {
		t.Fatalf("credential left at %v — every agent uid but the owner is still locked out",
			fi.Mode().Perm())
	}
}

// TestFixEntry_CredentialRepairGrantsReadNotWrite: the file is an OAuth token,
// and the CLIs replace it by temp-file-and-rename inside a group-writable
// directory, so no agent needs write on the file itself. Widening further than
// the failure requires would be gratuitous.
func TestFixEntry_CredentialRepairGrantsReadNotWrite(t *testing.T) {
	resetPermWarnDedupe()
	path := filepath.Join(t.TempDir(), ".credentials.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	fixOne(t, path)

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	perm := fi.Mode().Perm()
	if perm&0o040 == 0 {
		t.Fatalf("group cannot read: %v", perm)
	}
	if perm&0o020 != 0 {
		t.Errorf("credential granted group WRITE (%v); read is all the fleet needs", perm)
	}
	if perm&0o007 != 0 {
		t.Errorf("credential granted world access (%v)", perm)
	}
}

// TestFixEntry_CredentialRepairIsIdempotent: an already-correct credential must
// come back byte-identical, so a watcher ticking every 10s neither churns the
// inode nor logs on a healthy file.
func TestFixEntry_CredentialRepairIsIdempotent(t *testing.T) {
	resetPermWarnDedupe()
	path := filepath.Join(t.TempDir(), ".credentials.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o640); err != nil {
		t.Fatal(err)
	}

	fixOne(t, path)
	fixOne(t, path)

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o640 {
		t.Fatalf("mode changed on an already-correct credential: %v, want -rw-r-----", got)
	}
}

// TestSharedCredentialBases_IsAnExactSet guards the blast radius. The carve-out
// runs BEFORE the owner guard that otherwise stops the watcher touching another
// uid's files, so it has to name exact files rather than match a pattern.
func TestSharedCredentialBases_IsAnExactSet(t *testing.T) {
	for _, near := range []string{
		"credentials.json",      // no leading dot
		".credentials.json.bak", // a backup
		".credentials.json.tmp", // a CLI's rename source
		"config.json",           // too generic to claim
		"oauth-token",
	} {
		if sharedCredentialBases[near] {
			t.Errorf("%q is treated as a shared credential; the carve-out must name exact files", near)
		}
	}
	if !sharedCredentialBases[".credentials.json"] {
		t.Error(".credentials.json is not recognised — the file this bug is about")
	}
}

// TestSharedCredentialBasesCoversClaudeCredentialsPath keeps the watcher's
// basename set honest against the path pkg/claude actually uses. A rename there
// would otherwise silently un-fix this bug: the watcher would keep walking, the
// credential would keep going 0600, and nothing would fail.
func TestSharedCredentialBasesCoversClaudeCredentialsPath(t *testing.T) {
	base := filepath.Base(claude.CredentialsPath)
	if !sharedCredentialBases[base] {
		t.Fatalf("claude.CredentialsPath is %q but %q is not in sharedCredentialBases — "+
			"the shared credential would go unrepaired", claude.CredentialsPath, base)
	}
}

// TestFixEntry_CarveOutIsFileOnly: a DIRECTORY named .credentials.json must
// not be claimed by the credential carve-out. Directories have their own arm
// (behind the owner guard, and needing g+rwx rather than g+r), so a carve-out
// that fired on them would hand a directory a mode nothing asked for.
func TestFixEntry_CarveOutIsFileOnly(t *testing.T) {
	resetPermWarnDedupe()
	// Point DevUID away from the test user's UID so the directory arm below
	// fixEntry's owner guard cannot run. GitHub's Linux runner happens to use
	// uid 1001, the production DevUID, so relying on the default made this test
	// exercise the generic directory repair rather than the credential carve-out.
	origDevUID := DevUID
	DevUID = os.Getuid() + 1
	t.Cleanup(func() { DevUID = origDevUID })
	path := filepath.Join(t.TempDir(), ".credentials.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	fixOne(t, path)

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() {
		t.Fatal("expected a directory")
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Fatalf("directory mode changed to %v; the file-only credential carve-out must not touch it", got)
	}
}

// TestFixSharedCredential_UnfixableIsLoud: on the deployment that produced
// #5730 the watcher runs as dev and the credential is owned by hive-<agent>, so
// chmod(2) returns EPERM and the in-process layer cannot repair it — the
// entrypoint's root guard does. What the watcher must never do is fail
// silently: an unrepairable shared credential is the difference between a fleet
// that works and one at a login prompt, so it is reported at ERROR with the
// mode, the owner and the exact command.
func TestFixSharedCredential_UnfixableIsLoud(t *testing.T) {
	resetPermWarnDedupe()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// A path that cannot be chmod'ed: it does not exist.
	missing := filepath.Join(t.TempDir(), "gone", ".credentials.json")
	fixSharedCredentialGroupRead(missing, 0o600, 2010, logger)

	logged := buf.String()
	if !strings.Contains(logged, "level=ERROR") {
		t.Errorf("an unrepairable shared credential was not reported at ERROR; got: %s", logged)
	}
	if !strings.Contains(logged, "chmod g+r") {
		t.Errorf("log does not give the operator the command that fixes this; got: %s", logged)
	}
	if !strings.Contains(logged, "owner_uid=2010") {
		t.Errorf("log does not name the owning uid; got: %s", logged)
	}
}

// TestFixSharedCredential_RepeatFailuresAreDeduped: the watcher ticks every 10s
// and this condition can stand for hours. The first failure warns; identical
// repeats drop to debug, like every other arm of the watcher (#4488).
func TestFixSharedCredential_RepeatFailuresAreDeduped(t *testing.T) {
	resetPermWarnDedupe()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	missing := filepath.Join(t.TempDir(), "gone", ".credentials.json")
	fixSharedCredentialGroupRead(missing, 0o600, 2010, logger)
	fixSharedCredentialGroupRead(missing, 0o600, 2010, logger)
	fixSharedCredentialGroupRead(missing, 0o600, 2010, logger)

	if got := strings.Count(buf.String(), "level=ERROR"); got != 1 {
		t.Errorf("standing failure logged at ERROR %d times, want 1 (the rest at debug)", got)
	}
}

// ── The watchdog half ───────────────────────────────────────────────────────

// writeClaudeCredsLive writes a credential whose access token is live — the
// state the file was in throughout the incident.
func writeClaudeCredsLive(t *testing.T, path string) {
	t.Helper()
	body := map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":           "sk-ant-oat-live",
			"expiresAt":             time.Now().Add(8 * time.Hour).UnixMilli(),
			"refreshToken":          "sk-ant-ort-live",
			"refreshTokenExpiresAt": time.Now().Add(28 * 24 * time.Hour).UnixMilli(),
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestClaudeTokenUsable_UnreadableIsNotAnExpiredLogin is the misdiagnosis this
// bug turned on. A credential the hive process cannot OPEN reported "login
// expired (no usable refresh grant)" and sent the operator to redo an OAuth
// flow — for a file holding a live access token and a valid refresh grant,
// where the actual fix is one chmod and a login changes nothing beyond
// resetting the 8-hour clock. That misreading is why #5454 looked for so long
// like "hive needs a daily re-login".
func TestClaudeTokenUsable_UnreadableIsNotAnExpiredLogin(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny access, so EACCES cannot be staged")
	}
	path := filepath.Join(t.TempDir(), ".credentials.json")
	writeClaudeCredsLive(t, path)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}

	res := claudeTokenUsable(path)

	if res.ok {
		t.Fatal("an unreadable credential must be reported as unusable")
	}
	if strings.Contains(res.reason, "login expired") {
		t.Errorf("unreadable credential reported as an expired login: %q — "+
			"this is the message that sends operators to a login that cannot help", res.reason)
	}
	if !strings.Contains(res.reason, "unreadable") && !strings.Contains(res.reason, "permission") {
		t.Errorf("reason %q does not name the real condition", res.reason)
	}
	if !strings.Contains(res.recovery, "chmod") {
		t.Errorf("recovery %q does not give the operator the one command that fixes this", res.recovery)
	}
	if strings.Contains(res.recovery, defaultCredentialRecovery) {
		t.Errorf("recovery still prescribes a device-flow login: %q", res.recovery)
	}

	// The mode and owner belong in the log: they are what turn "an agent is at a
	// login prompt" into "the shared credential is 0600 and owned by uid 2010".
	fields := fmt.Sprint(res.fields...)
	if !strings.Contains(fields, "mode") || !strings.Contains(fields, "owner_uid") {
		t.Errorf("probe fields %v carry neither mode nor owner_uid", res.fields)
	}
	// A probe must never emit token material, whatever else it reports.
	if strings.Contains(fields, "sk-ant-") {
		t.Fatalf("probe leaked token material in its log fields: %v", res.fields)
	}
}

// TestClaudeTokenUsable_SpentLoginStillSaysLoginExpired: the new arm must not
// swallow the case the watchdog was right about. A credential with no usable
// grant still prescribes an operator login.
func TestClaudeTokenUsable_SpentLoginStillSaysLoginExpired(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	writeClaudeCredsWithExpiry(t, path, "sk-ant-oat-old", time.Now().Add(-time.Hour).UnixMilli())

	res := claudeTokenUsable(path)

	if res.ok {
		t.Fatal("a spent credential is unusable")
	}
	if res.reason != "login expired (no usable refresh grant)" {
		t.Errorf("reason = %q, want the expired-login reason", res.reason)
	}
	if res.recovery != defaultCredentialRecovery {
		t.Errorf("recovery = %q, want the operator login", res.recovery)
	}
}

// TestEvalCredentialWatch_UsesProbeRecovery proves the recovery reaches the
// operator. The watchdog used to hardcode "operator dashboard device-flow
// login" for every condition, so even a correct probe verdict would have been
// reported with the wrong instruction.
func TestEvalCredentialWatch_UsesProbeRecovery(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := NewManager(map[string]config.AgentConfig{}, logger, ProjectContext{})
	m.mu.Lock()
	m.agents["a"] = &AgentProcess{Name: "a", Config: config.AgentConfig{Backend: "claude"}}
	m.mu.Unlock()

	w := credentialWatch{
		backend:     "claude",
		path:        "/data/home/.claude/.credentials.json",
		auditAction: AuditClaudeTokenMissing,
		probe: func(string) credentialProbe {
			return credentialProbe{
				reason:   "unreadable by the hive process (permission denied)",
				recovery: "chmod g+r /data/home/.claude/.credentials.json — the credential itself is fine",
				fields:   []any{"mode", "-rw-------", "owner_uid", uint32(2010)},
			}
		},
	}
	m.evalCredentialWatch(w, map[string]bool{})

	logged := buf.String()
	if !strings.Contains(logged, "chmod g+r") {
		t.Errorf("watchdog log does not carry the probe's recovery; got: %s", logged)
	}
	if strings.Contains(logged, defaultCredentialRecovery) {
		t.Errorf("watchdog still prescribes a device-flow login for an unreadable file; got: %s", logged)
	}
	if !strings.Contains(logged, "owner_uid=2010") {
		t.Errorf("watchdog log drops the probe's structured fields; got: %s", logged)
	}
}

// TestEvalCredentialWatch_DefaultRecoveryWhenProbeIsSilent: a probe that names
// no recovery must still tell the operator something. This is the path every
// pre-#5730 probe took.
func TestEvalCredentialWatch_DefaultRecoveryWhenProbeIsSilent(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := NewManager(map[string]config.AgentConfig{}, logger, ProjectContext{})
	m.mu.Lock()
	m.agents["a"] = &AgentProcess{Name: "a", Config: config.AgentConfig{Backend: "claude"}}
	m.mu.Unlock()

	w := credentialWatch{
		backend:     "claude",
		path:        "/data/home/.claude/.credentials.json",
		auditAction: AuditClaudeTokenMissing,
		probe:       func(string) credentialProbe { return credentialProbe{reason: "missing"} },
	}
	m.evalCredentialWatch(w, map[string]bool{})

	if !strings.Contains(buf.String(), defaultCredentialRecovery) {
		t.Errorf("a recovery-less probe produced no operator instruction; got: %s", buf.String())
	}
}
