package agent

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

const rfMuseHarnessLog = "testdata/repeat_failure/muse/session.jsonl"

// rfMuseFixture is the bash call and its result the real CLI logged, in log
// order.
func rfMuseFixture(t *testing.T) []string {
	t.Helper()
	f, err := os.Open(rfMuseHarnessLog)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			out = append(out, line)
		}
	}
	if len(out) != 2 {
		t.Fatalf("harness log = %d lines, want the bash call and its result", len(out))
	}
	return out
}

// rfMuseCall is the fixture with its tool_call_id replaced by id.
func rfMuseCall(t *testing.T, id string) []string {
	t.Helper()
	var out []string
	for _, line := range rfMuseFixture(t) {
		out = append(out, strings.ReplaceAll(line, "call_rf_1", id))
	}
	return out
}

// rfMuseAppend appends lines to session id's log under root and returns the
// log path.
func rfMuseAppend(t *testing.T, root, id string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(root, "2026", "10", "28", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, museSessionLog)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, line := range lines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

type rfMuseScan struct {
	uid      int
	records  []RepeatFailureRecord
	starts   int
	finishes int
}

func (s *rfMuseScan) run(r *museRecordReader, root string) {
	r.scan(root, s.uid, func(rec RepeatFailureRecord) { s.records = append(s.records, rec) }, func(finished bool, _ time.Time) {
		if finished {
			s.finishes++
		} else {
			s.starts++
		}
	})
}

func rfMuseRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ".local", "share", "muse", "sessions")
}

// TestRepeatFailureMuseHarness is C-AC1 for muse: the session-log lines the
// real Muse Code CLI wrote for `ls /nonexistent` (recorded by
// testdata/repeat_failure/muse/run.sh) yield exactly one failing record with
// the exit code, and the shell call is reported to the safeguard once started
// and once finished.
func TestRepeatFailureMuseHarness(t *testing.T) {
	raw, err := os.ReadFile("testdata/repeat_failure/muse/version.txt")
	if err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(string(raw))
	if !strings.Contains(version, "Muse Code") {
		t.Fatalf("harness evidence lacks `muse --version`: %q", version)
	}
	if row := rfCoverageRow(t, "muse"); !strings.Contains(row.Evidence, version) {
		t.Fatalf("coverage evidence %q does not carry the harness version %q; update repeat_failure_coverage.go and the docs table", row.Evidence, version)
	}
	root := rfMuseRoot(t)
	r := newMuseRecordReader()
	var s rfMuseScan
	s.run(r, root)
	rfMuseAppend(t, root, "harness", rfMuseFixture(t)...)
	s.run(r, root)

	if len(s.records) != 1 {
		t.Fatalf("records = %+v, want exactly one", s.records)
	}
	rec := s.records[0]
	if rec.RawCommand != "ls /nonexistent" || rec.ExitCode == nil || *rec.ExitCode != 2 || !rec.Failed ||
		rec.Route != RepeatFailureRouteMuseSessions || rec.Backend != "muse" || rec.Time.IsZero() {
		t.Fatalf("record = %+v, want `ls /nonexistent` failed with exit 2 on route %s", rec, RepeatFailureRouteMuseSessions)
	}
	if s.starts != 1 || s.finishes != 1 {
		t.Fatalf("shell activity: %d starts, %d finishes; want 1 and 1", s.starts, s.finishes)
	}
}

// TestRepeatFailureMuseNeverFollowsLinks: the reader runs as Hive over files
// the agent controls, so a session directory or log that is a link (here, to
// another agent's session) is skipped, and in multi-uid mode so is a log
// owned by anyone but the agent.
func TestRepeatFailureMuseNeverFollowsLinks(t *testing.T) {
	other := rfMuseRoot(t)
	victim := rfMuseAppend(t, other, "victim")

	root := rfMuseRoot(t)
	day := filepath.Join(root, "2026", "10", "28")
	if err := os.MkdirAll(filepath.Join(day, "linked-log"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(victim), filepath.Join(day, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(day, "linked-log", museSessionLog)); err != nil {
		t.Fatal(err)
	}
	r := newMuseRecordReader()
	var s rfMuseScan
	s.run(r, root)
	rfMuseAppend(t, other, "victim", rfMuseFixture(t)...)
	s.run(r, root)
	if len(s.records) != 0 || s.starts != 0 {
		t.Fatalf("a linked session directory or log was read: %+v", s.records)
	}

	// The whole sessions tree as a link.
	linked := rfMuseRoot(t)
	if err := os.MkdirAll(filepath.Dir(linked), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, linked); err != nil {
		t.Fatal(err)
	}
	r2 := newMuseRecordReader()
	var s2 rfMuseScan
	s2.run(r2, linked)
	rfMuseAppend(t, other, "victim2", rfMuseFixture(t)...)
	s2.run(r2, linked)
	if len(s2.records) != 0 {
		t.Fatalf("a linked sessions tree was read: %+v", s2.records)
	}

	// Owner check: the agent's own uid reads, any other uid is skipped.
	own := rfMuseRoot(t)
	r3 := newMuseRecordReader()
	wrong := rfMuseScan{uid: os.Getuid() + 1}
	wrong.run(r3, own)
	rfMuseAppend(t, own, "s", rfMuseCall(t, "own")...)
	wrong.run(r3, own)
	if len(wrong.records) != 0 {
		t.Fatalf("a log owned by another uid was read: %+v", wrong.records)
	}
	right := rfMuseScan{uid: os.Getuid()}
	r4 := newMuseRecordReader()
	right.run(r4, own)
	rfMuseAppend(t, own, "s", rfMuseCall(t, "own-next")...)
	right.run(r4, own)
	if len(right.records) != 1 {
		t.Fatalf("the agent's own log was not read as uid %d: %+v", os.Getuid(), right.records)
	}
}

// TestRepeatFailureMuseOneRecordPerToolCall: re-reading, a rewritten log or a
// result logged again never records a tool_call_id twice, a log that existed
// when the reader started is not replayed, a subagent log is read, and a line
// still being written is read once complete.
func TestRepeatFailureMuseOneRecordPerToolCall(t *testing.T) {
	root := rfMuseRoot(t)
	path := rfMuseAppend(t, root, "old", rfMuseCall(t, "old")...)
	r := newMuseRecordReader()
	var s rfMuseScan
	s.run(r, root)
	if len(s.records) != 0 {
		t.Fatalf("history present at start was replayed: %+v", s.records)
	}
	rfMuseAppend(t, root, "old", rfMuseCall(t, "old-next")...)
	s.run(r, root)
	if len(s.records) != 1 {
		t.Fatalf("records = %+v, want the new call in the old log", s.records)
	}

	call := rfMuseCall(t, "a")
	rfMuseAppend(t, root, "new", call...)
	s.run(r, root)
	s.run(r, root)
	// The result logged again.
	rfMuseAppend(t, root, "new", call[1])
	s.run(r, root)
	if len(s.records) != 2 {
		t.Fatalf("records = %d, want one per tool_call_id: %+v", len(s.records), s.records)
	}

	// A subagent's log.
	sub := filepath.Join("new", "subagent", "child")
	rfMuseAppend(t, root, sub, rfMuseCall(t, "child")...)
	s.run(r, root)
	if len(s.records) != 3 {
		t.Fatalf("records = %+v, want the subagent's call", s.records)
	}

	// A line written in two parts.
	b := rfMuseCall(t, "b")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(b[0] + "\n" + b[1][:20]); err != nil {
		t.Fatal(err)
	}
	s.run(r, root)
	if _, err := f.WriteString(b[1][20:] + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	s.run(r, root)
	if len(s.records) != 4 || s.records[3].Route != RepeatFailureRouteMuseSessions {
		t.Fatalf("records = %+v, want the result written in two parts recorded once", s.records)
	}

	// Rewritten shorter: read again, but nothing is recorded twice.
	if err := os.WriteFile(path, []byte(strings.Join(b, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.run(r, root)
	if len(s.records) != 4 {
		t.Fatalf("a rewritten log recorded a call again: %+v", s.records)
	}
}

// TestRepeatFailureMuseResult covers the result rules: the exit code is the
// result; a command still running (no exit code), timed out, refused (not the
// shell's JSON) or a call of another tool is not recorded.
func TestRepeatFailureMuseResult(t *testing.T) {
	call := func(id, tool string) string {
		return `{"recorded_at":1793170680910529,"payload":{"event":{"kind":"assistant_tool_calls_committed","tool_calls":[{"call_id":"` + id +
			`","name":"` + tool + `","args":"{\"command\":\"make ` + id + `\",\"description\":\"d\"}"}]}}}`
	}
	result := func(id, text string) string {
		return `{"recorded_at":1793170680920529,"payload":{"event":{"kind":"tool_result_batch_committed","results":[{"tool_call_id":"` + id +
			`","text":` + strconv.Quote(text) + `}]}}}`
	}
	lines := []string{
		call("ok", "bash"), result("ok", `{"command":"make ok","exit_code":0,"terminal_status":"completed"}`),
		call("fail", "bash"), result("fail", `{"command":"make fail","exit_code":1,"terminal_status":"failed"}`),
		call("running", "bash"), result("running", `{"command":"make running","session_id":7}`),
		call("timeout", "bash"), result("timeout", `{"command":"make timeout","exit_code":137,"terminal_status":"timed_out"}`),
		call("denied", "bash"), result("denied", "tool call denied by policy"),
		call("input", "bash_input"), result("input", `{"exit_code":1}`),
		result("orphan", `{"exit_code":1}`),
		"not json",
	}
	root := rfMuseRoot(t)
	r := newMuseRecordReader()
	var s rfMuseScan
	s.run(r, root)
	rfMuseAppend(t, root, "c", lines...)
	s.run(r, root)

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
	if s.starts != 5 || s.finishes != 5 {
		t.Errorf("shell activity: %d starts, %d finishes; want 5 and 5 (bash calls only)", s.starts, s.finishes)
	}
}

// TestRepeatFailureMuseSessionsRoot: the reader looks where Muse Code writes,
// ${XDG_DATA_HOME:-$HOME/.local/share}/muse/sessions, with XDG_DATA_HOME as
// the agent's launch environment carries it.
func TestRepeatFailureMuseSessionsRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", "")
	if got, want := museSessionsRoot("scanner", 0, home), filepath.Join(home, ".local", "share", "muse", "sessions"); got != want {
		t.Errorf("no XDG_DATA_HOME: root = %q, want %q", got, want)
	}
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	if got, want := museSessionsRoot("scanner", 0, home), filepath.Join(data, "muse", "sessions"); got != want {
		t.Errorf("XDG_DATA_HOME inherited: root = %q, want %q", got, want)
	}
	t.Setenv("HIVE_SHARED_AGENT_HOME", "")
	agentHome := AgentHome("scanner", 1234, "muse")
	if got, want := museSessionsRoot("scanner", 1234, agentHome), filepath.Join(agentXDGDataHome(agentHome), "muse", "sessions"); got != want {
		t.Errorf("per-agent home: root = %q, want %q", got, want)
	}
}

// TestRepeatFailureMuseRoute runs the route end to end through the
// collector: repeated failures in a Muse Code agent's session log raise the
// note, the covered agent shows the check on, and with the check off the
// reader writes no record file.
func TestRepeatFailureMuseRoute(t *testing.T) {
	m := rfStatusManager(t, "muse")
	home := os.Getenv("HOME")
	t.Setenv("XDG_DATA_HOME", "")
	root := museSessionsRoot("scanner", 0, home)
	on := false
	m.SetRepeatFailureEnabledResolver(func() bool { return on })
	m.SetRepeatFailureConfigResolver(func() *config.RepeatFailureConfig { return &config.RepeatFailureConfig{Enabled: true} })
	now := time.Now()

	var lines []string
	for i := range 5 {
		lines = append(lines, rfMuseCall(t, "call_"+strconv.Itoa(i))...)
	}
	m.CollectRepeatFailures(now)
	rfMuseAppend(t, root, "off", lines...)
	m.CollectRepeatFailures(now.Add(time.Second))
	if _, err := os.Stat(filepath.Dir(RepeatFailureRecordPath(home))); !os.IsNotExist(err) {
		t.Fatalf("check off: the muse route created the record dir (err %v)", err)
	}

	on = true
	m.CollectRepeatFailures(now.Add(2 * time.Second))
	rfMuseAppend(t, root, "on", lines...)
	m.CollectRepeatFailures(now.Add(3 * time.Second))
	st := rfStatus(m)
	if st.Check != RepeatFailureCheckOn {
		t.Fatalf("muse check = %q, want %q (covered)", st.Check, RepeatFailureCheckOn)
	}
	if want := "Repeating failure: `ls /nonexistent` failed 5 times in 10 min (exit 2)"; st.Note != want {
		t.Fatalf("note = %q, want %q", st.Note, want)
	}
}
