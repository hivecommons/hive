package logread

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a settable clock for the output log's stamps, rotation names
// and age bound.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func openTestOutput(t *testing.T, dir, stem string, clock *fakeClock) *OutputLog {
	t.Helper()
	l, err := openOutputLog(dir, stem, clock.now)
	if err != nil {
		t.Fatalf("openOutputLog: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func listNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func decodeRecords(t *testing.T, path string) []outputRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []outputRecord
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var r outputRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("record %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

func TestOutputDir(t *testing.T) {
	if got := OutputDir(nil); got != DefaultOutputDir {
		t.Fatalf("OutputDir(nil) = %q", got)
	}
	if got := OutputDir(func(string) string { return "  " }); got != DefaultOutputDir {
		t.Fatalf("blank override = %q", got)
	}
	env := map[string]string{OutputDirEnv: " /srv/logs "}
	if got := OutputDir(func(k string) string { return env[k] }); got != "/srv/logs" {
		t.Fatalf("override = %q", got)
	}
}

func TestOpenOutputLogRefusals(t *testing.T) {
	if _, err := OpenOutputLog("", SourceProxy); err == nil {
		t.Fatal("an empty directory was accepted")
	}
	if _, err := OpenOutputLog(t.TempDir(), " "); err == nil {
		t.Fatal("an empty name was accepted")
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenOutputLog(filepath.Join(blocker, "logs"), SourceProxy); err == nil {
		t.Fatal("a directory under a file was accepted")
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, SourceProxy+".log"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenOutputLog(dir, SourceProxy); err == nil {
		t.Fatal("a directory in place of the current file was accepted")
	}
}

// AC6 (permissions): the directory is 0700 and every file 0600, even when the
// directory or the file already existed with wider modes.
func TestOutputLogModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(dir, SourceLiteLLM+".log")
	if err := os.WriteFile(current, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{t: time.Now()}
	l := openTestOutput(t, dir, SourceLiteLLM, clock)
	l.maxBytes = 64
	w := l.Stream(StreamStdout)
	for i := 0; i < 5; i++ {
		fmt.Fprintf(w, "line %d with some padding\n", i)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v (%v), want 0700", st.Mode().Perm(), err)
	}
	names := listNames(t, dir)
	if len(names) < 2 {
		t.Fatalf("expected rotated history, got %q", names)
	}
	for _, name := range names {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v (%v), want 0600", name, st.Mode().Perm(), err)
		}
	}
}

// Stream writers stamp each line with its time and stream, split runaway lines,
// trim CR, keep partial lines until they end (or the log closes), and never
// fail even once the log is closed.
func TestOutputLogStream(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{t: base}
	l := openTestOutput(t, dir, SourceProxy, clock)
	out, errw := l.Stream(StreamStdout), l.Stream(StreamStderr)

	fmt.Fprint(out, "proxy listening")
	fmt.Fprint(errw, "warn: no dashboard token\r\n")
	clock.advance(time.Second)
	fmt.Fprint(out, " on :3001\nsecond\n")
	if n, err := out.Write([]byte(strings.Repeat("x", outputLineMaxBytes+10))); err != nil || n != outputLineMaxBytes+10 {
		t.Fatalf("long write = %d, %v", n, err)
	}
	fmt.Fprint(errw, "unfinished")
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if n, err := out.Write([]byte("after close\n")); err != nil || n != len("after close\n") {
		t.Fatalf("write after close = %d, %v; a stream writer must never fail", n, err)
	}
	if _, err := l.Write([]byte("raw\n")); err == nil {
		t.Fatal("raw write after close succeeded")
	}

	recsOut := decodeRecords(t, filepath.Join(dir, SourceProxy+".log"))
	got := make([]string, 0, len(recsOut))
	for _, r := range recsOut {
		got = append(got, fmt.Sprintf("%s|%s|%d", r.Stream, r.Time, len(r.Text)))
	}
	if len(recsOut) != 6 {
		t.Fatalf("records = %q", got)
	}
	if recsOut[0].Stream != StreamStderr || recsOut[0].Text != "warn: no dashboard token" || recsOut[0].Time != base.Format(time.RFC3339Nano) {
		t.Fatalf("stderr record = %+v", recsOut[0])
	}
	if recsOut[1].Stream != StreamStdout || recsOut[1].Text != "proxy listening on :3001" || recsOut[1].Time != base.Add(time.Second).Format(time.RFC3339Nano) {
		t.Fatalf("stdout record = %+v", recsOut[1])
	}
	if recsOut[2].Text != "second" || len(recsOut[3].Text) != outputLineMaxBytes {
		t.Fatalf("records = %q", got)
	}
	// The remainder of the long line and the unfinished stderr line are kept
	// at Close.
	if len(recsOut[4].Text) != 10 || recsOut[5].Text != "unfinished" || recsOut[5].Stream != StreamStderr {
		t.Fatalf("records kept at close = %q", got)
	}
}

// AC2 shape: a later open appends, so an earlier run's output stays readable.
func TestOutputLogAppendsAcrossOpens(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{t: time.Now()}
	first := openTestOutput(t, dir, SourceHiveOutput, clock)
	fmt.Fprintln(first.Stream(StreamStdout), "failed to load config")
	_ = first.Close()
	second := openTestOutput(t, dir, SourceHiveOutput, clock)
	fmt.Fprintln(second.Stream(StreamStdout), "boot ok")
	_ = second.Close()
	recs := decodeRecords(t, filepath.Join(dir, SourceHiveOutput+".log"))
	if len(recs) != 2 || recs[0].Text != "failed to load config" || recs[1].Text != "boot ok" {
		t.Fatalf("records = %+v", recs)
	}
}

// AC6 (bounds): more than the size bound rotates, history beyond the file
// count is removed oldest first, and history older than the age bound is
// removed, as is a current file whose newest line is that old.
func TestOutputLogBounds(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{t: time.Now()}
	l := openTestOutput(t, dir, SourceRequests, clock)
	l.maxBytes = 100
	record := strings.Repeat("r", 59) + "\n" // two records exceed the bound
	for i := 0; i < 12; i++ {
		if _, err := l.Write([]byte(record)); err != nil {
			t.Fatal(err)
		}
		if i%3 == 0 {
			clock.advance(time.Minute)
		}
	}
	files, err := historyFiles(dir, SourceRequests)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != OutputMaxFiles || !files[0].current {
		t.Fatalf("files after rotation = %q, want %d with the current first", listNames(t, dir), OutputMaxFiles)
	}
	// Two rotations at the same instant still get distinct names.
	stamps := map[time.Time]bool{}
	for _, f := range files[1:] {
		if stamps[f.stamp] {
			t.Fatalf("two backups share stamp %v: %q", f.stamp, listNames(t, dir))
		}
		stamps[f.stamp] = true
	}
	if st, err := os.Stat(filepath.Join(dir, SourceRequests+".log")); err != nil || st.Size() > l.maxBytes {
		t.Fatalf("current file size %v (%v), over the bound", st.Size(), err)
	}

	// Eight days later every backup is past the age bound; so is the current
	// file, whose newest line was written then.
	old := clock.now()
	if err := os.Chtimes(filepath.Join(dir, SourceRequests+".log"), old, old); err != nil {
		t.Fatal(err)
	}
	clock.advance(8 * 24 * time.Hour)
	if _, err := l.Write([]byte("fresh\n")); err != nil {
		t.Fatal(err)
	}
	if names := listNames(t, dir); len(names) != 1 || names[0] != SourceRequests+".log" {
		t.Fatalf("after the age bound: %q, want only the current file", names)
	}
	data, err := os.ReadFile(filepath.Join(dir, SourceRequests+".log"))
	if err != nil || string(data) != "fresh\n" {
		t.Fatalf("current file after the age bound = %q (%v)", data, err)
	}
}

// A stale current file is emptied when the log opens, and history past the
// bounds left by an earlier run is pruned then too.
func TestOutputLogOpenPrunes(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	current := filepath.Join(dir, SourceProxy+".log")
	if err := os.WriteFile(current, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := now.Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(current, stale, stale); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 7; i++ {
		name := SourceProxy + "-" + now.Add(-time.Duration(i)*time.Hour).UTC().Format(backupTimeFormat) + ".log"
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ancient := SourceProxy + "-" + stale.UTC().Format(backupTimeFormat) + ".log"
	if err := os.WriteFile(filepath.Join(dir, ancient), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := openTestOutput(t, dir, SourceProxy, &fakeClock{t: now})
	if l.size != 0 {
		t.Fatalf("stale current file kept %d bytes", l.size)
	}
	names := listNames(t, dir)
	if len(names) != OutputMaxFiles {
		t.Fatalf("after open: %q, want %d files", names, OutputMaxFiles)
	}
	for _, name := range names {
		if name == ancient {
			t.Fatalf("history past the age bound survived: %q", names)
		}
	}
}

// A rotation that cannot reopen the current file reports it, and the log
// stays closed rather than writing somewhere unexpected.
func TestOutputLogRotateFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	l := openTestOutput(t, dir, SourceLiteLLM, &fakeClock{t: time.Now()})
	l.maxBytes = 10
	if _, err := l.Write([]byte("0123456789\n")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Write([]byte("more\n")); err == nil {
		t.Fatal("rotation into a removed directory succeeded")
	}
	if _, err := l.Write([]byte("again\n")); err == nil {
		t.Fatal("a log whose rotation failed kept writing")
	}
}

func TestParseStampedAndLineHeads(t *testing.T) {
	when := base.Add(3 * time.Second)
	stamped := func(stream, text string) string {
		b, _ := json.Marshal(outputRecord{Time: when.Format(time.RFC3339Nano), Stream: stream, Text: text})
		return string(b)
	}
	lines := []string{
		stamped(StreamStderr, "plain text"),
		stamped("tty", rec(9, "ERROR", "early failure")),
		`{"time":"not a time","text":"x"}`,
		"not json at all",
		rec(4, "WARN", "slog record"),
	}
	heads := lineHeads(lines, true)
	if heads[0].stream != StreamStderr || !heads[0].when.Equal(when) || heads[0].level != "" || lines[0] != "plain text" {
		t.Fatalf("stderr line: %+v %q", heads[0], lines[0])
	}
	if heads[1].stream != "" || heads[1].level != "error" || !strings.Contains(lines[1], "early failure") {
		t.Fatalf("unknown stream / slog text: %+v %q", heads[1], lines[1])
	}
	if !heads[2].when.IsZero() || lines[2] != `{"time":"not a time","text":"x"}` {
		t.Fatalf("bad time kept raw: %+v %q", heads[2], lines[2])
	}
	if !heads[3].when.IsZero() || lines[3] != "not json at all" {
		t.Fatalf("non-JSON kept raw: %+v %q", heads[3], lines[3])
	}
	if heads[4].level != "warn" || heads[4].when.IsZero() {
		t.Fatalf("slog fallback: %+v", heads[4])
	}
	raw := []string{stamped(StreamStdout, "x")}
	if h := lineHeads(raw, false); h[0].stream != "" || !strings.HasPrefix(raw[0], "{") {
		t.Fatalf("unstamped source decoded a record: %+v %q", h[0], raw[0])
	}
}

// AC3/AC4: availability of each source, with reasons.
func TestOutputSourcesAvailability(t *testing.T) {
	logDir := t.TempDir()
	writePlain(t, logDir, HiveLogFilename, recs(0, 1))
	byName := func(opts Options) map[string]SourceInfo {
		out := map[string]SourceInfo{}
		for _, info := range Sources(opts).Sources {
			out[info.Name] = info
		}
		return out
	}

	hub := byName(Options{LogDir: logDir, OutputDir: t.TempDir()})
	if hub[SourceProxy].Available || !strings.Contains(hub[SourceProxy].Reason, "auth proxy") {
		t.Fatalf("proxy without the auth proxy = %+v", hub[SourceProxy])
	}
	if hub[SourceLiteLLM].Available || !strings.Contains(hub[SourceLiteLLM].Reason, "local_proxy") {
		t.Fatalf("litellm when disabled = %+v", hub[SourceLiteLLM])
	}
	for _, name := range []string{SourceHiveOutput, SourceRequests} {
		if !hub[name].Available || hub[name].Description == "" {
			t.Fatalf("%s = %+v, want available everywhere", name, hub[name])
		}
	}
	res := Read(Request{Source: SourceLiteLLM}, Options{OutputDir: t.TempDir()})
	if res.Status != StatusNotAvailable || res.Reason != hub[SourceLiteLLM].Reason {
		t.Fatalf("read of disabled litellm = %+v", res)
	}

	spoke := byName(Options{LogDir: logDir, OutputDir: t.TempDir(), AuthProxyInstalled: true, LocalLiteLLM: true})
	for _, name := range []string{SourceHive, SourceProxy, SourceHiveOutput, SourceLiteLLM, SourceRequests} {
		if !spoke[name].Available {
			t.Fatalf("spoke %s = %+v", name, spoke[name])
		}
	}

	unset := byName(Options{AuthProxyInstalled: true, LocalLiteLLM: true})
	for _, name := range []string{SourceProxy, SourceHiveOutput, SourceLiteLLM, SourceRequests} {
		if unset[name].Available || unset[name].Reason == "" {
			t.Fatalf("%s with no output directory = %+v", name, unset[name])
		}
	}
}

// An output source whose process has not written yet reads as empty, not as
// an error; a directory that cannot be listed is not available.
func TestOutputSourceBeforeFirstLine(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	for _, dir := range []string{missing, t.TempDir()} {
		res := Read(Request{Source: SourceRequests}, Options{OutputDir: dir})
		if res.Status != StatusOK || res.LineCount != 0 || !res.Coverage.Complete {
			t.Fatalf("%s: %+v", dir, res)
		}
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	res := Read(Request{Source: SourceHiveOutput}, Options{OutputDir: blocker})
	if res.Status != StatusNotAvailable || res.Reason == "" {
		t.Fatalf("unlistable directory: %+v", res)
	}
}

// AC1/AC2/AC3 end to end: what a stream writer keeps reads back by source
// with time, stream and text (and level when the text is slog JSON), across
// rotated files, scrubbed like every other source.
func TestOutputSourceReadBack(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{t: time.Now().UTC().Add(-time.Hour)}
	l := openTestOutput(t, dir, SourceProxy, clock)
	l.maxBytes = 1200
	out, errw := l.Stream(StreamStdout), l.Stream(StreamStderr)
	fmt.Fprintln(out, "[hive-proxy] Dashboard proxy on :3001")
	clock.advance(time.Second)
	fmt.Fprintln(errw, "[SECURITY] HIVE_DASHBOARD_TOKEN is not set")
	for i := 0; i < 20; i++ {
		clock.advance(time.Second)
		fmt.Fprintf(out, "request %02d Authorization: Basic aGl2ZTpzdXBlci1zZWNyZXQtdmFsdWU=\n", i)
	}
	_ = l.Close()
	if files, _ := historyFiles(dir, SourceProxy); len(files) < 2 {
		t.Fatalf("expected rotated proxy history, got %q", listNames(t, dir))
	}

	opts := Options{OutputDir: dir, AuthProxyInstalled: true}
	all, _ := readAll(t, Request{Source: SourceProxy, Lines: 3}, opts)
	if len(all) != 22 || all[0] != "[hive-proxy] Dashboard proxy on :3001" {
		t.Fatalf("paged read back %d lines: %q", len(all), all)
	}
	res := Read(Request{Source: SourceProxy, Include: "SECURITY"}, opts)
	if res.Status != StatusOK || res.LineCount != 1 {
		t.Fatalf("include SECURITY: %+v", res)
	}
	line := res.Lines[0]
	if line.Stream != StreamStderr || line.Source != SourceProxy || line.Time == "" || line.Text != "[SECURITY] HIVE_DASHBOARD_TOKEN is not set" {
		t.Fatalf("stderr line = %+v", line)
	}
	res = Read(Request{Source: SourceProxy, Lines: 1}, opts)
	if res.LineCount != 1 || res.Lines[0].Stream != StreamStdout || strings.Contains(res.Lines[0].Text, "aGl2ZTpzdXBlci1zZWNyZXQtdmFsdWU=") || len(res.Redactions) == 0 {
		t.Fatalf("newest line not scrubbed: %+v", res)
	}

	// hive-output carries slog JSON, so its lines have a level to filter on.
	hdir := t.TempDir()
	h := openTestOutput(t, hdir, SourceHiveOutput, clock)
	fmt.Fprintln(h.Stream(StreamStdout), rec(0, "ERROR", "failed to load config"))
	fmt.Fprintln(h.Stream(StreamStdout), "plain line with no level")
	_ = h.Close()
	res = Read(Request{Source: SourceHiveOutput, MinLevel: "error"}, Options{OutputDir: hdir})
	if res.LineCount != 1 || res.Lines[0].Level != "error" || !strings.Contains(res.Lines[0].Text, "failed to load config") || res.UnlevelledDropped != 1 {
		t.Fatalf("hive-output min_level: %+v", res)
	}
}
