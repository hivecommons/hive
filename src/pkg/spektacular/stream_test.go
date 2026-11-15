package spektacular

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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
