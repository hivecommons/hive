package substratebridge

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKickServerRequiresACommand(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var stderr bytes.Buffer
	k := &KickServer{Token: kickTestToken, Stderr: &stderr}
	if code := k.Serve(context.Background(), ln); code != ExitCannotRun {
		t.Errorf("exit code = %d, want ExitCannotRun", code)
	}
	if !strings.Contains(stderr.String(), "a command is required") {
		t.Errorf("stderr = %q, want the missing-command message", stderr.String())
	}
}

func TestKickServerReportsACommandThatCannotStart(t *testing.T) {
	k := &KickServer{Command: []string{filepath.Join(t.TempDir(), "no-such-binary")}}
	var stderr bytes.Buffer
	k.Stderr = &stderr
	if code := k.run(context.Background()); code != ExitCannotRun {
		t.Errorf("exit code = %d, want ExitCannotRun", code)
	}
	if !strings.Contains(stderr.String(), "job bridge: running") {
		t.Errorf("stderr = %q, want the start failure", stderr.String())
	}
}

func TestKickServerRejectsOutOfOrderAndUnknownRequests(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "in.txt"), "hello")
	var packed bytes.Buffer
	if err := Pack(src, &packed); err != nil {
		t.Fatal(err)
	}
	k := &KickServer{Workspace: t.TempDir(), Command: []string{"true"}, Token: kickTestToken}
	k.pushed, k.done = make(chan struct{}), make(chan struct{})
	srv := &http.Server{Handler: k, ReadHeaderTimeout: time.Second}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	base := "http://" + ln.Addr().String()

	cases := []struct {
		name, method, path string
		body               []byte
		want               int
	}{
		{"unknown route", http.MethodGet, "/nope", nil, http.StatusNotFound},
		{"done before the run", http.MethodPost, PathDone, nil, http.StatusConflict},
		{"garbage push", http.MethodPut, PathWorkspace, []byte("not a tar archive"), http.StatusBadRequest},
		{"push", http.MethodPut, PathWorkspace, packed.Bytes(), http.StatusNoContent},
		{"second push", http.MethodPut, PathWorkspace, packed.Bytes(), http.StatusConflict},
		{"done while running", http.MethodPost, PathDone, nil, http.StatusConflict},
	}
	for _, c := range cases {
		resp := kickRequest(t, c.method, base+c.path, kickTestToken, c.body)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s = %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}

	k.setPhase(phaseExited)
	for i := 0; i < 2; i++ { // a repeated /done stays a 204
		resp := kickRequest(t, http.MethodPost, base+PathDone, kickTestToken, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("done %d = %d, want 204", i, resp.StatusCode)
		}
	}
}

func TestKickServerStopsWaitingForTheCollect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	k := &KickServer{
		Workspace: t.TempDir(),
		Command:   []string{"sh", "-c", "exit 4"},
		Token:     kickTestToken,
		Wait:      300 * time.Millisecond,
		Stderr:    &stderr,
	}
	codeCh := make(chan int, 1)
	go func() { codeCh <- k.Serve(context.Background(), ln) }()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "in.txt"), "x")
	var packed bytes.Buffer
	if err := Pack(src, &packed); err != nil {
		t.Fatal(err)
	}
	resp := kickRequest(t, http.MethodPut, "http://"+ln.Addr().String()+PathWorkspace, kickTestToken, packed.Bytes())
	resp.Body.Close()
	select {
	case code := <-codeCh:
		if code != 4 {
			t.Errorf("exit code = %d, want the command's 4", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after the collect wait")
	}
	if !strings.Contains(stderr.String(), "did not collect the workspace") {
		t.Errorf("stderr = %q, want the collect timeout", stderr.String())
	}
}

func TestKickServerDefaults(t *testing.T) {
	k := &KickServer{}
	if got := k.wait(); got != DefaultKickWait {
		t.Errorf("wait() = %s, want DefaultKickWait", got)
	}
	if k.stderr() == nil {
		t.Error("stderr() must never be nil")
	}
	env := []string{"A=1", "TOK=x", "B=2"}
	if got := withoutEnv(env, ""); len(got) != 3 {
		t.Errorf("withoutEnv with no name = %v, want env unchanged", got)
	}
	if got := withoutEnv(env, "TOK"); len(got) != 2 {
		t.Errorf("withoutEnv(TOK) = %v, want TOK dropped", got)
	}
}
