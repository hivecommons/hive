package agent

import (
	"bufio"
	"fmt"
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

// rfMuseCallAt is rfMuseCall with both lines' recorded_at moved to at, for
// readers whose start cannot be pinned before the fixture's instant.
func rfMuseCallAt(t *testing.T, id string, at time.Time) []string {
	t.Helper()
	stamp := strconv.FormatInt(at.UnixMicro(), 10)
	var out []string
	for _, line := range rfMuseCall(t, id) {
		line = strings.ReplaceAll(line, "1793170680910529", stamp)
		out = append(out, strings.ReplaceAll(line, "1793170680992012", stamp))
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

// rfMuseFixtureAt is the recorded_at of the fixture's lines, truncated to the
// second.
var rfMuseFixtureAt = time.UnixMicro(1793170680000000)

// rfMuseReader is a reader that started just before the fixture's lines were
// recorded, so a fixture log written after its first scan reads as new
// activity whatever the wall clock says.
func rfMuseReader() *museRecordReader {
	r := newMuseRecordReader()
	r.startedAt = rfMuseFixtureAt
	return r
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
	r := rfMuseReader()
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
	r := rfMuseReader()
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
	r2 := rfMuseReader()
	var s2 rfMuseScan
	s2.run(r2, linked)
	rfMuseAppend(t, other, "victim2", rfMuseFixture(t)...)
	s2.run(r2, linked)
	if len(s2.records) != 0 {
		t.Fatalf("a linked sessions tree was read: %+v", s2.records)
	}

	// Owner check: the agent's own uid reads, any other uid is skipped.
	own := rfMuseRoot(t)
	r3 := rfMuseReader()
	wrong := rfMuseScan{uid: os.Getuid() + 1}
	wrong.run(r3, own)
	rfMuseAppend(t, own, "s", rfMuseCall(t, "own")...)
	wrong.run(r3, own)
	if len(wrong.records) != 0 {
		t.Fatalf("a log owned by another uid was read: %+v", wrong.records)
	}
	right := rfMuseScan{uid: os.Getuid()}
	r4 := rfMuseReader()
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
	r := rfMuseReader()
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
	r := rfMuseReader()
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

	// The Manager's reader starts at the wall clock, so fixture lines must be
	// stamped after it has started: a log it first reaches later drops
	// records dated before that instant as history.
	stamped := func() []string {
		var lines []string
		for i := range 5 {
			lines = append(lines, rfMuseCallAt(t, "call_"+strconv.Itoa(i), time.Now())...)
		}
		return lines
	}
	m.CollectRepeatFailures(now)
	rfMuseAppend(t, root, "off", stamped()...)
	m.CollectRepeatFailures(now.Add(time.Second))
	if _, err := os.Stat(filepath.Dir(RepeatFailureRecordPath(home))); !os.IsNotExist(err) {
		t.Fatalf("check off: the muse route created the record dir (err %v)", err)
	}

	on = true
	m.CollectRepeatFailures(now.Add(2 * time.Second))
	rfMuseAppend(t, root, "on", stamped()...)
	m.CollectRepeatFailures(now.Add(3 * time.Second))
	st := rfStatus(m)
	if st.Check != RepeatFailureCheckOn {
		t.Fatalf("muse check = %q, want %q (covered)", st.Check, RepeatFailureCheckOn)
	}
	if want := "Repeating failure: `ls /nonexistent` failed 5 times in 10 min (exit 2)"; st.Note != want {
		t.Fatalf("note = %q, want %q", st.Note, want)
	}
}

// TestRepeatFailureMuseWalkIsBounded: an agent-writable sessions tree with
// more directories or logs than the caps is walked only up to them, newest
// names and days first, and the walk reports that it left some out.
func TestRepeatFailureMuseWalkIsBounded(t *testing.T) {
	root := rfMuseRoot(t)
	day := filepath.Join(root, "2026", "10", "28")
	const extra = 50
	for i := 0; i < museDirEntriesMax+extra; i++ {
		if err := os.MkdirAll(filepath.Join(day, "s"+strconv.Itoa(1000000+i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got, truncated := museRealSubdirs(day)
	if len(got) != museDirEntriesMax || !truncated {
		t.Fatalf("subdirs read from one directory = %d (truncated %v), want %d truncated", len(got), truncated, museDirEntriesMax)
	}
	// The newest names survive the cap, whatever order the directory lists
	// them in: the oldest `extra` are the ones left out.
	newest := "s" + strconv.Itoa(1000000+museDirEntriesMax+extra-1)
	oldestKept := "s" + strconv.Itoa(1000000+extra)
	if got[0] != newest || got[len(got)-1] != oldestKept {
		t.Fatalf("kept %q..%q, want %q..%q", got[0], got[len(got)-1], newest, oldestKept)
	}
	if logs, truncated := museSessionLogs(root); len(logs) != museDirEntriesMax || !truncated {
		t.Fatalf("logs from the over-full day = %d (truncated %v), want %d truncated", len(logs), truncated, museDirEntriesMax)
	}
	if _, truncated := museRealSubdirs(filepath.Join(root, "2026", "10")); truncated {
		t.Fatal("an under-cap directory reported truncation")
	}

	// More logs than museSessionLogsMax across days: the oldest are left out.
	total := rfMuseRoot(t)
	perDay := museDirEntriesMax
	days := museSessionLogsMax/perDay + 2
	for d := 1; d <= days; d++ {
		for i := 0; i < perDay; i++ {
			dir := filepath.Join(total, "2026", "10", "0"+strconv.Itoa(d), "s"+strconv.Itoa(i))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	logs, truncated := museSessionLogs(total)
	if len(logs) != museSessionLogsMax || !truncated {
		t.Fatalf("logs collected = %d (truncated %v), want %d truncated", len(logs), truncated, museSessionLogsMax)
	}
	if !strings.Contains(logs[0], filepath.Join("10", "0"+strconv.Itoa(days))) {
		t.Fatalf("first log %q is not from the newest day", logs[0])
	}

	// Directory listings are budgeted too, so empty year/month/day subtrees
	// cannot make a scan list directories without bound.
	w := &museWalk{listings: museDirListingsMax}
	if got := w.subdirs(filepath.Join(total, "2026")); got != nil || !w.truncated {
		t.Fatalf("subdirs past the listing budget = %v (truncated %v), want none, truncated", got, w.truncated)
	}
	small := rfMuseRoot(t)
	if _, truncated := museSessionLogs(small); truncated {
		t.Fatal("a missing root reported truncation")
	}
}

// TestRepeatFailureMuseDisplacedSessionIsNotReplayed: a session log pushed
// out of a capped walk by newer siblings keeps its read offset, so when it is
// walked again nothing already recorded is emitted a second time.
func TestRepeatFailureMuseDisplacedSessionIsNotReplayed(t *testing.T) {
	root := rfMuseRoot(t)
	r := rfMuseReader()
	var s rfMuseScan
	s.run(r, root)
	rfMuseAppend(t, root, "a-session", rfMuseCall(t, "call_1")...)
	s.run(r, root)
	if len(s.records) != 1 {
		t.Fatalf("records = %+v, want exactly one", s.records)
	}
	const rel = "2026/10/28/a-session/" + museSessionLog
	if r.sessions[filepath.FromSlash(rel)] == nil {
		t.Fatalf("sessions = %v, want %q followed", r.sessions, rel)
	}

	// Flood the day with newer-named sessions so a-session falls out of the
	// per-directory cap.
	day := filepath.Join(root, "2026", "10", "28")
	var flood []string
	for i := 0; i < museDirEntriesMax; i++ {
		dir := filepath.Join(day, "z"+strconv.Itoa(1000000+i))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		flood = append(flood, dir)
	}
	s.run(r, root)
	if r.sessions[filepath.FromSlash(rel)] == nil {
		t.Fatal("displaced session was dropped while its log still exists")
	}
	for _, dir := range flood {
		if err := os.Remove(dir); err != nil {
			t.Fatal(err)
		}
	}
	rfMuseAppend(t, root, "a-session", rfMuseCall(t, "call_2")...)
	s.run(r, root)
	if len(s.records) != 2 || s.records[1].RawCommand != "ls /nonexistent" {
		t.Fatalf("records after the session returned = %+v, want exactly the second call added", s.records)
	}

	// Gone for real: a scan that saw the whole tree forgets it.
	if err := os.RemoveAll(filepath.Join(day, "a-session")); err != nil {
		t.Fatal(err)
	}
	s.run(r, root)
	if len(r.sessions) != 0 {
		t.Fatalf("sessions after deletion = %v, want none", r.sessions)
	}
}

// TestRepeatFailureMuseLogHiddenByCapIsNotReplayed: a log that existed when
// the reader started but that a cap kept out of the first walk is read from
// its start when it is finally reached, yet its pre-start history is not
// emitted.
func TestRepeatFailureMuseLogHiddenByCapIsNotReplayed(t *testing.T) {
	root := rfMuseRoot(t)
	oldLines := rfMuseCallAt(t, "call_old", time.UnixMicro(1000000000000000))
	rfMuseAppend(t, root, "a-session", oldLines...)

	day := filepath.Join(root, "2026", "10", "28")
	var flood []string
	for i := 0; i < museDirEntriesMax; i++ {
		dir := filepath.Join(day, "z"+strconv.Itoa(1000000+i))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		flood = append(flood, dir)
	}
	r := newMuseRecordReader()
	var s rfMuseScan
	s.run(r, root)
	if len(s.records) != 0 {
		t.Fatalf("records on the first scan = %+v, want none", s.records)
	}
	for _, dir := range flood {
		if err := os.Remove(dir); err != nil {
			t.Fatal(err)
		}
	}

	rfMuseAppend(t, root, "a-session", rfMuseCallAt(t, "call_new", time.Now())...)
	s.run(r, root)
	if len(s.records) != 1 {
		t.Fatalf("records = %+v, want exactly the call made after the reader started", s.records)
	}
	if s.starts != 1 || s.finishes != 1 {
		t.Fatalf("shell activity: %d starts, %d finishes; want 1 and 1 (history is not activity)", s.starts, s.finishes)
	}
}

// TestRepeatFailureMuseDisplacedSessionsAreCapped: sessions a truncated walk
// leaves out are kept while their logs exist, but at most
// museDisplacedSessionsMax of them, newest names first, so an agent minting
// newer-named sessions every scan cannot grow the sessions map without bound.
func TestRepeatFailureMuseDisplacedSessionsAreCapped(t *testing.T) {
	root := rfMuseRoot(t)
	r := rfMuseReader()
	var s rfMuseScan
	const perDay = museDirEntriesMax
	rfMuseDay := func(d int) string { return fmt.Sprintf("%02d", d) }
	daysPerBatch := museSessionLogsMax / perDay
	// addDays creates perDay empty session logs for each day in [from, to].
	addDays := func(from, to int) {
		for d := from; d <= to; d++ {
			for i := 0; i < perDay; i++ {
				dir := filepath.Join(root, "2026", "10", rfMuseDay(d), "s"+strconv.Itoa(i))
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, museSessionLog), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	rel := func(d int) string {
		return filepath.Join("2026", "10", rfMuseDay(d), "s0", museSessionLog)
	}

	addDays(1, daysPerBatch)
	s.run(r, root)
	if len(r.sessions) != museSessionLogsMax {
		t.Fatalf("sessions after the first scan = %d, want %d", len(r.sessions), museSessionLogsMax)
	}

	// Newer days push the first batch out of the walk: it is displaced, not
	// gone, and all of it fits under the cap.
	addDays(daysPerBatch+1, 2*daysPerBatch)
	s.run(r, root)
	if len(r.sessions) != 2*museSessionLogsMax {
		t.Fatalf("sessions after the second scan = %d, want %d", len(r.sessions), 2*museSessionLogsMax)
	}
	if r.sessions[rel(1)] == nil {
		t.Fatalf("displaced session %q was dropped while its log still exists", rel(1))
	}

	// A third batch displaces the second: now twice the cap is displaced, so
	// the oldest-named half is forgotten and the map stays bounded.
	addDays(2*daysPerBatch+1, 3*daysPerBatch)
	s.run(r, root)
	if len(r.sessions) != museSessionLogsMax+museDisplacedSessionsMax {
		t.Fatalf("sessions after the third scan = %d, want %d", len(r.sessions), museSessionLogsMax+museDisplacedSessionsMax)
	}
	if r.sessions[rel(daysPerBatch+1)] == nil {
		t.Fatalf("newest displaced session %q was evicted before older ones", rel(daysPerBatch+1))
	}
	for d := 1; d <= daysPerBatch; d++ {
		if r.sessions[rel(d)] != nil {
			t.Fatalf("oldest displaced session %q survived past the cap", rel(d))
		}
	}
	if r.sessions[rel(3*daysPerBatch)] == nil {
		t.Fatalf("walked session %q is not followed", rel(3*daysPerBatch))
	}
}
