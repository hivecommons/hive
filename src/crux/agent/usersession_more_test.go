package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
)

const (
	testResumeUUID     = "123e4567-e89b-12d3-a456-426614174000"
	testFakeTmuxScript = "#!/bin/sh\necho \"args=$*\"\ncat\ncase \"$3\" in fail) echo boom; exit 3;; esac\n"
	testScrubScript    = "agent-env-scrub.sh"
)

// sessionLifecycleTmux fakes a tmux server that only "exists" once new-session
// ran, and fails any call whose argv contains failOn.
func sessionLifecycleTmux(failOn string) *fakeTmux {
	created := false
	f := &fakeTmux{}
	f.handler = func(args []string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if failOn != "" && strings.Contains(joined, failOn) {
			return []byte("simulated failure"), errors.New("tmux failed")
		}
		switch {
		case args[0] == "display-message":
			if !created {
				return nil, errors.New("no session")
			}
			return []byte("0 0 1\n"), nil
		case strings.Contains(joined, "new-session"):
			created = true
		}
		return nil, nil
	}
	return f
}

func TestUserSessionStartStepFailuresAreReportedAndCleanedUp(t *testing.T) {
	cases := []struct {
		name, failOn, wantErr string
		wantKill              bool
	}{
		{"create", "remain-on-exit", "creating user session tmux server", false},
		{"environment", "set-environment", "setting session environment", true},
		{"respawn", "respawn-pane", "launching claude", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := sessionLifecycleTmux(tc.failOn)
			r := newTestSessionRuntime(t, f, nil)
			err := r.Start(crux.UserSessionSpec{ID: testSessionID, Vendor: "claude"})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Start error = %v, want it to mention %q", err, tc.wantErr)
			}
			killed := false
			for _, v := range f.verbs() {
				if v == "kill-session" {
					killed = true
				}
			}
			if killed != tc.wantKill {
				t.Errorf("kill-session issued = %v, want %v (calls %v)", killed, tc.wantKill, f.verbs())
			}
		})
	}
}

func TestUserSessionStartReplacesExistingSession(t *testing.T) {
	f := &fakeTmux{handler: func(args []string) ([]byte, error) {
		if args[0] == "display-message" {
			return []byte("1 0 1\n"), nil
		}
		return nil, nil
	}}
	r := newTestSessionRuntime(t, f, nil)
	if err := r.Start(crux.UserSessionSpec{ID: testSessionID, Vendor: "claude"}); err != nil {
		t.Fatal(err)
	}
	verbs := f.verbs()
	killAt, respawnAt := -1, -1
	for i, v := range verbs {
		switch v {
		case "kill-session":
			killAt = i
		case "respawn-pane":
			respawnAt = i
		}
	}
	if killAt < 0 || respawnAt < 0 || killAt > respawnAt {
		t.Errorf("dead session was not killed before relaunch: %v", verbs)
	}
}

func TestUserSessionStartSurfacesKillFailure(t *testing.T) {
	f := &fakeTmux{handler: func(args []string) ([]byte, error) {
		switch args[0] {
		case "display-message":
			return []byte("1 0 1\n"), nil
		case "kill-session":
			return []byte("server wedged"), errors.New("exit 1")
		}
		return nil, nil
	}}
	r := newTestSessionRuntime(t, f, nil)
	err := r.Start(crux.UserSessionSpec{ID: testSessionID, Vendor: "claude"})
	if err == nil || !strings.Contains(err.Error(), "killing user session") || !strings.Contains(err.Error(), "server wedged") {
		t.Fatalf("Start error = %v, want kill failure with tmux output", err)
	}
	for _, v := range f.verbs() {
		if v == "respawn-pane" {
			t.Error("CLI launched despite the old session not being killed")
		}
	}
}

func TestUserSessionStartFailsWhenHomeCannotBeCreated(t *testing.T) {
	f := sessionLifecycleTmux("")
	r := newTestSessionRuntime(t, f, nil)
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sharedAgentHome = filepath.Join(blocker, "home")
	err := r.Start(crux.UserSessionSpec{ID: testSessionID, Vendor: "claude"})
	if err == nil || !strings.Contains(err.Error(), "creating session dir") {
		t.Fatalf("Start error = %v, want session dir failure", err)
	}
	for _, v := range f.verbs() {
		if v == "respawn-pane" || v == "set-option" {
			t.Errorf("tmux %s ran after home setup failed", v)
		}
	}
}

func TestUserSessionStartRequiresInstalledBinary(t *testing.T) {
	f := &fakeTmux{}
	r := newTestSessionRuntime(t, f, nil)
	r.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	err := r.Start(crux.UserSessionSpec{ID: testSessionID, Vendor: "claude"})
	if err == nil || !strings.Contains(err.Error(), "claude is not installed") {
		t.Fatalf("Start error = %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("tmux reached for a missing binary: %v", f.verbs())
	}
}

func TestUserSessionLaunchArgv(t *testing.T) {
	r := newTestSessionRuntime(t, &fakeTmux{}, nil)
	cases := []struct {
		name    string
		spec    crux.UserSessionSpec
		want    []string
		wantErr string
	}{
		{"bare", crux.UserSessionSpec{Vendor: "codex"}, []string{"/usr/bin/codex"}, ""},
		{"resume", crux.UserSessionSpec{Vendor: "codex", ResumeCommand: "codex resume abc-1"}, []string{"/usr/bin/codex", "resume", "abc-1"}, ""},
		{"unknown vendor", crux.UserSessionSpec{Vendor: "nope"}, nil, `unsupported vendor "nope"`},
		{"metacharacters", crux.UserSessionSpec{Vendor: "claude", ResumeCommand: "claude --resume a|b"}, nil, "resume command rejected"},
		{"leading space", crux.UserSessionSpec{Vendor: "claude", ResumeCommand: " claude --resume a"}, nil, "resume command rejected"},
		{"wrong binary", crux.UserSessionSpec{Vendor: "claude", ResumeCommand: "copilot --resume a"}, nil, "does not belong to claude"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.launchArgv(tc.spec)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("argv = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestUserSessionEnvironmentCommandsStripThenSet(t *testing.T) {
	r := newTestSessionRuntime(t, &fakeTmux{}, nil)
	session := UserSessionTmuxName(testSessionID)
	args := r.environmentCommands(testSessionID, session)
	if len(args) == 0 || args[len(args)-1] == ";" {
		t.Fatalf("command list must not end with a separator: tail %v", args[len(args)-2:])
	}
	joined := strings.Join(args, " ")
	lastStrip := strings.LastIndex(joined, "-r ")
	firstSet := strings.Index(joined, "set-environment -t "+session+" HOME ")
	if firstSet < 0 || lastStrip > firstSet {
		t.Errorf("credentials must be removed before per-session vars are set:\n%s", joined)
	}
	for _, name := range userSessionStripNames() {
		if !strings.Contains(joined, "-r "+name+" ;") {
			t.Errorf("strip of %s missing", name)
		}
	}
	if !strings.Contains(joined, "HIVE_SESSION_ID "+testSessionID) {
		t.Error("HIVE_SESSION_ID not set")
	}
}

func TestUserSessionScrubScriptDiscovery(t *testing.T) {
	if _, err := os.Stat("/usr/local/bin/" + testScrubScript); err == nil {
		t.Skip("host has a system-wide scrub script")
	}
	r := newTestSessionRuntime(t, &fakeTmux{}, nil)
	binDir := t.TempDir()
	t.Setenv("HIVE_BIN_DIR", binDir)
	if got := r.scrubScript(); got != "" {
		t.Fatalf("scrubScript() = %q with no script present", got)
	}
	// A directory of that name is not a script.
	if err := os.Mkdir(filepath.Join(binDir, testScrubScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := r.scrubScript(); got != "" {
		t.Fatalf("scrubScript() accepted a directory: %q", got)
	}
	if err := os.Remove(filepath.Join(binDir, testScrubScript)); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(binDir, testScrubScript)
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := r.scrubScript(); got != script {
		t.Fatalf("scrubScript() = %q, want %q", got, script)
	}
	joined := strings.Join(r.environmentCommands(testSessionID, UserSessionTmuxName(testSessionID)), " ")
	if !strings.Contains(joined, "BASH_ENV "+script) || !strings.Contains(joined, "ENV "+script) {
		t.Errorf("scrub script not wired into BASH_ENV/ENV: %s", joined)
	}
}

func TestUserSessionEnvPairs(t *testing.T) {
	newTestSessionRuntime(t, &fakeTmux{}, nil)
	toMap := func(pairs [][2]string) map[string]string {
		m := map[string]string{}
		for _, p := range pairs {
			m[p[0]] = p[1]
		}
		return m
	}
	plain := toMap(userSessionEnvPairs(testSessionID, ""))
	if plain["HOME"] != userSessionHome(testSessionID) || plain["CODEX_HOME"] != userSessionCodexHome(testSessionID) ||
		plain["XDG_CONFIG_HOME"] != filepath.Join(userSessionHome(testSessionID), ".config") ||
		plain["HIVE_SESSION_ID"] != testSessionID {
		t.Errorf("pairs = %v", plain)
	}
	if _, ok := plain["BASH_ENV"]; ok {
		t.Error("BASH_ENV set without a scrub script")
	}
	scrubbed := toMap(userSessionEnvPairs(testSessionID, "/opt/scrub.sh"))
	if scrubbed["BASH_ENV"] != "/opt/scrub.sh" || scrubbed["ENV"] != "/opt/scrub.sh" {
		t.Errorf("scrub pairs = %v", scrubbed)
	}
}

func TestUserSessionPathHelpers(t *testing.T) {
	newTestSessionRuntime(t, &fakeTmux{}, nil)
	t.Setenv(userSessionsDirEnv, "")
	if got := userSessionsDir(); got != defaultUserSessionsDir {
		t.Errorf("default sessions dir = %q", got)
	}
	root := t.TempDir()
	t.Setenv(userSessionsDirEnv, root)
	if got := userSessionsDir(); got != root {
		t.Errorf("sessions dir override = %q", got)
	}
	r := &UserSessionRuntime{}
	want := filepath.Join(root, testSessionID, userSessionWorkDirName)
	if got := r.WorkDir(testSessionID); got != want {
		t.Errorf("WorkDir = %q, want %q", got, want)
	}
	if got := userSessionCodexHome(testSessionID); got != filepath.Join(userSessionHome(testSessionID), ".codex") {
		t.Errorf("codex home = %q", got)
	}
	if !strings.HasPrefix(userSessionHome(testSessionID), sharedAgentHome) {
		t.Errorf("session home %q outside shared home", userSessionHome(testSessionID))
	}
}

func TestCheckUserSessionID(t *testing.T) {
	for _, id := range []string{testSessionID, strings.Repeat("a", 8), strings.Repeat("f", 32)} {
		if err := checkUserSessionID(id); err != nil {
			t.Errorf("checkUserSessionID(%q) = %v", id, err)
		}
	}
	for _, id := range []string{"", "abc", strings.Repeat("a", 33), "ABCDEF0123", "0123456789ab/..", "0123456789zz", "0123456789ab\n"} {
		if err := checkUserSessionID(id); err == nil {
			t.Errorf("checkUserSessionID(%q) accepted", id)
		}
	}
}

func TestUserSessionSupportsResume(t *testing.T) {
	r := &UserSessionRuntime{}
	if !r.SupportsResume("claude") || !r.SupportsResume("codex") {
		t.Error("claude and codex keep resume handles")
	}
	if r.SupportsResume("") || r.SupportsResume("bash") {
		t.Error("unknown vendors must not report resume support")
	}
}

func TestUserSessionResumeInfo(t *testing.T) {
	r := newTestSessionRuntime(t, &fakeTmux{}, nil)
	if _, ok := r.ResumeInfo("../x", "claude"); ok {
		t.Error("invalid id produced resume info")
	}
	if _, ok := r.ResumeInfo(testSessionID, "bash"); ok {
		t.Error("unsupported vendor produced resume info")
	}
	if _, ok := r.ResumeInfo(testSessionID, "claude"); ok {
		t.Error("session without transcripts produced resume info")
	}
	dir := filepath.Join(userSessionHome(testSessionID), ".claude", "projects", "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, testResumeUUID+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := r.ResumeInfo(testSessionID, "claude")
	if !ok || got.SessionID != testResumeUUID || got.Command != "claude --resume "+testResumeUUID {
		t.Fatalf("ResumeInfo = %+v, %v", got, ok)
	}
	// Another session's transcripts are invisible: homes are per session.
	if _, ok := r.ResumeInfo("fedcba987654", "claude"); ok {
		t.Error("resume info leaked across session homes")
	}
}

func TestUserSessionBaseEnvProxyAndMalformedEntries(t *testing.T) {
	env := userSessionBaseEnv([]string{
		"PATH=/usr/bin",
		"NOEQUALS",
		"=emptykey",
		"https_proxy=http://proxy.example:3128",
		"HTTP_PROXY=http://user:pw@proxy.example:3128",
		"NO_PROXY=localhost,10.0.0.0/8",
		"CUSTOM_VENDOR_KEY=abc",
		"LANG=C.UTF-8",
	}, " ,CUSTOM_VENDOR_KEY, ")
	joined := strings.Join(env, "\n")
	for _, want := range []string{"PATH=/usr/bin", "https_proxy=http://proxy.example:3128", "NO_PROXY=localhost,10.0.0.0/8", "CUSTOM_VENDOR_KEY=abc", "LANG=C.UTF-8", "TERM=" + userSessionDefaultTerm} {
		if !strings.Contains(joined, want) {
			t.Errorf("env lacks %q:\n%s", want, joined)
		}
	}
	for _, bad := range []string{"NOEQUALS", "emptykey", "pw@"} {
		if strings.Contains(joined, bad) {
			t.Errorf("env contains %q:\n%s", bad, joined)
		}
	}
	if env[len(env)-1] != "TERM="+userSessionDefaultTerm {
		t.Errorf("TERM must be last, got %v", env)
	}
}

func TestProxyValueHasNoUserinfo(t *testing.T) {
	cases := map[string]bool{
		"":                              true,
		"localhost,10.0.0.0/8":          true,
		"http://proxy.example:3128":     true,
		"http://user@proxy.example":     false,
		"http://user:pw@proxy.example":  false,
		"user:pw@proxy.example:3128":    false,
		"http://u@[::1":                 false,
		"http://proxy.example/path?a@b": true,
	}
	for in, want := range cases {
		if got := proxyValueHasNoUserinfo(in); got != want {
			t.Errorf("proxyValueHasNoUserinfo(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestBridgeSessionEntry(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "shared")
	link := filepath.Join(dir, "link")

	bridgeSessionEntry(link, target)
	if _, err := os.Lstat(link); err == nil {
		t.Fatal("link created for a missing target")
	}
	if err := os.WriteFile(target, []byte("login"), 0o600); err != nil {
		t.Fatal(err)
	}
	bridgeSessionEntry(link, target)
	if got, err := os.Readlink(link); err != nil || got != target {
		t.Fatalf("Readlink = %q, %v", got, err)
	}
	// An existing entry is left alone, even when it points elsewhere.
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	bridgeSessionEntry(link, target)
	if got, _ := os.Readlink(link); got != other {
		t.Errorf("existing link overwritten: %q", got)
	}
}

func TestSetupUserSessionHomeLayoutAndIdempotence(t *testing.T) {
	newTestSessionRuntime(t, &fakeTmux{}, nil)
	shared := filepath.Join(sharedAgentHome, ".config", "goose")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := setupUserSessionHome(testSessionID); err != nil {
			t.Fatalf("setup #%d: %v", i, err)
		}
	}
	home := userSessionHome(testSessionID)
	for _, d := range []string{home, filepath.Join(home, ".config"), agentXDGDataHome(home), agentXDGStateHome(home), userSessionCodexHome(testSessionID), userSessionWorkDir(testSessionID)} {
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			t.Errorf("%s not created: %v", d, err)
		}
	}
	if got, err := os.Readlink(filepath.Join(home, ".config", "goose")); err != nil || got != shared {
		t.Errorf("goose bridge = %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".config", "opencode")); err == nil {
		t.Error("bridge created for a vendor that was never signed in")
	}
}

func TestSetupUserSessionHomeFailsWhenVendorDirBlocked(t *testing.T) {
	newTestSessionRuntime(t, &fakeTmux{}, nil)
	home := userSessionHome(testSessionID)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(home, perAgentCLIHomes[0].dir)
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := setupUserSessionHome(testSessionID)
	if err == nil || !strings.Contains(err.Error(), blocked) {
		t.Fatalf("setup error = %v, want failure naming %s", err, blocked)
	}
}

func TestNewUserSessionRuntimeWiresRealDependencies(t *testing.T) {
	r := NewUserSessionRuntime(nil)
	if r.logger == nil || r.run == nil || r.lookPath == nil || r.environ == nil || r.now == nil {
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

func TestExecUserSessionTmux(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(testFakeTmuxScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":/bin:/usr/bin")
	env := []string{"PATH=/bin:/usr/bin"}

	out, err := execUserSessionTmux(context.Background(), env, "typed text", "hive-sess-x", "load-buffer", "-")
	if err != nil {
		t.Fatalf("err = %v, out %q", err, out)
	}
	if !strings.Contains(string(out), "args=-L hive-sess-x load-buffer -") || !strings.Contains(string(out), "typed text") {
		t.Errorf("socket flag or stdin not passed: %q", out)
	}

	out, err = execUserSessionTmux(context.Background(), env, "", "hive-sess-x", "fail")
	if err == nil || !strings.Contains(err.Error(), "tmux fail") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want wrapped failure with tmux output", err)
	}
	if !strings.Contains(string(out), "boom") {
		t.Errorf("output not returned on failure: %q", out)
	}

	t.Setenv("PATH", t.TempDir())
	if _, err := execUserSessionTmux(context.Background(), env, "", "hive-sess-x", "display-message"); err == nil || !strings.Contains(err.Error(), "tmux display-message") {
		t.Errorf("missing binary error = %v", err)
	}
}
