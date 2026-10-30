package agent

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/internal/testutil"
)

const piIdlePane = `╭──────────────────────────────────────────────╮
│ pi coding agent                              │
╰──────────────────────────────────────────────╯

↑37k ↓20k R756k CH99.6% $0.013 5.9%/1.0M (auto)
`

func TestPiPaneReadiness(t *testing.T) {
	if piContextMarker != "%/" {
		t.Fatalf("piContextMarker = %q, want %%/", piContextMarker)
	}
	if !paneHasCLIMarker(piIdlePane) {
		t.Fatal("paneHasCLIMarker must recognise a live pi pane")
	}
	if !paneShowsInputPrompt(piIdlePane) {
		t.Fatal("paneShowsInputPrompt must treat a live pi status bar as input-ready")
	}
}

func TestDockerfileInstallsPiCLI(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dockerfile := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "Dockerfile"))
	body, err := os.ReadFile(dockerfile)
	if errors.Is(err, fs.ErrNotExist) {
		// src/Dockerfile belongs to the hive root module; absent when the crux
		// module is built standalone (hivecommons/crux).
		testutil.SkipUnlessRequired(t, "src/Dockerfile is not part of the crux module (standalone build)")
	}
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	text := string(body)
	for _, want := range []string{"ARG PI_VERSION=", "@earendil-works/pi-coding-agent@${PI_VERSION}"} {
		if !strings.Contains(text, want) {
			t.Fatalf("Dockerfile must install pi CLI; missing %q", want)
		}
	}
}

func TestPiLaunchCommandPassesModelFlag(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(file), "backend_launch_cmd.go"))
	if err != nil {
		t.Fatalf("read backend_launch_cmd.go: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "case \"pi\":") || !strings.Contains(text, "launchCmd = fmt.Sprintf(\"%s --model %s\", binary, model)") {
		t.Fatal("pi launch path must invoke pi with --model")
	}
}
