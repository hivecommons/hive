package main

// Tests for three previously uncovered boot-time observability helpers in
// main.go: setupLogger (0%), logAgentSandboxPosture (0%), and
// superviseLocalLiteLLM's context guard (0%). Each is exercised through its
// public behavior — the log file on disk, the emitted WARN lines, the prompt
// return on a cancelled context — not through internals.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// TestSetupLoggerWritesJSONToLogFile pins the happy path: the returned logger
// tees JSON records into <dir>/hive.log, so a crashed pod's last words are on
// the volume and not only in a scrolled-away stdout.
func TestSetupLoggerWritesJSONToLogFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	logger := setupLogger(dir, 1, 1, 1, false, "info")
	if logger == nil {
		t.Fatal("setupLogger returned nil")
	}

	logger.Info("boot observability probe", "probe_key", "probe_value")

	data, err := os.ReadFile(filepath.Join(dir, logFilename))
	if err != nil {
		t.Fatalf("expected %s in %s: %v", logFilename, dir, err)
	}
	line := strings.TrimSpace(string(data))
	if !strings.Contains(line, "boot observability probe") {
		t.Fatalf("log file does not contain the logged message: %q", line)
	}
	// The handler must be JSON — operators grep and jq this file.
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.Split(line, "\n")[0]), &rec); err != nil {
		t.Fatalf("log file line is not JSON: %v (%q)", err, line)
	}
	if rec["probe_key"] != "probe_value" {
		t.Errorf("structured attr lost: got %v", rec["probe_key"])
	}
}

// TestSetupLoggerRespectsConfiguredLevel: the level string from config must
// gate the file handler. A hive configured for "error" must not fill the PVC
// with info spam.
func TestSetupLoggerRespectsConfiguredLevel(t *testing.T) {
	dir := t.TempDir()
	logger := setupLogger(dir, 1, 1, 1, false, "error")

	logger.Info("filtered info line")
	logger.Error("kept error line")

	data, err := os.ReadFile(filepath.Join(dir, logFilename))
	if err != nil {
		t.Fatalf("reading log file: %v", err)
	}
	got := string(data)
	if strings.Contains(got, "filtered info line") {
		t.Errorf("info record leaked past level=error: %q", got)
	}
	if !strings.Contains(got, "kept error line") {
		t.Errorf("error record missing at level=error: %q", got)
	}
}

// TestSetupLoggerFallsBackToStdoutWhenDirUncreatable: when the log dir cannot
// be created (read-only volume, path shadowed by a file) boot must continue
// with a stdout-only logger — a logging problem must never take the hub down.
func TestSetupLoggerFallsBackToStdoutWhenDirUncreatable(t *testing.T) {
	shadow := filepath.Join(t.TempDir(), "shadow")
	if err := os.WriteFile(shadow, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(shadow, "logs") // MkdirAll fails: parent is a file

	logger := setupLogger(dir, 1, 1, 1, false, "info")
	if logger == nil {
		t.Fatal("setupLogger must return a usable fallback logger, got nil")
	}
	// The fallback must not have created the directory or a log file.
	// (Stat fails with ENOTDIR here, not ENOENT — the parent is a file.)
	if _, err := os.Stat(dir); err == nil {
		t.Errorf("fallback path unexpectedly created %s", dir)
	}
	// And it must be safe to use.
	logger.Info("fallback logger works")
}

// sandboxPostureLogger captures WARN output for the posture tests.
func sandboxPostureLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})), &buf
}

// TestLogAgentSandboxPostureEmitsOneWarnPerWarning: the #4918 inert-gate state
// must surface as a WARN naming the posture, at boot AND on config reload —
// this helper is the shared reporting path for both.
func TestLogAgentSandboxPostureEmitsOneWarnPerWarning(t *testing.T) {
	logger, buf := sandboxPostureLogger()
	cfg := &config.Config{
		AgentSandbox: config.AgentSandboxConfig{Enabled: true, Image: "ghcr.io/example/agent:latest"},
		Agents: map[string]config.AgentConfig{
			"scanner": {},
			"quality": {},
		},
	}

	logAgentSandboxPosture(logger, cfg)

	out := buf.String()
	if got := strings.Count(out, "agent sandbox posture"); got != 1 {
		t.Fatalf("want exactly 1 posture WARN for the inert-gate config, got %d: %q", got, out)
	}
	if !strings.Contains(out, "NO agent is opted in") {
		t.Errorf("WARN does not carry the gate warning text: %q", out)
	}
}

// TestLogAgentSandboxPostureSilentOnCleanConfig: the documented default
// (sandbox off) is not a misconfiguration; the helper must emit nothing, or
// operators learn to ignore the line that matters.
func TestLogAgentSandboxPostureSilentOnCleanConfig(t *testing.T) {
	logger, buf := sandboxPostureLogger()

	logAgentSandboxPosture(logger, &config.Config{
		Agents: map[string]config.AgentConfig{"scanner": {}},
	})
	logAgentSandboxPosture(logger, nil) // nil cfg is the pre-load window

	if out := buf.String(); out != "" {
		t.Errorf("clean/nil config must log nothing, got: %q", out)
	}
}

// TestSuperviseLocalLiteLLMReturnsOnCancelledContext pins the supervisor's
// shutdown contract: a cancelled context makes it return before ever spawning
// the litellm process, so hub shutdown is not held up by the fallback proxy.
func TestSuperviseLocalLiteLLMReturnsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		superviseLocalLiteLLM(ctx, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("superviseLocalLiteLLM did not return on a pre-cancelled context")
	}
}
