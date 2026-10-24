package tmux

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// installFakeTmux puts a tiny script first in PATH so no test touches a
// developer's real tmux server.
func installFakeTmux(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, Binary)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir)
	return path
}

func TestLookPathResolvesThroughPATH(t *testing.T) {
	want := installFakeTmux(t, "exit 0")
	got, err := LookPath()
	if err != nil {
		t.Fatalf("LookPath: %v", err)
	}
	if got != want {
		t.Fatalf("LookPath = %q, want %q", got, want)
	}

	t.Setenv("PATH", t.TempDir())
	if _, err := LookPath(); err == nil {
		t.Fatal("LookPath succeeded with no tmux on PATH")
	}
}

func TestCommandKeepsBinaryAsArgs0(t *testing.T) {
	path := installFakeTmux(t, "exit 0")
	for name, cmd := range map[string][]string{
		"Command":        Command("ls").Args,
		"CommandContext": CommandContext(context.Background(), "ls").Args,
	} {
		if want := []string{Binary, "ls"}; !reflect.DeepEqual(cmd, want) {
			t.Errorf("%s args = %q, want %q", name, cmd, want)
		}
	}
	if got := Command("ls").Path; got != path {
		t.Errorf("Command path = %q, want fake tmux %q", got, path)
	}
}

func TestAttachBuildsAttachTarget(t *testing.T) {
	installFakeTmux(t, "exit 0")
	want := []string{Binary, "attach", "-t", "hive-scanner"}
	if got := Attach("hive-scanner").Args; !reflect.DeepEqual(got, want) {
		t.Fatalf("Attach args = %q, want %q", got, want)
	}
}

func TestHasSessionReturnsOutputAndExitError(t *testing.T) {
	installFakeTmux(t, `
if [ "$1" = "has-session" ] && [ "$3" = "hive-present" ]; then exit 0; fi
echo "can't find session: $3" >&2
exit 1`)

	if out, err := HasSession("hive-present"); err != nil || len(out) != 0 {
		t.Fatalf("HasSession(present) = %q, %v; want empty, nil", out, err)
	}
	out, err := HasSession("hive-missing")
	if err == nil {
		t.Fatal("HasSession(missing) returned nil error")
	}
	if !strings.Contains(string(out), "can't find session: hive-missing") {
		t.Fatalf("HasSession(missing) output = %q, want tmux diagnostic", out)
	}
}
