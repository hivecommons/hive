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
	if got, want := backendLaunchCmd("pi", "gpt-5.6-luna", "pi", false, ""), "pi --model gpt-5.6-luna"; got != want {
		t.Fatalf("pi launch path must invoke pi with --model: got %q, want %q", got, want)
	}
	// An empty model omits the flag rather than emitting a bare --model.
	if got, want := backendLaunchCmd("pi", "", "pi", false, ""), "pi"; got != want {
		t.Fatalf("pi launch with no model = %q, want %q", got, want)
	}
}
