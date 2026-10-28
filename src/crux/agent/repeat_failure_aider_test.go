package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/config"
)

// TestRepeatFailureAiderBashrc_RecordsInteractiveDashC is C1/C3 for aider:
// the per-agent ~/.bashrc keeps the shared rc file and makes the
// `$SHELL -i -c <cmd>` shell aider runs write one aider_bashrc record, while a
// non-interactive shell and the plain interactive pane shell write none, and a
// leftover line with the script gone does nothing (C14).
func TestRepeatFailureAiderBashrc_RecordsInteractiveDashC(t *testing.T) {
	requireBash(t)
	home := t.TempDir()
	shared := filepath.Join(t.TempDir(), ".bashrc")
	if err := os.WriteFile(shared, []byte("export HIVE_RF_SHARED_RC=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	record := RepeatFailureRecordPath(home)
	if err := os.MkdirAll(filepath.Dir(record), 0o700); err != nil {
		t.Fatal(err)
	}
	script := repeatFailureBashEnvPath(home)
	if err := os.WriteFile(script, renderRepeatFailureBashEnv("scanner", "aider", RepeatFailureRouteAiderBashrc, record, ""), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), renderRepeatFailureAiderBashrc(shared, script), 0o644); err != nil {
		t.Fatal(err)
	}

	res := runRFBash(t, home, nil, "-i", "-c", `test -n "$HIVE_RF_SHARED_RC" && ls /no/such/path`)
	if res.code == 0 {
		t.Fatalf("command exit = 0, want non-zero (shared rc sourced and ls failed); stderr=%s", res.stderr)
	}
	recs := readRFRecords(t, record)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(recs), recs)
	}
	rec := recs[0]
	if rec.Route != RepeatFailureRouteAiderBashrc || rec.Backend != "aider" || rec.Agent != "scanner" {
		t.Errorf("record route/backend/agent = %q/%q/%q", rec.Route, rec.Backend, rec.Agent)
	}
	if rec.RawCommand != `test -n "$HIVE_RF_SHARED_RC" && ls /no/such/path` || !rec.Failed || rec.ExitCode == nil || *rec.ExitCode != res.code {
		t.Errorf("record = %+v, want the command with exit %d", rec, res.code)
	}

	runRFBash(t, home, nil, "-c", "false")
	runRFBash(t, home, []string{"PS1=$ "}, "-i", "-s")
	if got := len(readRFRecords(t, record)); got != 1 {
		t.Fatalf("non-interactive or plain interactive shells must not record; got %d records", got)
	}

	if err := os.Remove(script); err != nil {
		t.Fatal(err)
	}
	if res := runRFBash(t, home, nil, "-i", "-c", "exit 3"); res.code != 3 {
		t.Errorf("leftover line changed the exit status: %d", res.code)
	}
	if got := len(readRFRecords(t, record)); got != 1 {
		t.Fatalf("leftover line with the script gone must not record; got %d records", got)
	}
}

func aiderRFManager(t *testing.T, on *bool) *Manager {
	t.Helper()
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "aider", Model: "m"},
	}, discardLogger(), ProjectContext{})
	m.SetRepeatFailureEnabledResolver(func() bool { return *on })
	return m
}

// TestProvisionRepeatFailureAiderBashrc covers C14/C-AC12 for the aider rc
// file: on replaces the .bashrc bridge (never the shared file), off or another
// backend removes it and the bridge comes back, and non-Hive homes or an rc
// file Hive did not write are left alone.
func TestProvisionRepeatFailureAiderBashrc(t *testing.T) {
	shared := withSharedAgentHome(t)
	sharedRC := filepath.Join(shared, ".bashrc")
	if err := os.WriteFile(sharedRC, []byte("# shared\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	on := true
	m := aiderRFManager(t, &on)
	ap := &AgentProcess{Name: "scanner", UID: 4242, Config: config.AgentConfig{Backend: "aider", Model: "m"}}
	home := interactiveHomePath(ap.Name)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	rc := filepath.Join(home, ".bashrc")
	m.bridgeInteractiveHome(ap.Name, home)

	m.provisionRepeatFailureAiderBashrc(ap, home, "aider")
	info, err := os.Lstat(rc)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("on must replace the bridge with a per-agent rc file: %v %v", info, err)
	}
	data, _ := os.ReadFile(rc)
	if !strings.HasPrefix(string(data), repeatFailureAiderBashrcHeader) ||
		!strings.Contains(string(data), shellQuote(sharedRC)) ||
		!strings.Contains(string(data), shellQuote(repeatFailureBashEnvPath(home))) {
		t.Errorf("rc file must source the shared rc and the route script:\n%s", data)
	}
	if got, _ := os.ReadFile(sharedRC); string(got) != "# shared\n" {
		t.Errorf("shared rc file changed: %q", got)
	}
	m.provisionRepeatFailureAiderBashrc(ap, home, "aider")
	if again, _ := os.ReadFile(rc); string(again) != string(data) {
		t.Error("re-provisioning must be idempotent")
	}

	for _, tc := range []struct {
		name    string
		on      bool
		backend string
	}{
		{"off", false, "aider"},
		{"other backend", true, "claude"},
	} {
		on = true
		m.provisionRepeatFailureAiderBashrc(ap, home, "aider")
		on = tc.on
		m.provisionRepeatFailureAiderBashrc(ap, home, tc.backend)
		m.bridgeInteractiveHome(ap.Name, home)
		if target, err := os.Readlink(rc); err != nil || target != sharedRC {
			t.Fatalf("%s: the .bashrc bridge must come back, got %q %v", tc.name, target, err)
		}
	}

	on = true
	if err := os.Remove(rc); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rc, []byte("# the agent's own rc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.provisionRepeatFailureAiderBashrc(ap, home, "aider")
	if got, _ := os.ReadFile(rc); string(got) != "# the agent's own rc\n" {
		t.Errorf("an rc file Hive did not write must be left alone, got %q", got)
	}

	for _, tc := range []struct {
		name  string
		uid   int
		force bool
	}{
		{"contributor local mode (no agent uid)", 0, false},
		{"HIVE_SHARED_AGENT_HOME=1", 4242, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.force {
				t.Setenv("HIVE_SHARED_AGENT_HOME", "1")
			}
			dir := t.TempDir()
			local := &AgentProcess{Name: "scanner", UID: tc.uid, Config: ap.Config}
			m.provisionRepeatFailureAiderBashrc(local, dir, "aider")
			if _, err := os.Lstat(filepath.Join(dir, ".bashrc")); !os.IsNotExist(err) {
				t.Errorf("non-Hive home must get no rc file, lstat err = %v", err)
			}
			if _, ok := rfEnvValue(m.agentEnvPairs(local), "SHELL"); ok {
				t.Error("non-Hive home must get no SHELL from the route")
			}
		})
	}
}

// TestRepeatFailureAiderRoute_LaunchEnvAndScript is C14 for the launch side:
// on, an aider agent gets SHELL=/bin/bash (no BASH_ENV) and the route script
// with the aider_bashrc route; off, neither.
func TestRepeatFailureAiderRoute_LaunchEnvAndScript(t *testing.T) {
	withSharedAgentHome(t)
	on := true
	m := aiderRFManager(t, &on)
	ap := &AgentProcess{Name: "scanner", UID: 4242, Config: config.AgentConfig{Backend: "aider", Model: "m"}}

	pairs := m.agentEnvPairs(ap)
	if shell, ok := rfEnvValue(pairs, "SHELL"); !ok || shell != repeatFailureShell {
		t.Errorf("SHELL = %q (set %v), want %q", shell, ok, repeatFailureShell)
	}
	if v, ok := rfEnvValue(pairs, "BASH_ENV"); ok {
		t.Errorf("aider must not get BASH_ENV, got %q", v)
	}
	content := string(renderRepeatFailureBashEnv("scanner", "aider", RepeatFailureRouteAiderBashrc, "/r/records.jsonl", ""))
	if !strings.Contains(content, `"route":"aider_bashrc"`) || strings.Contains(content, `"route":"bash_env"`) {
		t.Errorf("aider script must carry the aider_bashrc route:\n%s", content)
	}

	on = false
	if _, ok := rfEnvValue(m.agentEnvPairs(ap), "SHELL"); ok {
		t.Error("off must add no SHELL")
	}
}
