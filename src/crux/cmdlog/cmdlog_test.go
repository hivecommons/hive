package cmdlog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
)

// clock hands out readings one second apart so durations are exact.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

func newRecorder(p Policy) *Recorder {
	c := &clock{t: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	return &Recorder{Policy: p, Now: c.now}
}

func readAll(t *testing.T, dir string) api.CommandLog {
	t.Helper()
	page, err := Read(dir, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func TestRecordsAreWrittenByTheRecorderAndFoldedOnRead(t *testing.T) {
	dir := t.TempDir()
	r := newRecorder(Policy{})
	token := "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	seq, err := r.Started(dir, Start{Command: []string{"git", "push", "https://x:" + token + "@github.com/o/r"}, Cwd: "/ws/proj"})
	if err != nil || seq != 1 {
		t.Fatalf("Started = %d, %v", seq, err)
	}
	// Before the ended line the command is unfinished, with no end.
	page := readAll(t, dir)
	if len(page.Commands) != 1 || page.Commands[0].Status != api.CommandUnfinished || page.Commands[0].EndedAt != nil || page.Commands[0].Stdout != nil {
		t.Fatalf("open command = %+v", page.Commands)
	}
	if err := r.Ended(dir, seq, End{Status: api.CommandCompleted, ExitCode: new(0), Stdout: "pushed with " + token + "\n", Stderr: ""}); err != nil {
		t.Fatal(err)
	}
	page = readAll(t, dir)
	if len(page.Commands) != 1 || page.NextSeq != 2 || page.More {
		t.Fatalf("page = %+v", page)
	}
	rec := page.Commands[0]
	if rec.Seq != 1 || rec.EndSeq != 2 || rec.Status != api.CommandCompleted || rec.ExitCode == nil || *rec.ExitCode != 0 ||
		rec.Capture != api.CaptureRunner || rec.Cwd != "/ws/proj" || rec.DurationMs == nil || *rec.DurationMs != 1000 ||
		rec.EndedAt == nil || !rec.EndedAt.Equal(rec.StartedAt.Add(time.Second)) {
		t.Fatalf("record = %+v", rec)
	}
	raw, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) || !strings.Contains(strings.Join(rec.Command, " "), "git push") {
		t.Fatalf("token reached the log or the command was lost:\n%s", raw)
	}
	if rec.Stdout == nil || strings.Contains(rec.Stdout.Head, token) || rec.Stdout.Bytes != int64(len(rec.Stdout.Head)) || rec.Stdout.Truncated {
		t.Fatalf("stdout = %+v", rec.Stdout)
	}
	if rec.Stderr == nil || rec.Stderr.Bytes != 0 || rec.Stderr.Head != "" {
		t.Fatalf("stderr = %+v", rec.Stderr)
	}
	if lines := strings.Count(string(raw), "\n"); lines != 2 {
		t.Fatalf("log has %d lines, want one started and one ended:\n%s", lines, raw)
	}
}

func TestOutputKeepsHeadAndTailOnRuneBoundaries(t *testing.T) {
	dir := t.TempDir()
	r := newRecorder(Policy{HeadBytes: 4, TailBytes: 4})
	r.Scrub = func(s string) string { return s }
	// "é" is two bytes. Byte 4 is the second byte of the second "é", so
	// the head backs off to "aé"; the fourth byte from the end is the
	// second byte of the "é" before "a", so the tail moves on to "aé".
	text := "aéé-middle-éaé"
	seq, _ := r.Started(dir, Start{Command: []string{"x"}, Capture: "test"})
	if err := r.Ended(dir, seq, End{Status: api.CommandFailed, Reason: "timed out", Stdout: text, Stderr: "12345678"}); err != nil {
		t.Fatal(err)
	}
	rec := readAll(t, dir).Commands[0]
	if rec.Capture != "test" || rec.Status != api.CommandFailed || rec.ExitCode != nil || rec.Reason != "timed out" {
		t.Fatalf("record = %+v", rec)
	}
	out := rec.Stdout
	if out.Bytes != int64(len(text)) || out.Head != "aé" || out.Tail != "aé" || !out.Truncated {
		t.Fatalf("stdout = %+v", out)
	}
	// Exactly head+tail bytes is kept whole.
	if rec.Stderr.Head != "12345678" || rec.Stderr.Tail != "" || rec.Stderr.Truncated {
		t.Fatalf("stderr = %+v", rec.Stderr)
	}
}

func TestZeroRecorderUsesTheDefaultPolicy(t *testing.T) {
	dir := t.TempDir()
	var r Recorder
	seq, err := r.Started(dir, Start{Command: []string{"yes"}})
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("y\n", DefaultHeadBytes+1)
	if err := r.Ended(dir, seq, End{Status: api.CommandCancelled, Stdout: long}); err != nil {
		t.Fatal(err)
	}
	out := readAll(t, dir).Commands[0].Stdout
	if !out.Truncated || len(out.Head) != DefaultHeadBytes || len(out.Tail) != DefaultTailBytes || out.Bytes != int64(len(long)) {
		t.Fatalf("stdout head %d tail %d bytes %d truncated %v", len(out.Head), len(out.Tail), out.Bytes, out.Truncated)
	}
}

func TestSequenceContinuesAcrossRecorders(t *testing.T) {
	dir := t.TempDir()
	first := newRecorder(Policy{})
	a, _ := first.Started(dir, Start{Command: []string{"a"}})
	b, _ := first.Started(dir, Start{Command: []string{"b"}})
	if a != 1 || b != 2 {
		t.Fatalf("seqs %d %d", a, b)
	}
	if err := first.Ended(dir, b, End{Status: api.CommandCompleted, ExitCode: new(1)}); err != nil {
		t.Fatal(err)
	}
	if len(first.files) != 1 {
		t.Fatalf("state dropped while a is open: %v", first.files)
	}
	if err := first.Ended(dir, a, End{Status: api.CommandCompleted, ExitCode: new(0)}); err != nil {
		t.Fatal(err)
	}
	// Once nothing is open the state is dropped: the map holds live logs
	// only.
	if len(first.files) != 0 {
		t.Fatalf("first still holds %v", first.files)
	}
	// A second recorder (a restarted server) reads the file and continues
	// the sequence; it cannot end a command it did not start.
	second := newRecorder(Policy{})
	c, err := second.Started(dir, Start{Command: []string{"c"}})
	if err != nil || c != 5 {
		t.Fatalf("second recorder seq = %d, %v", c, err)
	}
	if err := second.Ended(dir, a, End{Status: api.CommandCompleted}); err == nil {
		t.Fatal("a recorder ended a command it did not start")
	}
	if _, err := second.Started(dir, Start{Command: []string{"d"}}); err != nil {
		t.Fatal(err)
	}
	if err := second.Ended(dir, c, End{Status: api.CommandFailed, Reason: "timed out"}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, rec := range readAll(t, dir).Commands {
		got = append(got, rec.Command[0]+":"+rec.Status)
	}
	// Ordered by latest line: b ended at 3, a at 4, d started at 6, c
	// ended at 7.
	if strings.Join(got, " ") != "b:completed a:completed d:unfinished c:failed" {
		t.Fatalf("order = %v", got)
	}
}

func TestPagingResurfacesACommandWhenItEnds(t *testing.T) {
	dir := t.TempDir()
	r := newRecorder(Policy{})
	one, _ := r.Started(dir, Start{Command: []string{"one"}})
	two, _ := r.Started(dir, Start{Command: []string{"two"}})
	_ = r.Ended(dir, two, End{Status: api.CommandCompleted, ExitCode: new(0)})

	page, err := Read(dir, 0, 1)
	if err != nil || len(page.Commands) != 1 || page.Commands[0].Seq != one || !page.More || page.NextSeq != 1 {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	page, err = Read(dir, page.NextSeq, 1)
	if err != nil || len(page.Commands) != 1 || page.Commands[0].Seq != two || page.More || page.NextSeq != 3 {
		t.Fatalf("second page = %+v, %v", page, err)
	}
	empty, err := Read(dir, page.NextSeq, 1)
	if err != nil || len(empty.Commands) != 0 || empty.Commands == nil || empty.NextSeq != 3 || empty.More {
		t.Fatalf("caught-up page = %+v, %v", empty, err)
	}
	_ = r.Ended(dir, one, End{Status: api.CommandCancelled})
	page, err = Read(dir, page.NextSeq, 1)
	if err != nil || len(page.Commands) != 1 || page.Commands[0].Seq != one || page.Commands[0].Status != api.CommandCancelled || page.NextSeq != 4 {
		t.Fatalf("resurfaced = %+v, %v", page, err)
	}
}

func TestReadLimitIsDefaultedAndCapped(t *testing.T) {
	dir := t.TempDir()
	r := newRecorder(Policy{})
	for range api.CmdlogMaxLimit + 1 {
		if _, err := r.Started(dir, Start{Command: []string{"x"}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ limit, want int }{{0, api.CmdlogDefaultLimit}, {-3, api.CmdlogDefaultLimit}, {api.CmdlogMaxLimit + 50, api.CmdlogMaxLimit}, {7, 7}} {
		page, err := Read(dir, 0, tc.limit)
		if err != nil || len(page.Commands) != tc.want || !page.More {
			t.Fatalf("limit %d: %d commands more=%v err=%v", tc.limit, len(page.Commands), page.More, err)
		}
	}
}

func TestMissingLogIsEmpty(t *testing.T) {
	page := readAll(t, filepath.Join(t.TempDir(), "never-ran"))
	if len(page.Commands) != 0 || page.NextSeq != 0 {
		t.Fatalf("page = %+v", page)
	}
}

func TestTornLastLineIsUncommitted(t *testing.T) {
	dir := t.TempDir()
	r := newRecorder(Policy{})
	if _, err := r.Started(dir, Start{Command: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(Path(dir), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"seq":2,"event":"ended","start_se`)
	_ = f.Close()
	page := readAll(t, dir)
	if len(page.Commands) != 1 || page.Commands[0].Status != api.CommandUnfinished {
		t.Fatalf("torn tail was folded: %+v", page)
	}
	if _, err := newRecorder(Policy{}).Started(dir, Start{Command: []string{"b"}}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("append after a torn line = %v", err)
	}
	// A file holding nothing but a torn line is an empty, uncommitted log.
	only := t.TempDir()
	if err := os.WriteFile(Path(only), []byte(`{"seq":1`), 0o600); err != nil {
		t.Fatal(err)
	}
	if page := readAll(t, only); len(page.Commands) != 0 {
		t.Fatalf("torn-only log = %+v", page)
	}
}

func TestCorruptLogsAreRefused(t *testing.T) {
	started := `{"seq":1,"event":"started","at":"2026-11-08T12:00:00Z","command":["a"],"capture":"runner"}`
	for name, body := range map[string]string{
		"garbage line":        "not json\n",
		"seq goes backwards":  started + "\n" + `{"seq":1,"event":"started","at":"2026-11-08T12:00:00Z","command":["b"]}` + "\n",
		"ended without start": `{"seq":1,"event":"ended","start_seq":9,"status":"completed"}` + "\n",
		"ended twice": started + "\n" + `{"seq":2,"event":"ended","start_seq":1,"status":"completed"}` + "\n" +
			`{"seq":3,"event":"ended","start_seq":1,"status":"failed"}` + "\n",
		"unknown event": `{"seq":1,"event":"forked"}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(Path(dir), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Read(dir, 0, 0); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Read = %v", err)
			}
		})
	}
	// The recorder only needs the sequence, but it will not append after a
	// line it cannot parse either.
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newRecorder(Policy{}).Started(dir, Start{Command: []string{"a"}}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Started on a corrupt log = %v", err)
	}
}

func TestRecorderRefusals(t *testing.T) {
	dir := t.TempDir()
	r := newRecorder(Policy{})
	if _, err := r.Started(dir, Start{}); err == nil {
		t.Fatal("started a command without a command line")
	}
	if err := r.Ended(dir, 1, End{Status: api.CommandCompleted}); err == nil {
		t.Fatal("ended a command that was never started")
	}
	seq, _ := r.Started(dir, Start{Command: []string{"a"}})
	if err := r.Ended(dir, seq, End{Status: "succeeded"}); err == nil {
		t.Fatal("accepted a status outside the closed set")
	}
	if err := r.Ended(dir, seq+7, End{Status: api.CommandCompleted}); err == nil {
		t.Fatal("ended an unknown seq of an open log")
	}
	if err := r.Ended(dir, seq, End{Status: api.CommandCompleted}); err != nil {
		t.Fatal(err)
	}
}

func TestStorageFailures(t *testing.T) {
	r := newRecorder(Policy{})
	// The log path is a directory: it can be neither read nor appended.
	dir := t.TempDir()
	if err := os.Mkdir(Path(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Started(dir, Start{Command: []string{"a"}}); err == nil || errors.Is(err, ErrCorrupt) {
		t.Fatalf("Started over a directory = %v", err)
	}
	if _, err := Read(dir, 0, 0); err == nil {
		t.Fatal("Read over a directory succeeded")
	}

	// The run directory does not exist: nothing to read, nothing appended,
	// and a failed first start leaves no state behind.
	gone := filepath.Join(t.TempDir(), "gone")
	if _, err := r.Started(gone, Start{Command: []string{"a"}}); err == nil {
		t.Fatal("Started in a missing directory succeeded")
	}
	if len(r.files) != 0 {
		t.Fatalf("failed start kept state: %v", r.files)
	}

	// An ended line that cannot be written still closes the command.
	live := t.TempDir()
	seq, err := r.Started(live, Start{Command: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(Path(live)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(Path(live), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := r.Ended(live, seq, End{Status: api.CommandCompleted}); err == nil {
		t.Fatal("Ended over a directory succeeded")
	}
	if len(r.files) != 0 {
		t.Fatalf("closed command kept state: %v", r.files)
	}

	// A line that cannot be encoded (a timestamp past year 9999) is not
	// written and does not advance the sequence.
	far := &Recorder{Now: func() time.Time { return time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) }}
	if _, err := far.Started(t.TempDir(), Start{Command: []string{"a"}}); err == nil {
		t.Fatal("encoded an out-of-range timestamp")
	}
}
