package spektacular

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func readLine(t *testing.T, s *Stream) string {
	t.Helper()
	select {
	case l, ok := <-s.Lines():
		if !ok {
			t.Fatal("lines closed")
		}
		return string(l)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a line")
	}
	return ""
}

func waitDone(t *testing.T, s *Stream) {
	t.Helper()
	select {
	case <-s.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("stream never finished")
	}
}

func TestStreamEchoAndClose(t *testing.T) {
	logs := &lockedLog{}
	s := StreamExec("sh", []string{"-c", `echo started >&2; while read -r l; do echo "$l"; done`}, StreamOptions{
		Logger: slog.New(slog.NewTextHandler(logs, nil)),
	})
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("a second Start must fail")
	}
	if err := s.Send(map[string]string{"type": "segment", "text": "hi"}); err != nil {
		t.Fatal(err)
	}
	if got := readLine(t, s); got != `{"text":"hi","type":"segment"}` {
		t.Fatalf("echo = %q", got)
	}
	if err := s.Send(func() {}); err == nil {
		t.Fatal("an unmarshalable value must fail")
	}
	done := make(chan struct{})
	go func() {
		s.Close(map[string]string{"type": "end"})
		close(done)
	}()
	if got := readLine(t, s); got != `{"type":"end"}` {
		t.Fatalf("last line = %q", got)
	}
	<-done
	waitDone(t, s)
	if s.Err() != nil {
		t.Fatalf("clean exit reported %v", s.Err())
	}
	if !errors.Is(s.SendLine([]byte("{}")), ErrStreamNotRunning) {
		t.Fatal("Send after close must fail")
	}
	if !strings.Contains(logs.String(), "started") {
		t.Fatalf("stderr not logged: %q", logs.String())
	}
	if s.Restarts() != 0 {
		t.Fatal("no restarts expected")
	}
}

func TestStreamRestartsThenGivesUp(t *testing.T) {
	var restarted atomic.Int32
	s := StreamExec("sh", []string{"-c", "echo hi; exit 3"}, StreamOptions{
		BackoffMin: time.Millisecond, BackoffMax: 2 * time.Millisecond, MaxRestarts: 2,
		OnRestart: func() { restarted.Add(1) },
	})
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	n := 0
	for range s.Lines() {
		n++
	}
	waitDone(t, s)
	if n != 3 || s.Restarts() != 2 || restarted.Load() != 2 {
		t.Fatalf("lines=%d restarts=%d onRestart=%d", n, s.Restarts(), restarted.Load())
	}
	if s.Err() == nil {
		t.Fatal("want the last exit error")
	}
}

func TestStreamStartFailure(t *testing.T) {
	s := StreamExec(filepath.Join(t.TempDir(), "missing"), nil, StreamOptions{})
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("want a start error")
	}
	waitDone(t, s)
	if _, ok := <-s.Lines(); ok {
		t.Fatal("lines must be closed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s2 := StreamExec("sh", []string{"-c", "true"}, StreamOptions{})
	if err := s2.Start(ctx); err == nil {
		t.Fatal("a cancelled context must not start a child")
	}
	// Close before Start returns at once.
	StreamExec("sh", nil, StreamOptions{}).Close(nil)
}

func TestStreamCloseKillsAfterGrace(t *testing.T) {
	s := StreamExec("sleep", []string{"30"}, StreamOptions{CloseGrace: 50 * time.Millisecond})
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s.Close(nil)
	waitDone(t, s)
	if time.Since(start) > 10*time.Second {
		t.Fatal("Close did not kill the child")
	}
}

func TestStreamContextCancelKills(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := StreamExec("sleep", []string{"30"}, StreamOptions{})
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	waitDone(t, s)
}

func TestStreamCancelDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := StreamExec("sh", []string{"-c", "echo x; exit 1"}, StreamOptions{BackoffMin: time.Hour})
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readLine(t, s); got != "x" {
		t.Fatalf("line %q", got)
	}
	cancel()
	waitDone(t, s)
}

func TestStreamLogWriter(t *testing.T) {
	logs := &lockedLog{}
	w := &streamLogWriter{log: slog.New(slog.NewTextHandler(logs, nil))}
	_, _ = w.Write([]byte("a\n\npart"))
	_, _ = w.Write([]byte("ial\n"))
	_, _ = w.Write(bytes.Repeat([]byte("x"), maxStreamLine+1))
	if !strings.Contains(logs.String(), "line=a") || !strings.Contains(logs.String(), "line=partial") || len(w.buf) != 0 {
		t.Fatalf("logs %q buf %d", logs.String(), len(w.buf))
	}
}

// fakeCLI writes an executable shell script standing in for spektacular and
// returns its path and a file counting its invocations.
func fakeCLI(t *testing.T, body string) (bin, calls string) {
	t.Helper()
	dir := t.TempDir()
	bin, calls = filepath.Join(dir, "spektacular"), filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho x >> '" + calls + "'\n" + body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

func callCount(t *testing.T, calls string) int {
	t.Helper()
	b, err := os.ReadFile(calls)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "x\n")
}

// oldCLIBody is what a spektacular release without `meeting` prints for it:
// cobra's unknown-command error in the CLI's indented JSON error envelope on
// stdout, exit 1 (hivecommons/hive#18630).
const oldCLIBody = `cat <<'JSON'
{
  "error": true,
  "code": "internal_error",
  "message": "unknown command \"meeting\" for \"spektacular\"",
  "next_action": ""
}
JSON
exit 1`

// TestStreamProbeUnsupportedNeverStartsChild pins #18630: when the probe
// says the verb is unknown, no child is started or restarted, the stream is
// done and Unsupported, and the probe runs once per installed binary.
func TestStreamProbeUnsupportedNeverStartsChild(t *testing.T) {
	bin, calls := fakeCLI(t, oldCLIBody)
	logs := &lockedLog{}
	opts := StreamOptions{
		Logger: slog.New(slog.NewTextHandler(logs, nil)), Probe: []string{"meeting", "--help"},
		BackoffMin: time.Millisecond,
	}
	for i := 0; i < 2; i++ {
		s := StreamExec(bin, []string{"meeting", "--from", "stdin"}, opts)
		if err := s.Start(context.Background()); err != nil {
			t.Fatalf("Start = %v, want nil for an unsupported verb", err)
		}
		waitDone(t, s)
		if _, ok := <-s.Lines(); ok {
			t.Fatal("lines must be closed")
		}
		if !s.Unsupported() || !errors.Is(s.Err(), ErrStreamUnsupported) || s.Restarts() != 0 {
			t.Fatalf("unsupported=%v err=%v restarts=%d", s.Unsupported(), s.Err(), s.Restarts())
		}
		if !errors.Is(s.SendLine([]byte("{}")), ErrStreamNotRunning) {
			t.Fatal("Send must fail")
		}
	}
	if n := callCount(t, calls); n != 1 {
		t.Fatalf("binary ran %d times, want only the one probe", n)
	}
	out := logs.String()
	if strings.Contains(out, "level=WARN") || strings.Contains(out, "level=ERROR") || strings.Count(out, "live mode unavailable") != 1 {
		t.Fatalf("want one INFO line and no warnings:\n%s", out)
	}
}

// TestStreamUnknownCommandExitDoesNotRestart covers a stream without a
// probe: the child's exit reason says the verb is unknown, so it is not
// restarted.
func TestStreamUnknownCommandExitDoesNotRestart(t *testing.T) {
	bin, calls := fakeCLI(t, oldCLIBody)
	s := StreamExec(bin, []string{"meeting"}, StreamOptions{BackoffMin: time.Millisecond, MaxRestarts: 3})
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range s.Lines() {
	}
	waitDone(t, s)
	if !s.Unsupported() || s.Restarts() != 0 || callCount(t, calls) != 1 {
		t.Fatalf("unsupported=%v restarts=%d calls=%d", s.Unsupported(), s.Restarts(), callCount(t, calls))
	}
}

// TestStreamProbeSupportedStarts: a binary that knows the verb starts as
// before.
func TestStreamProbeSupportedStarts(t *testing.T) {
	bin, calls := fakeCLI(t, `[ "$2" = "--help" ] && exit 0
while read -r l; do echo "$l"; done`)
	s := StreamExec(bin, []string{"meeting"}, StreamOptions{Probe: []string{"meeting", "--help"}})
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.SendLine([]byte(`{"type":"segment"}`)); err != nil {
		t.Fatal(err)
	}
	if got := readLine(t, s); got != `{"type":"segment"}` {
		t.Fatalf("echo = %q", got)
	}
	s.Close(nil)
	waitDone(t, s)
	if s.Unsupported() || callCount(t, calls) != 2 {
		t.Fatalf("unsupported=%v calls=%d", s.Unsupported(), callCount(t, calls))
	}
}

// TestStreamRestartWarnsOncePerBackoffWindow pins #18630: a child that keeps
// exiting is restarted with backoff, but the restart line is a WARN at most
// once per BackoffMax window (INFO otherwise), so one backoff series does not
// look like a repeated unclassified error.
func TestStreamRestartWarnsOncePerBackoffWindow(t *testing.T) {
	logs := &lockedLog{}
	s := StreamExec("sh", []string{"-c", "exit 3"}, StreamOptions{
		Logger:     slog.New(slog.NewTextHandler(logs, nil)),
		BackoffMin: time.Millisecond, BackoffMax: time.Hour, MaxRestarts: 4,
	})
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range s.Lines() {
	}
	waitDone(t, s)
	if s.Restarts() != 4 {
		t.Fatalf("restarts = %d", s.Restarts())
	}
	warn, info := 0, 0
	for _, l := range strings.Split(logs.String(), "\n") {
		if !strings.Contains(l, "stream child exited; restarting") {
			continue
		}
		switch {
		case strings.Contains(l, "level=WARN"):
			warn++
		case strings.Contains(l, "level=INFO"):
			info++
		}
	}
	if warn != 1 || info != 3 {
		t.Fatalf("warn=%d info=%d, want 1 and 3:\n%s", warn, info, logs.String())
	}
}
