package substratebridge

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPackUnpackRoundTrip(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a.txt"), "alpha")
	writeFile(t, filepath.Join(src, "sub", "deep", "b.txt"), "beta")
	if err := os.Mkdir(filepath.Join(src, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "a.txt"), 0o755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Pack(src, &buf); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	if err := Unpack(&buf, dst); err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(got) != "alpha" {
		t.Errorf("a.txt = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "sub", "deep", "b.txt")); string(got) != "beta" {
		t.Errorf("b.txt = %q", got)
	}
	if st, err := os.Stat(filepath.Join(dst, "empty")); err != nil || !st.IsDir() {
		t.Errorf("empty dir not restored: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "link")); err != nil || target != "a.txt" {
		t.Errorf("symlink = %q, %v", target, err)
	}
	if st, _ := os.Stat(filepath.Join(dst, "a.txt")); st.Mode()&0o100 == 0 {
		t.Errorf("executable bit lost: %v", st.Mode())
	}
}

func TestPackMissingDir(t *testing.T) {
	if err := Pack(filepath.Join(t.TempDir(), "nope"), io.Discard); err == nil {
		t.Fatal("Pack of a missing dir succeeded")
	}
}

func tarStream(t *testing.T, build func(tw *tar.Writer)) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	build(tw)
	// A deliberately truncated entry (header without body) makes Close
	// complain; the header is already on the wire, which is what the
	// test wants.
	_ = tw.Close()
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func TestUnpackRejectsBadStreams(t *testing.T) {
	dst := t.TempDir()
	if err := Unpack(strings.NewReader("not gzip"), dst); err == nil || !strings.Contains(err.Error(), "gzip") {
		t.Errorf("non-gzip accepted: %v", err)
	}
	escape := tarStream(t, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: "../evil", Typeflag: tar.TypeReg, Size: 0, Mode: 0o644})
	})
	if err := Unpack(escape, dst); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Errorf("escaping entry accepted: %v", err)
	}
	abs := tarStream(t, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: "/etc/passwd", Typeflag: tar.TypeReg, Size: 0, Mode: 0o644})
	})
	if err := Unpack(abs, dst); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Errorf("absolute entry accepted: %v", err)
	}
	fifo := tarStream(t, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: "pipe", Typeflag: tar.TypeFifo, Mode: 0o644})
	})
	if err := Unpack(fifo, dst); err == nil || !strings.Contains(err.Error(), "unsupported type") {
		t.Errorf("fifo accepted: %v", err)
	}
	huge := tarStream(t, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: "big", Typeflag: tar.TypeReg, Size: maxTarEntry + 1, Mode: 0o644})
	})
	if err := Unpack(huge, dst); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("oversized entry accepted: %v", err)
	}
	// Truncated tar body: gzip is fine, tar is not.
	var truncated bytes.Buffer
	gz := gzip.NewWriter(&truncated)
	_, _ = gz.Write([]byte("garbage that is not a tar header at all"))
	_ = gz.Close()
	if err := Unpack(&truncated, dst); err == nil || !strings.Contains(err.Error(), "tar") {
		t.Errorf("garbage tar accepted: %v", err)
	}
	// Destination that cannot be created.
	blocker := filepath.Join(t.TempDir(), "file")
	writeFile(t, blocker, "x")
	empty := tarStream(t, func(*tar.Writer) {})
	if err := Unpack(empty, filepath.Join(blocker, "sub")); err == nil {
		t.Errorf("unpack under a regular file succeeded")
	}
}

func TestUnpackSymlinkReplacesExistingAndNestedEntries(t *testing.T) {
	dst := t.TempDir()
	writeFile(t, filepath.Join(dst, "link"), "a file where the link goes")
	stream := tarStream(t, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "target", Mode: 0o777})
		_ = tw.WriteHeader(&tar.Header{Name: "d/e/link2", Typeflag: tar.TypeSymlink, Linkname: "../x", Mode: 0o777})
		_ = tw.WriteHeader(&tar.Header{Name: "d/e/f.txt", Typeflag: tar.TypeReg, Size: 2, Mode: 0o600})
		_, _ = tw.Write([]byte("hi"))
	})
	if err := Unpack(stream, dst); err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "link")); err != nil || target != "target" {
		t.Errorf("link = %q, %v", target, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "d", "e", "f.txt")); string(got) != "hi" {
		t.Errorf("f.txt = %q", got)
	}
}

func TestUnpackRefusesWriteThroughSymlink(t *testing.T) {
	for name, linkname := range map[string]func(outside string) string{
		"absolute": func(outside string) string { return outside },
		"relative": func(string) string { return "../outside-rel" },
	} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dst := filepath.Join(parent, "ws")
			outside := t.TempDir()
			if name == "relative" {
				outside = filepath.Join(parent, "outside-rel")
				if err := os.Mkdir(outside, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			stream := tarStream(t, func(tw *tar.Writer) {
				_ = tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: linkname(outside), Mode: 0o777})
				_ = tw.WriteHeader(&tar.Header{Name: "link/pwned", Typeflag: tar.TypeReg, Size: 2, Mode: 0o644})
				_, _ = tw.Write([]byte("no"))
			})
			if err := Unpack(stream, dst); err == nil {
				t.Fatal("write through escaping symlink succeeded")
			}
			if _, err := os.Stat(filepath.Join(outside, "pwned")); !os.IsNotExist(err) {
				t.Errorf("file written outside the workspace: %v", err)
			}
		})
	}
}

func TestUnpackEntryErrorsWhenParentIsAFile(t *testing.T) {
	dst := t.TempDir()
	writeFile(t, filepath.Join(dst, "blocker"), "x")
	for _, hdr := range []*tar.Header{
		{Name: "blocker/sub/file", Typeflag: tar.TypeReg, Size: 0, Mode: 0o644},
		{Name: "blocker/sub/link", Typeflag: tar.TypeSymlink, Linkname: "x", Mode: 0o777},
		{Name: "blocker/sub/", Typeflag: tar.TypeDir, Mode: 0o755},
	} {
		h := hdr
		stream := tarStream(t, func(tw *tar.Writer) { _ = tw.WriteHeader(h) })
		if err := Unpack(stream, dst); err == nil {
			t.Errorf("entry %q under a regular file succeeded", h.Name)
		}
	}
	// A regular file whose target is an existing directory cannot be opened.
	if err := os.Mkdir(filepath.Join(dst, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	stream := tarStream(t, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: "dir", Typeflag: tar.TypeReg, Size: 0, Mode: 0o644})
	})
	if err := Unpack(stream, dst); err == nil {
		t.Errorf("regular file over a directory succeeded")
	}
}

func TestRunReportsExitCodesAndErrors(t *testing.T) {
	ws := t.TempDir()
	ok := Run(context.Background(), RunRequest{Command: []string{"sh", "-c", "echo out; echo err >&2; pwd"}}, ws)
	if ok.ExitCode != 0 || ok.Error != "" || !strings.Contains(ok.Stdout, "out") || !strings.Contains(ok.Stderr, "err") {
		t.Errorf("ok run = %+v", ok)
	}
	if evalWS, _ := filepath.EvalSymlinks(ws); !strings.Contains(ok.Stdout, filepath.Base(evalWS)) {
		t.Errorf("command did not run in the workspace: %q", ok.Stdout)
	}

	fail := Run(context.Background(), RunRequest{Command: []string{"sh", "-c", "exit 7"}}, ws)
	if fail.ExitCode != 7 || fail.Error != "" {
		t.Errorf("exit 7 run = %+v", fail)
	}

	missing := Run(context.Background(), RunRequest{Command: []string{"/definitely/not/a/binary"}}, ws)
	if missing.ExitCode != ExitCodeUnknown || missing.Error == "" {
		t.Errorf("missing binary run = %+v", missing)
	}

	env := Run(context.Background(), RunRequest{Command: []string{"sh", "-c", "echo $FOO:$BAR; env | grep -c ^HOME= || true"}, Env: map[string]string{"FOO": "1", "BAR": "2"}}, ws)
	if !strings.HasPrefix(env.Stdout, "1:2\n0") {
		t.Errorf("env not replaced wholesale: %q", env.Stdout)
	}
	withPath := Run(context.Background(), RunRequest{Command: []string{"sh", "-c", "echo $PATH"}, Env: map[string]string{"PATH": "/custom/bin"}}, ws)
	if strings.TrimSpace(withPath.Stdout) != "/custom/bin" {
		t.Errorf("explicit PATH not honoured: %q", withPath.Stdout)
	}

	other := t.TempDir()
	wd := Run(context.Background(), RunRequest{Command: []string{"pwd"}, WorkDir: other}, ws)
	if evalOther, _ := filepath.EvalSymlinks(other); !strings.Contains(wd.Stdout, filepath.Base(evalOther)) {
		t.Errorf("workdir not honoured: %q", wd.Stdout)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := Run(ctx, RunRequest{Command: []string{"sh", "-c", "exit 0"}}, ws)
	if cancelled.Error == "" {
		t.Errorf("cancelled run reported no error: %+v", cancelled)
	}
}

func TestCapString(t *testing.T) {
	if got := capString(bytes.Repeat([]byte("x"), MaxOutput+10)); len(got) != MaxOutput {
		t.Errorf("len = %d", len(got))
	}
}

func newBridgeServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	ws := filepath.Join(t.TempDir(), "ws")
	srv := httptest.NewServer(&Handler{Workspace: ws})
	t.Cleanup(srv.Close)
	return srv, ws
}

func TestHandlerRoutes(t *testing.T) {
	srv, ws := newBridgeServer(t)

	resp, err := http.Get(srv.URL + PathHealth)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %v %v", err, resp)
	}
	resp.Body.Close()

	src := t.TempDir()
	writeFile(t, filepath.Join(src, "hello.txt"), "hi")
	var buf bytes.Buffer
	if err := Pack(src, &buf); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPut, srv.URL+PathWorkspace, &buf)
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("put workspace: %v %v", err, resp)
	}
	resp.Body.Close()
	if got, _ := os.ReadFile(filepath.Join(ws, "hello.txt")); string(got) != "hi" {
		t.Errorf("workspace not unpacked: %q", got)
	}

	body, _ := json.Marshal(RunRequest{Command: []string{"sh", "-c", "echo changed > hello.txt; exit 3"}})
	resp, err = http.Post(srv.URL+PathRun, contentTypeJSON, bytes.NewReader(body))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("run: %v %v", err, resp)
	}
	var out RunResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.ExitCode != 3 {
		t.Errorf("run response = %+v", out)
	}

	resp, err = http.Get(srv.URL + PathWorkspace)
	if err != nil || resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != contentTypeTar {
		t.Fatalf("get workspace: %v %v", err, resp)
	}
	dst := t.TempDir()
	if err := Unpack(resp.Body, dst); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got, _ := os.ReadFile(filepath.Join(dst, "hello.txt")); strings.TrimSpace(string(got)) != "changed" {
		t.Errorf("pulled workspace = %q", got)
	}
}

func TestHandlerErrors(t *testing.T) {
	srv, ws := newBridgeServer(t)
	status := func(method, path string, body io.Reader) int {
		req, _ := http.NewRequest(method, srv.URL+path, body)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if got := status(http.MethodGet, "/nope", nil); got != http.StatusNotFound {
		t.Errorf("unknown route = %d", got)
	}
	if got := status(http.MethodDelete, PathWorkspace, nil); got != http.StatusNotFound {
		t.Errorf("unknown method = %d", got)
	}
	if got := status(http.MethodPut, PathWorkspace, strings.NewReader("not a tarball")); got != http.StatusBadRequest {
		t.Errorf("bad tarball = %d", got)
	}
	if got := status(http.MethodPost, PathRun, strings.NewReader("{")); got != http.StatusBadRequest {
		t.Errorf("bad json = %d", got)
	}
	if got := status(http.MethodPost, PathRun, strings.NewReader(`{"command":[]}`)); got != http.StatusBadRequest {
		t.Errorf("empty command = %d", got)
	}
	// Workspace missing: GET cannot pack it.
	if got := status(http.MethodGet, PathWorkspace, nil); got != http.StatusInternalServerError {
		t.Errorf("get of missing workspace = %d", got)
	}
	// Workspace parent is a file: PUT cannot recreate it.
	_ = ws
	blocked := httptest.NewServer(&Handler{Workspace: filepath.Join(ws, "..", "ws-file", "sub")})
	defer blocked.Close()
	writeFile(t, filepath.Join(ws, "..", "ws-file"), "x")
	empty := tarStream(t, func(*tar.Writer) {})
	req, _ := http.NewRequest(http.MethodPut, blocked.URL+PathWorkspace, empty)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("put under a file = %d, want 500 from clearing the workspace", resp.StatusCode)
	}
}

func TestHandlerDefaultsAndRunTimeout(t *testing.T) {
	h := &Handler{}
	if h.workspace() != DefaultWorkspace {
		t.Errorf("default workspace = %q", h.workspace())
	}
	// RunTimeout already expired: the command is killed and the response
	// says why, without the test waiting on it.
	srv := httptest.NewServer(&Handler{Workspace: t.TempDir(), RunTimeout: 1})
	defer srv.Close()
	body, _ := json.Marshal(RunRequest{Command: []string{"sh", "-c", "sleep 30"}})
	resp, err := http.Post(srv.URL+PathRun, contentTypeJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out RunResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Error == "" {
		t.Errorf("timed-out run reported no error: %+v", out)
	}
}

func TestServeStopsOnContextAndListenerClose(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, &Handler{Workspace: t.TempDir()}) }()
	resp, err := http.Get("http://" + ln.Addr().String() + PathHealth)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz over Serve: %v %v", err, resp)
	}
	resp.Body.Close()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Serve returned %v, want context.Canceled", err)
	}

	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done2 := make(chan error, 1)
	go func() { done2 <- Serve(context.Background(), ln2, &Handler{}) }()
	// Closing the listener makes Serve return the listener error (not
	// ErrServerClosed, which only Shutdown produces).
	_ = ln2.Close()
	if err := <-done2; err == nil {
		t.Errorf("Serve returned nil after the listener closed")
	}
}

// Inside the actor /workspace is a durableDir mount point; PUT must empty it
// in place rather than unlink it (which fails with EBUSY on a mount).
func TestPutWorkspaceClearsInPlace(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "stale.txt"), "old")
	writeFile(t, filepath.Join(ws, "nested", "deep.txt"), "old")
	before, err := os.Stat(ws)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(&Handler{Workspace: ws})
	defer srv.Close()
	body := tarStream(t, func(tw *tar.Writer) {
		content := []byte("new")
		_ = tw.WriteHeader(&tar.Header{Name: "fresh.txt", Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg})
		_, _ = tw.Write(content)
	})
	req, _ := http.NewRequest(http.MethodPut, srv.URL+PathWorkspace, body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		t.Fatalf("put = %d", resp.StatusCode)
	}
	after, err := os.Stat(ws)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("workspace directory was recreated; it must be emptied in place")
	}
	if _, err := os.Stat(filepath.Join(ws, "stale.txt")); !os.IsNotExist(err) {
		t.Error("stale.txt survived PUT")
	}
	if _, err := os.Stat(filepath.Join(ws, "nested")); !os.IsNotExist(err) {
		t.Error("nested/ survived PUT")
	}
	if got, err := os.ReadFile(filepath.Join(ws, "fresh.txt")); err != nil || string(got) != "new" {
		t.Errorf("fresh.txt = %q, %v", got, err)
	}
	if err := clearDir(filepath.Join(ws, "does-not-exist")); err != nil {
		t.Errorf("clearDir of a missing dir = %v, want nil", err)
	}
}

func TestHandlerStreamsRunOutput(t *testing.T) {
	srv, ws := newBridgeServer(t)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(RunRequest{Command: []string{"sh", "-c", "echo one; echo two >&2; exit 2"}, Stream: true})
	resp, err := http.Post(srv.URL+PathRun, contentTypeJSON, bytes.NewReader(body))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("run: %v %v", err, resp)
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	var stdout, stderr strings.Builder
	var frames []RunResponse
	for {
		var f RunResponse
		if err := dec.Decode(&f); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, f)
		stdout.WriteString(f.Stdout)
		stderr.WriteString(f.Stderr)
	}
	if len(frames) < 2 {
		t.Fatalf("frames = %+v", frames)
	}
	for _, f := range frames[:len(frames)-1] {
		if !f.Partial {
			t.Fatalf("only the last frame may be final: %+v", frames)
		}
	}
	if last := frames[len(frames)-1]; last.Partial || last.ExitCode != 2 || last.Stdout != "" || last.Stderr != "" {
		t.Fatalf("final frame = %+v", last)
	}
	if stdout.String() != "one\n" || stderr.String() != "two\n" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestFrameWriterCapsOutput(t *testing.T) {
	var buf bytes.Buffer
	w := &frameWriter{frames: &frameEncoder{enc: json.NewEncoder(&buf)}}
	big := make([]byte, MaxOutput+5)
	if n, err := w.Write(big); err != nil || n != len(big) {
		t.Fatalf("Write = %d %v", n, err)
	}
	buf.Reset()
	if n, err := w.Write([]byte("more")); err != nil || n != 4 || buf.Len() != 0 {
		t.Fatalf("past the cap: n=%d err=%v frames=%q", n, err, buf.String())
	}
}
