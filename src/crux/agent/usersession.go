package agent

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agent/resumehandle"
	"github.com/hivecommons/hive/crux/agent/tmux"
)

// UserSessionTmuxPrefix is the tmux session (and socket) name prefix of every
// user session; ttyd-tmux.sh finds the socket by this name.
const UserSessionTmuxPrefix = "hive-sess-"

const (
	// userSessionCmdTimeout bounds every tmux call so a wedged server cannot
	// hold an HTTP handler.
	userSessionCmdTimeout = 10 * time.Second
	// userSessionBufferName is the tmux paste buffer used for typed input.
	userSessionBufferName = "hive-sess-input"
	// userSessionMaxCaptureLines caps a scrollback read.
	userSessionMaxCaptureLines = fullLogCaptureLines
	// userSessionProbeFields is the field count of the probe format below.
	userSessionProbeFields = 3
	userSessionProbeFormat = "#{pane_dead} #{session_attached} #{session_activity}"
)

var (
	userSessionIDPattern = regexp.MustCompile(`^[a-f0-9]{8,32}$`)
	// userSessionResumeCmdPattern admits the resume commands the vendors
	// print: words, dots, dashes, slashes, colons and equals.
	userSessionResumeCmdPattern = regexp.MustCompile(`^[A-Za-z0-9._/:=@-]+( [A-Za-z0-9._/:=@-]+)*$`)
)

// userSessionVendor describes a CLI a session may run. Args are the vendor's
// plain interactive invocation: NO permission-bypass flag is ever added here,
// which is the point of this table existing separately from
// backends.conf's backend_perm_flag (that function emits the unattended
// bypass flags agents run with, and is deliberately not consulted).
type userSessionVendor struct {
	id     string
	label  string
	binary string
}

// userSessionVendors is the interactive-capable subset of KNOWN_BACKENDS
// (config/backends.conf; parity is asserted by a test). Headless runners
// (cline, muse, omnigent), the litellm gateway and any shell are excluded.
var userSessionVendors = []userSessionVendor{
	{"claude", "Claude Code", "claude"},
	{"copilot", "GitHub Copilot CLI", "copilot"},
	{"codex", "Codex", "codex"},
	{"goose", "Goose", "goose"},
	{"agy", "Antigravity", "agy"},
	{"bob", "Bob", "bob"},
	{"pi", "Pi", "pi"},
	{"aider", "Aider", "aider"},
	{"opencode", "OpenCode", "opencode"},
	{"kilo", "Kilo", "kilo"},
	{"omp", "OMP", "omp"},
	{"openhands", "OpenHands", "openhands"},
}

func findUserSessionVendor(id string) (userSessionVendor, bool) {
	for _, v := range userSessionVendors {
		if v.id == id {
			return v, true
		}
	}
	return userSessionVendor{}, false
}

// tmuxRunFunc runs one tmux invocation against a session socket. It is the
// test seam: tests substitute a recorder so no real tmux is ever touched.
type tmuxRunFunc func(ctx context.Context, env []string, stdin, socket string, args ...string) ([]byte, error)

// UserSessionRuntime starts, probes, types into and kills the tmux sessions
// behind the dashboard's Sessions feature. Each session gets its OWN tmux
// server (socket hive-sess-<id>), its own HOME and a credential-stripped
// environment. It holds no per-session state: the dashboard's store is the
// source of truth for names, groups and ownership.
type UserSessionRuntime struct {
	logger   *slog.Logger
	run      tmuxRunFunc
	lookPath func(string) (string, error)
	environ  func() []string
	now      func() time.Time
}

// NewUserSessionRuntime returns a runtime that drives the real tmux binary.
func NewUserSessionRuntime(logger *slog.Logger) *UserSessionRuntime {
	if logger == nil {
		logger = slog.Default()
	}
	return &UserSessionRuntime{
		logger:   logger,
		run:      execUserSessionTmux,
		lookPath: exec.LookPath,
		environ:  os.Environ,
		now:      time.Now,
	}
}

func execUserSessionTmux(ctx context.Context, env []string, stdin, socket string, args ...string) ([]byte, error) {
	argv := append([]string{"-L", socket}, args...)
	cmd := exec.CommandContext(ctx, tmux.Binary, argv...)
	cmd.Env = env
	cmd.WaitDelay = tmuxCmdWaitDelay
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, outputErr("tmux "+args[0], err, out)
	}
	return out, nil
}

// UserSessionTmuxName is the tmux session name for a session id.
func UserSessionTmuxName(id string) string { return UserSessionTmuxPrefix + id }

func (r *UserSessionRuntime) tmuxEnv() []string {
	return userSessionBaseEnv(r.environ(), os.Getenv(userSessionEnvPassthroughEnv))
}

func (r *UserSessionRuntime) tmux(id, stdin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), userSessionCmdTimeout)
	defer cancel()
	return r.run(ctx, r.tmuxEnv(), stdin, UserSessionTmuxName(id), args...)
}

func checkUserSessionID(id string) error {
	if !userSessionIDPattern.MatchString(id) {
		return fmt.Errorf("invalid session id")
	}
	return nil
}

// Vendors lists the vendors a session may run with binary presence.
func (r *UserSessionRuntime) Vendors() []crux.UserSessionVendor {
	out := make([]crux.UserSessionVendor, 0, len(userSessionVendors))
	for _, v := range userSessionVendors {
		_, err := r.lookPath(v.binary)
		out = append(out, crux.UserSessionVendor{ID: v.id, Label: v.label, Available: err == nil})
	}
	return out
}

// SupportsResume reports whether the vendor keeps a capturable resume handle.
func (r *UserSessionRuntime) SupportsResume(vendor string) bool {
	return BackendSupportsResumeHandle(vendor)
}

// WorkDir is the session's working directory; the dashboard stores uploads
// beneath it.
func (r *UserSessionRuntime) WorkDir(id string) string { return userSessionWorkDir(id) }

// Start creates the session's tmux server, strips and sets its environment and
// launches the vendor CLI as the pane's only process (there is no shell to
// fall back to when the CLI exits).
func (r *UserSessionRuntime) Start(spec crux.UserSessionSpec) error {
	if err := checkUserSessionID(spec.ID); err != nil {
		return err
	}
	argv, err := r.launchArgv(spec)
	if err != nil {
		return err
	}
	// An ended session's dead pane may still hold the tmux server open; a
	// resume replaces it.
	if p := r.Probe(spec.ID); p.Exists {
		if err := r.Kill(spec.ID); err != nil {
			return err
		}
	}
	if err := setupUserSessionHome(spec.ID); err != nil {
		return err
	}
	session := UserSessionTmuxName(spec.ID)
	create := newSessionCommands(session, userSessionWorkDir(spec.ID))
	// Keep the pane (and its scrollback) after the CLI exits so a late viewer
	// can still read what happened; Probe reports it as Dead.
	create = append(create, ";", "set-option", "-w", "-t", session, "remain-on-exit", "on")
	if out, err := r.tmux(spec.ID, "", create...); err != nil {
		return outputErr("creating user session tmux server", err, out)
	}
	if _, err := r.tmux(spec.ID, "", r.environmentCommands(spec.ID, session)...); err != nil {
		_ = r.Kill(spec.ID)
		return fmt.Errorf("setting session environment: %w", err)
	}
	// respawn-pane -k replaces the pre-env shell with the CLI itself, forked
	// after the environment was populated.
	respawn := append([]string{"respawn-pane", "-k", "-t", session, "-c", userSessionWorkDir(spec.ID), "--"}, argv...)
	if _, err := r.tmux(spec.ID, "", respawn...); err != nil {
		_ = r.Kill(spec.ID)
		return fmt.Errorf("launching %s: %w", spec.Vendor, err)
	}
	r.logger.Info("user session started", "id", spec.ID, "vendor", spec.Vendor)
	return nil
}

// environmentCommands removes every credential name from the session
// environment and then sets the per-session variables, as one tmux invocation.
func (r *UserSessionRuntime) environmentCommands(id, session string) []string {
	var args []string
	for _, k := range userSessionStripNames() {
		args = append(args, "set-environment", "-t", session, "-r", k, ";")
	}
	for _, kv := range userSessionEnvPairs(id, r.scrubScript()) {
		args = append(args, "set-environment", "-t", session, kv[0], kv[1], ";")
	}
	return args[:len(args)-1]
}

func (r *UserSessionRuntime) scrubScript() string {
	for _, p := range []string{"/usr/local/bin/agent-env-scrub.sh", filepath.Join(os.Getenv("HIVE_BIN_DIR"), "agent-env-scrub.sh")} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// launchArgv resolves the pane command: the vendor binary alone, or the
// validated resume command for that vendor. Never a shell.
func (r *UserSessionRuntime) launchArgv(spec crux.UserSessionSpec) ([]string, error) {
	v, ok := findUserSessionVendor(spec.Vendor)
	if !ok {
		return nil, fmt.Errorf("unsupported vendor %q", spec.Vendor)
	}
	bin, err := r.lookPath(v.binary)
	if err != nil {
		return nil, fmt.Errorf("%s is not installed on this spoke", v.binary)
	}
	if spec.ResumeCommand == "" {
		return []string{bin}, nil
	}
	if !userSessionResumeCmdPattern.MatchString(spec.ResumeCommand) {
		return nil, fmt.Errorf("resume command rejected")
	}
	fields := strings.Fields(spec.ResumeCommand)
	if fields[0] != v.binary {
		return nil, fmt.Errorf("resume command does not belong to %s", v.id)
	}
	return append([]string{bin}, fields[1:]...), nil
}

// Probe reads the session's live tmux state in one call.
func (r *UserSessionRuntime) Probe(id string) crux.UserSessionProbe {
	if checkUserSessionID(id) != nil {
		return crux.UserSessionProbe{}
	}
	out, err := r.tmux(id, "", "display-message", "-p", "-t", UserSessionTmuxName(id), userSessionProbeFormat)
	if err != nil {
		return crux.UserSessionProbe{}
	}
	fields := strings.Fields(string(out))
	if len(fields) != userSessionProbeFields {
		return crux.UserSessionProbe{Exists: true}
	}
	attached, _ := strconv.Atoi(fields[1])
	secs, _ := strconv.ParseInt(fields[2], 10, 64)
	return crux.UserSessionProbe{
		Exists:   true,
		Dead:     fields[0] == "1",
		Attached: attached,
		Activity: time.Unix(secs, 0),
	}
}

// Kill ends the session's tmux server. A missing session is not an error.
func (r *UserSessionRuntime) Kill(id string) error {
	if err := checkUserSessionID(id); err != nil {
		return err
	}
	if !r.Probe(id).Exists {
		return nil
	}
	if out, err := r.tmux(id, "", "kill-session", "-t", UserSessionTmuxName(id)); err != nil {
		return outputErr("killing user session", err, out)
	}
	return nil
}

// Capture returns up to lines lines of the session's scrollback as text.
func (r *UserSessionRuntime) Capture(id string, lines int) (string, error) {
	if err := checkUserSessionID(id); err != nil {
		return "", err
	}
	if lines <= 0 || lines > userSessionMaxCaptureLines {
		lines = userSessionMaxCaptureLines
	}
	out, err := r.tmux(id, "", "capture-pane", "-t", UserSessionTmuxName(id), "-p", "-J",
		"-S", "-"+strconv.Itoa(lines), "-E", "-")
	if err != nil {
		return "", fmt.Errorf("capturing session: %w", err)
	}
	return string(out), nil
}

// SendText types text into the session. It travels through a tmux paste
// buffer (bracketed paste when the CLI asked for it), so nothing is
// interpreted as a key name and a multi-line paste does not execute line by
// line. When submit is true a single Enter follows. The text is sanitised
// first: control and escape characters are refused, because an ESC could close
// the bracketed paste early and turn data into keystrokes.
func (r *UserSessionRuntime) SendText(id, text string, submit bool) error {
	if err := checkUserSessionID(id); err != nil {
		return err
	}
	clean, err := SanitizeSessionInput(text)
	if err != nil {
		return err
	}
	session := UserSessionTmuxName(id)
	if p := r.Probe(id); !p.Exists || p.Dead {
		return fmt.Errorf("session is not running")
	}
	if clean != "" {
		if out, err := r.tmux(id, clean, "load-buffer", "-b", userSessionBufferName, "-"); err != nil {
			return outputErr("loading input", err, out)
		}
		if out, err := r.tmux(id, "", "paste-buffer", "-p", "-r", "-d", "-b", userSessionBufferName, "-t", session); err != nil {
			return outputErr("pasting input", err, out)
		}
	}
	if submit {
		if out, err := r.tmux(id, "", "send-keys", "-t", session, "Enter"); err != nil {
			return outputErr("submitting input", err, out)
		}
	}
	return nil
}

// SanitizeSessionInput validates text for typing into a session: it must be
// valid UTF-8 and contain no control characters other than tab and newline
// (CRLF is folded to LF). It returns the text to paste.
func SanitizeSessionInput(text string) (string, error) {
	if !utf8.ValidString(text) {
		return "", fmt.Errorf("input is not valid UTF-8")
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var b bytes.Buffer
	for _, r := range text {
		if r == '\n' || r == '\t' {
			b.WriteRune(r)
			continue
		}
		if unicode.IsControl(r) {
			return "", fmt.Errorf("input contains a control character")
		}
		b.WriteRune(r)
	}
	return b.String(), nil
}

// ResumeInfo reads the newest vendor-native conversation from the session's
// own HOME.
func (r *UserSessionRuntime) ResumeInfo(id, vendor string) (crux.UserSessionResume, bool) {
	if checkUserSessionID(id) != nil || !r.SupportsResume(vendor) {
		return crux.UserSessionResume{}, false
	}
	h, ok := resumehandle.CaptureResumeHandleIn(userSessionHome(id), userSessionCodexHome(id), vendor, userSessionWorkDir(id), r.now())
	if !ok || h.SessionID == "" || h.Command == "" {
		return crux.UserSessionResume{}, false
	}
	return crux.UserSessionResume{SessionID: h.SessionID, Command: h.Command}, true
}
