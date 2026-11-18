package spektacular

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Stream defaults.
const (
	// DefaultStreamBackoffMin is the first restart delay.
	DefaultStreamBackoffMin = time.Second
	// DefaultStreamBackoffMax caps the restart delay.
	DefaultStreamBackoffMax = 30 * time.Second
	// DefaultStreamMaxRestarts bounds restarts of one stream; a child that
	// keeps dying is given up on rather than respawned forever.
	DefaultStreamMaxRestarts = 5
	// DefaultStreamCloseGrace is how long Close waits for the child to exit
	// after its stdin is closed before killing it.
	DefaultStreamCloseGrace = 30 * time.Second
	// maxStreamLine bounds one stdout line.
	maxStreamLine = 4 << 20
	// streamLineBuffer is the stdout line channel depth.
	streamLineBuffer = 256
	// DefaultStreamProbeTimeout bounds the one-time capability probe.
	DefaultStreamProbeTimeout = 10 * time.Second
	// maxExitOutput bounds the stdout kept from a child to read its exit
	// reason (the CLI's JSON error envelope is a few hundred bytes).
	maxExitOutput = 4 << 10
	// unknownCommandMarker is how the CLI (cobra, wrapped in its JSON
	// error envelope) says the verb does not exist in this build, for
	// example `unknown command "meeting" for "spektacular"`.
	unknownCommandMarker = "unknown command"
)

var (
	// ErrStreamNotRunning is returned by Send when no child is running.
	ErrStreamNotRunning = errors.New("spektacular: stream child is not running")
	// ErrStreamUnsupported is Err() for a stream whose verb the installed
	// spektacular does not have (a release without `meeting`,
	// hivecommons/spektacular#126). Such a stream never starts or restarts
	// a child: Lines() is closed at once and Unsupported reports true.
	ErrStreamUnsupported = errors.New("spektacular: the installed spektacular has no such command")
)

// probeCache remembers, per binary file and probe, whether the verb exists,
// so the probe runs once per installed binary rather than once per stream.
// Keys are probeKey values; values are bool (true means supported).
var probeCache sync.Map

// unsupportedLogged remembers which binaries were already logged as lacking
// the verb, so the INFO line is written once per binary, not per meeting.
var unsupportedLogged sync.Map

// StreamOptions tunes a Stream. Zero values use the defaults.
type StreamOptions struct {
	Logger      *slog.Logger
	BackoffMin  time.Duration
	BackoffMax  time.Duration
	MaxRestarts int
	CloseGrace  time.Duration
	// OnRestart runs after a replacement child starts, so the caller can
	// re-send context the new child needs.
	OnRestart func()
	// Env is the child environment; nil inherits hive's.
	Env []string
	// Probe, when set, is a cheap command line (for example
	// `meeting --help`) Start runs once per installed binary before the
	// first child. When the CLI answers that the command is unknown, the
	// stream is unsupported: no child is started or restarted.
	Probe []string
	// ProbeTimeout bounds the probe.
	ProbeTimeout time.Duration
}

// Stream supervises one long-lived child. Lines() carries stdout lines
// across restarts and is closed when the stream is done.
type Stream struct {
	binary string
	args   []string
	opts   StreamOptions
	log    *slog.Logger
	lines  chan []byte
	done   chan struct{}

	mu       sync.Mutex
	stdin    io.WriteCloser
	cmd      *exec.Cmd
	closing  bool
	started  bool
	restarts int
	exitErr  error
	// lastWarn is when the restart WARN was last written; restarts inside
	// one BackoffMax window after it log at INFO.
	lastWarn time.Time
}

func newStream(binary string, args []string, opts StreamOptions) *Stream {
	if opts.BackoffMin <= 0 {
		opts.BackoffMin = DefaultStreamBackoffMin
	}
	if opts.BackoffMax <= 0 {
		opts.BackoffMax = DefaultStreamBackoffMax
	}
	if opts.MaxRestarts <= 0 {
		opts.MaxRestarts = DefaultStreamMaxRestarts
	}
	if opts.CloseGrace <= 0 {
		opts.CloseGrace = DefaultStreamCloseGrace
	}
	if opts.ProbeTimeout <= 0 {
		opts.ProbeTimeout = DefaultStreamProbeTimeout
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Stream{
		binary: binary, args: append([]string(nil), args...), opts: opts, log: log,
		lines: make(chan []byte, streamLineBuffer), done: make(chan struct{}),
	}
}

// Lines returns the stdout line channel.
func (s *Stream) Lines() <-chan []byte { return s.lines }

// Done is closed when the stream has stopped for good.
func (s *Stream) Done() <-chan struct{} { return s.done }

// Restarts reports how many times the child was restarted.
func (s *Stream) Restarts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restarts
}

// Err is the last child's exit error after Done (nil for a clean exit).
func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitErr
}

// Unsupported reports whether the installed spektacular lacks the stream's
// verb, so the stream ended without (re)starting a child.
func (s *Stream) Unsupported() bool { return errors.Is(s.Err(), ErrStreamUnsupported) }

// Start launches the first child and the supervision loop. It fails when
// the first child cannot be started at all. A stream whose verb the
// installed spektacular lacks (see StreamOptions.Probe) is not an error:
// Start returns nil with the stream already done and Unsupported true, so
// the caller carries on without it.
func (s *Stream) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("spektacular: stream already started")
	}
	s.started = true
	s.mu.Unlock()
	if !s.supported(ctx) {
		s.finishUnsupported()
		close(s.lines)
		close(s.done)
		return nil
	}
	cmd, out, err := s.spawn(ctx)
	if err != nil {
		close(s.lines)
		close(s.done)
		return err
	}
	go s.loop(ctx, cmd, out)
	return nil
}

// finishUnsupported ends a stream whose verb does not exist, before or
// instead of any (re)start.
func (s *Stream) finishUnsupported() {
	s.mu.Lock()
	s.exitErr = ErrStreamUnsupported
	s.mu.Unlock()
	if _, seen := unsupportedLogged.LoadOrStore(s.binary, true); !seen {
		s.log.Info("[spektacular] live mode unavailable: the installed spektacular has no such command; not starting it",
			"binary", s.binary, "args", strings.Join(s.args, " "))
	}
}

// probeKey identifies one installed binary file and probe command line, so
// a replaced binary is probed again.
func probeKey(path string, fi os.FileInfo, probe []string) string {
	return fmt.Sprintf("%s|%d|%d|%s", path, fi.Size(), fi.ModTime().UnixNano(), strings.Join(probe, "\x00"))
}

// supported runs the probe (once per binary) and reports whether the verb
// exists. Anything inconclusive (no probe, binary not found, probe timeout
// or a failure that is not "unknown command") counts as supported, so the
// normal start path and its errors apply.
func (s *Stream) supported(ctx context.Context) bool {
	if len(s.opts.Probe) == 0 {
		return true
	}
	path, err := exec.LookPath(s.binary)
	if err != nil {
		return true
	}
	fi, err := os.Stat(path)
	if err != nil {
		return true
	}
	key := probeKey(path, fi, s.opts.Probe)
	if v, cached := probeCache.Load(key); cached {
		known, _ := v.(bool)
		return known
	}
	pctx, cancel := context.WithTimeout(ctx, s.opts.ProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(pctx, path, s.opts.Probe...)
	cmd.Env = s.opts.Env
	out, runErr := cmd.CombinedOutput()
	if pctx.Err() != nil {
		return true
	}
	ok := runErr == nil || !isUnknownCommand(out)
	probeCache.Store(key, ok)
	return ok
}

// isUnknownCommand reports whether CLI output says the verb does not exist.
func isUnknownCommand(out []byte) bool {
	return bytes.Contains(bytes.ToLower(out), []byte(unknownCommandMarker))
}

func (s *Stream) spawn(ctx context.Context) (*exec.Cmd, io.ReadCloser, error) {
	cmd := exec.Command(s.binary, s.args...)
	cmd.Env = s.opts.Env
	cmd.Stderr = &streamLogWriter{log: s.log}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("spektacular: starting %s: %w", s.binary, err)
	}
	s.mu.Lock()
	s.stdin, s.cmd = stdin, cmd
	s.mu.Unlock()
	return cmd, stdout, nil
}

func (s *Stream) loop(ctx context.Context, cmd *exec.Cmd, out io.ReadCloser) {
	defer close(s.done)
	defer close(s.lines)
	backoff := s.opts.BackoffMin
	for {
		head := &headBuffer{max: maxExitOutput}
		err := s.pump(ctx, cmd, out, head)
		s.mu.Lock()
		s.stdin, s.cmd, s.exitErr = nil, nil, err
		closing := s.closing
		restarts := s.restarts
		s.mu.Unlock()
		if closing || ctx.Err() != nil || err == nil {
			return
		}
		// The verb does not exist in this build (no probe was configured,
		// or the binary changed under it): restarting cannot help.
		if isUnknownCommand(head.b) {
			s.finishUnsupported()
			return
		}
		if restarts >= s.opts.MaxRestarts {
			s.log.Error("[spektacular] stream child keeps exiting; giving up", "binary", s.binary, "restarts", restarts, "error", err)
			return
		}
		s.logRestart(err, backoff, restarts)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, s.opts.BackoffMax)
		s.mu.Lock()
		if s.closing {
			s.mu.Unlock()
			return
		}
		s.restarts++
		s.mu.Unlock()
		cmd, out, err = s.spawn(ctx)
		if err != nil {
			s.mu.Lock()
			s.exitErr = err
			s.mu.Unlock()
			s.log.Error("[spektacular] stream child did not restart", "error", err)
			return
		}
		if s.opts.OnRestart != nil {
			s.opts.OnRestart()
		}
	}
}

// logRestart writes the restart line at WARN at most once per BackoffMax
// window and at INFO otherwise, so one child's backoff series is a single
// warning rather than one per attempt (hivecommons/hive#18630).
func (s *Stream) logRestart(err error, backoff time.Duration, restarts int) {
	now := time.Now()
	s.mu.Lock()
	warn := s.lastWarn.IsZero() || now.Sub(s.lastWarn) >= s.opts.BackoffMax
	if warn {
		s.lastWarn = now
	}
	s.mu.Unlock()
	level := slog.LevelInfo
	if warn {
		level = slog.LevelWarn
	}
	s.log.Log(context.Background(), level, "[spektacular] stream child exited; restarting",
		"error", err, "backoff", backoff.String(), "restarts", restarts)
}

// headBuffer keeps the first max bytes of a child's stdout.
type headBuffer struct {
	max int
	b   []byte
}

func (h *headBuffer) add(line []byte) {
	if room := h.max - len(h.b); room > 0 {
		h.b = append(h.b, line[:min(len(line), room)]...)
		h.b = append(h.b, '\n')
	}
}

// pump copies stdout lines until the child exits, killing it when ctx
// ends. It returns the child's exit error. head, when set, keeps the start
// of stdout so the caller can read the exit reason.
func (s *Stream) pump(ctx context.Context, cmd *exec.Cmd, out io.Reader, head *headBuffer) error {
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 0, 64<<10), maxStreamLine)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			if head != nil {
				head.add(line)
			}
			s.lines <- append([]byte(nil), line...)
		}
	}()
	waited := make(chan error, 1)
	go func() {
		<-readDone
		waited <- cmd.Wait()
	}()
	select {
	case err := <-waited:
		return err
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return <-waited
	}
}

// Send writes v as one JSON line to the running child's stdin.
func (s *Stream) Send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.SendLine(data)
}

// SendLine writes one raw JSON line to the running child's stdin.
func (s *Stream) SendLine(line []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stdin == nil || s.closing {
		return ErrStreamNotRunning
	}
	_, err := s.stdin.Write(append(append([]byte(nil), line...), '\n'))
	return err
}

// Close ends the stream: it optionally writes last (for example
// {"type":"end"}), closes stdin, waits up to CloseGrace for the child to
// exit and flush, then kills it. It blocks until the stream is done.
func (s *Stream) Close(last any) {
	if last != nil {
		_ = s.Send(last)
	}
	s.mu.Lock()
	s.closing = true
	stdin, cmd, started := s.stdin, s.cmd, s.started
	s.mu.Unlock()
	if !started {
		return
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	select {
	case <-s.done:
	case <-time.After(s.opts.CloseGrace):
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-s.done
	}
}

// streamLogWriter forwards child stderr to the logger line by line.
type streamLogWriter struct {
	log *slog.Logger
	mu  sync.Mutex
	buf []byte
}

func (w *streamLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			if len(w.buf) > maxStreamLine {
				w.buf = w.buf[:0]
			}
			return len(p), nil
		}
		if line := bytes.TrimSpace(w.buf[:i]); len(line) > 0 {
			w.log.Info("[spektacular] stderr", "line", string(line))
		}
		w.buf = w.buf[i+1:]
	}
}
