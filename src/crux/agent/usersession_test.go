package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
)

const testSessionID = "0123456789ab"

type fakeTmuxCall struct {
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

func (f *fakeTmux) run(_ context.Context, env []string, stdin, socket string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, fakeTmuxCall{env: env, stdin: stdin, socket: socket, args: args})
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

func newTestSessionRuntime(t *testing.T, f *fakeTmux, environ []string) *UserSessionRuntime {
	t.Helper()
	oldHome := sharedAgentHome
	sharedAgentHome = filepath.Join(t.TempDir(), "home")
	t.Cleanup(func() { sharedAgentHome = oldHome })
	t.Setenv(userSessionsDirEnv, filepath.Join(t.TempDir(), "sessions"))
	t.Setenv(userSessionEnvPassthroughEnv, "")
	return &UserSessionRuntime{
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		run:      f.run,
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		environ:  func() []string { return environ },
		now:      time.Now,
	}
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
	all = append(all, "DASHBOARD_AUTH_TOKEN", "HIVE_DASHBOARD_TOKEN", "HIVE_TERMINAL_KEY", "HIVE_HUB_SECRET", "HIVE_HEARTBEAT_KEY", "HIVE_TTYD_CREDENTIAL")
	for _, name := range all {
		if !sessionEnvForbidden(name) {
			t.Errorf("%s is not forbidden in sessions", name)
		}
		if !strip[name] {
			t.Errorf("%s is not in the set-environment -r strip list", name)
		}
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
		if err := r.Start(crux.UserSessionSpec{ID: testSessionID, Vendor: v.id}); err != nil {
			t.Fatalf("Start(%s): %v", v.id, err)
		}
		var respawn []string
		var envCmd []string
		for _, c := range f.calls {
			if c.socket != UserSessionTmuxPrefix+testSessionID {
				t.Fatalf("socket = %q, want per-session socket", c.socket)
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
	}
	for _, spec := range cases {
		if err := r.Start(spec); err == nil {
			t.Errorf("Start(%+v) succeeded, want error", spec)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("rejected starts reached tmux: %v", f.verbs())
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
	spec := crux.UserSessionSpec{ID: testSessionID, Vendor: "claude", ResumeCommand: "claude --resume 123e4567-e89b-12d3-a456-426614174000"}
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

func TestUserSessionHomeBridgesVendorLoginsButNotGH(t *testing.T) {
	newTestSessionRuntime(t, &fakeTmux{}, nil)
	shared := sharedAgentHome
	for _, p := range []string{
		filepath.Join(shared, ".claude", ".credentials.json"),
		filepath.Join(shared, ".config", "gh", "hosts.yml"),
		filepath.Join(shared, ".codex", "auth.json"),
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := setupUserSessionHome(testSessionID); err != nil {
		t.Fatal(err)
	}
	home := userSessionHome(testSessionID)
	if _, err := os.Lstat(filepath.Join(home, ".claude", ".credentials.json")); err != nil {
		t.Errorf("claude login not bridged: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(userSessionCodexHome(testSessionID), "auth.json")); err != nil {
		t.Errorf("codex login not bridged: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".config", "gh")); err == nil {
		t.Error("gh config (hosts.yml token) was bridged into a session")
	}
}
