package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type prTextRunner struct{ gitLog string }

func (r prTextRunner) Run(_ context.Context, _ string, _ []string, name string, args ...string) ([]byte, error) {
	if name == "git" && len(args) > 0 && args[0] == "log" {
		return []byte(r.gitLog), nil
	}
	return nil, nil
}

func TestSandboxPRText_PrefersWorkspaceFiles(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".hive"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(ws, ".hive", "pr-title.txt"), []byte("deps: bump torch-spyre to b49e05c\nsecond line ignored\n"), 0o644)
	os.WriteFile(filepath.Join(ws, ".hive", "pr-body.md"), []byte("## Summary\n\nbuilt + smoke test green\n"), 0o644)
	e := &SandboxExecutor{Runner: prTextRunner{gitLog: "commit subject\n\ncommit body"}}
	title, body := e.sandboxPRText(context.Background(), ws, "deps")
	if title != "deps: bump torch-spyre to b49e05c" {
		t.Fatalf("title = %q", title)
	}
	if !strings.HasPrefix(body, "## Summary") {
		t.Fatalf("body = %q", body)
	}
}

func TestSandboxPRText_FallsBackToHeadCommit(t *testing.T) {
	e := &SandboxExecutor{Runner: prTextRunner{gitLog: "fix: tighten retry\n\nwhy it matters\n\nSigned-off-by: x\n"}}
	title, body := e.sandboxPRText(context.Background(), t.TempDir(), "deps")
	if title != "fix: tighten retry" {
		t.Fatalf("title = %q", title)
	}
	if !strings.HasPrefix(body, "why it matters") {
		t.Fatalf("body = %q", body)
	}
}

func TestSandboxPRText_PlaceholderLast(t *testing.T) {
	e := &SandboxExecutor{Runner: prTextRunner{}}
	title, body := e.sandboxPRText(context.Background(), t.TempDir(), "deps")
	if title != "[deps] sandbox changes" || !strings.Contains(body, "`deps`") {
		t.Fatalf("title=%q body=%q", title, body)
	}
	if strings.Contains(body, "#2804") {
		t.Fatalf("placeholder must not reference hive's own tracker: %q", body)
	}
}

func TestSandboxPRText_TitleIsBounded(t *testing.T) {
	ws := t.TempDir()
	os.MkdirAll(filepath.Join(ws, ".hive"), 0o755)
	os.WriteFile(filepath.Join(ws, ".hive", "pr-title.txt"), []byte(strings.Repeat("x", 500)), 0o644)
	e := &SandboxExecutor{Runner: prTextRunner{}}
	title, _ := e.sandboxPRText(context.Background(), ws, "deps")
	if len([]rune(title)) != sandboxPRTitleMaxLen {
		t.Fatalf("title len = %d", len([]rune(title)))
	}
}
