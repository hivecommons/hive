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
	"os/exec"
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
)

// ErrStreamNotRunning is returned by Send when no child is running.
var ErrStreamNotRunning = errors.New("spektacular: stream child is not running")

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

// Start launches the first child and the supervision loop. It fails when
// the first child cannot be started at all.
func (s *Stream) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("spektacular: stream already started")
	}
	s.started = true
	s.mu.Unlock()
	cmd, out, err := s.spawn(ctx)
	if err != nil {
		close(s.lines)
		close(s.done)
		return err
	}
	go s.loop(ctx, cmd, out)
	return nil
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
		err := s.pump(ctx, cmd, out)
		s.mu.Lock()
		s.stdin, s.cmd, s.exitErr = nil, nil, err
		closing := s.closing
		restarts := s.restarts
		s.mu.Unlock()
		if closing || ctx.Err() != nil || err == nil {
			return
		}
		if restarts >= s.opts.MaxRestarts {
			s.log.Error("[spektacular] stream child keeps exiting; giving up", "binary", s.binary, "restarts", restarts, "error", err)
			return
		}
		s.log.Warn("[spektacular] stream child exited; restarting", "error", err, "backoff", backoff.String())
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

// pump copies stdout lines until the child exits, killing it when ctx
// ends. It returns the child's exit error.
func (s *Stream) pump(ctx context.Context, cmd *exec.Cmd, out io.Reader) error {
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
