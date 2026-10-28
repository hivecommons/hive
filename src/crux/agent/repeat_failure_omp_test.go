package agent

import (
	"bufio"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/config"
)

// TestRepeatFailureOmpExtensionGolden pins the extension the omp harness
// (testdata/repeat_failure/omp/run.sh) loads to the one Hive renders, so the
// harness evidence is evidence for this code.
func TestRepeatFailureOmpExtensionGolden(t *testing.T) {
	want, err := os.ReadFile("testdata/repeat_failure/omp/omp_extension.ts")
	if err != nil {
		t.Fatal(err)
	}
	if got := renderRepeatFailureOmpExtension("rf-harness", "__RF_RECORD_PATH__"); string(got) != string(want) {
		t.Fatalf("rendered omp extension differs from the harness copy; regenerate testdata/repeat_failure/omp/omp_extension.ts and rerun run.sh.\ngot:\n%s", got)
	}
}

// TestRepeatFailureOmpHarnessEvidence is C-AC1 for omp: the committed harness
// run recorded `omp --version` and exactly one record, written by the real omp
// through Hive's extension, that the collector parses as `ls /nonexistent`
// failing with exit 2.
func TestRepeatFailureOmpHarnessEvidence(t *testing.T) {
	f, err := os.Open("testdata/repeat_failure/omp/evidence.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var version string
	var recs []RepeatFailureRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "omp/") {
			version = strings.TrimSpace(line)
		}
		if strings.HasPrefix(line, "{") {
			rec, ok := parseRepeatFailureRecord([]byte(line + "\n"))
			if !ok {
				t.Fatalf("collector rejects the extension's record line: %s", line)
			}
			recs = append(recs, rec)
		}
	}
	if version == "" {
		t.Fatal("evidence lacks the `omp --version` output")
	}
	if row := rfCoverageRow(t, "omp"); !strings.Contains(row.Evidence, version) {
		t.Fatalf("coverage evidence %q does not carry the harness version %q; update repeat_failure_coverage.go and the docs table", row.Evidence, version)
	}
	if len(recs) != 1 {
		t.Fatalf("evidence records = %+v, want exactly one", recs)
	}
	rec := recs[0]
	if rec.RawCommand != "ls /nonexistent" || rec.ExitCode == nil || *rec.ExitCode != 2 || !rec.Failed ||
		rec.Route != RepeatFailureRouteOmpExtension || rec.Backend != "omp" || rec.Time.IsZero() {
		t.Fatalf("evidence record = %+v", rec)
	}
}

// TestRepeatFailureOmpLaunchFlag covers C14/C-AC12 for omp: with the check on
// an omp launch writes Hive's extension into the record dir and loads it with
// its own --extension even with the advisor off; other backends get nothing;
// with the check off the flag is gone and the file Hive wrote is removed at
// the next launch, while a file Hive did not write is left alone.
func TestRepeatFailureOmpLaunchFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "omp", Model: "m"}}, discardLogger(), ProjectContext{})
	ap := &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "omp", Model: "m"}}
	on := true
	m.SetRepeatFailureEnabledResolver(func() bool { return on })
	ext := repeatFailureOmpExtensionPath(home)

	if m.advisorEnabledFor("scanner") {
		t.Fatal("test expects the advisor off")
	}
	flag := m.repeatFailureOmpLaunchFlag(ap, "omp")
	if flag != " --extension "+shellQuote(ext) {
		t.Fatalf("flag = %q, want the extension at %s", flag, ext)
	}
	data, err := os.ReadFile(ext)
	if err != nil {
		t.Fatalf("extension not written: %v", err)
	}
	if string(data) != string(renderRepeatFailureOmpExtension("scanner", RepeatFailureRecordPath(home))) {
		t.Fatalf("extension content:\n%s", data)
	}
	if got := m.repeatFailureOmpLaunchFlag(ap, "claude"); got != "" {
		t.Fatalf("claude launch got %q", got)
	}

	on = false
	if got := m.repeatFailureOmpLaunchFlag(ap, "omp"); got != "" {
		t.Fatalf("check off: flag = %q, want none", got)
	}
	if _, err := os.Stat(ext); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("check off must remove the extension at the next launch, stat err = %v", err)
	}
	if err := os.WriteFile(ext, []byte("// someone else's file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.repeatFailureOmpLaunchFlag(ap, "omp")
	if _, err := os.Stat(ext); err != nil {
		t.Fatalf("a file Hive did not write must be left alone: %v", err)
	}
}
