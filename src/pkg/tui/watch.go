package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/hivecommons/hive/pkg/hivectl"
)

const watchBufferSize = 64 * 1024

// watchModeKrun is the PID-record mode of an interactive krun contributor.
const watchModeKrun = "krun"

const (
	watchKrunSession  = "console only — krun refuses exec; press a to attach to the VM console"
	watchKrunLog      = "No relay log in krun interactive mode today — the relay's output is on the VM console; press a to attach"
	watchKrunReminder = "Ctrl-P Ctrl-Q detaches; Ctrl-C and Ctrl-D go to the session"
)

// watchTail is bounded even when a container produces a single enormous line.
type watchTail struct {
	mu   sync.Mutex
	data []byte
}

func (b *watchTail) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if n >= watchBufferSize {
		b.data = append(b.data[:0], p[n-watchBufferSize:]...)
	} else {
		if len(b.data)+n > watchBufferSize {
			b.data = b.data[len(b.data)+n-watchBufferSize:]
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}
func (b *watchTail) text() string { b.mu.Lock(); defer b.mu.Unlock(); return string(b.data) }

// contributorSessionCommand never creates a tmux client unless interactive is
// explicitly requested. In particular the mirror cannot suppress relay retries.
func contributorSessionCommand(r hivectl.ContributorRelay, interactive bool) *exec.Cmd {
	if r.Mode == "headless" {
		return nil
	}
	if r.Mode == watchModeKrun {
		// The krun handler refuses exec: the only way in is the VM console,
		// and only interactively. Nothing may poll it.
		if !interactive {
			return nil
		}
		return exec.Command(r.ContainerRuntime, "attach", r.ContainerName)
	}
	if r.TmuxSession == "" {
		return nil
	}
	args := []string{"capture-pane", "-p", "-e", "-t", r.TmuxSession}
	if interactive {
		args = []string{"attach", "-t", r.TmuxSession}
	}
	if r.ContainerName == "" {
		return exec.Command("tmux", args...)
	}
	prefix := []string{"exec"}
	if interactive {
		prefix = append(prefix, "-it")
	}
	prefix = append(prefix, r.ContainerName, "tmux")
	return exec.Command(r.ContainerRuntime, append(prefix, args...)...)
}

type watchSnapshot struct {
	relay                *hivectl.ContributorRelay
	session, log, status string
}
type watchSource struct {
	mu       sync.Mutex
	snapshot watchSnapshot
}

func (s *watchSource) get() watchSnapshot  { s.mu.Lock(); defer s.mu.Unlock(); return s.snapshot }
func (s *watchSource) set(v watchSnapshot) { s.mu.Lock(); defer s.mu.Unlock(); s.snapshot = v }

func readWatchLog(file string) string {
	f, err := os.Open(file)
	if err != nil {
		return "Relay log unavailable: " + err.Error()
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "Relay log unavailable: " + err.Error()
	}
	if info.Size() > watchBufferSize {
		if _, err := f.Seek(-watchBufferSize, io.SeekEnd); err != nil {
			return err.Error()
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, watchBufferSize))
	if err != nil {
		return "Relay log unavailable: " + err.Error()
	}
	return string(data)
}

// followWatchLog owns only the logs client, never the container. Cancellation
// kills and reaps that client; it does not stop/remove the contributor.
func followWatchLog(ctx context.Context, r hivectl.ContributorRelay, tail *watchTail) func() {
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, r.ContainerRuntime, "logs", "-f", "--tail", "100", r.ContainerName)
	cmd.Stdout, cmd.Stderr = tail, tail
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := cmd.Run(); err != nil && ctx.Err() == nil {
			_, _ = fmt.Fprintf(tail, "\nRelay log follower: %v", err)
		}
	}()
	return func() { cancel(); <-done }
}

// run is a single polling owner, so record replacement/rotation cannot leave
// orphan log followers. The caller cancels and joins it when the viewer exits.
func (s *watchSource) run(ctx context.Context, store *hivectl.ProfileStore) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var previous hivectl.ContributorRelay
	var tail *watchTail
	var stopLogs func()
	defer func() {
		if stopLogs != nil {
			stopLogs()
		}
	}()
	for {
		pollCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		r, err := hivectl.RunningContributor(pollCtx, store)
		cancel()
		snap := watchSnapshot{relay: r}
		if err != nil {
			snap.status = err.Error()
		}
		if r == nil {
			if stopLogs != nil {
				stopLogs()
				stopLogs = nil
			}
			previous = hivectl.ContributorRelay{}
			snap.session = "No contributor running — start one with `just contribute-hive …`"
			snap.log = "No relay log — contributor is not running"
		} else {
			if *r != previous {
				if stopLogs != nil {
					stopLogs()
					stopLogs = nil
				}
				tail = &watchTail{}
				if r.ContainerName != "" && r.Mode != watchModeKrun {
					stopLogs = followWatchLog(ctx, *r, tail)
				}
				previous = *r
			}
			snap.session = "session unknown — attach manually (older relay PID record)"
			if r.Mode == "headless" {
				snap.session = "headless: no session"
			} else if r.Mode == watchModeKrun {
				snap.session = watchKrunSession
			} else if cmd := contributorSessionCommand(*r, false); cmd != nil {
				captureCtx, captureCancel := context.WithTimeout(ctx, 1500*time.Millisecond)
				out, captureErr := exec.CommandContext(captureCtx, cmd.Path, cmd.Args[1:]...).CombinedOutput()
				captureCancel()
				snap.session = string(out)
				if captureErr != nil {
					snap.session = fmt.Sprintf("Session unavailable: %v\n%s", captureErr, out)
				}
			}
			if r.Mode == watchModeKrun {
				snap.log = watchKrunLog
			} else if r.ContainerName != "" {
				snap.log = tail.text()
			} else if r.LogFile != "" {
				// Reopen each time: relay rotation replaces the inode.
				snap.log = readWatchLog(r.LogFile)
			} else {
				snap.log = "Relay log unavailable — restart with the updated contribute-hive recipe"
			}
		}
		s.set(snap)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type watchTick struct{}
type watchActionDone struct{ text string }

// Actions are owned by the viewer lifetime, including when q arrives before a
// slow quota/stop command returns. Cancel and reap them before leaving RunWatch.
type watchActions struct {
	ctx context.Context
	wg  sync.WaitGroup
}

func (a *watchActions) start(run func(context.Context) tea.Msg) tea.Cmd {
	result := make(chan tea.Msg, 1)
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		result <- run(a.ctx)
	}()
	return func() tea.Msg { return <-result }
}

type watchModel struct {
	source                   *watchSource
	actions                  *watchActions
	snapshot                 watchSnapshot
	width, height            int
	help, quota, confirmStop bool
	focus                    bool
	status                   string
	output                   string
	outputOffset             int
	busy                     bool
}

func watchNextTick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return watchTick{} })
}
func (m watchModel) Init() tea.Cmd { return watchNextTick() }

var watchQuotaActions = []string{"status", "continue-once", "continue-until-reset", "pause-until-reset", "disable-session", "resume"}

func watchQuotaCommand(parent context.Context, action string) tea.Msg {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	// Reuse the recipe, including its contributor.env sourcing and pool scope.
	out, err := exec.CommandContext(ctx, "just", "contribute-quota", action).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		text = fmt.Sprintf("Quota: %v — %s (run from the Hive checkout)", err, text)
	}
	return watchActionDone{text: text}
}
func (m watchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case watchTick:
		m.snapshot = m.source.get()
		return m, watchNextTick()
	case watchActionDone:
		m.status = msg.text
		m.busy = false
		if strings.Contains(msg.text, "\n") {
			m.output = msg.text
			m.outputOffset = 0
		}
	case tea.KeyMsg:
		key := msg.String()
		if key == "q" || key == "ctrl+c" {
			return m, tea.Quit
		}
		if key == "esc" {
			m.help, m.quota, m.confirmStop = false, false, false
			m.output = ""
			return m, nil
		}
		if m.output != "" {
			switch key {
			case "up", "k":
				m.outputOffset = max(0, m.outputOffset-1)
			case "down", "j":
				m.outputOffset = min(len(strings.Split(m.output, "\n"))-1, m.outputOffset+1)
			}
			return m, nil
		}
		if m.confirmStop {
			m.confirmStop = false
			if key == "y" && m.snapshot.relay != nil && !m.busy {
				// Revalidate the record so an old confirmation cannot stop a replacement.
				expected := *m.snapshot.relay
				m.busy = true
				return m, m.actions.start(func(parent context.Context) tea.Msg {
					ctx, cancel := context.WithTimeout(parent, 30*time.Second)
					defer cancel()
					store, err := hivectl.DefaultProfileStore()
					if err != nil {
						return watchActionDone{text: err.Error()}
					}
					current, err := hivectl.RunningContributor(ctx, store)
					if err != nil {
						return watchActionDone{text: err.Error()}
					}
					if current == nil || *current != expected {
						return watchActionDone{text: "Contributor changed; refresh before stopping"}
					}
					if err := hivectl.StopContributor(ctx, store, *current); err != nil {
						return watchActionDone{text: err.Error()}
					}
					return watchActionDone{text: "Contributor stopped"}
				})
			}
			return m, nil
		}
		if m.quota {
			if len(key) == 1 && key[0] >= '1' && key[0] <= '6' && !m.busy {
				m.quota = false
				m.busy = true
				return m, m.actions.start(func(parent context.Context) tea.Msg {
					return watchQuotaCommand(parent, watchQuotaActions[int(key[0]-'1')])
				})
			}
			return m, nil
		}
		switch key {
		case "?":
			m.help = !m.help
		case "tab", "f":
			m.focus = !m.focus
		case "s":
			if r := m.snapshot.relay; r != nil && !m.busy {
				// Confirm in every mode; for a local relay the confirmed Stop
				// only reports what `just contribute-stop` prints (no signal).
				m.confirmStop = true
			}
		case "c":
			if m.snapshot.relay != nil && !m.busy {
				m.quota = true
			}
		case "a":
			if m.snapshot.relay != nil && !m.busy {
				if cmd := contributorSessionCommand(*m.snapshot.relay, true); cmd != nil {
					krun := m.snapshot.relay.Mode == watchModeKrun
					m.busy = true
					return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
						text := "Detached; read-only mirror resumed"
						if krun {
							text = "Detached from the VM console; the contributor keeps running"
						}
						if err != nil {
							text = "Attach: " + err.Error()
						}
						return watchActionDone{text: text}
					})
				}
				m.status = "No attach target (headless or session unknown)"
			}
		}
	}
	return m, nil
}

// Strip all terminal controls from untrusted CLI/log output, then crop by cell
// width, not bytes. No agent OSC sequences are replayed into the viewer terminal.
func watchPane(text string, width, height int, tail bool) string {
	text = ansi.Strip(text)
	text = strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' || r == 127 || r >= 0x80 && r <= 0x9f {
			return -1
		}
		return r
	}, text)
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > height {
		if tail {
			lines = lines[len(lines)-height:]
		} else {
			lines = lines[:height]
		}
	}
	for i := range lines {
		lines[i] = ansi.Truncate(strings.ReplaceAll(lines[i], "\t", "    "), max(1, width), "")
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}
func (m watchModel) View() string {
	if m.width < 30 || m.height < 12 {
		return "Enlarge terminal to watch. q: quit (contributor keeps running)"
	}
	if m.output != "" {
		lines := strings.Split(m.output, "\n")
		return "Action result — ↑/↓ scroll · esc dismiss · q quit\n" + watchPane(strings.Join(lines[m.outputOffset:], "\n"), m.width, m.height-2, false)
	}
	title := "Session — read-only mirror"
	if r := m.snapshot.relay; r != nil {
		title = fmt.Sprintf("Session: %s (%s) — read-only mirror", r.TmuxSession, r.Mode)
		if r.Mode == watchModeKrun {
			title = fmt.Sprintf("Session: %s (krun) — VM console", r.ContainerName)
		}
	}
	footerHeight := 2
	if m.quota {
		footerHeight = 4
	}
	if m.help && !m.quota {
		footerHeight = 3
	}
	h := m.height - 5 - footerHeight
	sessionHeight := h / 2
	if m.focus {
		sessionHeight = max(1, h/3)
	}
	logHeight := h - sessionHeight
	footer := "a attach · c quota · s stop · tab focus · ? help · q quit"
	if r := m.snapshot.relay; r != nil && r.Mode == watchModeKrun {
		footer = "a attach console (" + watchKrunReminder + ") · c quota · s stop · ? help · q quit"
	}
	if m.help {
		footer = "Attach pauses automatic retries while a human client is present.\nDetach with Ctrl-B D to return to the read-only mirror.\nQuitting this viewer leaves the contributor running."
	}
	if m.help && m.snapshot.relay != nil && m.snapshot.relay.Mode == watchModeKrun {
		footer = "Attach opens the VM console (podman attach). " + watchKrunReminder + ".\nQuitting this viewer leaves the contributor running."
	}
	if m.quota {
		footer = "Quota: 1 status · 2 continue-once\n3 continue-until-reset · 4 pause-until-reset\n5 disable-session · 6 resume\nesc cancel"
	}
	if m.confirmStop {
		footer = "Stop the running contributor? y confirms; any other key cancels"
	}
	status := m.status
	if status == "" {
		status = m.snapshot.status
	}
	if m.busy {
		status = "Action in progress…"
	}
	return watchPane(title, m.width, 1, false) + "\n" +
		watchPane(m.snapshot.session, m.width, sessionHeight, true) + "\n" +
		watchPane("Relay log — follow", m.width, 1, false) + "\n" +
		watchPane(m.snapshot.log, m.width, logHeight, true) + "\n" +
		watchPane(status, m.width, 2, true) + "\n" + watchPane(footer, m.width, footerHeight, false)
}

// RunWatch is contributor-local: no API client, dashboard token, or preflight.
func RunWatch() error {
	store, err := hivectl.DefaultProfileStore()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	source := &watchSource{snapshot: watchSnapshot{session: "Looking for a running contributor…"}}
	done := make(chan struct{})
	go func() { defer close(done); source.run(ctx, store) }()
	actions := &watchActions{ctx: ctx}
	defer func() { cancel(); <-done; actions.wg.Wait() }()
	_, err = tea.NewProgram(watchModel{source: source, actions: actions, snapshot: source.get()}, tea.WithAltScreen()).Run()
	return err
}
