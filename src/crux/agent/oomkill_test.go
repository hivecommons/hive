package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func writeOOMFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestReadCgroupOOMKills_V2ThenV1Fallback: the v2 memory.events line wins
// when present; otherwise the v1 oom_control file is parsed; neither → false.
func TestReadCgroupOOMKills_V2ThenV1Fallback(t *testing.T) {
	dir := t.TempDir()
	v2 := filepath.Join(dir, "memory.events")
	v1 := filepath.Join(dir, "memory.oom_control")
	writeOOMFile(t, v1, "oom_kill_disable 0\nunder_oom 0\noom_kill 3\n")
	if n, ok := readCgroupOOMKills([]string{v2, v1}); !ok || n != 3 {
		t.Fatalf("v1 fallback = %d,%v want 3,true", n, ok)
	}
	writeOOMFile(t, v2, "low 0\nhigh 0\nmax 12\noom 1\noom_kill 1\noom_group_kill 0\n")
	if n, ok := readCgroupOOMKills([]string{v2, v1}); !ok || n != 1 {
		t.Fatalf("v2 = %d,%v want 1,true", n, ok)
	}
	if _, ok := readCgroupOOMKills([]string{filepath.Join(dir, "missing")}); ok {
		t.Fatal("missing files must report false")
	}
}

// TestNoteOOMKillsForCrashes_FlagsOnlyRisesAfterBaseline: the first read only
// primes; a crash with an unchanged counter is not OOM; a crash after the
// counter rises flags every crashed agent exactly once.
func TestNoteOOMKillsForCrashes_FlagsOnlyRisesAfterBaseline(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "memory.events")
	writeOOMFile(t, f, "oom_kill 1\n")
	m := &Manager{logger: slog.New(slog.NewTextHandler(os.Stderr, nil)), oomKillFilesOverride: []string{f}}

	m.noteOOMKillsForCrashes([]string{"scanner"})
	if m.CrashOOMSuspected("scanner") {
		t.Fatal("first observation must only prime the baseline")
	}
	m.noteOOMKillsForCrashes([]string{"scanner"})
	if m.CrashOOMSuspected("scanner") {
		t.Fatal("unchanged counter is not an OOM crash")
	}
	writeOOMFile(t, f, "oom_kill 2\n")
	m.noteOOMKillsForCrashes(nil)
	if m.CrashOOMSuspected("scanner") {
		t.Fatal("a rise with no crashed agents flags nobody")
	}
	writeOOMFile(t, f, "oom_kill 3\n")
	m.noteOOMKillsForCrashes([]string{"scanner", "reviewer"})
	if !m.CrashOOMSuspected("scanner") || !m.CrashOOMSuspected("reviewer") {
		t.Fatal("rise during crash must flag both agents")
	}
	if m.CrashOOMSuspected("scanner") {
		t.Fatal("flag is consumed on read")
	}
}
