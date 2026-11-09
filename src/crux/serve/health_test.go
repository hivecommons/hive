package serve

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/api"
)

// fakeTmuxDir returns a directory holding an executable named tmux, for a
// PATH that resolves it without needing a real install.
func fakeTmuxDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestHealthTmuxMissingIs503(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var h api.Health
	newFamServer(t).expect(http.MethodGet, api.PathHealth, nil, http.StatusServiceUnavailable, &h)
	if h.Tmux != "" || h.Version != api.Version {
		t.Fatalf("health: %+v", h)
	}
	// The body is also an api.Error, so api.Client callers see the cause.
	if h.Code != api.CodeUnavailable || !strings.Contains(h.Error, "tmux not found on PATH") {
		t.Fatalf("health error: %+v", h)
	}
}

func TestHealthTmuxPresentIs200(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	var h api.Health
	newFamServer(t).expect(http.MethodGet, api.PathHealth, nil, http.StatusOK, &h)
	if h.Tmux == "" {
		t.Fatalf("health: %+v", h)
	}
}
