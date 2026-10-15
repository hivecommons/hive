package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/logread"
)

// AC1: what hive prints before its own log is set up still reaches stdout
// and is also readable as the hive-output source.
func TestBootConfigWith_EarlyOutputKeptAsHiveOutput(t *testing.T) {
	dir := t.TempDir()
	f := newBootConfigFake(t)
	f.env[logread.OutputDirEnv] = dir
	f.deps.earlyOutput = teeEarlyOutput
	f.deps.loadConfig = func(string) (*config.Config, error) {
		return nil, errors.New("config file is unreadable")
	}

	_, _, code := runBootConfig(t, &boot{}, f.deps)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(f.stdout.String(), "failed to load config") {
		t.Fatalf("stdout lost the early line: %q", f.stdout.String())
	}
	res := logread.Read(logread.Request{Source: logread.SourceHiveOutput, Include: "failed to load config"}, logread.Options{OutputDir: dir})
	if res.Status != logread.StatusOK || len(res.Lines) == 0 {
		t.Fatalf("hive-output read: status=%s reason=%q lines=%d", res.Status, res.Reason, len(res.Lines))
	}
	line := res.Lines[0]
	if line.Stream != logread.StreamStdout || line.Level != "error" || !strings.Contains(line.Text, "config file is unreadable") {
		t.Fatalf("kept line = %+v", line)
	}
}

// A nil seam leaves the early logger on stdout alone.
func TestBootConfigWith_NilEarlyOutputWritesStdoutOnly(t *testing.T) {
	f := newBootConfigFake(t)
	f.deps.earlyOutput = nil
	f.deps.loadConfig = func(string) (*config.Config, error) { return nil, errors.New("boom") }
	_, _, code := runBootConfig(t, &boot{}, f.deps)
	if code != 1 || !strings.Contains(f.stdout.String(), "failed to load config") {
		t.Fatalf("code=%d stdout=%q", code, f.stdout.String())
	}
}

func TestTeeEarlyOutput(t *testing.T) {
	t.Run("kept and passed through", func(t *testing.T) {
		dir := t.TempDir()
		var stdout bytes.Buffer
		w := teeEarlyOutput(&stdout, func(k string) string {
			if k == logread.OutputDirEnv {
				return dir
			}
			return ""
		})
		if _, err := w.Write([]byte("{\"time\":\"2026-01-02T03:04:05Z\",\"level\":\"INFO\",\"msg\":\"hello\"}\n")); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout.String(), "hello") {
			t.Fatalf("stdout = %q", stdout.String())
		}
		data, err := os.ReadFile(filepath.Join(dir, logread.SourceHiveOutput+".log"))
		if err != nil || !strings.Contains(string(data), `"stream":"stdout"`) || !strings.Contains(string(data), "hello") {
			t.Fatalf("hive-output file = %q (%v)", data, err)
		}
	})
	t.Run("falls back to stdout", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(blocker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout bytes.Buffer
		w := teeEarlyOutput(&stdout, func(k string) string {
			if k == logread.OutputDirEnv {
				return filepath.Join(blocker, "logs")
			}
			return ""
		})
		if w != &stdout {
			t.Fatalf("writer = %T, want stdout itself", w)
		}
		if !strings.Contains(stdout.String(), "early hive output is not kept") {
			t.Fatalf("missing warning: %q", stdout.String())
		}
	})
}
