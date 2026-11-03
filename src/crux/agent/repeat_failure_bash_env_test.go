package agent

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

// requireBash skips when the host has no /bin/bash to run the route with.
func requireBash(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(repeatFailureShell); err != nil {
		t.Skipf("%s not available: %v", repeatFailureShell, err)
	}
}

type rfFixture struct {
	dir    string
	record string
	script string
}

// newRFFixture writes the rendered script for agent "scanner" on claude into
// a temp dir, with the record file in a 0700 directory beside it.
func newRFFixture(t *testing.T, chain string) rfFixture {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "rf", "records.jsonl")
	if err := os.MkdirAll(filepath.Dir(record), 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, repeatFailureBashEnvName)
	if err := os.WriteFile(script, renderRepeatFailureBashEnv("scanner", "claude", RepeatFailureRouteBashEnv, record, chain), 0o600); err != nil {
		t.Fatal(err)
	}
	return rfFixture{dir: dir, record: record, script: script}
}

type rfResult struct {
	stdout, stderr string
	code           int
}

// runRFBash runs /bin/bash with a clean environment (never the test
// process's, which may itself carry a BASH_ENV or the marker) plus extra.
func runRFBash(t *testing.T, dir string, extra []string, args ...string) rfResult {
	t.Helper()
	cmd := exec.Command(repeatFailureShell, args...)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir}, extra...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("running bash %q: %v", args, err)
	}
	return rfResult{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

func readRFRecords(t *testing.T, path string) []RepeatFailureRecord {
	t.Helper()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, invalid, err := readRepeatFailureRecords(f)
	if err != nil || invalid != 0 {
		t.Fatalf("reading records: invalid=%d err=%v", invalid, err)
	}
	return recs
}

func TestRepeatFailureBashEnv_RecordsOnlyTheAgentsCommand(t *testing.T) {
	requireBash(t)
	cases := []struct {
		name     string
		args     []string
		script   string // written to ./inner.sh when set
		wantRaw  string
		wantCode int // -1: any non-zero
	}{
		{name: "failure", args: []string{"-c", "exit 3"}, wantRaw: "exit 3", wantCode: 3},
		{name: "success", args: []string{"-c", "true"}, wantRaw: "true", wantCode: 0},
		{name: "login shell", args: []string{"-lc", "false"}, wantRaw: "false", wantCode: 1},
		{
			name:     "nested bash -c is skipped",
			args:     []string{"-c", `bash -c false; bash -c "bash -c true"; false`},
			wantRaw:  `bash -c false; bash -c "bash -c true"; false`,
			wantCode: 1,
		},
		{
			// C-AC5: a script running 3 failing inner commands is one record.
			name:     "script and its inner commands",
			args:     []string{"-c", "bash ./inner.sh"},
			script:   "bash -c false\nbash -c 'ls /nonexistent'\n/bin/bash -c 'exit 7'\n",
			wantRaw:  "bash ./inner.sh",
			wantCode: 7,
		},
		{
			// C-AC5/C8: Codex's wrapper ends in exec /bin/bash -c '<cmd>'.
			name:     "exec wrapper records the inner command once",
			args:     []string{"-c", "true; exec /bin/bash -c 'ls /nonexistent'"},
			wantRaw:  "ls /nonexistent",
			wantCode: -1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRFFixture(t, "")
			if tc.script != "" {
				if err := os.WriteFile(filepath.Join(fx.dir, "inner.sh"), []byte(tc.script), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			res := runRFBash(t, fx.dir, []string{"BASH_ENV=" + fx.script}, tc.args...)
			recs := readRFRecords(t, fx.record)
			if len(recs) != 1 {
				t.Fatalf("got %d records, want exactly 1: %+v", len(recs), recs)
			}
			rec := recs[0]
			if rec.RawCommand != tc.wantRaw {
				t.Errorf("raw_command = %q, want %q", rec.RawCommand, tc.wantRaw)
			}
			if rec.ExitCode == nil || *rec.ExitCode != res.code {
				t.Errorf("exit_code = %v, want the command's status %d", rec.ExitCode, res.code)
			}
			if tc.wantCode >= 0 && res.code != tc.wantCode {
				t.Errorf("command exited %d, want %d", res.code, tc.wantCode)
			}
			if rec.Failed != (res.code != 0) {
				t.Errorf("failed = %v for exit %d", rec.Failed, res.code)
			}
			if rec.Agent != "scanner" || rec.Backend != "claude" || rec.Route != RepeatFailureRouteBashEnv || rec.Time.IsZero() {
				t.Errorf("record fields = %+v", rec)
			}
		})
	}
}

func TestRepeatFailureBashEnv_NoRecordWithoutExecutionString(t *testing.T) {
	requireBash(t)
	fx := newRFFixture(t, "")
	if err := os.WriteFile(filepath.Join(fx.dir, "s.sh"), []byte("false\nbash ./t.sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.dir, "t.sh"), []byte("exit 4\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	runRFBash(t, fx.dir, []string{"BASH_ENV=" + fx.script}, "./s.sh")
	if recs := readRFRecords(t, fx.record); len(recs) != 0 {
		t.Fatalf("a script run without -c recorded %+v", recs)
	}
}

func TestRepeatFailureBashEnv_ForeignMarkerSkips(t *testing.T) {
	requireBash(t)
	fx := newRFFixture(t, "")
	runRFBash(t, fx.dir, []string{"BASH_ENV=" + fx.script, repeatFailureMarkerEnv + "=1"}, "-c", "false")
	if recs := readRFRecords(t, fx.record); len(recs) != 0 {
		t.Fatalf("a shell nested under another recording shell recorded %+v", recs)
	}
}

func TestRepeatFailureBashEnv_EscapesCommand(t *testing.T) {
	requireBash(t)
	fx := newRFFixture(t, "")
	cmd := "echo 'q\" b\\s' >/dev/null\nprintf '%s\\n' \"t\there\" héllo ✓ >/dev/null\r"
	runRFBash(t, fx.dir, []string{"BASH_ENV=" + fx.script}, "-c", cmd)
	recs := readRFRecords(t, fx.record)
	if len(recs) != 1 || recs[0].RawCommand != cmd {
		t.Fatalf("records = %+v, want one with raw_command %q", recs, cmd)
	}
}

func TestRepeatFailureBashEnv_CutsLongCommandToOneLine(t *testing.T) {
	requireBash(t)
	fx := newRFFixture(t, "")
	cmd := ": " + strings.Repeat(`a"\`, 5000)
	runRFBash(t, fx.dir, []string{"BASH_ENV=" + fx.script}, "-c", cmd)
	data, err := os.ReadFile(fx.record)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > RepeatFailureRecordMaxLine || bytes.Count(data, []byte("\n")) != 1 {
		t.Fatalf("record is %d bytes with %d newlines, want one line of at most %d", len(data), bytes.Count(data, []byte("\n")), RepeatFailureRecordMaxLine)
	}
	recs := readRFRecords(t, fx.record)
	if len(recs) != 1 || !recs[0].Truncated || !strings.HasPrefix(cmd, recs[0].RawCommand) || len(recs[0].RawCommand) < 1000 {
		t.Fatalf("records = %d, truncated=%v, raw prefix ok=%v", len(recs), len(recs) == 1 && recs[0].Truncated, len(recs) == 1 && strings.HasPrefix(cmd, recs[0].RawCommand))
	}
}

// TestRepeatFailureBashEnv_NeverChangesTheCommand is C-AC14: stdout, stderr
// and exit status are byte-identical with the route on and off, including
// when the script is missing or the record file cannot be written.
func TestRepeatFailureBashEnv_NeverChangesTheCommand(t *testing.T) {
	requireBash(t)
	commands := []struct {
		name  string
		extra []string
		args  []string
	}{
		{name: "success", args: []string{"-c", "echo out; echo err >&2"}},
		{name: "failure", args: []string{"-c", "echo out; echo err >&2; exit 4"}},
		{name: "strict script", args: []string{"-c", "set -euo pipefail\ntrue | false\necho unreachable"}},
		{name: "strict unset", args: []string{"-c", "set -euo pipefail\necho \"$LINENO ${UNSET_X}\""}},
		{name: "strict flags", args: []string{"-euo", "pipefail", "-c", "echo $-; false | true; ls /nonexistent; echo unreachable"}},
		{name: "exported SHELLOPTS", extra: []string{"SHELLOPTS=errexit:nounset"}, args: []string{"-c", "echo \"$SHELLOPTS\"; false; echo unreachable"}},
		{name: "own EXIT trap", args: []string{"-c", "trap 'echo mine; exit 6' EXIT; false"}},
		{
			name: "lists its state",
			args: []string{"-c", "set -o; shopt; declare -F; printf '%q\\n' \"$IFS\"; trap -p ERR; " +
				"env | grep -v -e '^BASH_ENV=' -e '^" + repeatFailureMarkerEnv + "=' | sort"},
		},
		{name: "large output", args: []string{"-c", "head -c 300000 /dev/zero | tr '\\0' a; head -c 200000 /dev/zero | tr '\\0' b >&2; exit 9"}},
	}
	for _, tc := range commands {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRFFixture(t, "")
			want := runRFBash(t, fx.dir, tc.extra, tc.args...)
			unwritable := newRFFixture(t, "")
			if err := os.WriteFile(unwritable.script, renderRepeatFailureBashEnv("scanner", "claude", RepeatFailureRouteBashEnv, filepath.Join(unwritable.dir, "missing", "records.jsonl"), ""), 0o600); err != nil {
				t.Fatal(err)
			}
			hooks := map[string]rfFixture{
				"route on":            fx,
				"script missing":      {dir: fx.dir, script: filepath.Join(fx.dir, "absent.sh")},
				"record not writable": unwritable,
			}
			for hook, h := range hooks {
				got := runRFBash(t, fx.dir, append(append([]string(nil), tc.extra...), "BASH_ENV="+h.script), tc.args...)
				if got.code != want.code {
					t.Errorf("%s: exit %d, want %d", hook, got.code, want.code)
				}
				if got.stdout != want.stdout {
					t.Errorf("%s: stdout differs:\n got %.300q\nwant %.300q", hook, got.stdout, want.stdout)
				}
				if got.stderr != want.stderr {
					t.Errorf("%s: stderr differs:\n got %.300q\nwant %.300q", hook, got.stderr, want.stderr)
				}
			}
			if tc.name == "own EXIT trap" {
				return
			}
			if recs := readRFRecords(t, fx.record); len(recs) != 1 {
				t.Errorf("route on: got %d records, want 1", len(recs))
			}
		})
	}
}

// TestRepeatFailureBashEnv_ChainsExistingBashEnv is C-AC16: an existing
// BASH_ENV still runs and a record is still written.
func TestRepeatFailureBashEnv_ChainsExistingBashEnv(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	mark := filepath.Join(dir, "chained")
	chain := filepath.Join(dir, "user_env.sh")
	if err := os.WriteFile(chain, []byte("printf ran >> "+shellQuote(mark)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fx := newRFFixture(t, chain)
	res := runRFBash(t, fx.dir, []string{"BASH_ENV=" + fx.script}, "-c", "exit 2")
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2", res.code)
	}
	if data, err := os.ReadFile(mark); err != nil || string(data) != "ran" {
		t.Fatalf("chained BASH_ENV did not run once: %q, %v", data, err)
	}
	if recs := readRFRecords(t, fx.record); len(recs) != 1 || recs[0].RawCommand != "exit 2" {
		t.Fatalf("records = %+v, want one for %q", recs, "exit 2")
	}
}

func TestRepeatFailureChainedBashEnv(t *testing.T) {
	script := "/home/a/.hive/repeat-failure/" + repeatFailureBashEnvName
	t.Setenv("BASH_ENV", "")
	if got := repeatFailureChainedBashEnv(script); got != "" {
		t.Errorf("no BASH_ENV: chain = %q", got)
	}
	t.Setenv("BASH_ENV", script)
	if got := repeatFailureChainedBashEnv(script); got != "" {
		t.Errorf("Hive's own script must not chain itself, got %q", got)
	}
	t.Setenv("BASH_ENV", "/etc/user_env.sh")
	if got := repeatFailureChainedBashEnv(script); got != "/etc/user_env.sh" {
		t.Errorf("chain = %q, want /etc/user_env.sh", got)
	}
}

// TestRepeatFailureBashEnv_Overhead is C-AC15: the median time the route
// adds to 200 `true` commands stays under 5 ms.
func TestRepeatFailureBashEnv_Overhead(t *testing.T) {
	requireBash(t)
	if testing.Short() {
		t.Skip("benchmark-style test")
	}
	fx := newRFFixture(t, "")
	run := func(env []string) time.Duration {
		cmd := exec.Command(repeatFailureShell, "-c", "true")
		cmd.Dir = fx.dir
		cmd.Env = env
		start := time.Now()
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
		return time.Since(start)
	}
	base := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + fx.dir}
	hooked := append(slices.Clone(base), "BASH_ENV="+fx.script)
	const n = 200
	added := make([]time.Duration, 0, n)
	for range n {
		off := run(base)
		on := run(hooked)
		added = append(added, on-off)
	}
	sort.Slice(added, func(i, j int) bool { return added[i] < added[j] })
	if median := added[n/2]; median >= 5*time.Millisecond {
		t.Fatalf("median added time per command = %v, want < 5ms", median)
	}
	if recs := readRFRecords(t, fx.record); len(recs) != n {
		t.Fatalf("got %d records, want %d", len(recs), n)
	}
}

func rfBool(b bool) *bool { return &b }

func rfEnvValue(pairs []agentEnvPair, key string) (string, bool) {
	for _, p := range pairs {
		if p.Key == key {
			return p.Value, true
		}
	}
	return "", false
}

func TestAgentEnvPairs_RepeatFailureRoute(t *testing.T) {
	cases := []struct {
		name    string
		enabled *bool
		backend string
		want    bool
	}{
		{name: "no resolver", backend: "claude"},
		{name: "off", enabled: rfBool(false), backend: "claude"},
		{name: "on claude", enabled: rfBool(true), backend: "claude", want: true},
		{name: "on bob", enabled: rfBool(true), backend: "bob", want: true},
		{name: "on codex", enabled: rfBool(true), backend: "codex", want: true},
		{name: "on inference via claude", enabled: rfBool(true), backend: "vllm", want: true},
		{name: "on copilot has its own route", enabled: rfBool(true), backend: "copilot"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(map[string]config.AgentConfig{
				"scanner": {Backend: tc.backend, Model: "m"},
			}, discardLogger(), ProjectContext{})
			if tc.enabled != nil {
				on := *tc.enabled
				m.SetRepeatFailureEnabledResolver(func() bool { return on })
			}
			ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: tc.backend, Model: "m"}}
			pairs := m.agentEnvPairs(ap)
			bashEnv, hasBashEnv := rfEnvValue(pairs, "BASH_ENV")
			shell, hasShell := rfEnvValue(pairs, "SHELL")
			if !tc.want {
				if hasBashEnv || hasShell {
					t.Fatalf("route off must add nothing, got BASH_ENV=%q SHELL=%q", bashEnv, shell)
				}
				return
			}
			if want := repeatFailureBashEnvPath(AgentHome("scanner", 1001, tc.backend)); bashEnv != want {
				t.Errorf("BASH_ENV = %q, want %q", bashEnv, want)
			}
			if shell != repeatFailureShell {
				t.Errorf("SHELL = %q, want %q", shell, repeatFailureShell)
			}
		})
	}
}

// TestProvisionRepeatFailureBashEnv covers C14/C-AC12 for the files: on
// writes the script (chaining an existing BASH_ENV), off removes it at the
// next launch, and a file Hive did not write is never removed.
func TestProvisionRepeatFailureBashEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASH_ENV", "/etc/user_env.sh")
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Model: "m"},
	}, discardLogger(), ProjectContext{})
	on := true
	m.SetRepeatFailureEnabledResolver(func() bool { return on })
	ap := &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "claude", Model: "m"}}
	script := repeatFailureBashEnvPath(home)

	m.provisionRepeatFailureBashEnv(ap, "claude")
	data, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("script not written: %v", err)
	}
	if !strings.HasPrefix(string(data), repeatFailureBashEnvHeader) || !strings.Contains(string(data), "'/etc/user_env.sh'") {
		t.Errorf("script lacks the header or the chained BASH_ENV:\n%s", data)
	}
	if !strings.Contains(string(data), shellQuote(RepeatFailureRecordPath(home))[1:]) {
		t.Errorf("script does not name the record file")
	}
	info, err := os.Stat(filepath.Dir(script))
	if err != nil || info.Mode().Perm() != repeatFailureRecordDirMode {
		t.Errorf("record dir mode = %v, %v; want %o", info, err, repeatFailureRecordDirMode)
	}

	on = false
	m.provisionRepeatFailureBashEnv(ap, "claude")
	if _, err := os.Stat(script); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("switching off must remove the script at the next launch, stat err = %v", err)
	}
	if _, ok := rfEnvValue(m.agentEnvPairs(ap), "BASH_ENV"); ok {
		t.Fatal("switching off must drop BASH_ENV from the launch env")
	}

	if err := os.WriteFile(script, []byte("# someone else's file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.provisionRepeatFailureBashEnv(ap, "claude")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("a file Hive did not write must be left alone: %v", err)
	}
}
