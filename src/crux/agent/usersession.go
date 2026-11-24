package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agent/resumehandle"
	"github.com/hivecommons/hive/crux/agent/tmux"
	"github.com/hivecommons/hive/crux/pushbroker"
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
	// userSessionUploadsDirName is the work-dir child uploads are stored in.
	userSessionUploadsDirName = "uploads"
	// userSessionUploadNameMax bounds an upload file name.
	userSessionUploadNameMax = 150
	// userSessionUploadTimeout bounds one upload write.
	userSessionUploadTimeout = 5 * time.Minute
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
	// userSessionResumeIDPattern is a resume id: it never starts with '-', so
	// it cannot be read as a flag.
	userSessionResumeIDPattern   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{7,127}$`)
	userSessionUploadNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// userSessionResumeWords are the only dash-prefixed or subcommand words a
// resume command may carry besides the vendor binary and the id (the shapes
// resumehandle emits). Anything else starting with '-' is refused, so a
// planted transcript name cannot smuggle a flag such as
// --dangerously-skip-permissions into the launch.
var userSessionResumeWords = map[string]bool{
	"--resume": true, "--session": true, "--session-id": true,
	"resume": true, "session": true,
}

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

// tmuxRunFunc runs one tmux invocation against a session socket as the given
// su-exec identity (the session's own uid). It is the test seam: tests
// substitute a recorder so no real tmux is ever touched.
type tmuxRunFunc func(ctx context.Context, user string, env []string, stdin, socket string, args ...string) ([]byte, error)

// UserSessionRuntime starts, probes, types into and kills the tmux sessions
// behind the dashboard's Sessions feature. Each session gets its OWN tmux
// server (socket hive-sess-<id>), its own HOME and a credential-stripped
// environment. It holds no per-session state: the dashboard's store is the
// source of truth for names, groups and ownership.
type UserSessionRuntime struct {
	logger   *slog.Logger
	run      tmuxRunFunc
	priv     privRunFunc
	statUID  func(path string) (int, error)
	lookPath func(string) (string, error)
	environ  func() []string
	now      func() time.Time
	// git runs the hive-side git for the shared workspace clone
	// (usersession_workspace.go); cloneMinter mints its scoped token.
	git         gitRunFunc
	cloneMinter pushbroker.TokenMinter
	// forgeRemotes names the forge and minter of a repo that is not on
	// github.com (SetForgeRemotes); nil keeps every repo on github.com.
	forgeRemotes pushbroker.ForgeRemoteResolver
	// orphaned judges whether an attached tmux client has lost its terminal
	// attacher (usersession_clients.go); nil disables the reaper.
	orphaned clientOrphanFunc
	// workspaceMu serialises workspace clone, the lock of its git config,
	// worktree add/remove and spek init across sessions.
	workspaceMu sync.Mutex
	// securedClones records the shared clones whose git config this process
	// has locked (userSessionWorkspaceSecureScript); guarded by workspaceMu.
	securedClones map[string]bool
	// bufSeq makes every paste buffer name unique, so concurrent inputs to one
	// session cannot paste each other's text.
	bufSeq atomic.Uint64
	// auditSink records a login adopted from a session HOME into the shared
	// login (usersession_login.go); guarded by auditMu. Nil is allowed.
	auditMu   sync.Mutex
	auditSink crux.AuditSink
}

// NewUserSessionRuntime returns a runtime that drives the real tmux binary.
func NewUserSessionRuntime(logger *slog.Logger) *UserSessionRuntime {
	if logger == nil {
		logger = slog.Default()
	}
	return &UserSessionRuntime{
		logger:   logger,
		run:      execUserSessionTmux,
		priv:     execUserSessionPriv,
		statUID:  statPathUID,
		lookPath: exec.LookPath,
		environ:  os.Environ,
		now:      time.Now,
		git:      execUserSessionGit,
		orphaned: procClientOrphaned,
	}
}

func execUserSessionTmux(ctx context.Context, user string, env []string, stdin, socket string, args ...string) ([]byte, error) {
	if user == "" || user == userSessionRootSpec {
		return nil, errors.New("refusing to run a session tmux without an unprivileged session identity")
	}
	argv := append([]string{user, tmux.Binary, "-L", socket}, args...)
	cmd := exec.CommandContext(ctx, userSuExecBinary, argv...)
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

// tmuxAs runs one tmux call as the session's uid.
func (r *UserSessionRuntime) tmuxAs(uid int, id, stdin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), userSessionCmdTimeout)
	defer cancel()
	return r.run(ctx, userSessionSpec(uid), r.tmuxEnv(), stdin, UserSessionTmuxName(id), args...)
}

// tmux runs one tmux call as the uid that owns the session's sandbox.
func (r *UserSessionRuntime) tmux(id, stdin string, args ...string) ([]byte, error) {
	uid, err := r.sessionUID(id)
	if err != nil {
		return nil, err
	}
	return r.tmuxAs(uid, id, stdin, args...)
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
	// The uid comes from the allocated slot, never from the id, and is
	// validated against the agent uid range before anything is created.
	uid, err := userSessionUID(spec.UIDSlot)
	if err != nil {
		return err
	}
	argv, err := r.launchArgv(spec)
	if err != nil {
		return err
	}
	// An ended session's dead pane may still hold the tmux server open; a
	// resume replaces it.
	if p := r.probeAs(uid, spec.ID); p.Exists {
		if err := r.killAs(uid, spec.ID); err != nil {
			return err
		}
	}
	if err := r.prepareSandbox(spec.ID, uid); err != nil {
		return err
	}
	// The pane opens in the session's own worktree of the hive's shared
	// workspace clone when the spec names one, otherwise in the session's own
	// empty work dir.
	cwd, workspace := userSessionWorkDir(spec.ID), ""
	if spec.WorkspaceRepo != "" {
		ws, err := r.ensureWorkspace(spec, uid)
		if err != nil {
			return err
		}
		cwd, workspace = ws, ws
	}
	// First line against first-run screens: a seeded HOME never shows them
	// (usersession_startup.go).
	r.seedOnboarding(spec, uid, cwd)
	session := UserSessionTmuxName(spec.ID)
	create := newSessionCommands(session, cwd)
	// Keep the pane (and its scrollback) after the CLI exits so a late viewer
	// can still read what happened; Probe reports it as Dead. The clipboard
	// escape is refused server-wide: a session has no business writing to the
	// clipboard of whoever attaches (ttyd-tmux.sh does not re-enable it).
	create = append(create, ";", "set-option", "-w", "-t", session, "remain-on-exit", "on",
		";", "set-option", "-s", "set-clipboard", "off")
	if out, err := r.tmuxAs(uid, spec.ID, "", create...); err != nil {
		return outputErr("creating user session tmux server", err, out)
	}
	if err := r.openSocketDirToAttacher(uid); err != nil {
		_ = r.killAs(uid, spec.ID)
		return err
	}
	if _, err := r.tmuxAs(uid, spec.ID, "", r.environmentCommands(spec.ID, session, workspace)...); err != nil {
		_ = r.killAs(uid, spec.ID)
		return fmt.Errorf("setting session environment: %w", err)
	}
	// respawn-pane -k replaces the pre-env shell with the CLI itself, forked
	// after the environment was populated.
	respawn := append([]string{"respawn-pane", "-k", "-t", session, "-c", cwd, "--"}, argv...)
	if _, err := r.tmuxAs(uid, spec.ID, "", respawn...); err != nil {
		_ = r.killAs(uid, spec.ID)
		return fmt.Errorf("launching %s: %w", spec.Vendor, err)
	}
	r.logger.Info("user session started", "id", spec.ID, "vendor", spec.Vendor, "uid", uid, "cwd", cwd)
	return nil
}

// environmentCommands removes every credential name from the session
// environment and then sets the per-session variables, as one tmux invocation.
// workspace, when non-empty, adds the workspace variables.
func (r *UserSessionRuntime) environmentCommands(id, session, workspace string) []string {
	var args []string
	for _, k := range userSessionStripNames() {
		args = append(args, "set-environment", "-t", session, "-r", k, ";")
	}
	pairs := append(userSessionEnvPairs(id, r.scrubScript()), userSessionWorkspaceEnvPairs(workspace)...)
	for _, kv := range pairs {
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
	// Every argument is either a known resume word or an id that cannot be
	// read as a flag; nothing else (in particular nothing starting with '-')
	// reaches the vendor CLI.
	for _, f := range fields[1:] {
		if !userSessionResumeWords[f] && !userSessionResumeIDPattern.MatchString(f) {
			return nil, fmt.Errorf("resume command rejected")
		}
	}
	return append([]string{bin}, fields[1:]...), nil
}

// Probe reads the session's live tmux state in one call.
func (r *UserSessionRuntime) Probe(id string) crux.UserSessionProbe {
	if checkUserSessionID(id) != nil {
		return crux.UserSessionProbe{}
	}
	uid, err := r.sessionUID(id)
	if err != nil {
		return crux.UserSessionProbe{}
	}
	return r.probeAs(uid, id)
}

func (r *UserSessionRuntime) probeAs(uid int, id string) crux.UserSessionProbe {
	out, err := r.tmuxAs(uid, id, "", "display-message", "-p", "-t", UserSessionTmuxName(id), userSessionProbeFormat)
	if err != nil {
		return crux.UserSessionProbe{}
	}
	fields := strings.Fields(string(out))
	if len(fields) != userSessionProbeFields {
		return crux.UserSessionProbe{Exists: true}
	}
	attached, _ := strconv.Atoi(fields[1])
	secs, _ := strconv.ParseInt(fields[2], 10, 64)
	p := crux.UserSessionProbe{
		Exists:   true,
		Dead:     fields[0] == "1",
		Attached: attached,
		Activity: time.Unix(secs, 0),
	}
	// Attached is the LIVE count: clients whose attacher is gone are detached
	// here and not reported (usersession_clients.go).
	if !p.Dead && p.Attached > 0 {
		if live, ok := r.liveClients(uid, id); ok {
			p.Attached = live
		}
	}
	return p
}

// Kill ends the session's tmux server. A missing session is not an error.
func (r *UserSessionRuntime) Kill(id string) error {
	if err := checkUserSessionID(id); err != nil {
		return err
	}
	uid, err := r.sessionUID(id)
	if err != nil {
		// No sandbox owned by a session uid means there is no server to kill.
		return nil
	}
	return r.killAs(uid, id)
}

func (r *UserSessionRuntime) killAs(uid int, id string) error {
	if !r.probeAs(uid, id).Exists {
		return nil
	}
	if out, err := r.tmuxAs(uid, id, "", "kill-session", "-t", UserSessionTmuxName(id)); err != nil {
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
		buf := fmt.Sprintf("%s-%d", userSessionBufferName, r.bufSeq.Add(1))
		if out, err := r.tmux(id, clean, "load-buffer", "-b", buf, "-"); err != nil {
			return outputErr("loading input", err, out)
		}
		if out, err := r.tmux(id, "", "paste-buffer", "-p", "-r", "-d", "-b", buf, "-t", session); err != nil {
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

// userSessionWritableClientFormat lists each attached client as "<name> <readonly>".
const userSessionWritableClientFormat = "#{client_name} #{client_readonly}"

// DetachWritableClients detaches every attached tmux client that is not
// read-only. The dashboard calls it when the keyboard lease changes hands: the
// previous holder's ttyd client is the only writable one by construction
// (every other viewer attached with `attach-session -r`), so detaching the
// writable set is detaching the old holder. A missing session is not an error.
func (r *UserSessionRuntime) DetachWritableClients(id string) error {
	if err := checkUserSessionID(id); err != nil {
		return err
	}
	session := UserSessionTmuxName(id)
	if p := r.Probe(id); !p.Exists {
		return nil
	}
	out, err := r.tmux(id, "", "list-clients", "-t", session, "-F", userSessionWritableClientFormat)
	if err != nil {
		return outputErr("listing session clients", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != "0" {
			continue
		}
		if out, err := r.tmux(id, "", "detach-client", "-t", fields[0]); err != nil {
			return outputErr("detaching session client", err, out)
		}
	}
	return nil
}

// SanitizeSessionInput validates text for typing into a session: it must be
// valid UTF-8 and contain no control characters other than tab and newline
// (CRLF is folded to LF). It returns the text to paste.
func SanitizeSessionInput(text string) (string, error) {
	return crux.SanitizeUserSessionInput(text)
}

// ResumeInfo reads the newest vendor-native conversation from the session's
// own HOME. workDir is the directory the CLI ran in (vendors key transcripts
// by project directory); empty means the session's own work dir.
func (r *UserSessionRuntime) ResumeInfo(id, vendor, workDir string) (crux.UserSessionResume, bool) {
	if checkUserSessionID(id) != nil || !r.SupportsResume(vendor) {
		return crux.UserSessionResume{}, false
	}
	if workDir == "" {
		workDir = userSessionWorkDir(id)
	}
	r.makeHomeReadable(id)
	h, ok := resumehandle.CaptureResumeHandleIn(userSessionHome(id), userSessionCodexHome(id), vendor, workDir, r.now())
	if !ok || h.SessionID == "" || h.Command == "" {
		return crux.UserSessionResume{}, false
	}
	return crux.UserSessionResume{SessionID: h.SessionID, Command: h.Command}, true
}

// userSessionReadableScript makes the session's tree group-readable (the group
// is the hive's reader group via the setgid home) and then re-asserts the
// read-only mode on the login copies, which must stay owner-only.
const userSessionReadableScript = `h=$1; shift; chmod -R g+rX -- "$h" 2>/dev/null; for f in "$@"; do if [ -f "$f" ] && [ ! -L "$f" ]; then chmod ` + userSessionLoginFileMode + ` -- "$f"; fi; done; exit 0`

// makeHomeReadable lets the hive read transcripts the CLI wrote under the
// session uid with a private mode. Best effort: on failure ResumeInfo just
// finds nothing.
func (r *UserSessionRuntime) makeHomeReadable(id string) {
	uid, err := r.sessionUID(id)
	if err != nil {
		return
	}
	home := userSessionHome(id)
	args := []string{"sh", "-c", userSessionReadableScript, "sh", home}
	for _, rel := range userSessionLoginFiles {
		args = append(args, filepath.Join(home, rel))
	}
	_, _ = r.privRun(userSessionSpec(uid), nil, args...)
}

// countingReader counts bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// StoreUpload writes src as <work dir>/uploads/<fileName>, AS THE SESSION UID,
// so the file is session-owned (0600) from birth and no hive-side path is ever
// opened inside a directory the session controls (a planted symlink only
// reaches things the session can already reach). fileName must be a plain
// name. At most maxFiles files are kept and at most maxBytes are accepted;
// the typed errors crux.ErrUserSessionUploadLimit / ...TooLarge report which.
func (r *UserSessionRuntime) StoreUpload(id, fileName string, src io.Reader, maxBytes int64, maxFiles int) (string, int64, error) {
	if err := checkUserSessionID(id); err != nil {
		return "", 0, err
	}
	if len(fileName) > userSessionUploadNameMax || !userSessionUploadNamePattern.MatchString(fileName) {
		return "", 0, fmt.Errorf("invalid upload name")
	}
	uid, err := r.sessionUID(id)
	if err != nil {
		return "", 0, err
	}
	dir := filepath.Join(userSessionWorkDir(id), userSessionUploadsDirName)
	dest := filepath.Join(dir, fileName)
	cr := &countingReader{r: io.LimitReader(src, maxBytes+1)}
	// The upload may take longer than a tmux call; it is bounded by the
	// request, not by userSessionCmdTimeout.
	ctx, cancel := context.WithTimeout(context.Background(), userSessionUploadTimeout)
	defer cancel()
	out, err := r.priv(ctx, userSessionSpec(uid), cr, "sh", "-c", userSessionUploadScript, "sh", dir, strconv.Itoa(maxFiles), fileName)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == userSessionUploadFullExit {
			return "", 0, crux.ErrUserSessionUploadLimit
		}
		return "", 0, outputErr("storing upload", err, out)
	}
	if cr.n > maxBytes {
		_, _ = r.privRun(userSessionSpec(uid), nil, "rm", "-f", "--", dest)
		return "", cr.n, crux.ErrUserSessionUploadTooLarge
	}
	return dest, cr.n, nil
}

// userSessionUploadScript counts the files already stored ($1 dir, $2 max),
// refuses when full and otherwise creates $3 exclusively (noclobber) under
// umask 077.
const userSessionUploadScript = `d=$1; max=$2; name=$3
umask 077
mkdir -p -- "$d"
n=$(ls -1A -- "$d" | wc -l)
if [ "$n" -ge "$max" ]; then exit ` + userSessionUploadFullExitText + `; fi
set -C
cat > "$d/$name"`
