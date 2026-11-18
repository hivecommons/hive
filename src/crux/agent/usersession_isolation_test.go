package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
)

func startedRuntime(t *testing.T) (*UserSessionRuntime, *fakeTmux, *fakePriv) {
	t.Helper()
	f := sessionLifecycleTmux("")
	r, p := newTestSessionRuntimeWithPriv(t, f, sessionSecretEnv)
	if err := r.Start(crux.UserSessionSpec{ID: testSessionID, Vendor: "claude", UIDSlot: testSessionSlot}); err != nil {
		t.Fatal(err)
	}
	return r, f, p
}

// The invariant: a session never runs as the hive's own uid, root, the proxy
// uid or any agent uid. Every tmux call carries the reserved-range identity.
func TestUserSessionRunsAsReservedUnprivilegedUID(t *testing.T) {
	_, f, p := startedRuntime(t)
	if len(f.calls) == 0 {
		t.Fatal("no tmux calls")
	}
	for _, c := range f.calls {
		uid, _, _ := strings.Cut(c.user, ":")
		if c.user != testSessionSpecUser {
			t.Errorf("tmux %v ran as %q, want %q", c.args[0], c.user, testSessionSpecUser)
		}
		if uid == "0" || uid == "1001" || uid == "root" || uid == "" {
			t.Errorf("tmux ran as a privileged or hive uid: %q", c.user)
		}
	}
	if testSessionUID < userSessionMinUIDBase || testSessionUID <= baseAgentUID {
		t.Errorf("session uid %d overlaps the agent range", testSessionUID)
	}
	// Only the sandbox preparation and the opening of the tmux socket dir to
	// the terminal attacher run as root; everything else is the session uid.
	for _, c := range p.calls {
		if c.user == userSessionRootSpec {
			if len(c.argv) < 3 || (c.argv[2] != userSessionPrepareScript && c.argv[2] != userSessionSocketDirScript) {
				t.Errorf("unexpected root command during start: %v", c.argv)
			}
			continue
		}
		if c.user != testSessionSpecUser {
			t.Errorf("priv call %v ran as %q", c.argv, c.user)
		}
	}
}

func TestUserSessionUIDRangeRefusesCollisions(t *testing.T) {
	newTestSessionRuntime(t, &fakeTmux{}, nil)
	if uid, err := userSessionUID(1); err != nil || uid != defaultUserSessionUIDBase {
		t.Fatalf("slot 1 = %d, %v", uid, err)
	}
	for _, slot := range []int{0, -3, userSessionUIDSlots() + 1} {
		if _, err := userSessionUID(slot); err == nil {
			t.Errorf("slot %d accepted", slot)
		}
	}
	t.Setenv(userSessionUIDBaseEnv, "2001")
	if _, err := userSessionUID(1); err == nil {
		t.Error("a base inside the agent range was accepted")
	}
	t.Setenv(userSessionUIDBaseEnv, "30000")
	t.Setenv(agentUIDBaseEnv, "40000")
	if _, err := userSessionUID(1); err == nil {
		t.Error("a base below the agent base was accepted")
	}
	t.Setenv(agentUIDBaseEnv, "")
	// An agent uid inside the reserved range in the live uid map is refused.
	dir := t.TempDir()
	mapPath := filepath.Join(dir, "uid-map.json")
	m := NewUIDMap()
	m.Agents["scanner"] = 30010
	if err := m.Save(mapPath); err != nil {
		t.Fatal(err)
	}
	old := UIDMapPath
	UIDMapPath = mapPath
	t.Cleanup(func() { UIDMapPath = old })
	if _, err := userSessionUID(1); err == nil || !strings.Contains(err.Error(), "scanner") {
		t.Errorf("agent uid inside the session range not refused: %v", err)
	}
}

func TestUserSessionUIDRecoveredFromSandboxOwnerOnly(t *testing.T) {
	r, _, _ := startedRuntime(t)
	r.statUID = func(string) (int, error) { return 1001, nil }
	if _, err := r.sessionUID(testSessionID); err == nil {
		t.Error("a home owned by the hive uid was accepted")
	}
	r.statUID = func(string) (int, error) { return 2005, nil }
	if _, err := r.sessionUID(testSessionID); err == nil {
		t.Error("a home owned by an agent uid was accepted")
	}
	f := &fakeTmux{}
	r.run = f.run
	if p := r.Probe(testSessionID); p.Exists {
		t.Error("probe acted on a sandbox outside the reserved range")
	}
	if len(f.calls) != 0 {
		t.Errorf("tmux reached for a sandbox outside the reserved range: %v", f.verbs())
	}
}

func TestUserSessionSandboxIsPrivateAndRootOwned(t *testing.T) {
	_, _, p := startedRuntime(t)
	var prep *fakePrivCall
	for i := range p.calls {
		if len(p.calls[i].argv) > 2 && p.calls[i].argv[2] == userSessionPrepareScript {
			prep = &p.calls[i]
		}
	}
	if prep == nil {
		t.Fatal("sandbox was not prepared")
	}
	a := prep.argv
	if a[4] != userSessionSandboxDir(testSessionID) || a[5] != "20003" || a[6] != "4242" {
		t.Errorf("prepare args = %v", a)
	}
	for _, want := range []string{"chown root:", "chmod 0751", "chmod 2750", "chown -h"} {
		if !strings.Contains(userSessionPrepareScript, want) {
			t.Errorf("prepare script lacks %q", want)
		}
	}
	// the session cannot write the sandbox dir itself: no o+w / g+w anywhere
	if strings.Contains(userSessionPrepareScript, "777") || strings.Contains(userSessionPrepareScript, "o+") {
		t.Error("prepare script grants world access")
	}
	if !strings.Contains(userSessionMkdirScript, "umask 027") {
		t.Error("session dirs must not be world accessible")
	}
}

func TestUserSessionReaderGIDFailsClosed(t *testing.T) {
	newTestSessionRuntime(t, &fakeTmux{}, nil)
	for _, bad := range []string{"not-a-number", "0", "-5"} {
		t.Setenv(userSessionReaderGIDEnv, bad)
		if _, err := userSessionReaderGID(); err == nil {
			t.Errorf("reader gid %q accepted", bad)
		}
	}
	t.Setenv(userSessionReaderGIDEnv, "4242")
	if gid, err := userSessionReaderGID(); err != nil || gid != 4242 {
		t.Errorf("reader gid = %d, %v", gid, err)
	}
}

func TestUserSessionStartFailsClosedWithoutSandbox(t *testing.T) {
	f := sessionLifecycleTmux("")
	r, p := newTestSessionRuntimeWithPriv(t, f, nil)
	p.failOn = userSessionPrepareScript
	err := r.Start(crux.UserSessionSpec{ID: testSessionID, Vendor: "claude", UIDSlot: testSessionSlot})
	if err == nil || !strings.Contains(err.Error(), "creating session sandbox") {
		t.Fatalf("Start error = %v", err)
	}
	for _, v := range f.verbs() {
		if v == "respawn-pane" || v == "new-session" {
			t.Errorf("tmux %s ran although the sandbox could not be created", v)
		}
	}
}

func TestExecUserSessionTmuxRefusesPrivilegedIdentity(t *testing.T) {
	for _, u := range []string{"", userSessionRootSpec} {
		if _, err := execUserSessionTmux(context.Background(), u, nil, "", "hive-sess-x", "ls"); err == nil {
			t.Errorf("execUserSessionTmux accepted identity %q", u)
		}
	}
	if _, err := execUserSessionPriv(context.Background(), "", nil, "id"); err == nil {
		t.Error("execUserSessionPriv accepted an empty identity")
	}
}

func TestExecUserSessionTmuxGoesThroughSuExec(t *testing.T) {
	binDir := t.TempDir()
	for name, body := range map[string]string{"tmux": testFakeTmuxScript, "su-exec": testFakeSuExec} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir+":/bin:/usr/bin")
	// The fake su-exec resolves tmux through the CHILD env's PATH.
	env := []string{"PATH=" + binDir + ":/bin:/usr/bin"}

	out, err := execUserSessionTmux(context.Background(), testSessionSpecUser, env, "typed text", "hive-sess-x", "load-buffer", "-")
	if err != nil {
		t.Fatalf("err = %v, out %q", err, out)
	}
	for _, want := range []string{"suexec=" + testSessionSpecUser, "args=-L hive-sess-x load-buffer -", "typed text"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %q: %q", want, out)
		}
	}
	out, err = execUserSessionTmux(context.Background(), testSessionSpecUser, env, "", "hive-sess-x", "fail")
	if err == nil || !strings.Contains(err.Error(), "tmux fail") || !strings.Contains(string(out), "boom") {
		t.Fatalf("err = %v, out %q", err, out)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := execUserSessionTmux(context.Background(), testSessionSpecUser, env, "", "hive-sess-x", "display-message"); err == nil || !strings.Contains(err.Error(), "tmux display-message") {
		t.Errorf("missing binary error = %v", err)
	}
}

func TestExecUserSessionPrivUsesCleanEnvironment(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "su-exec"), []byte("#!/bin/sh\nshift\nenv\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":/bin:/usr/bin")
	t.Setenv("HIVE_TERMINAL_KEY", "term-secret")
	t.Setenv("GH_TOKEN", "ghp_secret")
	out, err := execUserSessionPriv(context.Background(), testSessionSpecUser, nil, "true")
	if err != nil {
		t.Fatalf("err = %v, out %q", err, out)
	}
	if strings.Contains(string(out), "term-secret") || strings.Contains(string(out), "ghp_secret") {
		t.Errorf("privileged helper inherited the hive's credentials: %q", out)
	}
}

func TestNewUserSessionRuntimeWiresRealDependencies(t *testing.T) {
	r := NewUserSessionRuntime(nil)
	if r.logger == nil || r.run == nil || r.priv == nil || r.statUID == nil || r.lookPath == nil || r.environ == nil || r.now == nil {
		t.Fatalf("runtime not fully wired: %+v", r)
	}
	logger := slog.Default()
	if got := NewUserSessionRuntime(logger); got.logger != logger {
		t.Error("supplied logger not used")
	}
	t.Setenv("HIVE_TEST_SESSION_ENVIRON_MARKER", "1")
	found := false
	for _, kv := range r.environ() {
		if kv == "HIVE_TEST_SESSION_ENVIRON_MARKER=1" {
			found = true
		}
	}
	if !found {
		t.Error("environ is not the process environment")
	}
}

func TestStatPathUID(t *testing.T) {
	dir := t.TempDir()
	if uid, err := statPathUID(dir); err != nil || uid != os.Getuid() {
		t.Errorf("statPathUID = %d, %v; want %d", uid, err, os.Getuid())
	}
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := statPathUID(file); err == nil {
		t.Error("a regular file was accepted as a sandbox")
	}
	link := filepath.Join(dir, "l")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := statPathUID(link); err == nil {
		t.Error("a symlink was followed")
	}
}

// ---- vendor login handling

func writeSharedLogin(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(sharedAgentHome, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUserSessionLoginsAreCopiedNeverSymlinkedOrBridgedConfig(t *testing.T) {
	f := sessionLifecycleTmux("")
	r, p := newTestSessionRuntimeWithPriv(t, f, nil)
	for _, rel := range userSessionLoginFiles {
		content := "login-" + rel
		if rel == userSessionClaudeLoginRel {
			// The Claude login is copied only when it parses as a credential
			// (usersession_login.go); the fixture is synthetic.
			content = string(bridgeTestCreds(t, "shared", time.Now().Add(time.Hour).UnixMilli()))
		}
		if l, ok := rotatingSessionLoginFor(rel); ok {
			// Codex and OpenCode logins are copied only when they parse as a
			// login of that vendor (usersession_login_rotating.go).
			content = string(rotatingTestVendors()[l.vendor].login("shared", time.Now()))
		}
		writeSharedLogin(t, rel, content)
	}
	// Files that execute commands or hold unrelated secrets must never travel.
	forbidden := []string{
		".claude/settings.json", ".gemini/settings.json", ".bob/settings.json",
		".config/gh/hosts.yml", ".config/goose/config.yaml", ".copilot/hooks.json",
	}
	for _, rel := range forbidden {
		writeSharedLogin(t, rel, "do-not-copy")
	}
	if err := r.Start(crux.UserSessionSpec{ID: testSessionID, Vendor: "claude", UIDSlot: testSessionSlot}); err != nil {
		t.Fatal(err)
	}
	home := userSessionHome(testSessionID)
	for _, rel := range userSessionLoginFiles {
		dest := filepath.Join(home, rel)
		st, err := os.Lstat(dest)
		if err != nil {
			t.Errorf("%s not copied: %v", rel, err)
			continue
		}
		if st.Mode()&os.ModeSymlink != 0 {
			t.Errorf("%s is a symlink into shared config", rel)
		}
		if st.Mode().Perm() != 0o400 {
			t.Errorf("%s mode = %o, want 0400", rel, st.Mode().Perm())
		}
	}
	for _, rel := range forbidden {
		if _, err := os.Lstat(filepath.Join(home, rel)); err == nil {
			t.Errorf("%s was copied into the session", rel)
		}
	}
	// No symlink anywhere in the session home.
	_ = filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.Type()&os.ModeSymlink != 0 {
			t.Errorf("symlink in session home: %s", path)
		}
		return nil
	})
	// The copy is written by the session uid, through the fixed script.
	copies := 0
	for _, c := range p.calls {
		if len(c.argv) > 2 && c.argv[2] == userSessionCopyLoginScript {
			copies++
			if c.user != testSessionSpecUser {
				t.Errorf("login copy ran as %q", c.user)
			}
		}
	}
	if copies != len(userSessionLoginFiles) {
		t.Errorf("copies = %d, want %d", copies, len(userSessionLoginFiles))
	}
}

func TestUserSessionLoginAllowlistHasNoCommandBearingConfig(t *testing.T) {
	for _, rel := range userSessionLoginFiles {
		base := filepath.Base(rel)
		for _, bad := range []string{"settings", "hooks", "mcp", "extensions", "hosts"} {
			if strings.Contains(strings.ToLower(base), bad) {
				t.Errorf("login allowlist entry %q looks like command-bearing config", rel)
			}
		}
		if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
			t.Errorf("login entry %q escapes the home", rel)
		}
	}
	if !strings.Contains(userSessionCopyLoginScript, "set -C") || !strings.Contains(userSessionCopyLoginScript, userSessionLoginFileMode) {
		t.Error("copy script must refuse to follow links and end read-only")
	}
}

func TestReadLoginSourceBounds(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok")
	if err := os.WriteFile(ok, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, good := readLoginSource(ok); !good || got != "x" {
		t.Errorf("readLoginSource = %q, %v", got, good)
	}
	if _, good := readLoginSource(dir); good {
		t.Error("a directory was read as a login file")
	}
	if _, good := readLoginSource(filepath.Join(dir, "missing")); good {
		t.Error("a missing file was read")
	}
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, []byte(strings.Repeat("a", userSessionLoginMaxBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, good := readLoginSource(big); good {
		t.Error("an oversized file was read")
	}
}

// ---- resume ids

func TestUserSessionResumeCommandRejectsFlagInjection(t *testing.T) {
	r := newTestSessionRuntime(t, &fakeTmux{}, nil)
	bad := []string{
		"claude --resume --dangerously-skip-permissions",
		"claude --resume abcdefgh --dangerously-skip-permissions",
		"claude --resume -abcdefgh",
		"claude --dangerously-skip-permissions",
		"claude -p abcdefgh",
		"codex resume --yolo",
		"codex --resume=abcdefgh",
	}
	for _, cmd := range bad {
		if _, err := r.launchArgv(crux.UserSessionSpec{Vendor: strings.Fields(cmd)[0], ResumeCommand: cmd}); err == nil {
			t.Errorf("resume command %q accepted", cmd)
		}
	}
	good, err := r.launchArgv(crux.UserSessionSpec{Vendor: "claude", ResumeCommand: "claude --resume " + testResumeUUID})
	if err != nil || strings.Join(good, " ") != "/usr/bin/claude --resume "+testResumeUUID {
		t.Errorf("valid resume = %v, %v", good, err)
	}
}

// ---- uploads

func TestUserSessionStoreUploadRunsAsSessionUID(t *testing.T) {
	r, _, p := startedRuntime(t)
	p.calls = nil
	path, n, err := r.StoreUpload(testSessionID, "ab12cd34-note.txt", strings.NewReader("hello"), 100, 5)
	if err != nil || n != 5 {
		t.Fatalf("StoreUpload = %q, %d, %v", path, n, err)
	}
	if path != filepath.Join(userSessionWorkDir(testSessionID), userSessionUploadsDirName, "ab12cd34-note.txt") {
		t.Errorf("path = %q", path)
	}
	if len(p.calls) != 1 || p.calls[0].user != testSessionSpecUser {
		t.Fatalf("upload must be written by the session uid only: %+v", p.calls)
	}
	if b, _ := os.ReadFile(path); string(b) != "hello" {
		t.Errorf("content = %q", b)
	}
	if !strings.Contains(userSessionUploadScript, "set -C") || !strings.Contains(userSessionUploadScript, "umask 077") {
		t.Error("upload script must create exclusively under umask 077")
	}
}

func TestUserSessionStoreUploadLimits(t *testing.T) {
	r, _, _ := startedRuntime(t)
	if _, _, err := r.StoreUpload(testSessionID, "aaaaaaaa-1", strings.NewReader("x"), 10, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.StoreUpload(testSessionID, "bbbbbbbb-2", strings.NewReader("x"), 10, 1); !errors.Is(err, crux.ErrUserSessionUploadLimit) {
		t.Errorf("second upload past the cap: %v", err)
	}
	dest := filepath.Join(userSessionWorkDir(testSessionID), userSessionUploadsDirName, "cccccccc-3")
	if _, _, err := r.StoreUpload(testSessionID, "cccccccc-3", strings.NewReader(strings.Repeat("x", 50)), 10, 99); !errors.Is(err, crux.ErrUserSessionUploadTooLarge) {
		t.Errorf("oversized upload: %v", err)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Error("oversized upload left a file behind")
	}
	for _, name := range []string{"../x", "-rf", ".hidden", "a/b", "", strings.Repeat("a", userSessionUploadNameMax+1)} {
		if _, _, err := r.StoreUpload(testSessionID, name, strings.NewReader("x"), 10, 9); err == nil {
			t.Errorf("upload name %q accepted", name)
		}
	}
	if _, _, err := r.StoreUpload("../etc", "aaaaaaaa-1", strings.NewReader("x"), 10, 9); err == nil {
		t.Error("bad session id accepted")
	}
}

// ---- cleanup

func TestUserSessionRemoveDeletesSandboxSocketsAndLegacyDirs(t *testing.T) {
	r, _, p := startedRuntime(t)
	legacyHome := filepath.Join(sharedAgentHome, legacyUserSessionHomeDir, testSessionID)
	legacyWork := filepath.Join(userSessionsDir(), testSessionID)
	for _, d := range []string{legacyHome, legacyWork} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p.calls = nil
	if err := r.Remove(testSessionID); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{userSessionSandboxDir(testSessionID), legacyHome, legacyWork} {
		if _, err := os.Stat(d); err == nil {
			t.Errorf("%s survived Remove", d)
		}
	}
	var removed []string
	for _, c := range p.calls {
		if len(c.argv) > 0 && c.argv[0] == "rm" {
			if c.user != userSessionRootSpec {
				t.Errorf("rm ran as %q", c.user)
			}
			removed = append(removed, c.argv[len(c.argv)-1])
		}
	}
	if len(removed) != 2 || removed[0] != userSessionSandboxDir(testSessionID) || removed[1] != userSessionTmuxSocketDir(testSessionUID) {
		t.Errorf("removed = %v", removed)
	}
}

func TestUserSessionRemoveReportsFailureSoTheSlotIsNotReused(t *testing.T) {
	r, _, p := startedRuntime(t)
	p.failOn = "rm -rf"
	if err := r.Remove(testSessionID); err == nil {
		t.Error("a failed removal was reported as success")
	}
	if err := r.Remove("../x"); err == nil {
		t.Error("bad id accepted")
	}
}

func TestUserSessionRemoveOfNeverStartedSessionIsClean(t *testing.T) {
	r, _ := newTestSessionRuntimeWithPriv(t, &fakeTmux{}, nil)
	if err := r.Remove(testSessionID); err != nil {
		t.Errorf("Remove of an unknown session = %v", err)
	}
}

// ---- tmux hardening

func TestUserSessionDisablesClipboardAndUsesUniqueBuffers(t *testing.T) {
	r, f, _ := startedRuntime(t)
	joined := ""
	for _, c := range f.calls {
		joined += " " + strings.Join(c.args, " ")
	}
	if !strings.Contains(joined, "set-option -s set-clipboard off") {
		t.Error("OSC52 clipboard not disabled for sessions")
	}
	f.handler = func(args []string) ([]byte, error) {
		if args[0] == "display-message" {
			return []byte("0 0 1700000000\n"), nil
		}
		return nil, nil
	}
	f.calls = nil
	for i := 0; i < 2; i++ {
		if err := r.SendText(testSessionID, "x", false); err != nil {
			t.Fatal(err)
		}
	}
	var names []string
	for _, c := range f.calls {
		if c.args[0] == "load-buffer" {
			names = append(names, c.args[2])
		}
	}
	if len(names) != 2 || names[0] == names[1] {
		t.Errorf("paste buffer names not unique: %v", names)
	}
}

func TestUserSessionEnvPairsSetPrivateTmpdir(t *testing.T) {
	newTestSessionRuntime(t, &fakeTmux{}, nil)
	for _, kv := range userSessionEnvPairs(testSessionID, "") {
		if kv[0] == "TMPDIR" {
			if kv[1] != userSessionTmpDir(testSessionID) || !strings.HasPrefix(kv[1], userSessionSandboxDir(testSessionID)) {
				t.Errorf("TMPDIR = %q", kv[1])
			}
			return
		}
	}
	t.Error("TMPDIR not set per session")
}

func TestUserSessionPathLayout(t *testing.T) {
	newTestSessionRuntime(t, &fakeTmux{}, nil)
	sb := userSessionSandboxDir(testSessionID)
	for name, got := range map[string]string{
		"home": userSessionHome(testSessionID), "work": userSessionWorkDir(testSessionID), "tmp": userSessionTmpDir(testSessionID),
	} {
		if filepath.Dir(got) != sb {
			t.Errorf("%s = %q is not directly under the sandbox %q", name, got, sb)
		}
	}
	if got := userSessionCodexHome(testSessionID); got != filepath.Join(userSessionHome(testSessionID), ".codex") {
		t.Errorf("codex home = %q", got)
	}
	t.Setenv(userSessionSandboxDirEnv, "")
	if userSessionSandboxRoot() != defaultUserSessionSandboxDir {
		t.Error("default sandbox root changed")
	}
	t.Setenv(userSessionsDirEnv, "")
	if userSessionsDir() != defaultUserSessionsDir {
		t.Error("default sessions dir changed")
	}
	r := &UserSessionRuntime{}
	t.Setenv(userSessionSandboxDirEnv, "/x/sb")
	if got := r.WorkDir(testSessionID); got != filepath.Join("/x/sb", testSessionID, userSessionWorkDirName) {
		t.Errorf("WorkDir = %q", got)
	}
}
