package agent

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

func rfReadRecords(t *testing.T, path string) []RepeatFailureRecord {
	t.Helper()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, invalid, err := readRepeatFailureRecords(f)
	if err != nil || invalid != 0 {
		t.Fatalf("reading %s: %d invalid lines, %v", path, invalid, err)
	}
	return recs
}

// TestRepeatFailureAgyTurnRecordsRunCommand: with --repeat-failure-record the
// agy-turn runner appends one record for the run_command step agy streamed
// (no exit status in agy's output: DONE means succeeded), and without the
// flag it writes nothing (C14).
func TestRepeatFailureAgyTurnRecordsRunCommand(t *testing.T) {
	fake := newFakeAgy(t)
	dir := t.TempDir()
	prompt := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(prompt, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "records.jsonl")

	if code, _, stderr := runAgyTurnForTest(t, "--agy", fake.binary, "--prompt-file", prompt); code != 0 {
		t.Fatalf("turn without the flag: exit %d, stderr %s", code, stderr)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("check off: the runner wrote a record file (err %v)", err)
	}

	t.Setenv("HIVE_AGENT", "scanner")
	if code, _, stderr := runAgyTurnForTest(t, "--agy", fake.binary, "--prompt-file", prompt, "--repeat-failure-record", record); code != 0 {
		t.Fatalf("turn with the flag: exit %d, stderr %s", code, stderr)
	}
	recs := rfReadRecords(t, record)
	if len(recs) != 1 {
		t.Fatalf("records = %+v, want one for the run_command step", recs)
	}
	rec := recs[0]
	if rec.RawCommand != "gh pr list" || rec.Failed || rec.ExitCode != nil || rec.Route != RepeatFailureRouteAgyTurn ||
		rec.Backend != "agy" || rec.Agent != "scanner" || rec.Time.IsZero() {
		t.Fatalf("record = %+v", rec)
	}
}

// TestRepeatFailureAgyRunCommandSteps covers the step rules: the command of a
// final update that omits it comes from the step's ACTIVE update, an exit
// status stated in the output wins, a failed state is a failure, other tools
// and repeated final updates add nothing.
func TestRepeatFailureAgyRunCommandSteps(t *testing.T) {
	record := filepath.Join(t.TempDir(), "records.jsonl")
	r := newAgyTurnRenderer(io.Discard, time.Now())
	r.recordFile = record
	for _, line := range []string{
		`{"event":"step_update","step_update":{"step_index":1,"state":"ACTIVE","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"CommandLine":"ls /nonexistent"}}}}`,
		`{"event":"step_update","step_update":{"step_index":1,"state":"DONE","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","output":"ls: cannot access '/nonexistent'\nExit code: 2\n"}}}`,
		`{"event":"step_update","step_update":{"step_index":1,"state":"DONE","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","output":"Exit code: 2"}}}`,
		`{"event":"step_update","step_update":{"step_index":2,"state":"ACTIVE","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"CommandLine":"make test"}}}}`,
		`{"event":"step_update","step_update":{"step_index":2,"state":"ERROR","step_type":"tool","tool_name":"run_command"}}`,
		`{"event":"step_update","step_update":{"step_index":3,"state":"DONE","step_type":"tool","tool_name":"view_file","tool_info":{"name":"view_file","parameters":{"CommandLine":"not a command"}}}}`,
		`{"event":"step_update","step_update":{"step_index":4,"state":"ACTIVE","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"CommandLine":"sleep 600"}}}}`,
	} {
		r.handleLine([]byte(line))
	}
	recs := rfReadRecords(t, record)
	if len(recs) != 2 {
		t.Fatalf("records = %+v, want ls and make only", recs)
	}
	if rec := recs[0]; rec.RawCommand != "ls /nonexistent" || rec.ExitCode == nil || *rec.ExitCode != 2 || !rec.Failed {
		t.Errorf("ls: %+v, want exit 2 from the output", rec)
	}
	if rec := recs[1]; rec.RawCommand != "make test" || rec.ExitCode != nil || !rec.Failed {
		t.Errorf("make: %+v, want failed without an exit code", rec)
	}
}

// TestRepeatFailureAgyKickPassesRecordFile: a headless kick passes the record
// file only while the check is on, read at each kick.
func TestRepeatFailureAgyKickPassesRecordFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "agy", Model: "m"}}, discardLogger(), ProjectContext{})
	ap := &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "agy", Model: "m"}}
	on := false
	m.SetRepeatFailureEnabledResolver(func() bool { return on })
	if got := m.repeatFailureAgyRecordFile(ap); got != "" {
		t.Fatalf("check off: record file = %q, want none", got)
	}
	if cmd := agyHeadlessTurnShellCommand("hive", "agy", "", "", "/p", false, m.repeatFailureAgyRecordFile(ap)); strings.Contains(cmd, "--repeat-failure-record") {
		t.Fatalf("check off: kick passes the record flag: %q", cmd)
	}
	on = true
	want := RepeatFailureRecordPath(home)
	if got := m.repeatFailureAgyRecordFile(ap); got != want {
		t.Fatalf("check on: record file = %q, want %q", got, want)
	}
	cmd := agyHeadlessTurnShellCommand("hive", "agy", "", "", "/p", false, m.repeatFailureAgyRecordFile(ap))
	if !strings.Contains(cmd, "--repeat-failure-record "+shellQuote(want)) {
		t.Fatalf("check on: kick lacks the record flag: %q", cmd)
	}

	t.Setenv(agyLaunchModeEnv, agyLaunchModeHeadless)
	m.provisionRepeatFailureAgyRecordDir(ap, "agy")
	info, err := os.Stat(filepath.Dir(want))
	if err != nil || info.Mode().Perm() != repeatFailureRecordDirMode {
		t.Fatalf("record dir = %v, %v; want mode %o", info, err, repeatFailureRecordDirMode)
	}
}
