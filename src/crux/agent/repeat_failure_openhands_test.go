package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

const rfOpenHandsHarnessEvents = "testdata/repeat_failure/openhands/events"

// rfOpenHandsFixture is the terminal action and observation the real CLI
// saved, in event order.
func rfOpenHandsFixture(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(rfOpenHandsHarnessEvents)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var out []string
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(rfOpenHandsHarnessEvents, name))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(data))
	}
	if len(out) != 2 {
		t.Fatalf("harness events = %d files, want the terminal action and its observation", len(out))
	}
	return out
}

// rfOpenHandsWriteEvents writes events into conversation conv under home,
// starting at index first, and returns the events directory.
func rfOpenHandsWriteEvents(t *testing.T, home, conv string, first int, events ...string) string {
	t.Helper()
	dir := filepath.Join(home, ".openhands", "conversations", conv, "events")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, ev := range events {
		path := filepath.Join(dir, fmt.Sprintf("event-%05d-e%d.json", first+i, first+i))
		if err := os.WriteFile(path, []byte(ev), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// rfOpenHandsCall is the fixture with its tool_call_id replaced by id.
func rfOpenHandsCall(t *testing.T, id string) []string {
	t.Helper()
	var out []string
	for _, ev := range rfOpenHandsFixture(t) {
		out = append(out, strings.ReplaceAll(ev, "call_rf_1", id))
	}
	return out
}

type rfOpenHandsScan struct {
	uid      int
	records  []RepeatFailureRecord
	starts   int
	finishes int
}

func (s *rfOpenHandsScan) run(r *openhandsRecordReader, home string) {
	r.scan(home, s.uid, func(rec RepeatFailureRecord) { s.records = append(s.records, rec) }, func(finished bool, _ time.Time) {
		if finished {
			s.finishes++
		} else {
			s.starts++
		}
	})
}

// TestRepeatFailureOpenHandsHarness is C-AC1 for openhands: the events the
// real OpenHands CLI saved for `ls /nonexistent` (recorded by
// testdata/repeat_failure/openhands/run.sh) yield exactly one failing record
// with the exit code, although OpenHands marks the observation is_error=false,
// and the shell call is reported to the safeguard once started and once
// finished.
func TestRepeatFailureOpenHandsHarness(t *testing.T) {
	raw, err := os.ReadFile("testdata/repeat_failure/openhands/version.txt")
	if err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(string(raw))
	if !strings.Contains(version, "OpenHands CLI") {
		t.Fatalf("harness evidence lacks `openhands --version`: %q", version)
	}
	if row := rfCoverageRow(t, "openhands"); !strings.Contains(row.Evidence, version) {
		t.Fatalf("coverage evidence %q does not carry the harness version %q; update repeat_failure_coverage.go and the docs table", row.Evidence, version)
	}
	home := t.TempDir()
	r := newOpenHandsRecordReader()
	var s rfOpenHandsScan
	s.run(r, home)
	rfOpenHandsWriteEvents(t, home, "harness", 2, rfOpenHandsFixture(t)...)
	s.run(r, home)

	if len(s.records) != 1 {
		t.Fatalf("records = %+v, want exactly one", s.records)
	}
	rec := s.records[0]
	if rec.RawCommand != "ls /nonexistent" || rec.ExitCode == nil || *rec.ExitCode != 2 || !rec.Failed ||
		rec.Route != RepeatFailureRouteOpenHandsEvents || rec.Backend != "openhands" || rec.Time.IsZero() {
		t.Fatalf("record = %+v, want `ls /nonexistent` failed with exit 2 on route %s", rec, RepeatFailureRouteOpenHandsEvents)
	}
	if s.starts != 1 || s.finishes != 1 {
		t.Fatalf("shell activity: %d starts, %d finishes; want 1 and 1", s.starts, s.finishes)
	}
}

// TestRepeatFailureOpenHandsNeverFollowsLinks: the reader runs as Hive over
// files the agent controls, so a conversations tree, a conversation or events
// directory or an event file that is a link (here, to another agent's
// conversation) is skipped, and in multi-uid mode so is an event file owned
// by anyone but the agent.
func TestRepeatFailureOpenHandsNeverFollowsLinks(t *testing.T) {
	fixture := rfOpenHandsFixture(t)
	other := t.TempDir()
	otherEvents := rfOpenHandsWriteEvents(t, other, "victim", 0)
	otherConvs := filepath.Dir(filepath.Dir(otherEvents))

	home := t.TempDir()
	convs := filepath.Join(home, ".openhands", "conversations")
	if err := os.MkdirAll(filepath.Join(convs, "linked-events"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(otherEvents), filepath.Join(convs, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(otherEvents, filepath.Join(convs, "linked-events", "events")); err != nil {
		t.Fatal(err)
	}
	fileEvents := rfOpenHandsWriteEvents(t, home, "linked-file", 0)
	r := newOpenHandsRecordReader()
	var s rfOpenHandsScan
	s.run(r, home)
	rfOpenHandsWriteEvents(t, other, "victim", 0, fixture...)
	for i := range fixture {
		name := fmt.Sprintf("event-%05d-e%d.json", i, i)
		if err := os.Symlink(filepath.Join(otherEvents, name), filepath.Join(fileEvents, name)); err != nil {
			t.Fatal(err)
		}
	}
	s.run(r, home)
	if len(s.records) != 0 || s.starts != 0 {
		t.Fatalf("a linked conversation, events dir or event file was read: %+v", s.records)
	}

	// The whole conversations tree as a link.
	linkedHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(linkedHome, ".openhands"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(otherConvs, filepath.Join(linkedHome, ".openhands", "conversations")); err != nil {
		t.Fatal(err)
	}
	r2 := newOpenHandsRecordReader()
	var s2 rfOpenHandsScan
	s2.run(r2, linkedHome)
	rfOpenHandsWriteEvents(t, other, "victim2", 0, fixture...)
	s2.run(r2, linkedHome)
	if len(s2.records) != 0 {
		t.Fatalf("a linked conversations tree was read: %+v", s2.records)
	}

	// Owner check: the agent's own uid reads, any other uid is skipped.
	ownHome := t.TempDir()
	r3 := newOpenHandsRecordReader()
	wrong := rfOpenHandsScan{uid: os.Getuid() + 1}
	wrong.run(r3, ownHome)
	rfOpenHandsWriteEvents(t, ownHome, "c", 0, rfOpenHandsCall(t, "own")...)
	wrong.run(r3, ownHome)
	if len(wrong.records) != 0 {
		t.Fatalf("an event file owned by another uid was read: %+v", wrong.records)
	}
	// The skipped action stays skipped; the observation is read as the agent.
	right := rfOpenHandsScan{uid: os.Getuid()}
	right.run(r3, ownHome)
	if len(right.records) != 1 {
		t.Fatalf("the agent's own events were not read as uid %d: %+v", os.Getuid(), right.records)
	}
}

// TestRepeatFailureOpenHandsOneRecordPerToolCall: re-reading or an
// observation saved again never records a tool_call_id twice, a conversation that existed when the reader started is not
// replayed, and an event file that does not parse yet is retried.
func TestRepeatFailureOpenHandsOneRecordPerToolCall(t *testing.T) {
	home := t.TempDir()
	rfOpenHandsWriteEvents(t, home, "old", 0, rfOpenHandsCall(t, "old")...)
	r := newOpenHandsRecordReader()
	var s rfOpenHandsScan
	s.run(r, home)
	if len(s.records) != 0 {
		t.Fatalf("history present at start was replayed: %+v", s.records)
	}
	// New events in a conversation that existed at start are read.
	rfOpenHandsWriteEvents(t, home, "old", 2, rfOpenHandsCall(t, "old-next")...)
	s.run(r, home)
	if len(s.records) != 1 {
		t.Fatalf("records = %+v, want the new call in the old conversation", s.records)
	}

	call := rfOpenHandsCall(t, "a")
	rfOpenHandsWriteEvents(t, home, "new", 0, call...)
	s.run(r, home)
	s.run(r, home)
	// The observation saved again under a later index.
	rfOpenHandsWriteEvents(t, home, "new", 2, call[1])
	s.run(r, home)
	if len(s.records) != 2 {
		t.Fatalf("records = %d, want one per tool_call_id: %+v", len(s.records), s.records)
	}

	// The newest event is not valid JSON yet: retried, then read.
	dir := rfOpenHandsWriteEvents(t, home, "new", 3, call[0])
	partial := filepath.Join(dir, "event-00004-e4.json")
	b := rfOpenHandsCall(t, "b")
	if err := os.WriteFile(partial, []byte(b[1][:20]), 0o600); err != nil {
		t.Fatal(err)
	}
	s.run(r, home)
	if err := os.WriteFile(partial, []byte(b[1]), 0o600); err != nil {
		t.Fatal(err)
	}
	s.run(r, home)
	if len(s.records) != 3 || s.records[2].Route != RepeatFailureRouteOpenHandsEvents {
		t.Fatalf("records = %+v, want the observation written in two parts recorded once", s.records)
	}
}

// TestRepeatFailureOpenHandsResult covers the result rules: the exit code is
// the result, a command that timed out or has no exit code yet (-1) is not
// recorded, and neither is an observation of another tool.
func TestRepeatFailureOpenHandsResult(t *testing.T) {
	obs := func(id, tool, kind, extra string) string {
		return `{"kind":"ObservationEvent","timestamp":"2026-10-28T01:00:00.5","tool_name":"` + tool + `","tool_call_id":"` + id +
			`","observation":{"kind":"` + kind + `","command":"make ` + id + `",` + extra + `}}`
	}
	events := []string{
		obs("ok", "terminal", "TerminalObservation", `"exit_code":0,"is_error":false,"timeout":false`),
		obs("fail", "terminal", "TerminalObservation", `"exit_code":1,"is_error":false,"timeout":false`),
		obs("timeout", "terminal", "TerminalObservation", `"exit_code":-1,"timeout":true`),
		obs("running", "terminal", "TerminalObservation", `"exit_code":-1,"timeout":false`),
		obs("nocode", "terminal", "TerminalObservation", `"timeout":false`),
		obs("edit", "file_editor", "FileEditorObservation", `"exit_code":1`),
		"not json",
		`{"kind":"MessageEvent","timestamp":"2026-10-28T01:00:01Z"}`,
	}
	home := t.TempDir()
	r := newOpenHandsRecordReader()
	var s rfOpenHandsScan
	s.run(r, home)
	rfOpenHandsWriteEvents(t, home, "c", 0, events...)
	s.run(r, home)

	got := map[string]RepeatFailureRecord{}
	for _, rec := range s.records {
		got[strings.TrimPrefix(rec.RawCommand, "make ")] = rec
	}
	if len(got) != 2 {
		t.Fatalf("records = %+v, want ok and fail only", s.records)
	}
	if rec := got["ok"]; rec.ExitCode == nil || *rec.ExitCode != 0 || rec.Failed || rec.Time.IsZero() {
		t.Errorf("ok: %+v", rec)
	}
	if rec := got["fail"]; rec.ExitCode == nil || *rec.ExitCode != 1 || !rec.Failed {
		t.Errorf("fail: %+v", rec)
	}
}

// TestRepeatFailureOpenHandsRoute runs the route end to end through the
// collector: repeated failures in an OpenHands agent's saved events raise the
// note, the covered agent shows the check on, and with the check off the
// reader writes no record file.
func TestRepeatFailureOpenHandsRoute(t *testing.T) {
	m := rfStatusManager(t, "openhands")
	home := os.Getenv("HOME")
	on := false
	m.SetRepeatFailureEnabledResolver(func() bool { return on })
	m.SetRepeatFailureConfigResolver(func() *config.RepeatFailureConfig { return &config.RepeatFailureConfig{Enabled: true} })
	now := time.Now()

	var events []string
	for i := range 5 {
		events = append(events, rfOpenHandsCall(t, "call_"+strconv.Itoa(i))...)
	}
	m.CollectRepeatFailures(now)
	rfOpenHandsWriteEvents(t, home, "off", 0, events...)
	m.CollectRepeatFailures(now.Add(time.Second))
	if _, err := os.Stat(filepath.Dir(RepeatFailureRecordPath(home))); !os.IsNotExist(err) {
		t.Fatalf("check off: the openhands route created the record dir (err %v)", err)
	}

	on = true
	m.CollectRepeatFailures(now.Add(2 * time.Second))
	rfOpenHandsWriteEvents(t, home, "on", 0, events...)
	m.CollectRepeatFailures(now.Add(3 * time.Second))
	st := rfStatus(m)
	if st.Check != RepeatFailureCheckOn {
		t.Fatalf("openhands check = %q, want %q (covered)", st.Check, RepeatFailureCheckOn)
	}
	if want := "Repeating failure: `ls /nonexistent` failed 5 times in 10 min (exit 2)"; st.Note != want {
		t.Fatalf("note = %q, want %q", st.Note, want)
	}
}
