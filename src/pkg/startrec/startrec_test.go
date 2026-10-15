package startrec

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/logread"
)

// The helper process: the test binary re-executed with helperEnv set runs a
// start the parent can kill or watch crash. It exists only in this test
// binary; the production hive has no way to crash on demand.
const (
	helperEnv    = "HIVE_STARTREC_TEST_HELPER"
	helperDirEnv = "HIVE_STARTREC_TEST_HELPER_DIR"
	helperPanic  = "startrec-helper-panic"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		os.Exit(runHelper(mode, os.Getenv(helperDirEnv)))
	}
	os.Exit(m.Run())
}

func runHelper(mode, dir string) int {
	switch mode {
	case "hold":
		rec, err := Begin(Options{Dir: dir, ID: "holder01", SetCrashOutput: noCrashOutput})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 3
		}
		rec.StartHeartbeat(HeartbeatInterval)
		fmt.Println("ready")
		// Held until the parent kills this process; stdin stays open until then.
		_, _ = io.Copy(io.Discard, os.Stdin)
		return 0
	case "crash":
		if _, err := Begin(Options{Dir: dir, ID: "crasher1"}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 3
		}
		panic(helperPanic)
	}
	return 4
}

func noCrashOutput(*os.File) error { return nil }

func helperCmd(t *testing.T, mode, dir string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), helperEnv+"="+mode, helperDirEnv+"="+dir)
	return cmd
}

// fakeClock advances by step on every reading.
type fakeClock struct {
	mu   sync.Mutex
	now  time.Time
	step time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(c.step)
	return c.now
}

func begin(t *testing.T, dir, id string, clock *fakeClock, memory func() (MemoryReading, bool)) *Recorder {
	t.Helper()
	opts := Options{Dir: dir, ID: id, SetCrashOutput: noCrashOutput, Memory: memory}
	if clock != nil {
		opts.Now = clock.Now
	}
	rec, err := Begin(opts)
	if err != nil {
		t.Fatalf("Begin(%s): %v", id, err)
	}
	return rec
}

func readStarts(t *testing.T, dir string) []Record {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, StartsFilename))
	if err != nil {
		t.Fatal(err)
	}
	var out []Record
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("starts line %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

func readCurrent(t *testing.T, dir string) state {
	t.Helper()
	st, ok := readState(dir)
	if !ok {
		t.Fatal("no start-current.json")
	}
	return st
}

// AC1: unknown with no prior record, then clean, self-upgrade-failed and
// not-closed after each kind of end.
func TestVerdicts(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), step: time.Second}

	first := begin(t, dir, "aaaa0001", clock, nil)
	if got := first.Previous(); got.Verdict != VerdictUnknown || got.Start != "" {
		t.Fatalf("first start previous = %+v, want unknown", got)
	}
	if err := first.Close(VerdictClean); err != nil {
		t.Fatal(err)
	}

	second := begin(t, dir, "aaaa0002", clock, nil)
	if got := second.Previous(); got.Verdict != VerdictClean || got.Start != "aaaa0001" || got.EndedAt == "" {
		t.Fatalf("after a clean close previous = %+v", got)
	}
	if err := second.Close(VerdictSelfUpgradeFailed); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(VerdictClean); err != nil {
		t.Fatal(err)
	}

	third := begin(t, dir, "aaaa0003", clock, nil)
	if got := third.Previous(); got.Verdict != VerdictSelfUpgradeFailed || got.Start != "aaaa0002" {
		t.Fatalf("after a self-upgrade failure previous = %+v (first close must win)", got)
	}

	fourth := begin(t, dir, "aaaa0004", clock, nil)
	if got := fourth.Previous(); got.Verdict != VerdictNotClosed || got.Start != "aaaa0003" || got.EndedAt != "" || got.LastSeenAlive == "" {
		t.Fatalf("after no close previous = %+v", got)
	}
	if err := fourth.Close("anything-else"); err != nil {
		t.Fatal(err)
	}
	if st := readCurrent(t, dir); st.Ended != VerdictClean {
		t.Fatalf("an unknown end verdict recorded %q, want clean", st.Ended)
	}

	starts := readStarts(t, dir)
	if len(starts) != 4 {
		t.Fatalf("starts.log has %d lines, want 4", len(starts))
	}
	want := []struct{ id, verdict, level string }{
		{"aaaa0001", VerdictUnknown, "INFO"},
		{"aaaa0002", VerdictClean, "INFO"},
		{"aaaa0003", VerdictSelfUpgradeFailed, "WARN"},
		{"aaaa0004", VerdictNotClosed, "WARN"},
	}
	for i, w := range want {
		if starts[i].Start != w.id || starts[i].Previous.Verdict != w.verdict || starts[i].Level != w.level || starts[i].PID != os.Getpid() {
			t.Fatalf("starts[%d] = %+v, want %+v", i, starts[i], w)
		}
	}

	res := logread.Read(logread.Request{Source: logread.SourceStarts, MinLevel: "warn"}, logread.Options{OutputDir: dir})
	if res.Status != logread.StatusOK || res.LineCount != 2 || !strings.Contains(res.Lines[1].Text, `"verdict":"not-closed"`) {
		t.Fatalf("starts source warn read = %+v", res)
	}
}

// AC1: a holder killed with SIGKILL reads not-closed, last seen alive no
// later than the kill.
func TestNotClosedAfterHolderKilled(t *testing.T) {
	dir := t.TempDir()
	cmd := helperCmd(t, "hold", dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err == nil && strings.TrimSpace(line) != "ready" {
			err = fmt.Errorf("helper said %q", line)
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			_ = cmd.Process.Kill()
			t.Fatalf("helper did not become ready: %v", err)
		}
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("helper did not become ready within 60s")
	}
	killedAt := time.Now()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	next := begin(t, dir, "next0001", nil, nil)
	prev := next.Previous()
	if prev.Verdict != VerdictNotClosed || prev.Start != "holder01" {
		t.Fatalf("after SIGKILL previous = %+v, want not-closed holder01", prev)
	}
	seen, err := time.Parse(time.RFC3339Nano, prev.LastSeenAlive)
	if err != nil {
		t.Fatalf("last seen alive %q: %v", prev.LastSeenAlive, err)
	}
	if seen.After(killedAt) {
		t.Fatalf("last seen alive %s is after the kill at %s", seen, killedAt)
	}
}

// AC3: an unrecovered panic's trace is readable as the crash source after the
// next start.
func TestCrashTraceReadableAfterRestart(t *testing.T) {
	dir := t.TempDir()
	cmd := helperCmd(t, "crash", dir)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() == 3 || exitErr.ExitCode() == 4 {
		t.Fatalf("crash helper: err=%v output=%s", err, out)
	}

	next := begin(t, dir, "next0002", nil, nil)
	if prev := next.Previous(); prev.Verdict != VerdictNotClosed || prev.Start != "crasher1" || !prev.CrashTrace {
		t.Fatalf("after a crash previous = %+v, want not-closed with a crash trace", prev)
	}
	res := logread.Read(logread.Request{Source: logread.SourceCrash, Lines: logread.MaxLines}, logread.Options{OutputDir: dir})
	if res.Status != logread.StatusOK {
		t.Fatalf("crash read = %+v", res)
	}
	all := strings.Join(func() []string {
		var s []string
		for _, l := range res.Lines {
			s = append(s, l.Text)
		}
		return s
	}(), "\n")
	for _, want := range []string{"panic: " + helperPanic, "goroutine ", "crash output of hive start crasher1", "crash output of hive start next0002"} {
		if !strings.Contains(all, want) {
			t.Fatalf("crash source lacks %q:\n%s", want, all)
		}
	}
}

// AC3: at most MaxCrashTraces files are kept, empty ones are dropped and
// each trace is cut to MaxCrashBytes.
func TestCrashTracesBounded(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	header := `{"time":"2026-09-01T00:00:00Z","level":"INFO","msg":"crash output","start":"x"}` + "\n"
	write := func(i int, body string) string {
		name := logread.HistoryFileName(logread.SourceCrash, base.Add(time.Duration(i)*time.Hour))
		if err := os.WriteFile(filepath.Join(dir, name), []byte(header+body), 0o600); err != nil {
			t.Fatal(err)
		}
		return name
	}
	for i := 0; i < 12; i++ {
		write(i, "panic: trace\n")
	}
	empty := write(20, "")
	big := write(21, strings.Repeat("x", MaxCrashBytes+4096))
	noHeader := logread.HistoryFileName(logread.SourceCrash, base.Add(22*time.Hour))
	if err := os.WriteFile(filepath.Join(dir, noHeader), []byte("fatal error: no header\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	begin(t, dir, "bound001", nil, nil)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var crash []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), logread.SourceCrash+"-") {
			crash = append(crash, e.Name())
		}
	}
	if len(crash) != MaxCrashTraces {
		t.Fatalf("kept %d crash files, want %d: %v", len(crash), MaxCrashTraces, crash)
	}
	if _, err := os.Stat(filepath.Join(dir, empty)); !os.IsNotExist(err) {
		t.Fatalf("header-only crash file kept: %v", err)
	}
	st, err := os.Stat(filepath.Join(dir, big))
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > MaxCrashBytes {
		t.Fatalf("big trace kept at %d bytes, want at most %d", st.Size(), MaxCrashBytes)
	}
	if !hasTrace(filepath.Join(dir, noHeader)) {
		t.Fatal("a crash file without our header must count as a trace")
	}
	if hasTrace(filepath.Join(dir, "missing")) {
		t.Fatal("a missing file holds no trace")
	}
}

// AC4: the last memory reading before a run ended is reported with the next
// start's record, and the heartbeat refreshes it.
func TestMemoryReadingSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{now: time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC), step: time.Second}
	var kills int64
	memory := func() (MemoryReading, bool) {
		kills++
		n := kills
		return MemoryReading{UsedBytes: 100 * n, Limit: "512 MiB", LimitBytes: 512 << 20, OOMKills: &n}, true
	}
	rec := begin(t, dir, "mem00001", clock, memory)
	firstSeen := readCurrent(t, dir).LastSeenAlive
	if err := rec.Heartbeat(); err != nil {
		t.Fatal(err)
	}
	st := readCurrent(t, dir)
	if st.LastSeenAlive == firstSeen || st.Memory == nil || st.Memory.OOMKills == nil || *st.Memory.OOMKills != 2 {
		t.Fatalf("heartbeat state = %+v", st)
	}

	next := begin(t, dir, "mem00002", clock, func() (MemoryReading, bool) { return MemoryReading{}, false })
	prev := next.Previous()
	if prev.Verdict != VerdictNotClosed || prev.Memory == nil || prev.Memory.OOMKills == nil || *prev.Memory.OOMKills != 2 || prev.Memory.Limit != "512 MiB" || prev.Memory.ReadAt == "" {
		t.Fatalf("previous memory = %+v", prev.Memory)
	}
	if got := readStarts(t, dir)[1].Previous.Memory; got == nil || *got.OOMKills != 2 {
		t.Fatalf("starts line memory = %+v", got)
	}
	if readCurrent(t, dir).Memory != nil {
		t.Fatal("a failed reading must record none")
	}

	// Close takes a final reading; an unset limit reads unknown.
	last := begin(t, dir, "mem00003", clock, func() (MemoryReading, bool) { return MemoryReading{UsedBytes: 7}, true })
	if err := last.Close(VerdictClean); err != nil {
		t.Fatal(err)
	}
	if err := last.Heartbeat(); err != nil {
		t.Fatal(err)
	}
	if st := readCurrent(t, dir); st.Memory == nil || st.Memory.Limit != LimitUnknown || st.Ended != VerdictClean {
		t.Fatalf("closed state = %+v", st)
	}
}

func TestStartHeartbeatTicksUntilClose(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{now: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC), step: time.Second}
	rec := begin(t, dir, "beat0001", clock, nil)
	first := readCurrent(t, dir).LastSeenAlive
	rec.StartHeartbeat(time.Millisecond)
	rec.StartHeartbeat(time.Millisecond)
	testutil.Eventually(t, 10*time.Second, func() bool {
		st, ok := readState(dir)
		return ok && st.LastSeenAlive != first
	}, "heartbeat never refreshed last_seen_alive")
	if err := rec.Close(VerdictClean); err != nil {
		t.Fatal(err)
	}
	closed := readCurrent(t, dir)
	if closed.Ended != VerdictClean {
		t.Fatalf("state after close = %+v", closed)
	}

	// Started after close, the heartbeat never runs.
	other := begin(t, t.TempDir(), "beat0002", clock, nil)
	if err := other.Close(VerdictClean); err != nil {
		t.Fatal(err)
	}
	other.StartHeartbeat(time.Millisecond)
	other.StartHeartbeat(0)
	if other.stop != nil {
		t.Fatal("heartbeat started on a closed record")
	}
}

func TestStartsLogBounded(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 0; i < MaxStartLines+50; i++ {
		fmt.Fprintf(&b, `{"time":"2026-01-01T00:00:00Z","level":"INFO","msg":"old %d"}`+"\n\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, StartsFilename), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	begin(t, dir, "bound002", nil, nil)
	starts := readStarts(t, dir)
	if len(starts) != MaxStartLines || starts[len(starts)-1].Start != "bound002" || starts[0].Msg != "old 51" {
		t.Fatalf("starts.log kept %d lines, first %q", len(starts), starts[0].Msg)
	}
}

func TestReadTailDropsPartialFirstLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("aaaa\nbbbb\ncccc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readTail(path, 7); err != nil || string(got) != "cccc\n" {
		t.Fatalf("readTail = %q, %v", got, err)
	}
	if got, err := readTail(path, 3); err != nil || len(got) != 0 {
		t.Fatalf("readTail inside one line = %q, %v", got, err)
	}
	if _, err := readTail(filepath.Join(t.TempDir(), "missing"), 10); err == nil {
		t.Fatal("readTail of a missing file must fail")
	}
}

func TestFileModesAndGarbageState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, StateFilename), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := begin(t, dir, "mode0001", nil, nil)
	if rec.Previous().Verdict != VerdictUnknown {
		t.Fatalf("garbage state read as %+v, want unknown", rec.Previous())
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v", st.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v", e.Name(), info.Mode().Perm())
		}
	}
}

func TestBeginErrors(t *testing.T) {
	if _, err := Begin(Options{ID: "x"}); err == nil {
		t.Fatal("Begin without a directory must fail")
	}
	if _, err := Begin(Options{Dir: t.TempDir()}); err == nil {
		t.Fatal("Begin without an identifier must fail")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Begin(Options{Dir: file, ID: "x"}); err == nil {
		t.Fatal("Begin in a path that is a file must fail")
	}

	dir := t.TempDir()
	setErr := errors.New("no crash output here")
	rec, err := Begin(Options{Dir: dir, ID: "crash002", SetCrashOutput: func(*os.File) error { return setErr }})
	if err != nil || !errors.Is(rec.CrashOutputErr(), setErr) {
		t.Fatalf("crash output failure: rec err=%v crash err=%v", err, rec.CrashOutputErr())
	}
	if readCurrent(t, dir).Start != "crash002" {
		t.Fatal("a crash output failure must still record the start")
	}

	// A start in the same millisecond as a kept trace gets its own file.
	fixed := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	same := t.TempDir()
	kept := logread.HistoryFileName(logread.SourceCrash, fixed)
	if err := os.WriteFile(filepath.Join(same, kept), []byte(`{"time":"x"}`+"\npanic: earlier\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Begin(Options{Dir: same, ID: "same0001", Now: func() time.Time { return fixed }, SetCrashOutput: noCrashOutput}); err != nil {
		t.Fatal(err)
	}
	if st := readCurrent(t, same); st.CrashFile != logread.HistoryFileName(logread.SourceCrash, fixed.Add(time.Millisecond)) {
		t.Fatalf("crash file = %q, want the next free millisecond", st.CrashFile)
	}
	if data, err := os.ReadFile(filepath.Join(same, kept)); err != nil || !strings.Contains(string(data), "panic: earlier") {
		t.Fatalf("kept trace overwritten: %q %v", data, err)
	}
}

func TestNilRecorderAndNewID(t *testing.T) {
	var r *Recorder
	if r.ID() != "" || r.Previous().Verdict != VerdictUnknown || r.CrashOutputErr() != nil || r.Heartbeat() != nil || r.Close(VerdictClean) != nil {
		t.Fatal("nil Recorder methods must be no-ops")
	}
	r.StartHeartbeat(time.Second)

	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		id := NewID()
		if len(id) != 2*idBytes || strings.Trim(id, "0123456789abcdef") != "" {
			t.Fatalf("NewID() = %q, want 8 hex characters", id)
		}
		seen[id] = true
	}
	if len(seen) < 30 {
		t.Fatalf("NewID repeats: %d distinct of 32", len(seen))
	}
	rec := begin(t, t.TempDir(), "idcheck1", nil, nil)
	if rec.ID() != "idcheck1" {
		t.Fatalf("ID() = %q", rec.ID())
	}
}
