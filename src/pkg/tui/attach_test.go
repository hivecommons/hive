package tui

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hivecommons/hive/pkg/tui/client"
)

var attachKey = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")}

// installFakeTmux puts a deliberately tiny executable first in PATH. Attach
// tests may exercise os/exec, but they must never inspect or join a developer's
// real tmux server — especially when the suite runs from inside Hive itself.
func installFakeTmux(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tmux")
	contents := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir)
	return path
}

func TestAttachCmdForConstructsLocalTmuxTarget(t *testing.T) {
	tmuxPath := installFakeTmux(t, "exit 0")

	cmd, err := attachCmdFor("scanner")
	if err != nil {
		t.Fatalf("attachCmdFor: %v", err)
	}
	wantArgs := []string{"tmux", "attach", "-t", "hive-scanner"}
	if !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Fatalf("command args = %q, want %q", cmd.Args, wantArgs)
	}
	if cmd.Path != tmuxPath {
		t.Errorf("command path = %q, want fake tmux %q", cmd.Path, tmuxPath)
	}
}

func TestPrepareAttachSurfacesMissingSessionInFooter(t *testing.T) {
	installFakeTmux(t, `
if [ "$1" = "has-session" ]; then
  echo "can't find session: hive-scanner" >&2
  exit 1
fi
exit 99`)

	// The pinned closedDashboard URL is loopback, so the missing local
	// session triggers the proxy fallback (#5644) — which fails too, against
	// an address nothing listens on. The footer must still lead with the
	// local cause the operator can see from their own machine.
	msg, ok := prepareAttach("scanner", client.New())().(attachReadyMsg)
	if !ok {
		t.Fatalf("preflight returned %T, want attachReadyMsg", msg)
	}
	if msg.cmd != nil {
		t.Fatal("missing session returned an attach command")
	}
	if msg.remote != nil {
		t.Fatal("missing session and unreachable dashboard returned a remote session")
	}
	var missing *tmuxSessionMissingError
	if !errors.As(msg.err, &missing) {
		t.Fatalf("preflight error = %T %v, want *tmuxSessionMissingError", msg.err, msg.err)
	}
	if missing.session != "hive-scanner" {
		t.Errorf("missing session = %q, want hive-scanner", missing.session)
	}

	m := modelWithAgent(false)
	m.width, m.height = 100, 30
	m.attachPending = true
	next, cmd := m.Update(msg)
	failed := next.(model)
	if cmd != nil {
		t.Fatal("missing-session result returned a command; it must not suspend or quit the TUI")
	}
	if failed.attachPending {
		t.Fatal("attach remained pending after the preflight failed")
	}
	for _, want := range []string{"Attach failed:", "hive-scanner", "can't find session"} {
		if !strings.Contains(failed.footerStatus, want) {
			t.Errorf("footer error %q does not contain %q", failed.footerStatus, want)
		}
		if !strings.Contains(failed.View(), want) {
			t.Errorf("rendered footer does not contain %q", want)
		}
	}
}

func TestAttachCmdForReportsMissingTmux(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	cmd, err := attachCmdFor("scanner")
	if cmd != nil {
		t.Fatalf("missing tmux returned command %v", cmd)
	}
	var missing *tmuxNotFoundError
	if !errors.As(err, &missing) {
		t.Fatalf("error = %T %v, want *tmuxNotFoundError", err, err)
	}
}

func TestAttachKeyRequiresFocusedSelectedAgent(t *testing.T) {
	m := modelWithAgent(false)
	m.focus = 1
	next, cmd := m.Update(attachKey)
	if cmd != nil || next.(model).attachPending {
		t.Fatal("a started an attach while another pane was focused")
	}

	empty := newModel()
	next, cmd = empty.Update(attachKey)
	if cmd != nil || next.(model).attachPending {
		t.Fatal("a started an attach without a selected row")
	}

	m = modelWithAgent(false)
	next, cmd = m.Update(attachKey)
	pending := next.(model)
	if cmd == nil || !pending.attachPending {
		t.Fatal("a did not start a preflight for the focused selected agent")
	}
	next, duplicate := pending.Update(attachKey)
	if duplicate != nil || !next.(model).attachPending {
		t.Fatal("a second a queued another attach while the first was pending")
	}
}

func TestAttachCompletionResumesAndRefreshes(t *testing.T) {
	installFakeTmux(t, "exit 0")
	attach, err := attachCmdFor("scanner")
	if err != nil {
		t.Fatalf("attachCmdFor: %v", err)
	}

	m := modelWithAgent(false)
	m.attachPending = true
	next, execCmd := m.Update(attachReadyMsg{cmd: attach})
	if execCmd == nil {
		t.Fatal("successful preflight did not return Bubble Tea's exec command")
	}
	if !next.(model).attachPending {
		t.Fatal("attach stopped being pending before the interactive command completed")
	}
	if msg := execCmd(); msg == nil {
		t.Fatal("exec command produced no Bubble Tea message")
	}

	next, refresh := next.(model).Update(attachDoneMsg{})
	resumed := next.(model)
	if resumed.attachPending {
		t.Fatal("attach remained pending after Bubble Tea restored the terminal")
	}
	if refresh == nil {
		t.Fatal("completed attach did not trigger an immediate fleet refresh")
	}
}
