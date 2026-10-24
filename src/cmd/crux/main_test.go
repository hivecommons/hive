package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/cli"
)

func TestRunPrintsUsageWithoutACommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--workdir", t.TempDir()}, strings.NewReader(""), &stdout, &stderr, func() (context.Context, context.CancelFunc) {
		return context.WithCancel(context.Background())
	})
	if code != cli.ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, cli.ExitUsage)
	}
	if !strings.Contains(stderr.String(), "usage: crux") {
		t.Fatalf("stderr = %q, want usage", stderr.String())
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"--workdir", t.TempDir(), "frobnicate"}, strings.NewReader(""), &bytes.Buffer{}, &stderr, func() (context.Context, context.CancelFunc) {
		return context.WithCancel(context.Background())
	})
	if code != cli.ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, cli.ExitUsage)
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestSignalContextIsCancellable(t *testing.T) {
	ctx, stop := signalContext()
	stop()
	<-ctx.Done()
}
