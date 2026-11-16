package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/transcribe/whisperbox"
)

func TestVersionAndUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"-version"}, nil, &out, &errb); code != 0 || strings.TrimSpace(out.String()) != version {
		t.Fatalf("version = %d %q", code, out.String())
	}
	if code := runMain([]string{"-version"}); code != 0 {
		t.Fatalf("runMain version = %d", code)
	}
	if code := run(context.Background(), []string{"-bogus"}, nil, &out, &errb); code != whisperbox.ExitUsage {
		t.Fatalf("bad flag = %d", code)
	}
}

func TestRunTranscribesWithFakeEngines(t *testing.T) {
	dir := t.TempDir()
	scripts := map[string]string{
		"ffmpeg":      "#!/bin/sh\nfor a; do last=$a; done\nhead -c 32044 /dev/zero > \"$last\"\n",
		"whisper-cli": "#!/bin/sh\nwhile [ $# -gt 0 ]; do case $1 in -of) of=$2; shift;; esac; shift; done\nprintf '{\"transcription\":[]}' > \"$of.json\"\n",
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	model := filepath.Join(dir, "model.bin")
	if err := os.WriteFile(model, []byte("m"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	args := []string{"-model", model, "-engine-dir", dir, "-limits=false", "-no-network=false", "-timeout", "20s", "-work-root", t.TempDir(), "-threads", "2", "-lang", "en"}
	if code := run(context.Background(), args, strings.NewReader("AUDIO"), &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if out.String() != `{"transcription":[]}` || !strings.Contains(errb.String(), "phase=transcribing audio_seconds=1.00") {
		t.Fatalf("out %q err %q", out.String(), errb.String())
	}
}
