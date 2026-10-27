package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"github.com/hivecommons/hive/crux"
)

// The control socket is how ps, logs, kick and stop reach the runtime a
// foreground `crux run` holds. It is a local convenience, not the Stage 3
// API: one JSON request per connection carrying the subcommand's argv, one
// JSON response carrying what the command wrote and its exit code. The
// server runs the same cmd* functions the local path runs, so a command
// behaves identically whichever runtime it lands on.

// controlRequest is one subcommand invocation: the subcommand name followed
// by its own flags and arguments (no global flags).
type controlRequest struct {
	Args []string `json:"args"`
}

// controlResponse is what the serving side's command wrote.
type controlResponse struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Code   int    `json:"code"`
}

// dialFunc connects to a control socket at path.
type dialFunc func(ctx context.Context, path string) (net.Conn, error)

func dialControl(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}

// forward sends args to the control socket when one is being served and
// relays the response. ok is false when there is no live socket, in which
// case the caller runs the command locally. A socket file that nobody
// answers on (a `run` that died without cleaning up) is reported and
// treated as absent.
func (e *Env) forward(ctx context.Context, opts globalOptions, args []string) (code int, ok bool) {
	if _, err := os.Stat(opts.socket); err != nil {
		return 0, false
	}
	conn, err := e.dial(ctx, opts.socket)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crux: control socket %s is not answering (%v); using a fresh in-process runtime\n", opts.socket, err)
		return 0, false
	}
	defer func() { _ = conn.Close() }()
	if err := json.NewEncoder(conn).Encode(controlRequest{Args: args}); err != nil {
		fmt.Fprintf(e.Stderr, "crux: control socket %s: sending request: %v\n", opts.socket, err)
		return ExitError, true
	}
	var resp controlResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		fmt.Fprintf(e.Stderr, "crux: control socket %s: reading response: %v\n", opts.socket, err)
		return ExitError, true
	}
	_, _ = io.WriteString(e.Stdout, resp.Stdout)
	_, _ = io.WriteString(e.Stderr, resp.Stderr)
	return resp.Code, true
}

// serveControl listens on path and answers requests against rt until ctx
// ends or the returned stop function is called. A stale socket file is
// replaced; a socket something is still listening on is an error, so two
// foreground runs in one work dir do not fight over it.
func (e *Env) serveControl(ctx context.Context, rt crux.Runtime, path string) (stop func(), err error) {
	if conn, derr := e.dial(ctx, path); derr == nil {
		_ = conn.Close()
		return nil, errors.New("another crux run is already serving it")
	} else if ctx.Err() != nil {
		// An interrupted probe proves nothing; do not remove a socket that
		// may be live.
		return nil, ctx.Err()
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	var wg sync.WaitGroup
	var once sync.Once
	stop = func() {
		once.Do(func() {
			_ = ln.Close()
			_ = os.Remove(path)
		})
		wg.Wait()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				e.serveConn(ctx, rt, conn)
			}()
		}
	}()
	go func() {
		<-ctx.Done()
		stop()
	}()
	return stop, nil
}

func (e *Env) serveConn(ctx context.Context, rt crux.Runtime, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	var req controlRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(controlResponse{Stderr: "crux: bad control request: " + err.Error() + "\n", Code: ExitUsage})
		return
	}
	var stdout, stderr bytes.Buffer
	served := &Env{Stdin: bytes.NewReader(nil), Stdout: &stdout, Stderr: &stderr}
	served.defaults()
	code := ExitUsage
	if len(req.Args) == 0 {
		stderr.WriteString("crux: empty control request\n")
	} else {
		code = served.observe(ctx, rt, req.Args[0], req.Args[1:])
	}
	_ = json.NewEncoder(conn).Encode(controlResponse{Stdout: stdout.String(), Stderr: stderr.String(), Code: code})
}
