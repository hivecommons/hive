package dashboard

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/scheduler"
)

func TestKickBrainstormSendKickSuccessPath(t *testing.T) {
	s, deps := apiServer(t)
	deps.Scheduler = scheduler.New(deps.Config, deps.Logger)
	done := make(chan struct{})
	var sent []string
	s.kickBrainstormDoneFn = func() { close(done) }
	s.kickBrainstormSendKickFn = func(name, msg string) error {
		if name != "brainstorm" {
			t.Fatalf("name = %q, want brainstorm", name)
		}
		sent = append(sent, msg)
		return nil
	}

	origDelay := kickBrainstormClearDelay
	kickBrainstormClearDelay = 0
	t.Cleanup(func() { kickBrainstormClearDelay = origDelay })

	s.kickBrainstorm()
	waitForKickBrainstorm(t, done)

	if len(sent) != 2 {
		t.Fatalf("sent %d messages, want clear plus kick: %#v", len(sent), sent)
	}
	if sent[0] != "/clear" {
		t.Fatalf("first message = %q, want /clear", sent[0])
	}
	if sent[1] == "" {
		t.Fatal("second message is empty")
	}
}

func TestKickBrainstormFallsBackToRestart(t *testing.T) {
	s, deps := apiServer(t)
	deps.Scheduler = scheduler.New(deps.Config, deps.Logger)
	done := make(chan struct{})
	restarted := make(chan string, 1)
	s.kickBrainstormDoneFn = func() { close(done) }
	s.kickBrainstormSendKickFn = func(name, msg string) error {
		return errors.New("no running session")
	}
	s.kickBrainstormRestartFn = func(ctx context.Context, name, prompt string) error {
		if name != "brainstorm" {
			t.Fatalf("restart name = %q, want brainstorm", name)
		}
		restarted <- prompt
		return nil
	}

	s.kickBrainstorm()
	waitForKickBrainstorm(t, done)

	select {
	case prompt := <-restarted:
		if prompt == "" {
			t.Fatal("restart prompt is empty")
		}
	default:
		t.Fatal("expected RestartWithBootstrap fallback")
	}
}

func TestKickBrainstormRecoversPanics(t *testing.T) {
	s, deps := apiServer(t)
	deps.Scheduler = scheduler.New(deps.Config, deps.Logger)
	done := make(chan struct{})
	s.kickBrainstormDoneFn = func() { close(done) }
	s.kickBrainstormSendKickFn = func(name, msg string) error {
		panic("send failed hard")
	}
	s.logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	s.kickBrainstorm()
	waitForKickBrainstorm(t, done)
}

func waitForKickBrainstorm(t *testing.T, done <-chan struct{}) {
	t.Helper()
	const timeout = 2 * time.Second
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("kickBrainstorm goroutine did not finish")
	}
}
