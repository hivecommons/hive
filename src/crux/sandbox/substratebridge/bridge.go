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
}

// RunResponse is the body POST /run answers with once the command exits.
type RunResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
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

func (h *Handler) putWorkspace(w http.ResponseWriter, r *http.Request) {
	dir := h.workspace()
	if err := os.RemoveAll(dir); err != nil {
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
	resp := Run(ctx, req, h.workspace())
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// Run executes one RunRequest with the request env replacing the process
// env — the launcher already filtered it through hive's credential-free
// allowlist — and reports the exit code the way the Podman launcher does.
func Run(ctx context.Context, req RunRequest, workspace string) RunResponse {
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
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	resp := RunResponse{Stdout: capString(stdout.Bytes()), Stderr: capString(stderr.Bytes())}
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

// Unpack reads a Pack stream into dir, refusing entries that would escape it.
func Unpack(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		target, err := safeJoin(dir, hdr.Name)
		if err != nil {
			return err
		}
		if err := unpackEntry(tr, hdr, target); err != nil {
			return err
		}
	}
}

func unpackEntry(tr *tar.Reader, hdr *tar.Header, target string) error {
	switch hdr.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(target, os.FileMode(hdr.Mode)&os.ModePerm|0o700)
	case tar.TypeSymlink:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		_ = os.Remove(target)
		return os.Symlink(hdr.Linkname, target)
	case tar.TypeReg:
		if hdr.Size > maxTarEntry {
			return fmt.Errorf("tar: entry %q is too large (%d bytes)", hdr.Name, hdr.Size)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&os.ModePerm|0o600)
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

func safeJoin(dir, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("tar: entry %q escapes the workspace", name)
	}
	return filepath.Join(dir, clean), nil
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
