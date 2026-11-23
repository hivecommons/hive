package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
)

const (
	testSessionID   = "0123456789ab"
	testSessionSlot = 3
	testReaderGID   = 4242
)

// testSessionUID is the uid slot testSessionSlot maps to with the default base.
const testSessionUID = defaultUserSessionUIDBase + testSessionSlot - 1

// testSessionSpecUser is the su-exec identity: the session uid with the
// sessions-only shared group as its single group.
var testSessionSpecUser = strconv.Itoa(testSessionUID) + ":" + strconv.Itoa(defaultUserSessionGID)

type fakeTmuxCall struct {
	user   string
	env    []string
	stdin  string
	socket string
	args   []string
}

// fakeTmux is a deterministic stand-in for the tmux binary. handler may be
// nil (every call succeeds with empty output).
type fakeTmux struct {
	calls   []fakeTmuxCall
	handler func(args []string) ([]byte, error)
}

func (f *fakeTmux) run(_ context.Context, user string, env []string, stdin, socket string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, fakeTmuxCall{user: user, env: env, stdin: stdin, socket: socket, args: args})
	if f.handler != nil {
		return f.handler(args)
	}
	return nil, nil
}

func (f *fakeTmux) verbs() []string {
	var out []string
	for _, c := range f.calls {
		out = append(out, c.args[0])
	}
	return out
}

type fakePrivCall struct {
	user  string
	argv  []string
	stdin string
}

// fakePriv stands in for su-exec. It records every privileged call and
// simulates the effect of the fixed scripts on the temp filesystem (no
// ownership is applied; ownership is asserted on the recorded identity).
type fakePriv struct {
	calls   []fakePrivCall
	failOn  string
	handler func(c fakePrivCall) (handled bool, out []byte, err error)
}

func exitStatusError(code int) error {
	return exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run()
}

func (f *fakePriv) run(_ context.Context, user string, stdin io.Reader, argv ...string) ([]byte, error) {
	c := fakePrivCall{user: user, argv: argv}
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		c.stdin = string(b)
	}
	f.calls = append(f.calls, c)
	if f.failOn != "" && strings.Contains(strings.Join(argv, " "), f.failOn) {
		return []byte("simulated priv failure"), errors.New("priv failed")
	}
	if f.handler != nil {
		if handled, out, err := f.handler(c); handled {
			return out, err
		}
	}
	if len(argv) >= 5 && argv[0] == "sh" {
		switch argv[2] {
		case userSessionPrepareScript:
			for _, n := range []string{userSessionHomeDirName, userSessionWorkDirName, userSessionTmpDirName} {
				if err := os.MkdirAll(filepath.Join(argv[4], n), 0o700); err != nil {
					return []byte(err.Error()), err
				}
			}
		case userSessionMkdirScript:
			for _, d := range argv[4:] {
				if err := os.MkdirAll(d, 0o700); err != nil {
					return []byte(err.Error()), err
				}
			}
		case userSessionCopyLoginScript:
			if err := os.MkdirAll(filepath.Dir(argv[4]), 0o700); err != nil {
				return nil, err
			}
			// The script replaces the previous (read-only) copy.
			_ = os.Remove(argv[4])
			if err := os.WriteFile(argv[4], []byte(c.stdin), 0o400); err != nil {
				return nil, err
			}
		case userSessionUploadScript:
			if err := os.MkdirAll(argv[4], 0o700); err != nil {
				return nil, err
			}
			max, _ := strconv.Atoi(argv[5])
			if ents, _ := os.ReadDir(argv[4]); len(ents) >= max {
				return nil, exitStatusError(userSessionUploadFullExit)
			}
			if err := os.WriteFile(filepath.Join(argv[4], argv[6]), []byte(c.stdin), 0o600); err != nil {
				return nil, err
			}
		}
	}
	if len(argv) > 1 && argv[0] == "rm" {
		_ = os.RemoveAll(argv[len(argv)-1])
	}
	return nil, nil
}

func newTestSessionRuntime(t *testing.T, f *fakeTmux, environ []string) *UserSessionRuntime {
	r, _ := newTestSessionRuntimeWithPriv(t, f, environ)
	return r
}

func newTestSessionRuntimeWithPriv(t *testing.T, f *fakeTmux, environ []string) (*UserSessionRuntime, *fakePriv) {
	t.Helper()
	oldHome := sharedAgentHome
	sharedAgentHome = filepath.Join(t.TempDir(), "home")
	t.Cleanup(func() { sharedAgentHome = oldHome })
	t.Setenv(userSessionsDirEnv, filepath.Join(t.TempDir(), "sessions"))
	t.Setenv(userSessionSandboxDirEnv, filepath.Join(t.TempDir(), "sandbox"))
	t.Setenv(userSessionReaderGIDEnv, strconv.Itoa(testReaderGID))
	t.Setenv(userSessionUIDBaseEnv, "")
	t.Setenv(userSessionUIDSlotsEnv, "")
	t.Setenv(agentUIDBaseEnv, "")
	t.Setenv(userSessionEnvPassthroughEnv, "")
	t.Setenv(userSessionGIDEnv, "")
	t.Setenv(userSessionWorkspaceDirEnv, "")
	p := &fakePriv{}
	return &UserSessionRuntime{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		run:    f.run,
		priv:   p.run,
		// The fake sandbox has no real ownership; every session home reads as
		// the test session uid (tests that need a missing or foreign owner
		// override this).
		statUID:  func(string) (int, error) { return testSessionUID, nil },
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		environ:  func() []string { return environ },
		now:      time.Now,
		git: func(context.Context, string, []string, ...string) ([]byte, error) {
			return nil, errors.New("no git in this test")
		},
	}, p
}

var sessionSecretEnv = []string{
	"PATH=/usr/bin",
	"DASHBOARD_AUTH_TOKEN=dash-secret",
	"HIVE_DASHBOARD_TOKEN=dash-secret-2",
	"HIVE_TERMINAL_KEY=term-secret",
	"HIVE_HUB_SECRET=hub-secret",
	"HIVE_HEARTBEAT_KEY=hb-secret",
	"HIVE_TTYD_CREDENTIAL=ttyd-secret",
	"HIVE_INVITE_KEY=invite-secret",
	"HIVE_GITHUB_TOKEN=ghs_installtoken",
	"GH_TOKEN=ghp_pat",
	"GITHUB_TOKEN=ghs_other",
	"GH_ENTERPRISE_TOKEN=ghe",
	"ANTHROPIC_API_KEY=sk-ant-secret",
	"HTTPS_PROXY=http://user:proxypass@proxy.example:3128",
}

// TestUserSessionBaseEnvStripsEveryCredential asserts the invariant itself:
// whatever the hive process env holds, no session env entry carries a
// dashboard, terminal, hub, heartbeat, ttyd or GitHub credential, by name or by
// value.
func TestUserSessionBaseEnvStripsEveryCredential(t *testing.T) {
	env := userSessionBaseEnv(sessionSecretEnv, "")
	joined := strings.Join(env, "\n")
	for _, secret := range []string{
		"dash-secret", "term-secret", "hub-secret", "hb-secret", "ttyd-secret", "invite-secret",
		"ghs_installtoken", "ghp_pat", "ghs_other", "sk-ant-secret", "proxypass",
	} {
		if strings.Contains(joined, secret) {
			t.Errorf("session env leaks %q:\n%s", secret, joined)
		}
	}
	if !strings.Contains(joined, "PATH=/usr/bin") {
		t.Errorf("session env lost PATH: %v", env)
	}
	if !strings.Contains(joined, "TERM="+userSessionDefaultTerm) {
		t.Errorf("session env missing TERM: %v", env)
	}
}

func TestUserSessionBaseEnvPassthroughCannotReintroduceCredentials(t *testing.T) {
	env := userSessionBaseEnv(sessionSecretEnv, "ANTHROPIC_API_KEY, HIVE_TERMINAL_KEY, GH_TOKEN, DASHBOARD_AUTH_TOKEN, HIVE_FUTURE_SECRET")
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "ANTHROPIC_API_KEY=sk-ant-secret") {
		t.Errorf("operator passthrough of a vendor key was dropped: %v", env)
	}
	for _, secret := range []string{"term-secret", "ghp_pat", "dash-secret"} {
		if strings.Contains(joined, secret) {
			t.Errorf("passthrough leaked %q", secret)
		}
	}
}

func TestUserSessionForbiddenCoversEveryKnownCredentialVar(t *testing.T) {
	strip := map[string]bool{}
	for _, n := range userSessionStripNames() {
		strip[n] = true
	}
	var all []string
	all = append(all, inheritedCredentialEnvVars...)
	all = append(all, githubTokenEnvVars...)
	all = append(all, "DASHBOARD_AUTH_TOKEN", "HIVE_DASHBOARD_TOKEN", "HIVE_TERMINAL_KEY", "HIVE_HUB_SECRET", "HIVE_HEARTBEAT_KEY", "HIVE_TTYD_CREDENTIAL", "HIVE_TTYD_ATTACH_KEY")
	for _, name := range all {
		if !sessionEnvForbidden(name) {
			t.Errorf("%s is not forbidden in sessions", name)
		}
		if !strip[name] {
			t.Errorf("%s is not in the set-environment -r strip list", name)
		}
	}
}

func TestUserSessionStripsConfiguredForgeTokenEnv(t *testing.T) {
	const custom = "CUSTOM_FORGE_PAT"
	config.AddExtraCredentialEnvVars(custom)
	strip := map[string]bool{}
	for _, n := range userSessionStripNames() {
		strip[n] = true
	}
	for _, name := range []string{"GITLAB_TOKEN", "GITEA_TOKEN", "FORGEJO_TOKEN", custom} {
		if !sessionEnvForbidden(name) {
			t.Errorf("%s is not forbidden in sessions", name)
		}
		if !strip[name] {
			t.Errorf("%s is not in the set-environment -r strip list", name)
		}
	}
	env := strings.Join(userSessionBaseEnv([]string{"GITLAB_TOKEN=x", "GITEA_TOKEN=x", "FORGEJO_TOKEN=x", custom + "=y"}, "GITLAB_TOKEN,GITEA_TOKEN,FORGEJO_TOKEN,"+custom), "\n")
	if strings.Contains(env, "GITLAB_TOKEN") || strings.Contains(env, "GITEA_TOKEN") || strings.Contains(env, "FORGEJO_TOKEN") || strings.Contains(env, custom) {
		t.Errorf("a forge token reached the session env: %q", env)
	}
}

func TestUserSessionStartLaunchesBareCLIWithoutBypassFlagsOrShell(t *testing.T) {
	f := &fakeTmux{handler: func(args []string) ([]byte, error) {
		if args[0] == "display-message" {
			return []byte("can't find session"), errors.New("no session")
		}
		return nil, nil
	}}
	r := newTestSessionRuntime(t, f, sessionSecretEnv)
	for _, v := range userSessionVendors {
		f.calls = nil
		if err := r.Start(crux.UserSessionSpec{ID: testSessionID, UIDSlot: testSessionSlot, Vendor: v.id}); err != nil {
			t.Fatalf("Start(%s): %v", v.id, err)
		}
		var respawn []string
		var envCmd []string
		for _, c := range f.calls {
			if c.socket != UserSessionTmuxPrefix+testSessionID {
				t.Fatalf("socket = %q, want per-session socket", c.socket)
			}
			if c.user != testSessionSpecUser {
				t.Fatalf("tmux ran as %q, want the session uid %q", c.user, testSessionSpecUser)
			}
			joined := strings.Join(c.env, "\n")
			for _, secret := range []string{"dash-secret", "term-secret", "hub-secret", "ghp_pat", "ghs_installtoken", "proxypass"} {
				if strings.Contains(joined, secret) {
					t.Fatalf("tmux client env leaks %q", secret)
				}
			}
			if c.args[0] == "respawn-pane" {
				respawn = c.args
			}
			for _, a := range c.args {
				if a == "set-environment" {
					envCmd = c.args
				}
			}
		}
		if respawn == nil {
			t.Fatalf("%s: no respawn-pane call in %v", v.id, f.verbs())
		}
		idx := -1
		for i, a := range respawn {
			if a == "--" {
				idx = i
			}
		}
		if idx < 0 {
			t.Fatalf("%s: respawn has no -- separator: %v", v.id, respawn)
		}
		argv := respawn[idx+1:]
		if len(argv) != 1 || argv[0] != "/usr/bin/"+v.binary {
			t.Errorf("%s: pane command = %v, want only the bare binary", v.id, argv)
		}
		for _, a := range respawn {
			if strings.Contains(a, "allow-all") || strings.Contains(a, "bypass") || strings.Contains(a, "yolo") ||
				strings.Contains(a, "dangerously") || strings.Contains(a, "auto-approve") {
				t.Errorf("%s: launch carries a permission-bypass flag: %v", v.id, respawn)
			}
			if a == "bash" || a == "sh" || a == "zsh" || strings.HasSuffix(a, "/bash") || strings.HasSuffix(a, "/sh") {
				t.Errorf("%s: launch reaches a shell: %v", v.id, respawn)
			}
		}
		if envCmd == nil {
			t.Fatalf("%s: no set-environment call", v.id)
		}
		joined := strings.Join(envCmd, " ")
		for _, name := range []string{"HIVE_TERMINAL_KEY", "DASHBOARD_AUTH_TOKEN", "GH_TOKEN", "HIVE_HUB_SECRET"} {
			if !strings.Contains(joined, "-r "+name) {
				t.Errorf("%s: session env does not remove %s", v.id, name)
			}
		}
		if !strings.Contains(joined, "HOME "+userSessionHome(testSessionID)) {
			t.Errorf("%s: session HOME not set to the per-session dir", v.id)
		}
	}
	if _, err := os.Stat(userSessionWorkDir(testSessionID)); err != nil {
		t.Errorf("work dir not created: %v", err)
	}
}

func TestUserSessionStartRejectsBadInput(t *testing.T) {
	f := &fakeTmux{}
	r := newTestSessionRuntime(t, f, nil)
	cases := []crux.UserSessionSpec{
		{ID: "../etc", Vendor: "claude"},
		{ID: "XYZ", Vendor: "claude"},
		{ID: testSessionID, Vendor: "bash"},
		{ID: testSessionID, Vendor: "sh"},
		{ID: testSessionID, Vendor: "claude", ResumeCommand: "claude --resume abc; rm -rf /"},
		{ID: testSessionID, Vendor: "claude", ResumeCommand: "codex resume abc"},
		{ID: testSessionID, Vendor: "claude", ResumeCommand: "claude --resume $(id)"},
		{ID: testSessionID, Vendor: "claude", ResumeCommand: "claude --resume --dangerously-skip-permissions"},
		{ID: testSessionID, Vendor: "claude", ResumeCommand: "claude --resume -abcdefgh1"},
	}
	for _, spec := range cases {
		spec.UIDSlot = testSessionSlot
		if err := r.Start(spec); err == nil {
			t.Errorf("Start(%+v) succeeded, want error", spec)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("rejected starts reached tmux: %v", f.verbs())
	}
	for _, slot := range []int{0, -1, userSessionUIDSlots() + 1} {
		if err := r.Start(crux.UserSessionSpec{ID: testSessionID, Vendor: "claude", UIDSlot: slot}); err == nil {
			t.Errorf("Start with uid slot %d succeeded", slot)
		}
	}
}

func TestUserSessionStartResumeUsesValidatedCommand(t *testing.T) {
	f := &fakeTmux{handler: func(args []string) ([]byte, error) {
		if args[0] == "display-message" {
			return nil, errors.New("no session")
		}
		return nil, nil
	}}
	r := newTestSessionRuntime(t, f, nil)
	spec := crux.UserSessionSpec{ID: testSessionID, UIDSlot: testSessionSlot, Vendor: "claude", ResumeCommand: "claude --resume 123e4567-e89b-12d3-a456-426614174000"}
	if err := r.Start(spec); err != nil {
		t.Fatal(err)
	}
	last := f.calls[len(f.calls)-1].args
	joined := strings.Join(last, " ")
	if !strings.HasSuffix(joined, "-- /usr/bin/claude --resume 123e4567-e89b-12d3-a456-426614174000") {
		t.Errorf("resume launch = %q", joined)
	}
}

func TestUserSessionProbe(t *testing.T) {
	f := &fakeTmux{handler: func(args []string) ([]byte, error) {
		return []byte("0 2 1700000000\n"), nil
	}}
	r := newTestSessionRuntime(t, f, nil)
	p := r.Probe(testSessionID)
	if !p.Exists || p.Dead || p.Attached != 2 || p.Activity.Unix() != 1700000000 {
		t.Errorf("probe = %+v", p)
	}
	f.handler = func(args []string) ([]byte, error) { return []byte("1 0 5\n"), nil }
	if p := r.Probe(testSessionID); !p.Dead {
		t.Errorf("dead pane not reported: %+v", p)
	}
	f.handler = func(args []string) ([]byte, error) { return nil, errors.New("can't find session") }
	if p := r.Probe(testSessionID); p.Exists {
		t.Errorf("missing session reported as existing: %+v", p)
	}
	if p := r.Probe("not-an-id"); p.Exists {
		t.Errorf("invalid id probed as existing")
	}
}

func TestUserSessionSendTextUsesPasteBufferNotKeyNames(t *testing.T) {
	f := &fakeTmux{handler: func(args []string) ([]byte, error) {
		if args[0] == "display-message" {
			return []byte("0 0 1700000000\n"), nil
		}
		return nil, nil
	}}
	r := newTestSessionRuntime(t, f, nil)
	text := "Enter C-c\nline two; $(rm -rf /)"
	if err := r.SendText(testSessionID, text, true); err != nil {
		t.Fatal(err)
	}
	var load, paste, keys *fakeTmuxCall
	for i := range f.calls {
		switch f.calls[i].args[0] {
		case "load-buffer":
			load = &f.calls[i]
		case "paste-buffer":
			paste = &f.calls[i]
		case "send-keys":
			keys = &f.calls[i]
		}
	}
	if load == nil || load.stdin != text {
		t.Fatalf("text was not loaded verbatim via stdin: %+v", load)
	}
	if paste == nil || !strings.Contains(strings.Join(paste.args, " "), "-p") {
		t.Fatalf("paste-buffer missing or not bracketed: %+v", paste)
	}
	for _, c := range f.calls {
		if c.args[0] == "send-keys" && strings.Contains(strings.Join(c.args, " "), "C-c") {
			t.Fatalf("user text reached send-keys as key names: %v", c.args)
		}
	}
	if keys == nil || keys.args[len(keys.args)-1] != "Enter" {
		t.Fatalf("submit did not press Enter: %+v", keys)
	}
	f.calls = nil
	if err := r.SendText(testSessionID, "no submit", false); err != nil {
		t.Fatal(err)
	}
	for _, v := range f.verbs() {
		if v == "send-keys" {
			t.Fatalf("Enter pressed without submit")
		}
	}
}

func TestUserSessionDetachWritableClientsLeavesReadOnlyViewers(t *testing.T) {
	f := &fakeTmux{handler: func(args []string) ([]byte, error) {
		switch args[0] {
		case "display-message":
			return []byte("0 3 1700000000\n"), nil
		case "list-clients":
			return []byte("/dev/pts/3 1\n/dev/pts/7 0\n/dev/pts/9 1\n"), nil
		}
		return nil, nil
	}}
	r := newTestSessionRuntime(t, f, nil)
	if err := r.DetachWritableClients(testSessionID); err != nil {
		t.Fatal(err)
	}
	var detached []string
	for _, c := range f.calls {
		if c.args[0] == "detach-client" {
			detached = append(detached, c.args[len(c.args)-1])
		}
	}
	if len(detached) != 1 || detached[0] != "/dev/pts/7" {
		t.Fatalf("detached %v, want only the writable client /dev/pts/7", detached)
	}
	// A session that is gone is not an error and nothing is listed.
	f.handler = func(args []string) ([]byte, error) { return nil, errors.New("no server running") }
	f.calls = nil
	if err := r.DetachWritableClients(testSessionID); err != nil {
		t.Fatalf("missing session: %v", err)
	}
	for _, v := range f.verbs() {
		if v == "list-clients" || v == "detach-client" {
			t.Errorf("missing session reached %s", v)
		}
	}
}

func TestUserSessionSendTextRefusesDeadOrInvalid(t *testing.T) {
	f := &fakeTmux{handler: func(args []string) ([]byte, error) { return []byte("1 0 1700000000\n"), nil }}
	r := newTestSessionRuntime(t, f, nil)
	if err := r.SendText(testSessionID, "hi", false); err == nil {
		t.Error("typing into a dead session succeeded")
	}
	f.handler = func(args []string) ([]byte, error) { return []byte("0 0 1700000000\n"), nil }
	f.calls = nil
	if err := r.SendText(testSessionID, "bad \x1b[201~ escape", false); err == nil {
		t.Error("escape sequence accepted")
	}
	for _, v := range f.verbs() {
		if v == "load-buffer" || v == "paste-buffer" {
			t.Errorf("rejected text reached tmux: %v", f.verbs())
		}
	}
}

func TestSanitizeSessionInput(t *testing.T) {
	ok := map[string]string{
		"plain":            "plain",
		"tab\tsep":         "tab\tsep",
		"a\r\nb":           "a\nb",
		"unicode: héllo ✓": "unicode: héllo ✓",
	}
	for in, want := range ok {
		got, err := SanitizeSessionInput(in)
		if err != nil || got != want {
			t.Errorf("Sanitize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"\x1b[31m", "nul\x00", "bell\a", "del\x7f", "c1\u0085", "bad\xff"} {
		if _, err := SanitizeSessionInput(in); err == nil {
			t.Errorf("Sanitize(%q) accepted", in)
		}
	}
}

func TestUserSessionKillIsIdempotent(t *testing.T) {
	f := &fakeTmux{handler: func(args []string) ([]byte, error) { return nil, errors.New("can't find session") }}
	r := newTestSessionRuntime(t, f, nil)
	if err := r.Kill(testSessionID); err != nil {
		t.Errorf("kill of missing session = %v", err)
	}
	for _, v := range f.verbs() {
		if v == "kill-session" {
			t.Error("kill-session issued for a missing session")
		}
	}
	f.handler = func(args []string) ([]byte, error) { return []byte("0 0 1\n"), nil }
	f.calls = nil
	if err := r.Kill(testSessionID); err != nil {
		t.Fatal(err)
	}
	last := f.calls[len(f.calls)-1].args
	if last[0] != "kill-session" || last[len(last)-1] != "hive-sess-"+testSessionID {
		t.Errorf("kill args = %v", last)
	}
}

func TestUserSessionCaptureCapsLines(t *testing.T) {
	f := &fakeTmux{handler: func(args []string) ([]byte, error) { return []byte("hello\n"), nil }}
	r := newTestSessionRuntime(t, f, nil)
	out, err := r.Capture(testSessionID, 0)
	if err != nil || out != "hello\n" {
		t.Fatalf("capture = %q, %v", out, err)
	}
	if !strings.Contains(strings.Join(f.calls[0].args, " "), "-S -50000") {
		t.Errorf("uncapped capture args: %v", f.calls[0].args)
	}
	f.calls = nil
	_, _ = r.Capture(testSessionID, 10_000_000)
	if !strings.Contains(strings.Join(f.calls[0].args, " "), "-S -50000") {
		t.Errorf("oversized capture not capped: %v", f.calls[0].args)
	}
	f.calls = nil
	_, _ = r.Capture(testSessionID, 25)
	if !strings.Contains(strings.Join(f.calls[0].args, " "), "-S -25 ") {
		t.Errorf("requested line count not honoured: %v", f.calls[0].args)
	}
}

func TestUserSessionVendorsMatchKnownBackends(t *testing.T) {
	const conf = "../../../config/backends.conf"
	if _, err := os.Stat(conf); err != nil {
		t.Skipf("backends.conf not reachable: %v", err)
	}
	out, err := exec.Command("bash", "-c", "source "+conf+" && printf '%s' \"$KNOWN_BACKENDS\"").Output()
	if err != nil {
		t.Fatalf("sourcing backends.conf: %v", err)
	}
	known := map[string]bool{}
	for _, b := range strings.Fields(string(out)) {
		known[b] = true
	}
	for _, v := range userSessionVendors {
		if !known[v.id] {
			t.Errorf("session vendor %q is not in KNOWN_BACKENDS", v.id)
		}
		if v.id == "bash" || v.id == "sh" || v.id == "shell" {
			t.Errorf("shell vendor %q must never be offered", v.id)
		}
	}
}

func TestUserSessionVendorsReportAvailability(t *testing.T) {
	r := newTestSessionRuntime(t, &fakeTmux{}, nil)
	r.lookPath = func(name string) (string, error) {
		if name == "claude" {
			return "/usr/bin/claude", nil
		}
		return "", errors.New("missing")
	}
	for _, v := range r.Vendors() {
		if v.Available != (v.ID == "claude") {
			t.Errorf("vendor %s available = %v", v.ID, v.Available)
		}
	}
}
