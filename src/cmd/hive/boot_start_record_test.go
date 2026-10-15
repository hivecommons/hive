package main

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/logread"
	"github.com/hivecommons/hive/pkg/startrec"
)

func noCrashOutput(*os.File) error { return nil }

// fakeBeginStart records starts in dir without touching the test process's
// crash output.
func fakeBeginStart(dir string, calls *int) func(string, func(string) string) (*startrec.Recorder, error) {
	return func(id string, _ func(string) string) (*startrec.Recorder, error) {
		*calls++
		return startrec.Begin(startrec.Options{Dir: dir, ID: id, SetCrashOutput: noCrashOutput})
	}
}

func TestWithStartID(t *testing.T) {
	if withStartID(nil, "abcd1234") != nil {
		t.Fatal("a nil logger must stay nil")
	}
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, nil))
	if withStartID(base, "") != base {
		t.Fatal("an empty identifier must leave the logger as it is")
	}
	withStartID(base, "abcd1234").Info("hello")
	if !strings.Contains(buf.String(), `"start":"abcd1234"`) {
		t.Fatalf("log line lacks the start identifier: %s", buf.String())
	}
}

// AC1/AC2: the boot records its start after the singleton lock, lines from
// both loggers carry the start identifier, and the close pushed on the
// cleanup stack makes the next start read clean.
func TestBootConfigWith_RecordsStartAndTagsLogLines(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	f := newBootConfigFake(t)
	f.deps.beginStart = fakeBeginStart(dir, &calls)
	b := &boot{}

	if returned, ok, _ := runBootConfig(t, b, f.deps); !returned || !ok {
		t.Fatalf("returned=%v ok=%v", returned, ok)
	}
	if calls != 1 || b.startRecord == nil || len(b.startID) != 8 || b.startRecord.ID() != b.startID {
		t.Fatalf("calls=%d record=%v id=%q", calls, b.startRecord, b.startID)
	}
	// The early logger is JSON on stdout; the file logger here is the fake's
	// text handler, which bypasses setupLogger, so the identifier must come
	// from withStartID around it.
	if !strings.Contains(f.stdout.String(), `"start":"`+b.startID+`"`) {
		t.Fatalf("early log lines lack the start identifier: %s", f.stdout.String())
	}
	if !strings.Contains(f.log.String(), "start="+b.startID) || !strings.Contains(f.stdout.String(), `"previous_run":"unknown"`) {
		t.Fatalf("file log lines lack the start identifier or the start line: %s", f.log.String())
	}
	if len(b.cleanup.fns) != 4 {
		t.Fatalf("registered %d cleanups, want 4 (lock, start record, cancel, tracing)", len(b.cleanup.fns))
	}
	b.cleanup.run()

	next, err := startrec.Begin(startrec.Options{Dir: dir, ID: "next0001", SetCrashOutput: noCrashOutput})
	if err != nil {
		t.Fatal(err)
	}
	if prev := next.Previous(); prev.Verdict != startrec.VerdictClean || prev.Start != b.startID {
		t.Fatalf("after the cleanup stack ran previous = %+v, want clean %s", prev, b.startID)
	}
	res := logread.Read(logread.Request{Source: logread.SourceStarts, Include: b.startID}, logread.Options{OutputDir: dir})
	if res.Status != logread.StatusOK || res.LineCount == 0 {
		t.Fatalf("starts source read = %+v", res)
	}
}

// AC1: a second hive process refused at start-up changes nothing.
func TestBootConfigWith_RefusedDuplicateLeavesStartRecord(t *testing.T) {
	dir := t.TempDir()
	if _, err := startrec.Begin(startrec.Options{Dir: dir, ID: "holder01", SetCrashOutput: noCrashOutput}); err != nil {
		t.Fatal(err)
	}
	snapshot := func() map[string]string {
		out := map[string]string{}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			out[e.Name()] = string(data)
		}
		return out
	}
	before := snapshot()

	calls := 0
	f := newBootConfigFake(t)
	f.deps.beginStart = fakeBeginStart(dir, &calls)
	f.deps.acquireLock = func(string) (func(), error) { return nil, errors.New("held by process 1") }
	_, _, code := runBootConfig(t, &boot{}, f.deps)
	if code != duplicateProcessExitCode {
		t.Fatalf("exit code = %d, want %d", code, duplicateProcessExitCode)
	}
	if calls != 0 {
		t.Fatal("a refused duplicate recorded a start")
	}
	after := snapshot()
	if len(after) != len(before) {
		t.Fatalf("files changed: before %v after %v", fileNames(before), fileNames(after))
	}
	for name, data := range before {
		if after[name] != data {
			t.Fatalf("%s changed under a refused duplicate", name)
		}
	}
}

func fileNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A start record that cannot be written is a warning, never a stopped boot.
func TestBootConfigWith_StartRecordFailureIsAWarning(t *testing.T) {
	f := newBootConfigFake(t)
	f.deps.beginStart = func(string, func(string) string) (*startrec.Recorder, error) {
		return nil, errors.New("read-only volume")
	}
	b := &boot{}
	if returned, ok, _ := runBootConfig(t, b, f.deps); !returned || !ok {
		t.Fatalf("returned=%v ok=%v", returned, ok)
	}
	if b.startRecord != nil || len(b.cleanup.fns) != 3 {
		t.Fatalf("record=%v cleanups=%d", b.startRecord, len(b.cleanup.fns))
	}
	if !strings.Contains(f.log.String(), "start record not written") {
		t.Fatalf("log lacks the warning: %s", f.log.String())
	}

	// A record whose crash output failed is kept and the failure logged.
	dir := t.TempDir()
	f = newBootConfigFake(t)
	f.deps.beginStart = func(id string, _ func(string) string) (*startrec.Recorder, error) {
		return startrec.Begin(startrec.Options{Dir: dir, ID: id, SetCrashOutput: func(*os.File) error { return errors.New("no crash output") }})
	}
	b = &boot{}
	if returned, ok, _ := runBootConfig(t, b, f.deps); !returned || !ok || b.startRecord == nil {
		t.Fatalf("returned=%v ok=%v record=%v", returned, ok, b.startRecord)
	}
	if !strings.Contains(f.log.String(), "crash output not kept") {
		t.Fatalf("log lacks the crash output warning: %s", f.log.String())
	}
	b.cleanup.run()
}

// The production seam writes into the output log directory with a memory
// reading.
func TestRecordStartUsesOutputDir(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { _ = debug.SetCrashOutput(nil, debug.CrashOptions{}) })
	rec, err := recordStart("prod0001", func(k string) string {
		if k == logread.OutputDirEnv {
			return dir
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Close(startrec.VerdictClean); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, startrec.StateFilename)); err != nil {
		t.Fatalf("no start record in the output dir: %v", err)
	}
	if m, ok := containerMemoryReading(); !ok || m.Limit == "" {
		t.Fatalf("containerMemoryReading = %+v, %v", m, ok)
	}
}

// The self-upgrade failure exit skips the cleanup stack, so main.go must
// close the start record as self-upgrade-failed right before it.
func TestSelfUpgradeFailureExitClosesStartRecord(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	exitIdx := strings.Index(body, "os.Exit(selfUpgradeFailureExitCode)")
	closeIdx := strings.Index(body, "b.startRecord.Close(startrec.VerdictSelfUpgradeFailed)")
	if exitIdx < 0 || closeIdx < 0 || closeIdx > exitIdx {
		t.Fatalf("main.go must close the start record as self-upgrade-failed before os.Exit(selfUpgradeFailureExitCode) (close=%d exit=%d)", closeIdx, exitIdx)
	}
	if between := body[closeIdx:exitIdx]; strings.Count(between, "\n") > 12 {
		t.Fatalf("the start record close is not right before the exit:\n%s", between)
	}
}
