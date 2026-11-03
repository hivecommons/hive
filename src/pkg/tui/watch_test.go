package tui

import (
	"context"
	"encoding/json"
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
	m := watchModel{source: &watchSource{}, snapshot: watchSnapshot{relay: &hivectl.ContributorRelay{PID: 123, Mode: "container", ContainerRuntime: "docker", ContainerName: "hive-contributor-test"}}}
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

// writeWatchRecord publishes a relay PID record the way the relay does, so the
// tests exercise the real discovery path instead of a stub.
func writeWatchRecord(t *testing.T, store *hivectl.ProfileStore, r hivectl.ContributorRelay) {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(store.RelayPIDPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.RelayPIDPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeWatchTool puts an executable named name first on PATH.
func fakeWatchTool(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestWatchSourceShowsEachRunningContributorState(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("requires a POSIX shell")
	}
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "tmux-args")
	t.Setenv("WATCH_TEST_ARGS", argsFile)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	fakeWatchTool(t, bin, "tmux", "printf '%s\\n' \"$@\" > \"$WATCH_TEST_ARGS\"\nif [ \"$5\" = broken ]; then echo 'no server'; exit 1; fi\nprintf 'agent pane\\n'\n")

	logFile := filepath.Join(t.TempDir(), "relay.log")
	if err := os.WriteFile(logFile, []byte("relay heartbeat\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := hivectl.NewProfileStore(t.TempDir())
	// The test process is a live native PID, so discovery accepts the record.
	pid := os.Getpid()
	writeWatchRecord(t, store, hivectl.ContributorRelay{PID: pid, Mode: "local", TmuxSession: "hive-pi-1", LogFile: logFile})

	source := &watchSource{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); source.run(ctx, store) }()
	t.Cleanup(func() { cancel(); <-done })

	waitFor := func(msg string, ok func(watchSnapshot) bool) {
		t.Helper()
		testutil.Eventually(t, 5*time.Second, func() bool { return ok(source.get()) }, "%s", msg)
	}
	waitFor("tmux mirror and relay log not shown", func(s watchSnapshot) bool {
		return s.session == "agent pane\n" && s.log == "relay heartbeat\n" && s.relay != nil && s.relay.PID == pid
	})
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(args) != "capture-pane\n-p\n-e\n-t\nhive-pi-1\n" {
		t.Fatalf("mirror must only capture the pane, got %q", args)
	}

	writeWatchRecord(t, store, hivectl.ContributorRelay{PID: pid, Mode: "local", TmuxSession: "broken", LogFile: logFile})
	waitFor("capture failure not reported", func(s watchSnapshot) bool {
		return strings.HasPrefix(s.session, "Session unavailable:") && strings.Contains(s.session, "no server")
	})

	writeWatchRecord(t, store, hivectl.ContributorRelay{PID: pid, Mode: "headless", TmuxSession: "hive-pi-1", LogFile: logFile})
	waitFor("headless state not shown", func(s watchSnapshot) bool {
		return s.session == "headless: no session" && s.log == "relay heartbeat\n"
	})

	writeWatchRecord(t, store, hivectl.ContributorRelay{PID: pid})
	waitFor("legacy record states not shown", func(s watchSnapshot) bool {
		return strings.HasPrefix(s.session, "session unknown") && strings.HasPrefix(s.log, "Relay log unavailable — restart")
	})

	if err := os.WriteFile(store.RelayPIDPath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor("corrupt record not reported", func(s watchSnapshot) bool {
		return s.relay == nil && strings.Contains(s.status, "parse contributor PID record") && strings.HasPrefix(s.session, "No contributor running")
	})
}

func TestWatchSourceFollowsContainerLogsAndSwapsFollowerOnReplacement(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("requires a POSIX shell")
	}
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	fakeWatchTool(t, bin, "docker", `case "$1" in
inspect) echo true ;;
logs) printf 'log from %s\n' "$5"; exec sleep 60 ;;
exec) printf 'pane of %s\n' "$2" ;;
esac
`)
	store := hivectl.NewProfileStore(t.TempDir())
	record := hivectl.ContributorRelay{PID: 1, Mode: "container", ContainerRuntime: "docker", ContainerName: "first", TmuxSession: "hive-pi-1"}
	writeWatchRecord(t, store, record)

	source := &watchSource{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); source.run(ctx, store) }()
	t.Cleanup(func() { cancel(); <-done })

	testutil.Eventually(t, 5*time.Second, func() bool {
		s := source.get()
		return s.log == "log from first\n" && s.session == "pane of first\n"
	}, "container log and pane not shown")

	record.ContainerName = "second"
	writeWatchRecord(t, store, record)
	testutil.Eventually(t, 5*time.Second, func() bool {
		return source.get().log == "log from second\n"
	}, "replacement contributor kept the old log follower")
}

func TestWatchLogReadsOnlyTheTailAndReportsMissingLog(t *testing.T) {
	file := filepath.Join(t.TempDir(), "relay.log")
	if got := readWatchLog(file); !strings.HasPrefix(got, "Relay log unavailable: ") {
		t.Fatalf("missing log: %q", got)
	}
	data := strings.Repeat("x", watchBufferSize) + "newest"
	if err := os.WriteFile(file, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readWatchLog(file)
	if len(got) != watchBufferSize || !strings.HasSuffix(got, "newest") {
		t.Fatalf("large log not tailed: len=%d", len(got))
	}
}

func watchKey(s string) tea.KeyMsg {
	switch s {
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func pressWatch(t *testing.T, m watchModel, keys ...string) (watchModel, tea.Cmd) {
	t.Helper()
	var cmd tea.Cmd
	for _, k := range keys {
		var next tea.Model
		next, cmd = m.Update(watchKey(k))
		m = next.(watchModel)
	}
	return m, cmd
}

func TestWatchQuotaKeysRunTheMatchingRecipeAction(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("requires a POSIX shell")
	}
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	fakeWatchTool(t, bin, "just", "if [ \"$2\" = resume ]; then echo 'not paused'; exit 3; fi\necho \"$@\"\n")
	ctx, cancel := context.WithCancel(context.Background())
	actions := &watchActions{ctx: ctx}
	t.Cleanup(func() { cancel(); actions.wg.Wait() })
	base := watchModel{source: &watchSource{}, actions: actions, snapshot: watchSnapshot{relay: &hivectl.ContributorRelay{PID: 1}}}

	for i, action := range watchQuotaActions[:5] {
		m, cmd := pressWatch(t, base, "c", string(rune('1'+i)))
		if cmd == nil || !m.busy || m.quota {
			t.Fatalf("key %d did not start %s", i+1, action)
		}
		if got := cmd().(watchActionDone).text; got != "contribute-quota "+action {
			t.Fatalf("key %d ran %q, want %s", i+1, got, action)
		}
	}
	_, cmd := pressWatch(t, base, "c", "6")
	if got := cmd().(watchActionDone).text; !strings.HasPrefix(got, "Quota: exit status 3 — not paused") {
		t.Fatalf("recipe failure not surfaced: %q", got)
	}

	m, cmd := pressWatch(t, base, "c", "7")
	if cmd != nil || !m.quota {
		t.Fatal("out-of-range quota key ran an action or closed the menu")
	}
	if m, _ = pressWatch(t, m, "esc"); m.quota {
		t.Fatal("esc did not close the quota menu")
	}
}

func TestWatchBusyOrMissingContributorBlocksActions(t *testing.T) {
	relay := &hivectl.ContributorRelay{PID: 1, TmuxSession: "hive-pi-1"}
	busy := watchModel{source: &watchSource{}, busy: true, snapshot: watchSnapshot{relay: relay}}
	idle := watchModel{source: &watchSource{}}
	for _, m := range []watchModel{busy, idle} {
		for _, k := range []string{"s", "c", "a"} {
			got, cmd := pressWatch(t, m, k)
			if cmd != nil || got.confirmStop || got.quota {
				t.Fatalf("busy=%v relay=%v: %q started an action", m.busy, m.snapshot.relay != nil, k)
			}
		}
	}
	// A confirmation that arrives after an action started must not stop anything.
	busy.confirmStop = true
	if got, cmd := pressWatch(t, busy, "y"); cmd != nil || got.confirmStop {
		t.Fatal("stop confirmed while another action was running")
	}
}

func TestWatchAttachOnlyWhenASessionIsKnown(t *testing.T) {
	m := watchModel{source: &watchSource{}, snapshot: watchSnapshot{relay: &hivectl.ContributorRelay{PID: 1, Mode: "headless", TmuxSession: "hive-pi-1"}}}
	got, cmd := pressWatch(t, m, "a")
	if cmd != nil || got.busy || got.status != "No attach target (headless or session unknown)" {
		t.Fatalf("headless attach: busy=%v status=%q", got.busy, got.status)
	}
	m.snapshot.relay = &hivectl.ContributorRelay{PID: 1, Mode: "local", TmuxSession: "hive-pi-1"}
	if got, cmd = pressWatch(t, m, "a"); cmd == nil || !got.busy {
		t.Fatal("attach to a known session did not suspend the viewer")
	}
	next, _ := got.Update(watchActionDone{text: "Detached; read-only mirror resumed"})
	if done := next.(watchModel); done.busy || done.status != "Detached; read-only mirror resumed" {
		t.Fatal("detach did not resume the mirror")
	}
}

func TestWatchStopRevalidatesTheRecordBeforeStopping(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := hivectl.NewProfileStore(filepath.Join(home, ".config", "hive"))
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("requires a POSIX shell")
	}
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	fakeWatchTool(t, bin, "docker", "case \"$1\" in inspect) echo true ;; esac\n")
	// The record now names a different (live) contributor than the one shown.
	writeWatchRecord(t, store, hivectl.ContributorRelay{Mode: "container", ContainerRuntime: "docker", ContainerName: "hive-contributor-new", StartedAt: "replacement"})
	ctx, cancel := context.WithCancel(context.Background())
	actions := &watchActions{ctx: ctx}
	t.Cleanup(func() { cancel(); actions.wg.Wait() })
	m := watchModel{source: &watchSource{}, actions: actions, snapshot: watchSnapshot{relay: &hivectl.ContributorRelay{Mode: "container", ContainerRuntime: "docker", ContainerName: "hive-contributor-old", StartedAt: "original"}}}
	m, cmd := pressWatch(t, m, "s", "y")
	if cmd == nil || !m.busy {
		t.Fatal("confirmed stop did not start")
	}
	if got := cmd().(watchActionDone).text; got != "Contributor changed; refresh before stopping" {
		t.Fatalf("stale confirmation acted on a replacement: %q", got)
	}
	if err := os.Remove(store.RelayPIDPath()); err != nil {
		t.Fatal(err)
	}
	_, cmd = pressWatch(t, m, "s", "y")
	if cmd != nil {
		t.Fatal("busy viewer started a second stop")
	}
	m.busy = false
	_, cmd = pressWatch(t, m, "s", "y")
	if got := cmd().(watchActionDone).text; got != "Contributor changed; refresh before stopping" {
		t.Fatalf("stop of an exited contributor: %q", got)
	}
}

func TestWatchActionOutputScrollsAndDismisses(t *testing.T) {
	m := watchModel{source: &watchSource{}, width: 60, height: 14, busy: true}
	next, _ := m.Update(watchActionDone{text: "line0\nline1\nline2"})
	m = next.(watchModel)
	if m.busy || m.output == "" || !strings.HasPrefix(m.View(), "Action result") {
		t.Fatal("multi-line result not shown in the output view")
	}
	m, _ = pressWatch(t, m, "down", "down", "down", "down")
	if m.outputOffset != 2 {
		t.Fatalf("scroll past the end: offset %d", m.outputOffset)
	}
	if !strings.HasPrefix(strings.Split(m.View(), "\n")[1], "line2") {
		t.Fatal("scrolled view does not start at the offset")
	}
	m, _ = pressWatch(t, m, "up", "up", "up")
	if m.outputOffset != 0 {
		t.Fatalf("scroll before the start: offset %d", m.outputOffset)
	}
	if m, _ = pressWatch(t, m, "s"); m.confirmStop {
		t.Fatal("keys reached the main view while output was open")
	}
	if m, _ = pressWatch(t, m, "esc"); m.output != "" {
		t.Fatal("esc did not dismiss the output")
	}
	next, _ = m.Update(watchActionDone{text: "single line"})
	if got := next.(watchModel); got.output != "" || got.status != "single line" {
		t.Fatal("single-line result should go to the status line")
	}
}

func TestWatchViewReflectsModeAndSnapshot(t *testing.T) {
	source := &watchSource{}
	source.set(watchSnapshot{relay: &hivectl.ContributorRelay{TmuxSession: "hive-pi-9", Mode: "container", ContainerRuntime: "docker", ContainerName: "hive-contributor-9"}, session: "pane", log: "log", status: "poll error"})
	m := watchModel{source: source}
	next, cmd := m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	if cmd != nil {
		t.Fatal("resize scheduled work")
	}
	next, cmd = next.Update(watchTick{})
	m = next.(watchModel)
	if cmd == nil || m.snapshot.session != "pane" {
		t.Fatal("tick did not refresh the snapshot and reschedule")
	}
	view := m.View()
	for _, want := range []string{"Session: hive-pi-9 (container)", "pane", "log", "poll error", "a attach · c quota"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
	if got := len(strings.Split(view, "\n")); got > m.height {
		t.Fatalf("view is %d lines for a %d-line terminal", got, m.height)
	}
	for _, tc := range []struct {
		keys []string
		want string
	}{
		{[]string{"?"}, "Quitting this viewer leaves the contributor running."},
		{[]string{"c"}, "5 disable-session · 6 resume"},
		{[]string{"s"}, "Stop the running contributor? y confirms"},
		{[]string{"tab"}, "a attach · c quota"},
	} {
		got, _ := pressWatch(t, m, tc.keys...)
		if v := got.View(); !strings.Contains(v, tc.want) || len(strings.Split(v, "\n")) > m.height {
			t.Fatalf("%v: view missing %q or wrong height:\n%s", tc.keys, tc.want, v)
		}
	}
	m.status, m.busy = "older status", true
	if !strings.Contains(m.View(), "Action in progress…") {
		t.Fatal("busy state hidden")
	}
	m.width = 20
	if !strings.HasPrefix(m.View(), "Enlarge terminal") {
		t.Fatal("tiny terminal not handled")
	}
}

func TestContributorSessionCommandForKrun(t *testing.T) {
	r := hivectl.ContributorRelay{Mode: "krun", ContainerRuntime: "podman", ContainerName: "hive-contributor-krun", TmuxSession: "contributor"}
	if contributorSessionCommand(r, false) != nil {
		t.Fatal("krun refuses exec: the mirror must not build a capture command")
	}
	cmd := contributorSessionCommand(r, true)
	if cmd == nil || !reflect.DeepEqual(cmd.Args, []string{"podman", "attach", "hive-contributor-krun"}) {
		t.Fatalf("krun attach must be podman attach <name>, got %v", cmd)
	}
	// krun headless is a plain headless container: no session, no attach.
	r.Mode = "headless"
	if contributorSessionCommand(r, false) != nil || contributorSessionCommand(r, true) != nil {
		t.Fatal("krun headless created a session command")
	}
}

// A krun contributor must never be exec'd into and must not get a log
// follower; a headless krun contributor follows `podman logs -f` like any
// headless container.
func TestWatchSourceKrunStatesNeverExecOrFollowLogsInteractive(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("requires a POSIX shell")
	}
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	t.Setenv("WATCH_TEST_CALLS", calls)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	fakeWatchTool(t, bin, "podman", `printf '%s\n' "$1" >> "$WATCH_TEST_CALLS"
case "$1" in
inspect) echo true ;;
logs) printf 'headless krun log\n'; exec sleep 60 ;;
esac
`)
	store := hivectl.NewProfileStore(t.TempDir())
	record := hivectl.ContributorRelay{Mode: "krun", ContainerRuntime: "podman", ContainerName: "hive-contributor-krun", TmuxSession: "contributor"}
	writeWatchRecord(t, store, record)

	source := &watchSource{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); source.run(ctx, store) }()
	t.Cleanup(func() { cancel(); <-done })

	testutil.Eventually(t, 5*time.Second, func() bool {
		s := source.get()
		return s.relay != nil && s.session == watchKrunSession && s.log == watchKrunLog
	}, "krun interactive states not shown")

	// Headless krun replaces the record: the log follower starts now, and the
	// whole run must never have exec'd, attached or followed logs before.
	record.Mode = "headless"
	record.TmuxSession = ""
	writeWatchRecord(t, store, record)
	testutil.Eventually(t, 5*time.Second, func() bool {
		s := source.get()
		return s.session == "headless: no session" && s.log == "headless krun log\n"
	}, "krun headless did not follow podman logs")

	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	seenLogs := false
	for _, verb := range strings.Fields(string(data)) {
		switch verb {
		case "exec", "attach":
			t.Fatalf("viewer ran podman %s against a krun contributor", verb)
		case "logs":
			seenLogs = true
		}
	}
	if !seenLogs {
		t.Fatal("headless krun never followed logs")
	}
}

func TestWatchKrunAttachAndView(t *testing.T) {
	relay := &hivectl.ContributorRelay{Mode: "krun", ContainerRuntime: "podman", ContainerName: "hive-contributor-krun", TmuxSession: "contributor"}
	m := watchModel{source: &watchSource{}, width: 120, height: 30, snapshot: watchSnapshot{relay: relay, session: watchKrunSession, log: watchKrunLog}}
	got, cmd := pressWatch(t, m, "a")
	if cmd == nil || !got.busy {
		t.Fatal("krun attach did not suspend the viewer")
	}
	next, _ := got.Update(watchActionDone{text: "Detached from the VM console; the contributor keeps running"})
	if done := next.(watchModel); done.busy {
		t.Fatal("detach did not resume the viewer")
	}
	for _, want := range []string{watchKrunSession, watchKrunLog, "Ctrl-P Ctrl-Q", "Ctrl-C and Ctrl-D"} {
		if !strings.Contains(m.View(), want) {
			t.Fatalf("krun view missing %q:\n%s", want, m.View())
		}
	}
	help, _ := pressWatch(t, m, "?")
	if v := help.View(); !strings.Contains(v, "Ctrl-P Ctrl-Q") || strings.Contains(v, "Ctrl-B D") {
		t.Fatalf("krun help must name the console detach keys, not tmux's:\n%s", v)
	}
}

// `just contribute-stop` leaves a local relay alone, so a confirmed Stop of a
// local relay reports the recipe's result and never signals it (#13357).
func TestWatchStopMatchesContributeStopPerMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := hivectl.NewProfileStore(filepath.Join(home, ".config", "hive"))
	ctx, cancel := context.WithCancel(context.Background())
	actions := &watchActions{ctx: ctx}
	t.Cleanup(func() { cancel(); actions.wg.Wait() })
	for _, mode := range []string{"local", "headless"} {
		// The test process is a live PID; a stray SIGTERM would kill it.
		relay := hivectl.ContributorRelay{PID: os.Getpid(), Mode: mode}
		writeWatchRecord(t, store, relay)
		m := watchModel{source: &watchSource{}, actions: actions, snapshot: watchSnapshot{relay: &relay}}
		m, cmd := pressWatch(t, m, "s")
		if cmd != nil || !m.confirmStop {
			t.Fatalf("%s: stop needs confirmation", mode)
		}
		_, cmd = pressWatch(t, m, "y")
		if cmd == nil {
			t.Fatalf("%s: confirmed stop did not run", mode)
		}
		if got := cmd().(watchActionDone).text; !strings.HasPrefix(got, "Not running.") {
			t.Fatalf("%s: local stop must report the recipe result, got %q", mode, got)
		}
	}
	for _, mode := range []string{"container", "krun", "headless"} {
		m := watchModel{source: &watchSource{}, snapshot: watchSnapshot{relay: &hivectl.ContributorRelay{Mode: mode, ContainerRuntime: "podman", ContainerName: "hive-contributor-x"}}}
		if got, _ := pressWatch(t, m, "s"); !got.confirmStop {
			t.Fatalf("%s container: stop needs confirmation", mode)
		}
	}
}
