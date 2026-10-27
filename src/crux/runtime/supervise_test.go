package runtime

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/cruxtest"
	"github.com/hivecommons/hive/crux/serve"
)

// waitFor polls cond until it holds or the test deadline nears.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-tick.C:
		}
	}
}

// fakeCrux writes a script that ignores its arguments and sleeps so the
// supervisor sees a long-lived child.
func fakeCrux(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	p := filepath.Join(t.TempDir(), "crux")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestServeArgs(t *testing.T) {
	got, err := ServeArgs("unix:///run/crux.sock")
	if err != nil || strings.Join(got, " ") != "serve --socket /run/crux.sock" {
		t.Fatalf("unix: %v %v", got, err)
	}
	got, err = ServeArgs("127.0.0.1:9000")
	if err != nil || strings.Join(got, " ") != "serve --listen 127.0.0.1:9000" {
		t.Fatalf("tcp: %v %v", got, err)
	}
	if _, err := ServeArgs(""); err == nil {
		t.Fatal("empty address must fail")
	}
}

func TestSupervisorBinaryNotFound(t *testing.T) {
	s := &Supervisor{Config: config.RuntimeConfig{Server: "127.0.0.1:1", Binary: filepath.Join(t.TempDir(), "nope")}, Logger: quietLogger()}
	err := s.Run(context.Background())
	if !errors.Is(err, ErrBinaryNotFound) {
		t.Fatalf("Run = %v, want ErrBinaryNotFound", err)
	}
	s = &Supervisor{Config: config.RuntimeConfig{Server: "", Binary: fakeCrux(t)}, Logger: quietLogger()}
	if err := s.Run(context.Background()); err == nil || errors.Is(err, ErrBinaryNotFound) {
		t.Fatalf("empty server = %v, want address error", err)
	}
}

func TestSupervisorRestartsAfterExit(t *testing.T) {
	rec := cruxtest.NewRecorder()
	srv := serve.New(rec)
	srv.Token = "tok"
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	s := &Supervisor{
		Config:     config.RuntimeConfig{Server: ts.URL, Token: "tok", Binary: fakeCrux(t)},
		Logger:     quietLogger(),
		MinBackoff: 10 * time.Millisecond,
		MaxBackoff: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	waitFor(t, "first child ready", func() bool { st := s.State(); return st.Ready && st.Running && st.PID > 0 })
	first := s.State()
	if first.Starts != 1 || first.Binary == "" {
		t.Fatalf("state after first start = %+v", first)
	}
	if err := syscall.Kill(first.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "restart", func() bool { st := s.State(); return st.Starts == 2 && st.Ready })
	second := s.State()
	if second.PID == first.PID || second.LastExit == "" || second.LastExitAt.IsZero() {
		t.Fatalf("state after restart = %+v (first pid %d)", second, first.PID)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run after cancel = %v", err)
	}
	waitFor(t, "child reaped", func() bool { return !s.State().Running })
}

func TestSupervisorKillsChildThatNeverBecomesReady(t *testing.T) {
	// Nothing listens on the configured address, so readiness must time out
	// and the child be killed and restarted.
	s := &Supervisor{
		Config:       config.RuntimeConfig{Server: "127.0.0.1:1", Binary: fakeCrux(t)},
		Logger:       quietLogger(),
		MinBackoff:   5 * time.Millisecond,
		MaxBackoff:   10 * time.Millisecond,
		ReadyTimeout: 50 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	waitFor(t, "two failed starts", func() bool { return s.State().Starts >= 2 })
	if st := s.State(); st.Ready || st.LastExit == "" {
		t.Fatalf("state = %+v", st)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorStartFailureBacksOff(t *testing.T) {
	var calls atomic.Int32
	s := &Supervisor{
		Config:     config.RuntimeConfig{Server: "127.0.0.1:1", Binary: fakeCrux(t)},
		Logger:     quietLogger(),
		MinBackoff: 5 * time.Millisecond,
		MaxBackoff: 10 * time.Millisecond,
		start: func(*exec.Cmd) error {
			calls.Add(1)
			return errors.New("boom")
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	waitFor(t, "retries", func() bool { return calls.Load() >= 3 })
	if st := s.State(); st.LastExit != "boom" || st.Starts != 0 || st.NextStart.IsZero() {
		t.Fatalf("state = %+v", st)
	}
	cancel()
	<-done
}

func TestSupervisorDefaults(t *testing.T) {
	var s Supervisor
	s.defaults()
	if s.MinBackoff != DefaultMinBackoff || s.MaxBackoff != DefaultMaxBackoff || s.ResetAfter != DefaultResetAfter || s.ReadyTimeout != DefaultReadyTimeout {
		t.Fatalf("defaults = %s %s %s %s", s.MinBackoff, s.MaxBackoff, s.ResetAfter, s.ReadyTimeout)
	}
	s = Supervisor{MinBackoff: 2 * time.Hour}
	s.defaults()
	if s.MaxBackoff != 2*time.Hour {
		t.Fatalf("max below min must clamp, got %s", s.MaxBackoff)
	}
	if s.logger() == nil {
		t.Fatal("nil logger")
	}
}
