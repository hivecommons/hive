package complexity

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeSource serves a fixed Go tree for every repo and records fetches.
// notify, when set, receives each fetched repo so loop tests can wait.
type fakeSource struct {
	mu      sync.Mutex
	dir     string
	fetched []string
	err     error
	cleaned int
	notify  chan string
}

func (f *fakeSource) Fetch(_ context.Context, repo string) (Checkout, error) {
	f.mu.Lock()
	f.fetched = append(f.fetched, repo)
	err := f.err
	f.mu.Unlock()
	if f.notify != nil {
		f.notify <- repo
	}
	if err != nil {
		return Checkout{}, err
	}
	return Checkout{Dir: f.dir, Ref: "main", Cleanup: func() {
		f.mu.Lock()
		f.cleaned++
		f.mu.Unlock()
	}}, nil
}

func (f *fakeSource) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.fetched)
}

// fakeClock is a goroutine-safe settable clock for Runner.Now.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
	// calls, when set, receives a signal on every now() read (dropped if full).
	calls chan struct{}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	t := c.t
	c.mu.Unlock()
	if c.calls != nil {
		select {
		case c.calls <- struct{}{}:
		default:
		}
	}
	return t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func newRunner(t *testing.T, repos []string, now time.Time) (*Runner, *fakeSource, string) {
	t.Helper()
	tree := t.TempDir()
	writeFile(t, tree, "p.go", src)
	root := t.TempDir()
	srcs := &fakeSource{dir: tree}
	r := &Runner{
		Root:   root,
		Repos:  func() []string { return repos },
		Source: srcs,
		Now:    func() time.Time { return now },
	}
	return r, srcs, root
}

func TestRunnerRunOnceWritesCurrentMonthOnly(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	r, srcs, root := newRunner(t, []string{"o/b", "o/a", "o/a", " "}, now)
	r.Analyzers = []Analyzer{
		{Tool: "fake", Run: func(context.Context, string) (string, error) { return "x.go:1:2: a\ny.go:3:4: b\n", nil }},
		{Tool: "missing", Run: func(context.Context, string) (string, error) { return "", ErrAnalyzerUnavailable }},
		{Tool: "broken", Run: func(context.Context, string) (string, error) { return "", errors.New("boom") }},
	}

	res := r.RunOnce(context.Background())
	if res.Month != "2026-10" || len(res.Written) != 2 || res.Written[0] != "o/a" || res.Written[1] != "o/b" || len(res.Failed) != 0 {
		t.Fatalf("first run: %+v", res)
	}
	if len(srcs.fetched) != 2 || srcs.cleaned != 2 {
		t.Fatalf("fetched=%v cleaned=%d", srcs.fetched, srcs.cleaned)
	}
	p, _ := Path(root, "o/a", "2026-10")
	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.Repo != "o/a" || s.Month != "2026-10" || s.Ref != "main" || !s.CapturedAt.Equal(now) || s.Functions != 5 {
		t.Fatalf("snapshot = %+v", s)
	}
	if s.Warnings != 2 || s.WarningsByTool["fake"] != 2 || len(s.WarningsByTool) != 1 {
		t.Fatalf("warnings = %d %v", s.Warnings, s.WarningsByTool)
	}

	// Same month: idempotent, nothing fetched.
	res = r.RunOnce(context.Background())
	if len(res.Written) != 0 || len(res.Skipped) != 2 || len(srcs.fetched) != 2 {
		t.Fatalf("second run: %+v fetched=%v", res, srcs.fetched)
	}
	if got := r.Due(now); len(got) != 0 {
		t.Fatalf("due = %v", got)
	}

	// Month boundary: due again.
	next := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if got := r.Due(next); len(got) != 2 {
		t.Fatalf("due next month = %v", got)
	}
	r.Now = func() time.Time { return next }
	res = r.RunOnce(context.Background())
	if res.Month != "2026-11" || len(res.Written) != 2 {
		t.Fatalf("third run: %+v", res)
	}
	files, _ := monthFiles(root, "o/a")
	if len(files) != 2 {
		t.Fatalf("files = %v", files)
	}
}

func TestRunnerRunOncePartialMonth(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	r, srcs, root := newRunner(t, []string{"o/a", "o/b"}, now)
	// o/b already has this month's file (e.g. written before a restart).
	if _, err := Write(root, Snapshot{SchemaVersion: SchemaVersion, Repo: "o/b", Month: "2026-10"}); err != nil {
		t.Fatal(err)
	}
	res := r.RunOnce(context.Background())
	if len(res.Written) != 1 || res.Written[0] != "o/a" || len(res.Skipped) != 1 || res.Skipped[0] != "o/b" {
		t.Fatalf("%+v", res)
	}
	if len(srcs.fetched) != 1 || srcs.fetched[0] != "o/a" {
		t.Fatalf("fetched = %v", srcs.fetched)
	}
}

func TestRunnerRunOnceFailuresAreSkipped(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	r, srcs, _ := newRunner(t, []string{"o/a", "bad"}, now)
	srcs.err = errors.New("fetch failed")
	res := r.RunOnce(context.Background())
	if len(res.Written) != 0 || len(res.Failed) != 2 {
		t.Fatalf("%+v", res)
	}
	if res.Failed["o/a"] == nil || res.Failed["bad"] == nil {
		t.Fatalf("failed = %v", res.Failed)
	}
	if len(srcs.fetched) != 1 {
		t.Fatalf("invalid repo must not be fetched: %v", srcs.fetched)
	}
	// Still due next pass.
	if got := r.Due(now); len(got) != 2 {
		t.Fatalf("due = %v", got)
	}
	// No source configured is a per-repo failure, not a panic.
	r.Source = nil
	if res := r.RunOnce(context.Background()); res.Failed["o/a"] == nil {
		t.Fatalf("%+v", res)
	}
}

func TestRunnerRunOnceCancelledContext(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	r, srcs, _ := newRunner(t, []string{"o/a"}, now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := r.RunOnce(ctx)
	if len(res.Written) != 0 || len(srcs.fetched) != 0 {
		t.Fatalf("cancelled run did work: %+v", res)
	}
}

func TestRunnerRunLoopHonoursTicksAndShutdown(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	r, srcs, _ := newRunner(t, []string{"o/a"}, now)
	clock := &fakeClock{t: now, calls: make(chan struct{}, 8)}
	r.Now = clock.now
	srcs.notify = make(chan string)
	ctx, cancel := context.WithCancel(context.Background())
	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx, tick)
	}()
	// Nothing runs until the first tick.
	if srcs.count() != 0 {
		t.Fatalf("fetched before first tick: %v", srcs.fetched)
	}
	tick <- now
	<-clock.calls
	<-srcs.notify
	// A tick in the same month is a no-op. The send only proves the loop
	// received the tick, so wait for the pass to sample the clock before
	// moving it; that pass saw the old month and fetches nothing.
	tick <- now
	<-clock.calls
	// After the month rolls the next tick writes again.
	clock.set(now.AddDate(0, 1, 0))
	tick <- now
	<-srcs.notify
	cancel()
	<-done
	if srcs.count() != 2 {
		t.Fatalf("fetched = %v", srcs.fetched)
	}
}

func TestRunnerStartStopsOnCancel(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	r, srcs, _ := newRunner(t, []string{"o/a"}, now)
	r.StartDelay = time.Nanosecond
	srcs.notify = make(chan string)
	ctx, cancel := context.WithCancel(context.Background())
	done := r.Start(ctx)
	<-srcs.notify
	cancel()
	<-done
	if srcs.count() != 1 {
		t.Fatalf("fetched = %v", srcs.fetched)
	}
}

func TestRunnerStartCancelledDuringDelayDoesNothing(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	r, srcs, _ := newRunner(t, []string{"o/a"}, now)
	// Default delay (minutes): cancelling first must exit without a pass or
	// a log line, which is what a boot test that tears down fast relies on.
	ctx, cancel := context.WithCancel(context.Background())
	done := r.Start(ctx)
	cancel()
	<-done
	if srcs.count() != 0 {
		t.Fatalf("fetched = %v", srcs.fetched)
	}
}

func TestRunnerPrunesToRetain(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	r, _, root := newRunner(t, []string{"o/a"}, now)
	r.Retain = 2
	for _, m := range []string{"2026-07", "2026-08", "2026-09"} {
		if _, err := Write(root, Snapshot{SchemaVersion: SchemaVersion, Repo: "o/a", Month: m}); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, root, "o/a/complexity/notes.txt", "keep")
	res := r.RunOnce(context.Background())
	if len(res.Written) != 1 {
		t.Fatalf("%+v", res)
	}
	files, err := monthFiles(root, "o/a")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || filepath.Base(files[0]) != "2026-09.json" || filepath.Base(files[1]) != "2026-10.json" {
		t.Fatalf("files = %v", files)
	}
	if _, err := os.Stat(filepath.Join(root, "o/a/complexity/notes.txt")); err != nil {
		t.Error("prune removed a non-snapshot file")
	}
}

func TestPrune(t *testing.T) {
	root := t.TempDir()
	if n, err := Prune(root, "o/a", 3); err != nil || n != 0 {
		t.Fatalf("missing dir: %d %v", n, err)
	}
	for _, m := range []string{"2026-01", "2026-02", "2026-03", "2026-04"} {
		if _, err := Write(root, Snapshot{SchemaVersion: SchemaVersion, Repo: "o/a", Month: m}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := Prune(root, "o/a", 3); err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
	files, _ := monthFiles(root, "o/a")
	if len(files) != 3 || filepath.Base(files[0]) != "2026-02.json" {
		t.Fatalf("files = %v", files)
	}
	if _, err := Prune(root, "nope", 3); err == nil {
		t.Error("invalid repo must error")
	}
}

func TestLoadSeries(t *testing.T) {
	root := t.TempDir()
	if got, err := LoadSeries(filepath.Join(root, "missing"), 0); err != nil || got != nil {
		t.Fatalf("absent root: %v %v", got, err)
	}
	for _, m := range []string{"2026-01", "2026-02", "2026-03"} {
		if _, err := Write(root, Snapshot{SchemaVersion: SchemaVersion, Repo: "o/b", Month: m, MedianCyclo: 2}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Write(root, Snapshot{SchemaVersion: SchemaVersion, Repo: "o/a", Month: "2026-03"}); err != nil {
		t.Fatal(err)
	}
	// Per-PR bundle dirs and junk are ignored.
	writeFile(t, root, "o/a/12/abc.json", "{}")
	writeFile(t, root, "o/b/complexity/2026-04.json", "{nope")
	writeFile(t, root, "o/b/complexity/README", "x")

	got, err := LoadSeries(root, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Repo != "o/a" || got[1].Repo != "o/b" {
		t.Fatalf("series = %+v", got)
	}
	if len(got[0].Snapshots) != 1 || got[0].Snapshots[0].Month != "2026-03" {
		t.Fatalf("o/a = %+v", got[0].Snapshots)
	}
	// Latest 2 of the 3 readable months (the corrupt 2026-04 is dropped).
	if len(got[1].Snapshots) != 1 || got[1].Snapshots[0].Month != "2026-03" {
		t.Fatalf("o/b = %+v", got[1].Snapshots)
	}
	all, _ := LoadSeries(root, 0)
	if len(all[1].Snapshots) != 3 || all[1].Snapshots[0].Month != "2026-01" {
		t.Fatalf("all o/b = %+v", all[1].Snapshots)
	}
}

func tarGz(t *testing.T, entries map[string]string, extra ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range extra {
		if err := tw.WriteHeader(&extra[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractTarGz(t *testing.T) {
	dest := t.TempDir()
	data := tarGz(t, map[string]string{
		"o-r-abc1234/p.go":     "package p\n",
		"o-r-abc1234/sub/q.go": "package q\n",
	}, tar.Header{Name: "o-r-abc1234/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	top, err := ExtractTarGz(bytes.NewReader(data), dest, 0)
	if err != nil {
		t.Fatal(err)
	}
	if top != filepath.Join(dest, "o-r-abc1234") {
		t.Fatalf("top = %s", top)
	}
	if b, err := os.ReadFile(filepath.Join(top, "sub", "q.go")); err != nil || string(b) != "package q\n" {
		t.Fatalf("q.go = %q %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(top, "link")); !errors.Is(err, os.ErrNotExist) {
		t.Error("symlink must be skipped")
	}

	// Path traversal is refused.
	bad := tarGz(t, map[string]string{"../escape.go": "x"})
	if _, err := ExtractTarGz(bytes.NewReader(bad), t.TempDir(), 0); err == nil {
		t.Error("traversal accepted")
	}
	// Size cap.
	big := tarGz(t, map[string]string{"d/big.go": "0123456789"})
	if _, err := ExtractTarGz(bytes.NewReader(big), t.TempDir(), 5); !errors.Is(err, ErrArchiveTooLarge) {
		t.Errorf("size cap: %v", err)
	}
	// Multiple top-level entries: dest itself.
	flat := tarGz(t, map[string]string{"a.go": "package a\n", "b.go": "package b\n"})
	d := t.TempDir()
	if top, err := ExtractTarGz(bytes.NewReader(flat), d, 0); err != nil || top != d {
		t.Errorf("flat: %s %v", top, err)
	}
	if _, err := ExtractTarGz(bytes.NewReader([]byte("not gzip")), t.TempDir(), 0); err == nil {
		t.Error("bad gzip accepted")
	}
}

func TestGoModDirs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "go.mod", "module a\n")
	writeFile(t, root, "sub/go.mod", "module b\n")
	writeFile(t, root, "vendor/x/go.mod", "module v\n")
	writeFile(t, root, "testdata/go.mod", "module t\n")
	dirs, err := goModDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 2 || dirs[0] != root || dirs[1] != filepath.Join(root, "sub") {
		t.Fatalf("dirs = %v", dirs)
	}
}

func TestRunToolUnavailable(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "go.mod", "module a\n")
	if _, err := runTool(context.Background(), root, "definitely-not-a-tool-xyz", "x"); !errors.Is(err, ErrAnalyzerUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if len(DefaultAnalyzers()) != 2 || DefaultAnalyzers()[0].Tool != "go vet" {
		t.Fatalf("analyzers = %+v", DefaultAnalyzers())
	}
	// No go.mod: a clear error, not a tool run.
	if _, err := runTool(context.Background(), t.TempDir(), "sh", "-c", "true"); err == nil {
		t.Fatal("expected no go.mod error")
	}
}

func TestRunToolFindingsAndFailures(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "go.mod", "module a\n")
	// Non-zero exit with diagnostics is findings, not failure.
	out, err := runTool(context.Background(), root, "sh", "-c", "echo 'f.go:1:2: oops'; exit 1")
	if err != nil || CountFindings(out) != 1 {
		t.Fatalf("findings: %q %v", out, err)
	}
	// Non-zero exit with no diagnostics is a failure.
	if _, err := runTool(context.Background(), root, "sh", "-c", "echo 'cannot load'; exit 2"); err == nil {
		t.Fatal("expected failure")
	}
}
