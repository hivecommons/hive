package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/crux/config"
)

const repeatFailureTestToken = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"

func enabledRepeatFailureConfig() *config.RepeatFailureConfig {
	return &config.RepeatFailureConfig{Enabled: true}
}

type repeatFailureSink struct {
	mu      sync.Mutex
	records []RepeatFailureRecord
}

func (s *repeatFailureSink) add(rec RepeatFailureRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, rec)
}

func (s *repeatFailureSink) all() []RepeatFailureRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RepeatFailureRecord(nil), s.records...)
}

func newRepeatFailureTestCollector(t *testing.T, paths map[string]string, sink *repeatFailureSink, logs *bytes.Buffer) *RepeatFailureCollector {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := NewRepeatFailureCollector(enabledRepeatFailureConfig(), func() map[string]string { return paths }, sink.add, logger)
	if c == nil {
		t.Fatal("an enabled config must produce a collector")
	}
	return c
}

func assertNoFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s must be gone, stat err = %v", path, err)
	}
}

// C-AC13: a command carrying a token-shaped string leaves the collector
// scrubbed, is never logged raw, and the raw entry is gone after the read.
func TestRepeatFailureCollectorScrubsAndRemovesRawRecord(t *testing.T) {
	path := RepeatFailureRecordPath(filepath.Join(t.TempDir(), "home"))
	if err := EnsureRepeatFailureRecordDir(path, -1); err != nil {
		t.Fatal(err)
	}
	exit := 1
	raw := "gh api -H 'Authorization: token " + repeatFailureTestToken + "' /user"
	if err := AppendRepeatFailureRecord(path, RepeatFailureRecord{
		Agent: "spoofed", Backend: "claude", Time: time.Unix(1_700_000_000, 0).UTC(),
		RawCommand: raw, ExitCode: &exit, Failed: true, Route: "bash-env",
	}); err != nil {
		t.Fatal(err)
	}
	rawBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(rawBytes, []byte(repeatFailureTestToken)) {
		t.Fatal("test setup: the raw file should hold the unscrubbed command")
	}

	var sink repeatFailureSink
	var logs bytes.Buffer
	c := newRepeatFailureTestCollector(t, map[string]string{"worker": path}, &sink, &logs)
	if n := c.Collect(); n != 1 {
		t.Fatalf("Collect = %d, want 1", n)
	}
	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("sink got %d records, want 1", len(got))
	}
	rec := got[0]
	if strings.Contains(rec.RawCommand, repeatFailureTestToken) {
		t.Fatalf("raw command reached the sink unscrubbed: %q", rec.RawCommand)
	}
	if !strings.Contains(rec.RawCommand, "[REDACTED]") || !strings.HasPrefix(rec.RawCommand, "gh api -H") {
		t.Fatalf("raw command not scrubbed in place: %q", rec.RawCommand)
	}
	if rec.Agent != "worker" {
		t.Fatalf("agent = %q, want the file's owner, not what the line claims", rec.Agent)
	}
	if rec.ExitCode == nil || *rec.ExitCode != 1 || !rec.Failed || rec.Backend != "claude" || rec.Route != "bash-env" {
		t.Fatalf("record fields not preserved: %+v", rec)
	}
	if strings.Contains(logs.String(), repeatFailureTestToken) {
		t.Fatalf("token leaked into logs: %s", logs.String())
	}
	assertNoFile(t, path)
	assertNoFile(t, path+repeatFailureCollectingSuffix)
}

func TestRepeatFailureRecordFilePermissions(t *testing.T) {
	path := RepeatFailureRecordPath(filepath.Join(t.TempDir(), "home"))
	if err := EnsureRepeatFailureRecordDir(path, -1); err != nil {
		t.Fatal(err)
	}
	if err := AppendRepeatFailureRecord(path, RepeatFailureRecord{RawCommand: "false", Failed: true}); err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("record dir mode = %o, want 700", perm)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fileInfo.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("record file mode = %o, must not be readable by group or others", perm)
	}
}

// C-AC14a: records appended while Hive collects repeatedly all arrive whole,
// exactly once, and the collected raw file is gone afterwards.
func TestRepeatFailureCollectorKeepsEveryRecordWhileAppending(t *testing.T) {
	const writers, perWriter = 4, 250
	path := RepeatFailureRecordPath(filepath.Join(t.TempDir(), "home"))
	if err := EnsureRepeatFailureRecordDir(path, -1); err != nil {
		t.Fatal(err)
	}
	var sink repeatFailureSink
	var logs bytes.Buffer
	c := newRepeatFailureTestCollector(t, map[string]string{"worker": path}, &sink, &logs)

	expected := map[string]bool{}
	for w := range writers {
		for i := range perWriter {
			expected[repeatFailureLoadCommand(w, i)] = true
		}
	}

	errs := make(chan error, writers)
	progress := make(chan struct{}, 1)
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range perWriter {
				exit := 1 + (i % 3)
				if err := AppendRepeatFailureRecord(path, RepeatFailureRecord{
					Backend: "codex", RawCommand: repeatFailureLoadCommand(w, i), ExitCode: &exit, Failed: true, Route: "bash-env",
				}); err != nil {
					errs <- err
					return
				}
				select {
				case progress <- struct{}{}:
				default:
				}
			}
		}(w)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	// Collect every time a writer reports progress, so collections
	// interleave with the appends for the whole run.
	collections := 0
	for running := true; running; {
		select {
		case <-progress:
			c.Collect()
			collections++
		case <-done:
			running = false
		}
	}
	c.Collect()
	t.Logf("%d collections while appending", collections)
	close(errs)
	for err := range errs {
		t.Fatalf("append failed: %v", err)
	}

	got := sink.all()
	if len(got) != writers*perWriter {
		t.Fatalf("collected %d records over %d collections, want %d", len(got), collections, writers*perWriter)
	}
	seen := map[string]bool{}
	for _, rec := range got {
		if !expected[rec.RawCommand] {
			t.Fatalf("torn or unexpected record: %q", rec.RawCommand)
		}
		if seen[rec.RawCommand] {
			t.Fatalf("record collected twice: %q", rec.RawCommand)
		}
		seen[rec.RawCommand] = true
		if rec.ExitCode == nil || !rec.Failed {
			t.Fatalf("record lost its result: %+v", rec)
		}
	}
	if strings.Contains(logs.String(), "dropped unreadable") {
		t.Fatalf("collector reported invalid records: %s", logs.String())
	}
	assertNoFile(t, path)
	assertNoFile(t, path+repeatFailureCollectingSuffix)
}

// repeatFailureLoadCommand is long enough that a torn write would show, and
// unique per record so loss and duplication are both visible.
func repeatFailureLoadCommand(w, i int) string {
	return fmt.Sprintf("npm run build -- --writer=%d --record=%04d %s", w, i, strings.Repeat("x", 512+(i%7)*200))
}

func TestRepeatFailureCollectorOffCollectsNothing(t *testing.T) {
	path := RepeatFailureRecordPath(filepath.Join(t.TempDir(), "home"))
	if err := EnsureRepeatFailureRecordDir(path, -1); err != nil {
		t.Fatal(err)
	}
	if err := AppendRepeatFailureRecord(path, RepeatFailureRecord{RawCommand: "false", Failed: true}); err != nil {
		t.Fatal(err)
	}
	called := false
	paths := func() map[string]string {
		called = true
		return map[string]string{"worker": path}
	}
	sink := func(RepeatFailureRecord) { t.Fatal("a disabled check must not deliver records") }
	for _, cfg := range []*config.RepeatFailureConfig{nil, {}, {Enabled: false, Threshold: 3}} {
		c := NewRepeatFailureCollector(cfg, paths, sink, nil)
		if c != nil {
			t.Fatalf("config %+v must not produce a collector", cfg)
		}
		if n := c.Collect(); n != 0 {
			t.Fatalf("nil collector collected %d records", n)
		}
		tick := make(chan time.Time, 1)
		tick <- time.Time{}
		c.Run(context.Background(), tick)
	}
	if called {
		t.Fatal("a disabled check must not look for record files")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a disabled check must leave files alone: %v", err)
	}
	assertNoFile(t, path+repeatFailureCollectingSuffix)
}

func TestRepeatFailureCollectorRunCollectsOnTick(t *testing.T) {
	path := RepeatFailureRecordPath(filepath.Join(t.TempDir(), "home"))
	if err := EnsureRepeatFailureRecordDir(path, -1); err != nil {
		t.Fatal(err)
	}
	got := make(chan RepeatFailureRecord, 1)
	c := NewRepeatFailureCollector(enabledRepeatFailureConfig(), func() map[string]string {
		return map[string]string{"worker": path}
	}, func(rec RepeatFailureRecord) { got <- rec }, slog.New(slog.DiscardHandler))
	fixed := time.Unix(1_800_000_000, 0).UTC()
	c.now = func() time.Time { return fixed }

	if err := AppendRepeatFailureRecord(path, RepeatFailureRecord{RawCommand: "ls /nonexistent", Failed: true, Route: "copilot-events"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tick := make(chan time.Time)
	stopped := make(chan struct{})
	go func() {
		c.Run(ctx, tick)
		close(stopped)
	}()
	tick <- fixed
	rec := <-got
	if rec.RawCommand != "ls /nonexistent" || !rec.Failed || rec.ExitCode != nil {
		t.Fatalf("unexpected record: %+v", rec)
	}
	if !rec.Time.Equal(fixed) {
		t.Fatalf("record without a time must get the collection time, got %s", rec.Time)
	}
	cancel()
	<-stopped
	assertNoFile(t, path)
}

func TestRepeatFailureRecordTruncatedToOneWrite(t *testing.T) {
	long := strings.Repeat("é\"\\", 3000) + "tail"
	line, err := encodeRepeatFailureRecord(RepeatFailureRecord{
		Agent: "worker", Backend: "goose", RawCommand: long, Command: strings.Repeat("y", 6000), Failed: true, Route: "bash-env",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(line) > RepeatFailureRecordMaxLine {
		t.Fatalf("line is %d bytes, want <= %d", len(line), RepeatFailureRecordMaxLine)
	}
	if bytes.Count(line, []byte("\n")) != 1 || line[len(line)-1] != '\n' {
		t.Fatal("a record must be exactly one newline-terminated line")
	}
	var rec RepeatFailureRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		t.Fatalf("truncated record is not valid JSON: %v", err)
	}
	if !rec.Truncated || !utf8.ValidString(rec.RawCommand) || !strings.HasPrefix(long, rec.RawCommand) || rec.RawCommand == "" {
		t.Fatalf("command not cut cleanly: truncated=%v len=%d", rec.Truncated, len(rec.RawCommand))
	}

	short, err := encodeRepeatFailureRecord(RepeatFailureRecord{RawCommand: "false", Failed: true})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(short, []byte("truncated")) {
		t.Fatal("a short command must not be marked truncated")
	}
}

func TestRepeatFailureCollectorDrainsLeftoverAndDropsInvalidLines(t *testing.T) {
	path := RepeatFailureRecordPath(filepath.Join(t.TempDir(), "home"))
	if err := EnsureRepeatFailureRecordDir(path, -1); err != nil {
		t.Fatal(err)
	}
	leftover := `{"raw_command":"make test","failed":true}` + "\n" +
		`{"raw_command":"torn` + "\n" +
		strings.Repeat("z", RepeatFailureRecordMaxLine*2) + "\n" +
		`{"failed":true}` + "\n"
	if err := os.WriteFile(path+repeatFailureCollectingSuffix, []byte(leftover), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AppendRepeatFailureRecord(path, RepeatFailureRecord{RawCommand: "make lint", Failed: true}); err != nil {
		t.Fatal(err)
	}
	var sink repeatFailureSink
	var logs bytes.Buffer
	c := newRepeatFailureTestCollector(t, map[string]string{"worker": path}, &sink, &logs)
	if n := c.Collect(); n != 2 {
		t.Fatalf("Collect = %d, want the leftover record and the live one", n)
	}
	var cmds []string
	for _, rec := range sink.all() {
		cmds = append(cmds, rec.RawCommand)
	}
	if strings.Join(cmds, ",") != "make test,make lint" {
		t.Fatalf("records = %v, want leftover first, then live", cmds)
	}
	if !strings.Contains(logs.String(), "count=3") {
		t.Fatalf("three invalid lines must be counted: %s", logs.String())
	}
	if strings.Contains(logs.String(), "zzzz") || strings.Contains(logs.String(), "torn") {
		t.Fatalf("invalid line content must not be logged: %s", logs.String())
	}
	assertNoFile(t, path)
	assertNoFile(t, path+repeatFailureCollectingSuffix)
}

func TestRepeatFailureCollectorRefusesSymlinkedRecordFile(t *testing.T) {
	dir := t.TempDir()
	path := RepeatFailureRecordPath(filepath.Join(dir, "home"))
	if err := EnsureRepeatFailureRecordDir(path, -1); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, []byte(`{"raw_command":"secret elsewhere","failed":true}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	var sink repeatFailureSink
	var logs bytes.Buffer
	c := newRepeatFailureTestCollector(t, map[string]string{"worker": path}, &sink, &logs)
	if n := c.Collect(); n != 0 {
		t.Fatalf("a symlinked record file must not be read, got %d records", n)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the symlink target must be left alone: %v", err)
	}
	if err := AppendRepeatFailureRecord(path, RepeatFailureRecord{RawCommand: "false"}); err == nil {
		t.Fatal("a writer must not follow a symlinked record file")
	}
}

// Hive runs as root and the agent under its own UID: parents the helper
// creates (~/.hive) must stay traversable, only the leaf is 0700, and the
// helper never has to see ~/.hive pre-exist.
func TestEnsureRepeatFailureRecordDirParentsTraversable(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	path := RepeatFailureRecordPath(home)
	if err := EnsureRepeatFailureRecordDir(path, -1); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Dir(path)
	if info, err := os.Lstat(leaf); err != nil || info.Mode().Perm() != repeatFailureRecordDirMode {
		t.Fatalf("leaf %s: info=%v err=%v, want mode %o", leaf, info, err, repeatFailureRecordDirMode)
	}
	parent := filepath.Dir(leaf)
	if info, err := os.Lstat(parent); err != nil || info.Mode().Perm() != repeatFailureRecordParentMode {
		t.Fatalf("parent %s: info=%v err=%v, want mode %o", parent, info, err, repeatFailureRecordParentMode)
	}
	if info, err := os.Lstat(home); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("existing ancestor %s must be untouched: info=%v err=%v", home, info, err)
	}
	// Idempotent on an existing tree.
	if err := EnsureRepeatFailureRecordDir(path, -1); err != nil {
		t.Fatal(err)
	}
}

// Audit F12: a symlink planted at any component the helper would chown/chmod
// is refused instead of followed.
func TestEnsureRepeatFailureRecordDirRefusesSymlink(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	target := filepath.Join(root, "elsewhere")
	for _, d := range []string{filepath.Join(home, ".hive"), target} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	path := RepeatFailureRecordPath(home)
	if err := os.Symlink(target, filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	err := EnsureRepeatFailureRecordDir(path, -1)
	if !errors.Is(err, errInferenceHomeSymlink) {
		t.Fatalf("leaf symlink: err = %v, want errInferenceHomeSymlink", err)
	}

	// Parent component is the link this time.
	home2 := filepath.Join(root, "home2")
	if err := os.Mkdir(home2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home2, ".hive")); err != nil {
		t.Fatal(err)
	}
	err = EnsureRepeatFailureRecordDir(RepeatFailureRecordPath(home2), -1)
	if !errors.Is(err, errInferenceHomeSymlink) {
		t.Fatalf("parent symlink: err = %v, want errInferenceHomeSymlink", err)
	}
	if _, err := os.Lstat(filepath.Join(target, "repeat-failure")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nothing may be created through the link, stat err = %v", err)
	}
}
