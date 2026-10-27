package inference

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/logread"
)

// TestMain keeps the litellm log source (#11944) out of the real
// /data/.hive/logs: on a hive host that is a live directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "inference-output-logs-")
	if err != nil {
		panic("inference TestMain: " + err.Error())
	}
	if err := os.Setenv(logread.OutputDirEnv, dir); err != nil {
		panic("inference TestMain: " + err.Error())
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// stubChattyLitellm places a fake `litellm` on PATH that prints one line to
// each stream, records the call, and exits non-zero.
func stubChattyLitellm(t *testing.T) (argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "invocations")
	script := fmt.Sprintf("#!/bin/sh\necho \"litellm ready on $4\"\necho \"litellm warning: no models\" >&2\necho \"$@\" >> %q\nexit 3\n", argsFile)
	if err := os.WriteFile(filepath.Join(dir, "litellm"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing litellm stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
}

// AC3: the local litellm proxy's output, discarded before, is readable as
// the litellm source with each line's stream.
func TestSuperviseLocalLiteLLMKeepsOutput(t *testing.T) {
	outDir := t.TempDir()
	t.Setenv(logread.OutputDirEnv, outDir)
	argsFile := stubChattyLitellm(t)
	_, cancel, done := runSupervisor(t)
	defer cancel()
	waitForInvocations(t, argsFile, 1, 5*time.Second)
	cancel()
	awaitReturn(t, done)

	res := logread.Read(logread.Request{Source: logread.SourceLiteLLM}, logread.Options{OutputDir: outDir, LocalLiteLLM: true})
	if res.Status != logread.StatusOK {
		t.Fatalf("litellm source: %s (%s)", res.Status, res.Reason)
	}
	streams := map[string]string{}
	for _, line := range res.Lines {
		if line.Time == "" {
			t.Errorf("line without time: %+v", line)
		}
		streams[line.Text] = line.Stream
	}
	if streams["litellm ready on "+fmt.Sprint(LocalLiteLLMProxyPort)] != logread.StreamStdout {
		t.Errorf("stdout line missing or mis-streamed: %+v", res.Lines)
	}
	if streams["litellm warning: no models"] != logread.StreamStderr {
		t.Errorf("stderr line missing or mis-streamed: %+v", res.Lines)
	}
	info, err := os.Stat(filepath.Join(outDir, logread.SourceLiteLLM+".log"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("litellm log mode = %v (%v), want 0600", info, err)
	}
}

// When the output file cannot be opened the proxy still runs, with its
// output discarded as before, and the reason is logged.
func TestSuperviseLocalLiteLLMRunsWhenOutputCannotBeKept(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(logread.OutputDirEnv, filepath.Join(blocker, "logs"))
	argsFile := stubChattyLitellm(t)
	logs, cancel, done := runSupervisor(t)
	defer cancel()
	waitForInvocations(t, argsFile, 1, 5*time.Second)
	cancel()
	awaitReturn(t, done)
	if !strings.Contains(logs.String(), "output is not kept") {
		t.Fatalf("missing warning:\n%s", logs.String())
	}
}
