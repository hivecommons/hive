package cmdlog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
)

// clock is a test clock that advances by a fixed step per call.
type clock struct {
	now  time.Time
	step time.Duration
}

func (c *clock) tick() time.Time {
	t := c.now
	c.now = c.now.Add(c.step)
	return t
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC), step: 250 * time.Millisecond}
}

func intp(v int) *int { return &v }

func openLog(t *testing.T, dir string, pol Policy) *Log {
	t.Helper()
	l, err := Open(dir, pol)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestStartedEndedFoldIntoOneRecord(t *testing.T) {
	dir := t.TempDir()
	c := newClock()
	l := openLog(t, dir, Policy{Now: c.tick})
	seq, err := l.Started(Start{Command: []string{"make", "test"}, Cwd: "/workspace"})
	if err != nil || seq != 1 {
		t.Fatalf("Started = %d, %v", seq, err)
	}
	if err := l.Ended(seq, End{ExitCode: intp(0), Stdout: "ok\n", Stderr: ""}); err != nil {
		t.Fatal(err)
	}
	page, err := Read(dir, 0, 0)
	if err != nil || len(page.Commands) != 1 || page.More || page.NextSeq != 1 {
		t.Fatalf("Read = %+v, %v", page, err)
	}
	rec := page.Commands[0]
	if rec.Seq != 1 || strings.Join(rec.Command, " ") != "make test" || rec.Cwd != "/workspace" || rec.Capture != api.CommandCaptureRunner {
		t.Fatalf("record = %+v", rec)
	}
	if rec.Status != api.CommandCompleted || rec.ExitCode == nil || *rec.ExitCode != 0 || rec.EndedAt == nil || rec.DurationMs == nil || *rec.DurationMs != 250 {
		t.Fatalf("outcome = %+v", rec)
	}
	if rec.Stdout == nil || rec.Stdout.Bytes != 3 || rec.Stdout.Head != "ok\n" || rec.Stdout.Truncated || rec.Stderr == nil || rec.Stderr.Bytes != 0 {
		t.Fatalf("output = %+v / %+v", rec.Stdout, rec.Stderr)
	}
	if st, err := os.Stat(filepath.Join(dir, FileName)); err != nil || st.Mode().Perm() != fileMode {
		t.Fatalf("mode = %v, %v", st, err)
	}
}

func TestUnendedCommandIsUnfinishedAndNeverGuessed(t *testing.T) {
	dir := t.TempDir()
	l := openLog(t, dir, Policy{})
	if _, err := l.Started(Start{Command: []string{"sleep", "forever"}}); err != nil {
		t.Fatal(err)
	}
	page, err := Read(dir, 0, 0)
	if err != nil || len(page.Commands) != 1 {
		t.Fatalf("Read = %+v, %v", page, err)
	}
	rec := page.Commands[0]
	if rec.Status != api.CommandUnfinished || rec.ExitCode != nil || rec.EndedAt != nil || rec.DurationMs != nil {
		t.Fatalf("unfinished = %+v", rec)
	}
}

func TestStatusDerivesFromExitUnlessNamed(t *testing.T) {
	for _, tc := range []struct {
		end  End
		want string
	}{
		{End{ExitCode: intp(0)}, api.CommandCompleted},
		{End{ExitCode: intp(2)}, api.CommandFailed},
		{End{}, api.CommandUnfinished},
		{End{ExitCode: intp(0), Status: api.CommandCancelled}, api.CommandCancelled},
	} {
		if got := deriveStatus(tc.end); got != tc.want {
			t.Errorf("deriveStatus(%+v) = %q want %q", tc.end, got, tc.want)
		}
	}
}

func TestEndedRefusesAnUnknownSeq(t *testing.T) {
	l := openLog(t, t.TempDir(), Policy{})
	if err := l.Ended(7, End{}); !errors.Is(err, ErrUnknownSeq) {
		t.Fatalf("Ended(7) = %v", err)
	}
}

func TestOpenResumesTheSeqCounter(t *testing.T) {
	dir := t.TempDir()
	first := openLog(t, dir, Policy{})
	for range 3 {
		if _, err := first.Started(Start{Command: []string{"x"}}); err != nil {
			t.Fatal(err)
		}
	}
	second := openLog(t, dir, Policy{})
	seq, err := second.Started(Start{Command: []string{"y"}})
	if err != nil || seq != 4 {
		t.Fatalf("resumed seq = %d, %v", seq, err)
	}
	// Ended still refuses a seq this log never started, even one Open ended.
	if err := second.Ended(1, End{}); !errors.Is(err, ErrUnknownSeq) {
		t.Fatalf("Ended(1) on resumed = %v", err)
	}
}

// A started line no writer will ever end (its runner died) is ended
// unfinished by the next Open, so it cannot pin NextSeq for the rest of
// the run (#17410).
func TestOpenEndsACommandAnEarlierWriterLeftOpen(t *testing.T) {
	dir := t.TempDir()
	c := newClock()
	first := openLog(t, dir, Policy{Now: c.tick})
	orphan, err := first.Started(Start{Command: []string{"make", "test"}})
	if err != nil {
		t.Fatal(err)
	}
	seq, err := first.Started(Start{Command: []string{"echo"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Ended(seq, End{ExitCode: intp(0)}); err != nil {
		t.Fatal(err)
	}
	second := openLog(t, dir, Policy{Now: c.tick})
	page, err := Read(dir, 0, 0)
	if err != nil || len(page.Commands) != 2 || page.NextSeq != seq || page.More {
		t.Fatalf("page = %+v, %v", page, err)
	}
	rec := page.Commands[0]
	if rec.Seq != orphan || rec.Status != api.CommandUnfinished || rec.EndedAt == nil || rec.ExitCode != nil || rec.Reason != OrphanReason || rec.DurationMs == nil || *rec.DurationMs != 750 {
		t.Fatalf("orphan = %+v", rec)
	}
	if page.Commands[1].Status != api.CommandCompleted {
		t.Fatalf("ended command = %+v", page.Commands[1])
	}
	// The orphan is ended once: reopening again appends nothing.
	path := filepath.Join(dir, FileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	openLog(t, dir, Policy{})
	if after, err := os.ReadFile(path); err != nil || string(after) != string(before) {
		t.Fatalf("second reopen rewrote the log: %q, %v", after, err)
	}
	if next, err := second.Started(Start{Command: []string{"y"}}); err != nil || next != seq+1 {
		t.Fatalf("resumed seq = %d, %v", next, err)
	}
}

func TestReadPagesWithSinceSeqAndLimit(t *testing.T) {
	dir := t.TempDir()
	l := openLog(t, dir, Policy{})
	for i := range 5 {
		seq, err := l.Started(Start{Command: []string{"cmd", string(rune('a' + i))}})
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Ended(seq, End{ExitCode: intp(i)}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := Read(dir, 0, 2)
	if err != nil || len(page.Commands) != 2 || !page.More || page.NextSeq != 2 || page.Commands[0].Seq != 1 || page.Commands[1].Seq != 2 {
		t.Fatalf("page 1 = %+v, %v", page, err)
	}
	page, err = Read(dir, page.NextSeq, 2)
	if err != nil || len(page.Commands) != 2 || !page.More || page.NextSeq != 4 || page.Commands[0].Seq != 3 {
		t.Fatalf("page 2 = %+v, %v", page, err)
	}
	page, err = Read(dir, page.NextSeq, 2)
	if err != nil || len(page.Commands) != 1 || page.More || page.NextSeq != 5 || page.Commands[0].Seq != 5 {
		t.Fatalf("page 3 = %+v, %v", page, err)
	}
	// Past the end: an empty page keeps the cursor where it was.
	page, err = Read(dir, 5, 0)
	if err != nil || len(page.Commands) != 0 || page.More || page.NextSeq != 5 {
		t.Fatalf("empty page = %+v, %v", page, err)
	}
	// An oversized limit is capped, not refused.
	if page, err = Read(dir, 0, MaxPageSize*10); err != nil || len(page.Commands) != 5 {
		t.Fatalf("capped = %+v, %v", page, err)
	}
}

func TestReadFollowerSeesEndedLineOfUnfinishedCommand(t *testing.T) {
	dir := t.TempDir()
	l := openLog(t, dir, Policy{})
	seq, err := l.Started(Start{Command: []string{"make", "test"}})
	if err != nil {
		t.Fatal(err)
	}
	page, err := Read(dir, 0, 0)
	if err != nil || len(page.Commands) != 1 || page.Commands[0].Status != api.CommandUnfinished || page.NextSeq != 0 {
		t.Fatalf("unfinished page = %+v, %v", page, err)
	}
	if err := l.Ended(seq, End{ExitCode: intp(0)}); err != nil {
		t.Fatal(err)
	}
	page, err = Read(dir, page.NextSeq, 0)
	if err != nil || len(page.Commands) != 1 || page.Commands[0].Status != api.CommandCompleted || page.NextSeq != seq {
		t.Fatalf("terminal page = %+v, %v", page, err)
	}
}

func TestReadNextSeqStopsBeforeFirstUnfinished(t *testing.T) {
	dir := t.TempDir()
	l := openLog(t, dir, Policy{})
	for i := range 3 {
		seq, err := l.Started(Start{Command: []string{"cmd", string(rune('a' + i))}})
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			continue
		}
		if err := l.Ended(seq, End{ExitCode: intp(0)}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := Read(dir, 0, 0)
	if err != nil || len(page.Commands) != 3 || page.NextSeq != 1 || page.Commands[1].Status != api.CommandUnfinished {
		t.Fatalf("mixed page = %+v, %v", page, err)
	}
}

// An ended line makes a record terminal whatever its status: a command
// ended unfinished (exit never known) must not hold NextSeq back (#17410).
func TestReadNextSeqPassesAnEndedUnfinishedRecord(t *testing.T) {
	dir := t.TempDir()
	l := openLog(t, dir, Policy{})
	for i := range 4 {
		seq, err := l.Started(Start{Command: []string{"cmd", string(rune('a' + i))}})
		if err != nil {
			t.Fatal(err)
		}
		end := End{ExitCode: intp(0)}
		if i == 1 {
			end = End{Reason: "launcher error"}
		}
		if err := l.Ended(seq, end); err != nil {
			t.Fatal(err)
		}
	}
	page, err := Read(dir, 0, 0)
	if err != nil || len(page.Commands) != 4 || page.NextSeq != 4 || page.More {
		t.Fatalf("page = %+v, %v", page, err)
	}
	if rec := page.Commands[1]; rec.Status != api.CommandUnfinished || rec.EndedAt == nil || rec.ExitCode != nil {
		t.Fatalf("ended unfinished = %+v", rec)
	}
	page, err = Read(dir, 0, 2)
	if err != nil || len(page.Commands) != 2 || page.NextSeq != 2 || !page.More {
		t.Fatalf("page 1 = %+v, %v", page, err)
	}
	page, err = Read(dir, page.NextSeq, 2)
	if err != nil || len(page.Commands) != 2 || page.NextSeq != 4 || page.More {
		t.Fatalf("page 2 = %+v, %v", page, err)
	}
}

// More is set only on a page whose cursor moved: with an in-flight command
// first on the page, the documented "page while More" loop would otherwise
// re-request the same since_seq forever (#17410).
func TestReadMoreOnlyWhenNextSeqAdvances(t *testing.T) {
	dir := t.TempDir()
	l := openLog(t, dir, Policy{})
	for i := range 6 {
		seq, err := l.Started(Start{Command: []string{"cmd", string(rune('a' + i))}})
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			continue
		}
		if err := l.Ended(seq, End{ExitCode: intp(0)}); err != nil {
			t.Fatal(err)
		}
	}
	// Seq 2 is in flight. The first page still moves the cursor past seq 1.
	page, err := Read(dir, 0, 2)
	if err != nil || len(page.Commands) != 2 || page.NextSeq != 1 || !page.More {
		t.Fatalf("page 1 = %+v, %v", page, err)
	}
	// From there seq 2 leads the page and the cursor cannot move.
	page, err = Read(dir, page.NextSeq, 2)
	if err != nil || len(page.Commands) != 2 || page.Commands[0].Seq != 2 || page.NextSeq != 1 || page.More {
		t.Fatalf("pinned page = %+v, %v", page, err)
	}
}

func TestOutputKeepsHeadAndTailPerPolicy(t *testing.T) {
	dir := t.TempDir()
	l := openLog(t, dir, Policy{HeadBytes: 4, TailBytes: 3})
	seq, _ := l.Started(Start{Command: []string{"x"}})
	if err := l.Ended(seq, End{ExitCode: intp(0), Stdout: "0123456789", Stderr: "short"}); err != nil {
		t.Fatal(err)
	}
	page, err := Read(dir, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := page.Commands[0].Stdout
	if out.Bytes != 10 || out.Head != "0123" || out.Tail != "789" || !out.Truncated {
		t.Fatalf("stdout = %+v", out)
	}
	if e := page.Commands[0].Stderr; e.Bytes != 5 || e.Head != "short" || e.Tail != "" || e.Truncated {
		t.Fatalf("stderr = %+v", e)
	}
}

func TestNegativeWindowsKeepNothing(t *testing.T) {
	out := summarize("abcdef", Policy{HeadBytes: -1, TailBytes: -1})
	if out.Bytes != 6 || out.Head != "" || out.Tail != "" || !out.Truncated {
		t.Fatalf("summarize = %+v", out)
	}
}

func TestSecretsAreScrubbedFromCommandAndOutput(t *testing.T) {
	dir := t.TempDir()
	const token = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"
	l := openLog(t, dir, Policy{})
	seq, err := l.Started(Start{Command: []string{"curl", "-H", "Authorization: Bearer " + token}})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Ended(seq, End{ExitCode: intp(1), Reason: "leaked " + token, Stdout: "token=" + token, Stderr: token}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatalf("token reached the file:\n%s", raw)
	}
}

func TestCustomScrubAppliesEverywhere(t *testing.T) {
	dir := t.TempDir()
	l := openLog(t, dir, Policy{Scrub: func(s string) string { return strings.ReplaceAll(s, "hush", "***") }})
	seq, _ := l.Started(Start{Command: []string{"say", "hush"}})
	_ = l.Ended(seq, End{ExitCode: intp(0), Stdout: "hush out", Stderr: "hush err", Reason: "hush why"})
	page, err := Read(dir, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	rec := page.Commands[0]
	if rec.Command[1] != "***" || rec.Stdout.Head != "*** out" || rec.Stderr.Head != "*** err" || rec.Reason != "*** why" {
		t.Fatalf("scrub missed: %+v", rec)
	}
}

func TestReadErrors(t *testing.T) {
	if _, err := Read(t.TempDir(), 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file = %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{\"seq\":1,\"event\":\"started\"}\nnot json\n"), fileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir, 0, 0); !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("corrupt = %v", err)
	}
	if _, err := Open(dir, Policy{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open over corrupt = %v", err)
	}
	// An ended line with no started line is dropped, not invented.
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("\n{\"seq\":9,\"event\":\"ended\",\"status\":\"completed\"}\n"), fileMode); err != nil {
		t.Fatal(err)
	}
	if page, err := Read(dir, 0, 0); err != nil || len(page.Commands) != 0 {
		t.Fatalf("orphan ended = %+v, %v", page, err)
	}
}

func TestAppendFailsWhenTheDirectoryIsGone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	l := openLog(t, dir, Policy{})
	if _, err := l.Started(Start{Command: []string{"x"}}); err == nil {
		t.Fatal("Started into a missing directory succeeded")
	}
}

func TestFileOpenerBuildsALogForTheRunDir(t *testing.T) {
	dir := t.TempDir()
	rec, err := FileOpener(Policy{})(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rec.Started(Start{Command: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Fatal(err)
	}
}
