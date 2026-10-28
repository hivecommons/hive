package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

const rfCopilotHarnessEvents = "testdata/repeat_failure/copilot/events.jsonl"

func rfCopilotFixture(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(rfCopilotHarnessEvents)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func rfCopilotWriteSession(t *testing.T, home, session, content string) string {
	t.Helper()
	dir := filepath.Join(home, ".copilot", "session-state", session)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type rfCopilotScan struct {
	uid      int
	records  []RepeatFailureRecord
	starts   int
	finishes int
}

func (s *rfCopilotScan) run(r *copilotRecordReader, home string) {
	r.scan(home, s.uid, func(rec RepeatFailureRecord) { s.records = append(s.records, rec) }, func(finished bool, _ time.Time) {
		if finished {
			s.finishes++
		} else {
			s.starts++
		}
	})
}

// TestRepeatFailureCopilotHarness is C-AC1 for copilot: the session log the
// real Copilot CLI wrote for `ls /nonexistent` (recorded by
// testdata/repeat_failure/copilot/record.sh) yields exactly one failing record
// with the exit code, although Copilot marks the call success=true, and the
// shell call is reported to the safeguard once started and once finished.
func TestRepeatFailureCopilotHarness(t *testing.T) {
	version := rfCopilotFixtureVersion(t)
	if !strings.Contains(version, "Copilot CLI") {
		t.Fatalf("harness evidence lacks `copilot --version`: %q", version)
	}
	if row := rfCoverageRow(t, "copilot"); !strings.Contains(row.Evidence, version) {
		t.Fatalf("coverage evidence %q does not carry the harness version %q; update repeat_failure_coverage.go and the docs table", row.Evidence, version)
	}
	home := t.TempDir()
	r := newCopilotRecordReader()
	var s rfCopilotScan
	s.run(r, home)
	rfCopilotWriteSession(t, home, "harness", rfCopilotFixture(t))
	s.run(r, home)

	if len(s.records) != 1 {
		t.Fatalf("records = %+v, want exactly one", s.records)
	}
	rec := s.records[0]
	if rec.RawCommand != "ls /nonexistent" || rec.ExitCode == nil || *rec.ExitCode != 2 || !rec.Failed ||
		rec.Route != RepeatFailureRouteCopilotEvents || rec.Backend != "copilot" || rec.Time.IsZero() {
		t.Fatalf("record = %+v, want `ls /nonexistent` failed with exit 2 on route %s", rec, RepeatFailureRouteCopilotEvents)
	}
	if s.starts != 1 || s.finishes != 1 {
		t.Fatalf("shell activity: %d starts, %d finishes; want 1 and 1", s.starts, s.finishes)
	}
}

// rfCopilotFixtureVersion is the `copilot --version` line the harness
// recorded, without its trailing period.
func rfCopilotFixtureVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/repeat_failure/copilot/version.txt")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(strings.TrimSpace(string(data)), ".")
}

// TestRepeatFailureCopilotNeverFollowsLinks: the reader runs as Hive over
// files the agent controls, so a session-state tree, a session directory or
// an events.jsonl that is a link (here, to another agent's log) is skipped,
// and in multi-uid mode so is a log owned by anyone but the agent.
func TestRepeatFailureCopilotNeverFollowsLinks(t *testing.T) {
	fixture := rfCopilotFixture(t)
	other := t.TempDir()
	otherLog := rfCopilotWriteSession(t, other, "victim", "")
	otherState := filepath.Dir(filepath.Dir(otherLog))

	home := t.TempDir()
	state := filepath.Join(home, ".copilot", "session-state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(otherLog), filepath.Join(state, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(state, "linked-file"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(otherLog, filepath.Join(state, "linked-file", "events.jsonl")); err != nil {
		t.Fatal(err)
	}
	r := newCopilotRecordReader()
	var s rfCopilotScan
	s.run(r, home)
	if err := os.WriteFile(otherLog, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	s.run(r, home)
	if len(s.records) != 0 || s.starts != 0 {
		t.Fatalf("a linked session dir or log was read: %+v", s.records)
	}

	// The whole session-state tree as a link.
	linkedHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(linkedHome, ".copilot"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(otherState, filepath.Join(linkedHome, ".copilot", "session-state")); err != nil {
		t.Fatal(err)
	}
	r2 := newCopilotRecordReader()
	var s2 rfCopilotScan
	s2.run(r2, linkedHome)
	if err := os.WriteFile(otherLog, []byte(fixture+fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	s2.run(r2, linkedHome)
	if len(s2.records) != 0 {
		t.Fatalf("a linked session-state tree was read: %+v", s2.records)
	}

	// Owner check: the agent's own uid reads, any other uid is skipped.
	ownHome := t.TempDir()
	path := rfCopilotWriteSession(t, ownHome, "s", "")
	r3 := newCopilotRecordReader()
	wrong := rfCopilotScan{uid: os.Getuid() + 1}
	wrong.run(r3, ownHome)
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	wrong.run(r3, ownHome)
	if len(wrong.records) != 0 {
		t.Fatalf("a log owned by another uid was read: %+v", wrong.records)
	}
	right := rfCopilotScan{uid: os.Getuid()}
	right.run(r3, ownHome)
	if len(right.records) != 1 {
		t.Fatalf("the agent's own log was not read as uid %d: %+v", os.Getuid(), right.records)
	}
}

// TestRepeatFailureCopilotOneRecordPerToolCall: re-reading a log, appending a
// duplicate of a recorded call or rewriting the log never records a
// toolCallId twice, and a log that existed when the reader started is not
// replayed.
func TestRepeatFailureCopilotOneRecordPerToolCall(t *testing.T) {
	home := t.TempDir()
	fixture := rfCopilotFixture(t)
	rfCopilotWriteSession(t, home, "old", fixture)
	r := newCopilotRecordReader()
	var s rfCopilotScan
	s.run(r, home)
	if len(s.records) != 0 {
		t.Fatalf("history present at start was replayed: %+v", s.records)
	}

	path := rfCopilotWriteSession(t, home, "new", fixture)
	s.run(r, home)
	s.run(r, home)
	if err := os.WriteFile(path, []byte(fixture+fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	s.run(r, home)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.run(r, home)
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	s.run(r, home)
	if len(s.records) != 1 {
		t.Fatalf("records = %d, want one per toolCallId across re-reads: %+v", len(s.records), s.records)
	}

	// A partial last line waits for its newline.
	second := strings.ReplaceAll(fixture, "toolu_", "toolu_second_")
	cut := strings.Index(second, "\n") + 20
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(second[:cut])
	s.run(r, home)
	_, _ = f.WriteString(second[cut:])
	_ = f.Close()
	s.run(r, home)
	if len(s.records) != 2 {
		t.Fatalf("records = %d after a line written in two parts, want 2", len(s.records))
	}
}

// TestRepeatFailureCopilotResult covers the result rules: the exit code wins
// over success, a call without one falls back to success, and a call that
// returned while its command keeps running (async, detached) or that is not a
// shell call is not recorded.
func TestRepeatFailureCopilotResult(t *testing.T) {
	start := func(id, tool string) string {
		return `{"type":"tool.execution_start","data":{"toolCallId":"` + id + `","toolName":"` + tool + `","arguments":{"command":"make ` + id + `"}},"timestamp":"2026-10-28T01:00:00Z"}` + "\n"
	}
	complete := func(id, data string) string {
		return `{"type":"tool.execution_complete","data":{"toolCallId":"` + id + `",` + data + `},"timestamp":"2026-10-28T01:00:01Z"}` + "\n"
	}
	log := start("ok", "bash") + complete("ok", `"shellExecution":{"exitCode":0},"success":true`) +
		start("nocode", "bash") + complete("nocode", `"success":false,"toolTelemetry":{"properties":{"executionMode":"sync"}}`) +
		start("async", "bash") + complete("async", `"success":true,"toolTelemetry":{"properties":{"executionMode":"async"}}`) +
		start("detached", "bash") + complete("detached", `"success":true,"toolTelemetry":{"properties":{"detached":true}}`) +
		start("view", "view") + complete("view", `"success":false`) +
		`{"type":"assistant.message","data":{"toolRequests":[{"toolCallId":"req","name":"bash","arguments":{"command":"make req"}}]}}` + "\n" +
		complete("req", `"shellExecution":{"exitCode":1},"success":true`) +
		"not json\n"

	home := t.TempDir()
	r := newCopilotRecordReader()
	var s rfCopilotScan
	s.run(r, home)
	rfCopilotWriteSession(t, home, "s", log)
	s.run(r, home)

	got := map[string]RepeatFailureRecord{}
	for _, rec := range s.records {
		got[strings.TrimPrefix(rec.RawCommand, "make ")] = rec
	}
	if len(got) != 3 {
		t.Fatalf("records = %+v, want ok, nocode and req only", s.records)
	}
	if rec := got["ok"]; rec.ExitCode == nil || *rec.ExitCode != 0 || rec.Failed {
		t.Errorf("ok: %+v", rec)
	}
	if rec := got["nocode"]; rec.ExitCode != nil || !rec.Failed {
		t.Errorf("nocode: %+v, want failed without an exit code", rec)
	}
	if rec := got["req"]; rec.ExitCode == nil || *rec.ExitCode != 1 || !rec.Failed {
		t.Errorf("req (command from toolRequests): %+v", rec)
	}
}

// TestRepeatFailureCopilotRoute runs the route end to end through the
// collector: repeated failures in a Copilot agent's session log raise the
// note, the covered agent shows the check on, and with the check off the
// reader writes no record file.
func TestRepeatFailureCopilotRoute(t *testing.T) {
	m := rfStatusManager(t, "copilot")
	home := os.Getenv("HOME")
	on := false
	m.SetRepeatFailureEnabledResolver(func() bool { return on })
	m.SetRepeatFailureConfigResolver(func() *config.RepeatFailureConfig { return &config.RepeatFailureConfig{Enabled: true} })
	now := time.Now()
	fixture := rfCopilotFixture(t)

	var log strings.Builder
	for i := range 5 {
		log.WriteString(strings.ReplaceAll(fixture, "toolu_", "toolu_"+strconv.Itoa(i)+"_"))
	}
	m.CollectRepeatFailures(now)
	rfCopilotWriteSession(t, home, "off", log.String())
	m.CollectRepeatFailures(now.Add(time.Second))
	if _, err := os.Stat(filepath.Dir(RepeatFailureRecordPath(home))); !os.IsNotExist(err) {
		t.Fatalf("check off: the copilot route created the record dir (err %v)", err)
	}

	on = true
	m.CollectRepeatFailures(now.Add(2 * time.Second))
	rfCopilotWriteSession(t, home, "on", log.String())
	m.CollectRepeatFailures(now.Add(3 * time.Second))
	st := rfStatus(m)
	if st.Check != RepeatFailureCheckOn {
		t.Fatalf("copilot check = %q, want %q (covered)", st.Check, RepeatFailureCheckOn)
	}
	if want := "Repeating failure: `ls /nonexistent` failed 5 times in 10 min (exit 2)"; st.Note != want {
		t.Fatalf("note = %q, want %q", st.Note, want)
	}
}
