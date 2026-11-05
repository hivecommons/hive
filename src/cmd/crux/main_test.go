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

func TestRunReportsVersion(t *testing.T) {
	var stdout bytes.Buffer
	code := run([]string{"--version"}, strings.NewReader(""), &stdout, &bytes.Buffer{}, func() (context.Context, context.CancelFunc) {
		return context.WithCancel(context.Background())
	})
	if code != cli.ExitOK || stdout.String() != "crux "+cli.DefaultVersion+"\n" {
		t.Fatalf("code %d, stdout %q", code, stdout.String())
	}
}

// A bad OTEL configuration costs telemetry, never the command
// (hivecommons/crux#19): run reports the refusal on stderr and the verb still
// completes with its own exit code.
func TestRunKeepsCommandWhenTelemetryConfigIsBad(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
	var stdout, stderr bytes.Buffer
	code := run([]string{"--version"}, strings.NewReader(""), &stdout, &stderr, func() (context.Context, context.CancelFunc) {
		return context.WithCancel(context.Background())
	})
	if code != cli.ExitOK || stdout.String() != "crux "+cli.DefaultVersion+"\n" {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "crux: telemetry disabled:") || !strings.Contains(stderr.String(), "OTEL_EXPORTER_OTLP_PROTOCOL") {
		t.Fatalf("stderr = %q, want the telemetry-disabled notice naming the bad variable", stderr.String())
	}
}
