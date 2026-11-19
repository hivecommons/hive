package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/mailin"
)

func TestRunMainVersion(t *testing.T) {
	var out bytes.Buffer
	if code := runMain([]string{"-version"}, func(string) string { return "" }, &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "hive-mx") {
		t.Fatalf("version output %q", out.String())
	}
}

func TestRunMainBadConfig(t *testing.T) {
	var out bytes.Buffer
	if code := runMain(nil, func(string) string { return "" }, &out); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(out.String(), mailin.EnvHubURL) {
		t.Fatalf("log %q does not name the missing setting", out.String())
	}
}

func TestRunMainRuns(t *testing.T) {
	orig := run
	t.Cleanup(func() { run = orig })
	env := map[string]string{mailin.EnvHubURL: "https://hub.example"}
	getenv := func(k string) string { return env[k] }

	var got mailin.Config
	run = func(_ context.Context, c mailin.Config, _ *slog.Logger) error { got = c; return nil }
	var out bytes.Buffer
	if code := runMain(nil, getenv, &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got.HubURL != "https://hub.example" {
		t.Fatalf("config not passed: %+v", got)
	}

	run = func(context.Context, mailin.Config, *slog.Logger) error { return errors.New("boom") }
	if code := runMain(nil, getenv, &out); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}
