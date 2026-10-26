package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/config"
)

// Supervisor keeps a sidecar's `crux serve` process alive
// (hivecommons/hive#14138 stage 4, config.SuperviseChild): it spawns the
// binary, waits for the health route, and when the process exits restarts
// it after a backoff that doubles up to MaxBackoff and resets once a child
// has stayed healthy for ResetAfter. The host's client.Runtime reconnects
// on its own; the supervisor only guarantees there is something to connect
// to. Under config.SuperviseExternal nothing here runs.
type Supervisor struct {
	// Config is the validated sidecar config: Server says what to listen
	// on, Binary what to run, Token what to require.
	Config config.RuntimeConfig
	// Args replaces the default argument list (`serve` plus the listen
	// flags derived from Config.Server) when non-nil.
	Args []string
	// Env is appended to the child's environment (the parent's is
	// inherited; CRUX_TOKEN is added from Config.Token when set).
	Env []string
	// Stdout and Stderr receive the child's output; nil means the
	// parent's.
	Stdout, Stderr io.Writer
	// Logger receives lifecycle events; nil means slog.Default().
	Logger *slog.Logger
	// MinBackoff, MaxBackoff and ResetAfter tune the restart loop; zero
	// means the defaults.
	MinBackoff, MaxBackoff, ResetAfter time.Duration
	// ReadyTimeout bounds the wait for a freshly started child's health
	// route; zero means DefaultReadyTimeout. A child that never answers
	// is killed and counted as a failed start.
	ReadyTimeout time.Duration

	// lookPath and start are the exec seams tests replace.
	lookPath func(string) (string, error)
	start    func(*exec.Cmd) error

	mu    sync.Mutex
	state SupervisorState
}

// Supervisor defaults.
const (
	DefaultMinBackoff   = time.Second
	DefaultMaxBackoff   = time.Minute
	DefaultResetAfter   = 5 * time.Minute
	DefaultReadyTimeout = 30 * time.Second
	readyPoll           = 100 * time.Millisecond
)

// SupervisorState is what the supervisor knows about its child.
type SupervisorState struct {
	// Running reports whether a child process exists right now.
	Running bool
	// PID is the current child's pid (0 when none).
	PID int
	// Ready reports whether the current child has answered its health
	// route.
	Ready bool
	// Starts counts child processes started so far; Restarts is Starts-1
	// floored at zero.
	Starts int
	// LastExit is the most recent exit error ("" when none or clean).
	LastExit string
	// LastExitAt is when the last child exited.
	LastExitAt time.Time
	// NextStart is when the next start is due during a backoff.
	NextStart time.Time
	// Binary is the resolved executable path.
	Binary string
}

// ErrBinaryNotFound is returned by Run when the `crux` executable cannot
// be found; the host then falls back to reconnect-only and says so.
var ErrBinaryNotFound = errors.New("crux binary not found")

// State is the current SupervisorState.
func (s *Supervisor) State() SupervisorState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *Supervisor) logger() *slog.Logger {
	if s.Logger == nil {
		return slog.Default()
	}
	return s.Logger
}

func (s *Supervisor) defaults() {
	if s.MinBackoff <= 0 {
		s.MinBackoff = DefaultMinBackoff
	}
	if s.MaxBackoff < s.MinBackoff {
		s.MaxBackoff = DefaultMaxBackoff
		if s.MaxBackoff < s.MinBackoff {
			s.MaxBackoff = s.MinBackoff
		}
	}
	if s.ResetAfter <= 0 {
		s.ResetAfter = DefaultResetAfter
	}
	if s.ReadyTimeout <= 0 {
		s.ReadyTimeout = DefaultReadyTimeout
	}
	if s.lookPath == nil {
		s.lookPath = exec.LookPath
	}
	if s.start == nil {
		s.start = (*exec.Cmd).Start
	}
}

// ServeArgs derives the `crux serve` argument list for a server address:
// a unix address becomes --socket, a TCP one --listen.
func ServeArgs(server string) ([]string, error) {
	addr, err := api.ParseAddress(server)
	if err != nil {
		return nil, err
	}
	if addr.Network == "unix" {
		return []string{"serve", "--socket", addr.Path}, nil
	}
	return []string{"serve", "--listen", addr.Host}, nil
}

// Run supervises until ctx ends. It returns ErrBinaryNotFound (wrapped)
// at once when the binary is missing, otherwise nil when ctx ends; the
// child is signalled to stop with the context.
func (s *Supervisor) Run(ctx context.Context) error {
	s.defaults()
	bin, err := s.lookPath(s.Config.Binary)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrBinaryNotFound, s.Config.Binary, err)
	}
	args := s.Args
	if args == nil {
		if args, err = ServeArgs(s.Config.Server); err != nil {
			return err
		}
	}
	probe, err := api.NewClient(s.Config.Server, s.Config.Token)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.state.Binary = bin
	s.mu.Unlock()
	backoff := s.MinBackoff
	for {
		startedAt := time.Now()
		exitErr := s.runOnce(ctx, bin, args, probe)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(startedAt) >= s.ResetAfter {
			backoff = s.MinBackoff
		}
		s.mu.Lock()
		s.state.NextStart = time.Now().Add(backoff)
		s.mu.Unlock()
		s.logger().Warn("crux serve exited; restarting", "err", exitErr, "backoff", backoff, "starts", s.State().Starts)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > s.MaxBackoff {
			backoff = s.MaxBackoff
		}
	}
}

// runOnce starts one child, waits for it to be ready, then waits for it to
// exit; it returns the exit error (nil for a clean exit).
func (s *Supervisor) runOnce(ctx context.Context, bin string, args []string, probe *api.Client) error {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, args...)
	cmd.Env = append(os.Environ(), s.Env...)
	if s.Config.Token != "" {
		cmd.Env = append(cmd.Env, config.ServeTokenEnvVar+"="+s.Config.Token)
	}
	cmd.Stdout, cmd.Stderr = s.Stdout, s.Stderr
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	cmd.WaitDelay = 5 * time.Second
	if err := s.start(cmd); err != nil {
		s.noteExit(err)
		return err
	}
	s.mu.Lock()
	s.state.Running, s.state.Ready, s.state.PID, s.state.NextStart = true, false, cmd.Process.Pid, time.Time{}
	s.state.Starts++
	s.mu.Unlock()
	s.logger().Info("crux serve started", "pid", cmd.Process.Pid, "binary", bin)

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	rctx, rcancel := context.WithTimeout(cctx, s.ReadyTimeout)
	ready := make(chan error, 1)
	go func() { ready <- probe.WaitReady(rctx, readyPoll) }()
	select {
	case err := <-exited:
		rcancel()
		<-ready
		s.noteExit(err)
		return err
	case err := <-ready:
		rcancel()
		if err != nil && cctx.Err() == nil {
			s.logger().Error("crux serve never became ready; killing", "pid", cmd.Process.Pid, "err", err)
			cancel()
			s.noteExit(<-exited)
			return fmt.Errorf("crux serve not ready within %s: %w", s.ReadyTimeout, err)
		}
		s.mu.Lock()
		s.state.Ready = true
		s.mu.Unlock()
	}
	err := <-exited
	s.noteExit(err)
	return err
}

func (s *Supervisor) noteExit(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Running, s.state.Ready, s.state.PID = false, false, 0
	s.state.LastExitAt = time.Now()
	s.state.LastExit = ""
	if err != nil {
		s.state.LastExit = err.Error()
	}
}
