package tui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/hivectl"
)

func TestContributorSessionCommands(t *testing.T) {
	for _, runtime := range []string{"", "docker", "podman"} {
		r := hivectl.ContributorRelay{TmuxSession: "hive-pi-abcd", Mode: "local"}
		if runtime != "" {
			r.ContainerRuntime = runtime
			r.ContainerName = "hive-contributor-pi"
			r.Mode = "container"
		}
		for _, attach := range []bool{false, true} {
			cmd := contributorSessionCommand(r, attach)
			want := []string{"tmux", "capture-pane", "-p", "-e", "-t", r.TmuxSession}
			if attach {
				want = []string{"tmux", "attach", "-t", r.TmuxSession}
			}
			if runtime != "" {
				prefix := []string{runtime, "exec"}
				if attach {
					prefix = append(prefix, "-it")
				}
				want = append(append(prefix, r.ContainerName), want...)
			}
			if !reflect.DeepEqual(cmd.Args, want) {
				t.Fatalf("got %q, want %q", cmd.Args, want)
			}
		}
		r.Mode = "headless"
		if contributorSessionCommand(r, false) != nil || contributorSessionCommand(r, true) != nil {
			t.Fatal("headless created a tmux command")
		}
	}
	if contributorSessionCommand(hivectl.ContributorRelay{}, true) != nil {
		t.Fatal("old record guessed a session")
	}
}

func TestWatchTailBounded(t *testing.T) {
	b := &watchTail{}
	_, _ = b.Write([]byte(strings.Repeat("a", watchBufferSize*2)))
	_, _ = b.Write([]byte("end"))
	if len(b.text()) != watchBufferSize || !strings.HasSuffix(b.text(), "end") {
		t.Fatal("log tail not bounded or not following")
	}
}

func TestWatchLogReopensAfterRotation(t *testing.T) {
	file := filepath.Join(t.TempDir(), "relay.log")
	if err := os.WriteFile(file, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if readWatchLog(file) != "old" {
		t.Fatal("initial log missing")
	}
	if err := os.Rename(file, file+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if readWatchLog(file) != "new" {
		t.Fatal("reader kept rotated inode")
	}
}

func TestWatchQuittingDoesNotStopContributor(t *testing.T) {
	m := watchModel{source: &watchSource{}, snapshot: watchSnapshot{relay: &hivectl.ContributorRelay{PID: 123}}}
	for _, key := range []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune{'q'}}, {Type: tea.KeyCtrlC}} {
		_, cmd := m.Update(key)
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatal("quit did not quit viewer")
		}
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	if cmd != nil || !next.(watchModel).confirmStop {
		t.Fatal("stop did not require confirmation")
	}
	next, cmd = next.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if cmd != nil || next.(watchModel).confirmStop {
		t.Fatal("cancel stop ran a command")
	}
}

func TestWatchExplicitStatesAndSafeRendering(t *testing.T) {
	for _, text := range []string{"headless: no session", "session unknown — attach manually", "No contributor running — start one with `just contribute-hive …`"} {
		m := watchModel{width: 100, height: 24, snapshot: watchSnapshot{session: text}}
		if !strings.Contains(m.View(), text) {
			t.Fatalf("missing state %q", text)
		}
	}
	got := watchPane("\x1b]52;c;ZXZpbA==\a\x1b[31mhello\x1b[0m\r\x00", 20, 1, false)
	if got != "hello" {
		t.Fatalf("terminal controls replayed: %q", got)
	}
	if got := watchPane("12345\n67890\nabcde", 3, 2, true); got != "678\nabc" {
		t.Fatalf("crop: %q", got)
	}
}

func TestWatchContainerLogFollowerUsesRecordedRuntimeAndReaps(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("requires a POSIX shell")
	}
	for _, name := range []string{"docker", "podman"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			runtime := filepath.Join(dir, name)
			argsFile := filepath.Join(dir, "args")
			t.Setenv("WATCH_TEST_ARGS", argsFile)
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$WATCH_TEST_ARGS\"\nprintf 'relay update\\n'\nexec sleep 60\n"
			if err := os.WriteFile(runtime, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			tail := &watchTail{}
			stop := followWatchLog(context.Background(), hivectl.ContributorRelay{ContainerRuntime: runtime, ContainerName: "hive-contributor-test"}, tail)
			defer stop()
			testutil.Eventually(t, 3*time.Second, func() bool {
				return strings.Contains(tail.text(), "relay update")
			}, "container log was not followed")
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			if string(args) != "logs\n-f\n--tail\n100\nhive-contributor-test\n" {
				t.Fatalf("wrong log command: %q", args)
			}
			stopped := make(chan struct{})
			go func() { stop(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(3 * time.Second):
				t.Fatal("log follower was not reaped")
			}
		})
	}
}

func TestWatchActionsAreCanceledAndJoined(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	actions := &watchActions{ctx: ctx}
	cmd := actions.start(func(ctx context.Context) tea.Msg {
		<-ctx.Done()
		return watchActionDone{text: "canceled"}
	})
	cancel()
	actions.wg.Wait()
	if got := cmd().(watchActionDone).text; got != "canceled" {
		t.Fatalf("action survived viewer: %q", got)
	}
}

func TestWatchSourceMissingRecordAndShutdown(t *testing.T) {
	source := &watchSource{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	store := hivectl.NewProfileStore(t.TempDir())
	go func() { defer close(done); source.run(ctx, store) }()
	t.Cleanup(func() { cancel(); <-done })
	testutil.Eventually(t, 3*time.Second, func() bool {
		return strings.Contains(source.get().session, "No contributor running")
	}, "missing record not displayed")
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("watch source did not shut down")
	}
}
