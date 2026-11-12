// Package substratebridge is the in-actor side of hive's Agent Substrate
// runtime (ADR-0026 phase 2). Substrate has no exec, attach or logs RPC and
// cannot mount an existing PersistentVolumeClaim, so the sandbox image runs
// this small HTTP server as the actor's entrypoint and hive drives the kick
// through it over the atenet-router mTLS CONNECT tunnel: push the workspace
// in, run the command, pull the workspace back out. The server listens on
// the actor's private network only; the router is the only way in and it
// admits only allowlisted SPIFFE identities.
package substratebridge

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultPort is the port the bridge serves on inside the actor and the
	// port hive's CONNECT tunnel targets.
	DefaultPort = 8477
	// DefaultWorkspace is where the bridge unpacks the kick workspace.
	DefaultWorkspace = "/workspace"

	PathHealth    = "/healthz"
	PathWorkspace = "/workspace"
	PathRun       = "/run"

	// MaxOutput caps each of stdout and stderr in a RunResponse; the same
	// 16 MiB the OpenShell launcher keeps.
	MaxOutput = 16 << 20

	// ExitCodeUnknown is reported when the command could not be started or
	// ended without an exit status.
	ExitCodeUnknown = -1

	contentTypeTar  = "application/x-tar+gzip"
	contentTypeJSON = "application/json"
	maxRequestJSON  = 1 << 20
	maxTarEntry     = 1 << 30
)

// RunRequest is the body of POST /run.
type RunRequest struct {
	Command []string          `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
	WorkDir string            `json:"workdir,omitempty"`
	// Stream asks for the output as it is produced: the response is then a
	// sequence of JSON RunResponse frames, every one but the last marked
	// Partial and carrying only new output, the last carrying the exit
	// code. A bridge that predates the field ignores it and answers with a
	// single complete RunResponse, which a streaming client reads as a
	// final frame, so either side can be upgraded first.
	Stream bool `json:"stream,omitempty"`
}

// RunResponse is the body POST /run answers with once the command exits,
// or one frame of a streamed answer (see RunRequest.Stream).
type RunResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
	// Partial marks a streamed output frame; its ExitCode means nothing.
	Partial bool `json:"partial,omitempty"`
}

// Handler serves the bridge protocol for one workspace directory.
type Handler struct {
	// Workspace is the directory PUT /workspace fills, GET /workspace packs
	// and POST /run runs in. Defaults to DefaultWorkspace.
	Workspace string
	// RunTimeout bounds one POST /run; zero means the request context alone.
	RunTimeout time.Duration
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == PathHealth && r.Method == http.MethodGet:
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	case r.URL.Path == PathWorkspace && r.Method == http.MethodPut:
		h.putWorkspace(w, r)
	case r.URL.Path == PathWorkspace && r.Method == http.MethodGet:
		h.getWorkspace(w, r)
	case r.URL.Path == PathRun && r.Method == http.MethodPost:
		h.run(w, r)
	default:
		http.Error(w, "substrate bridge: unknown route", http.StatusNotFound)
	}
}

func (h *Handler) workspace() string {
	if h.Workspace != "" {
		return h.Workspace
	}
	return DefaultWorkspace
}

// clearDir empties dir without removing dir itself: inside the actor the
// workspace is a durableDir mount point, and unlinking a mount point fails
// with EBUSY.
func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) putWorkspace(w http.ResponseWriter, r *http.Request) {
	dir := h.workspace()
	if err := clearDir(dir); err != nil {
		http.Error(w, fmt.Sprintf("substrate bridge: clearing %s: %v", dir, err), http.StatusInternalServerError)
		return
	}
	if err := Unpack(r.Body, dir); err != nil {
		http.Error(w, fmt.Sprintf("substrate bridge: unpacking workspace: %v", err), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) getWorkspace(w http.ResponseWriter, _ *http.Request) {
	var buf bytes.Buffer
	if err := Pack(h.workspace(), &buf); err != nil {
		http.Error(w, fmt.Sprintf("substrate bridge: packing workspace: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentTypeTar)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, &buf)
}

func (h *Handler) run(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRequestJSON)).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("substrate bridge: decoding run request: %v", err), http.StatusBadRequest)
		return
	}
	if len(req.Command) == 0 {
		http.Error(w, "substrate bridge: command is required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	if h.RunTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.RunTimeout)
		defer cancel()
	}
	w.Header().Set("Content-Type", contentTypeJSON)
	if !req.Stream {
		resp := Run(ctx, req, h.workspace())
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	w.WriteHeader(http.StatusOK)
	frames := &frameEncoder{enc: json.NewEncoder(w)}
	if f, ok := w.(http.Flusher); ok {
		frames.flush = f.Flush
	}
	resp := run(ctx, req, h.workspace(), &frameWriter{frames: frames, stderr: false}, &frameWriter{frames: frames, stderr: true})
	frames.send(RunResponse{ExitCode: resp.ExitCode, Error: resp.Error})
}

// frameEncoder serializes the streamed frames of one POST /run; stdout and
// stderr are written from separate goroutines by os/exec.
type frameEncoder struct {
	mu    sync.Mutex
	enc   *json.Encoder
	flush func()
}

func (f *frameEncoder) send(frame RunResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = f.enc.Encode(frame)
	if f.flush != nil {
		f.flush()
	}
}

// frameWriter turns one of the command's output streams into Partial
// frames, capped at MaxOutput like the buffered answer. It never fails the
// command: a client that went away only loses the rest of the output.
type frameWriter struct {
	frames  *frameEncoder
	stderr  bool
	written int
}

func (w *frameWriter) Write(p []byte) (int, error) {
	chunk := p
	if room := MaxOutput - w.written; len(chunk) > room {
		chunk = chunk[:max(room, 0)]
	}
	if len(chunk) > 0 {
		w.written += len(chunk)
		frame := RunResponse{Partial: true}
		if w.stderr {
			frame.Stderr = string(chunk)
		} else {
			frame.Stdout = string(chunk)
		}
		w.frames.send(frame)
	}
	return len(p), nil
}

// Run executes one RunRequest with the request env replacing the process
// env — the launcher already filtered it through hive's credential-free
// allowlist — and reports the exit code the way the Podman launcher does.
func Run(ctx context.Context, req RunRequest, workspace string) RunResponse {
	var stdout, stderr bytes.Buffer
	resp := run(ctx, req, workspace, &stdout, &stderr)
	resp.Stdout, resp.Stderr = capString(stdout.Bytes()), capString(stderr.Bytes())
	return resp
}

// run executes req with its output going to stdout and stderr and reports
// the exit code; the output fields of the result are left empty.
func run(ctx context.Context, req RunRequest, workspace string, stdout, stderr io.Writer) RunResponse {
	cmd := exec.CommandContext(ctx, req.Command[0], req.Command[1:]...)
	cmd.Dir = req.WorkDir
	if cmd.Dir == "" {
		cmd.Dir = workspace
	}
	keys := make([]string, 0, len(req.Env))
	for k := range req.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		env = append(env, k+"="+req.Env[k])
	}
	if _, ok := req.Env["PATH"]; !ok {
		env = append(env, "PATH="+os.Getenv("PATH"))
	}
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	var resp RunResponse
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		resp.ExitCode = exitErr.ExitCode()
	default:
		resp.ExitCode = ExitCodeUnknown
		resp.Error = err.Error()
	}
	if ctx.Err() != nil && resp.Error == "" {
		resp.Error = ctx.Err().Error()
	}
	return resp
}

func capString(b []byte) string {
	if len(b) > MaxOutput {
		b = b[:MaxOutput]
	}
	return string(b)
}

// Pack writes dir as a gzip-compressed tar stream: regular files, directories
// and symlinks, paths relative to dir. Git workspaces are small enough that a
// single stream is simpler than any chunked protocol.
func Pack(dir string, w io.Writer) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// Unpack reads a Pack stream into dir, refusing entries that escape it
// lexically (absolute or ".." names) or through symlinks: every write goes
// through an os.Root, so a symlink entry may be created but nothing is ever
// written through one that leaves dir.
func Unpack(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		rel, err := safeRel(hdr.Name)
		if err != nil {
			return err
		}
		if err := unpackEntry(tr, hdr, root, rel); err != nil {
			return fmt.Errorf("tar: entry %q: %w", hdr.Name, err)
		}
	}
}

func unpackEntry(tr *tar.Reader, hdr *tar.Header, root *os.Root, rel string) error {
	switch hdr.Typeflag {
	case tar.TypeDir:
		return root.MkdirAll(rel, os.FileMode(hdr.Mode)&os.ModePerm|0o700)
	case tar.TypeSymlink:
		if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
			return err
		}
		_ = root.Remove(rel)
		return root.Symlink(hdr.Linkname, rel)
	case tar.TypeReg:
		if hdr.Size > maxTarEntry {
			return fmt.Errorf("tar: entry %q is too large (%d bytes)", hdr.Name, hdr.Size)
		}
		if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
			return err
		}
		f, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&os.ModePerm|0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, io.LimitReader(tr, hdr.Size)); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	default:
		// Devices, FIFOs and hard links have no place in a git workspace.
		return fmt.Errorf("tar: entry %q has unsupported type %q", hdr.Name, hdr.Typeflag)
	}
}

// safeRel is the lexical first filter; os.Root enforces the rest.
func safeRel(name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("tar: entry %q escapes the workspace", name)
	}
	return clean, nil
}

// Serve runs the bridge on ln until ctx ends. The actor's command is
// expected to be the sandbox image's bridge entrypoint calling this.
func Serve(ctx context.Context, ln net.Listener, h *Handler) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return ctx.Err()
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
