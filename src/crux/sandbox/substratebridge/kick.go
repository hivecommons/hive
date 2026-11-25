package substratebridge

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// The Job runtime's copy transport (hivecommons/hive#19621). A Job whose
// workspace is not on a shared claim runs KickServer as its container
// command: hive pushes the workspace in (PUT /workspace), the server runs the
// kick command against it, then hive pulls the result back (GET /workspace)
// and says it is done (POST /done). The command's stdout and stderr go to the
// container's own, so the pod log stays the transcript and the container
// exit code stays the kick's exit code.

const (
	// PathDone ends a KickServer once hive has the workspace back.
	PathDone = "/done"
	// DefaultKickWait bounds each wait for hive: for the workspace before
	// the command runs and for the pull after it exits.
	DefaultKickWait = 10 * time.Minute
	// ExitNoWorkspace is the exit code when hive never pushed the workspace.
	ExitNoWorkspace = 125
	// ExitCannotRun is the exit code when the command could not be started.
	ExitCannotRun = 127
)

// Kick server phases.
const (
	phaseWaitPush = iota
	phaseRunning
	phaseExited
)

// KickServer serves one kick on the copy transport.
type KickServer struct {
	// Workspace is the directory the push fills and the pull packs.
	Workspace string
	// Command runs once the workspace is in, with this process's
	// environment minus TokenEnv.
	Command []string
	// Token is the bearer token every route but /healthz requires.
	Token string
	// TokenEnv names the variable the token came from; it is removed from
	// the command's environment.
	TokenEnv string
	// Wait bounds each wait for hive; zero means DefaultKickWait.
	Wait time.Duration
	// Stdout and Stderr receive the command's output.
	Stdout, Stderr io.Writer

	mu     sync.Mutex
	phase  int
	pushed chan struct{}
	done   chan struct{}
}

// Serve runs the kick on ln and returns the exit code the container should
// end with.
func (k *KickServer) Serve(ctx context.Context, ln net.Listener) int {
	if len(k.Command) == 0 {
		fmt.Fprintln(k.stderr(), "job bridge: a command is required")
		return ExitCannotRun
	}
	k.pushed, k.done = make(chan struct{}), make(chan struct{})
	srv := &http.Server{Handler: k, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if !k.await(ctx, k.pushed) {
		fmt.Fprintf(k.stderr(), "job bridge: hive did not push the workspace within %s\n", k.wait())
		return ExitNoWorkspace
	}
	code := k.run(ctx)
	k.setPhase(phaseExited)
	if !k.await(ctx, k.done) {
		fmt.Fprintf(k.stderr(), "job bridge: hive did not collect the workspace within %s\n", k.wait())
	}
	return code
}

func (k *KickServer) await(ctx context.Context, ch <-chan struct{}) bool {
	timer := time.NewTimer(k.wait())
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-ctx.Done():
	case <-timer.C:
	}
	return false
}

func (k *KickServer) run(ctx context.Context) int {
	cmd := exec.CommandContext(ctx, k.Command[0], k.Command[1:]...)
	cmd.Stdout, cmd.Stderr = k.Stdout, k.Stderr
	cmd.Env = withoutEnv(os.Environ(), k.TokenEnv)
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr) && exitErr.ExitCode() >= 0:
		return exitErr.ExitCode()
	default:
		fmt.Fprintf(k.stderr(), "job bridge: running %s: %v\n", k.Command[0], err)
		return ExitCannotRun
	}
}

// withoutEnv drops name from env.
func withoutEnv(env []string, name string) []string {
	if name == "" {
		return env
	}
	out := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, name+"=") {
			out = append(out, e)
		}
	}
	return out
}

func (k *KickServer) wait() time.Duration {
	if k.Wait > 0 {
		return k.Wait
	}
	return DefaultKickWait
}

func (k *KickServer) workspaceDir() string {
	if k.Workspace != "" {
		return k.Workspace
	}
	return DefaultWorkspace
}

func (k *KickServer) stderr() io.Writer {
	if k.Stderr != nil {
		return k.Stderr
	}
	return io.Discard
}

func (k *KickServer) setPhase(p int) {
	k.mu.Lock()
	k.phase = p
	k.mu.Unlock()
}

func (k *KickServer) authorized(r *http.Request) bool {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return k.Token != "" && subtle.ConstantTimeCompare([]byte(got), []byte(k.Token)) == 1
}

// ServeHTTP implements http.Handler.
func (k *KickServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == PathHealth && r.Method == http.MethodGet {
		_, _ = io.WriteString(w, "ok\n")
		return
	}
	if !k.authorized(r) {
		http.Error(w, "job bridge: unauthorized", http.StatusUnauthorized)
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	switch {
	case r.URL.Path == PathWorkspace && r.Method == http.MethodPut:
		if k.phase != phaseWaitPush {
			http.Error(w, "job bridge: the workspace was already pushed", http.StatusConflict)
			return
		}
		dir := k.workspaceDir()
		if err := clearDir(dir); err != nil {
			http.Error(w, fmt.Sprintf("job bridge: clearing %s: %v", dir, err), http.StatusInternalServerError)
			return
		}
		if err := Unpack(r.Body, dir); err != nil {
			http.Error(w, fmt.Sprintf("job bridge: unpacking workspace: %v", err), http.StatusBadRequest)
			return
		}
		k.phase = phaseRunning
		close(k.pushed)
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == PathWorkspace && r.Method == http.MethodGet:
		if k.phase != phaseExited {
			http.Error(w, "job bridge: the kick is still running", http.StatusConflict)
			return
		}
		dir := k.workspaceDir()
		var buf bytes.Buffer
		if err := Pack(dir, &buf); err != nil {
			http.Error(w, fmt.Sprintf("job bridge: packing workspace: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", contentTypeTar)
		_, _ = io.Copy(w, &buf)
	case r.URL.Path == PathDone && r.Method == http.MethodPost:
		if k.phase != phaseExited {
			http.Error(w, "job bridge: the kick is still running", http.StatusConflict)
			return
		}
		select {
		case <-k.done:
		default:
			close(k.done)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "job bridge: unknown route", http.StatusNotFound)
	}
}
