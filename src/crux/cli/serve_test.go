package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/config"
)

// socketClient is an http.Client that dials the unix socket at path.
func socketClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}
}

func TestServeExposesTheRuntimeOverTheAPI(t *testing.T) {
	t.Setenv(config.ServeTokenEnvVar, "")
	h := newHarness(t)
	cfgPath := writeFile(t, filepath.Join(t.TempDir(), "hive.yaml"), "agents: {fixer: {}, scout: {}}\n")
	socket := filepath.Join(shortSocketDir(t), "api.sock")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- h.env.Run(ctx, append(h.workDirArgs(), "--config", cfgPath, "serve", "--socket", socket))
	}()
	waitForSocket(t, socket)

	fi, err := os.Stat(socket)
	if err != nil || fi.Mode().Perm() != config.ServeSocketMode {
		t.Fatalf("socket stat %v mode %o type %v path %s", err, fi.Mode().Perm(), fi.Mode().Type(), socket)
	}
	client := socketClient(socket)
	resp, err := client.Get("http://crux" + api.PathAgents)
	if err != nil {
		t.Fatal(err)
	}
	var list api.ProcessList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(list.Processes) != 2 || list.Processes[0].Name != "fixer" || list.Processes[0].State != string(crux.StateStopped) {
		t.Fatalf("list = %+v", list)
	}
	if calls := h.rt.Calls(); calls[0] != "SetKickObserver(true)" || calls[1] != "AllStatuses()" {
		t.Fatalf("calls = %v", calls)
	}

	cancel()
	select {
	case code := <-done:
		if code != ExitOK {
			t.Fatalf("serve exit %d stderr %q", code, h.stderr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop")
	}
	if out := h.stdout.String(); !strings.Contains(out, "serving 2 agent definition(s) on unix://"+socket) || !strings.Contains(out, "serve stopped") {
		t.Fatalf("stdout = %q", out)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("socket should be removed on stop, stat err = %v", err)
	}
}

func TestServeTokenFromEnvironmentGatesTCP(t *testing.T) {
	t.Setenv(config.ServeTokenEnvVar, "from-env")
	h := newHarness(t)
	socket := filepath.Join(shortSocketDir(t), "api.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- h.env.Run(ctx, append(h.workDirArgs(), "serve", "--socket", socket, "--listen", "127.0.0.1:0"))
	}()
	waitForSocket(t, socket)

	// The token in the environment is required on the socket too.
	resp, err := socketClient(socket).Get("http://crux" + api.PathHealth)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://crux"+api.PathHealth, nil)
	req.Header.Set("Authorization", "Bearer from-env")
	resp, err = socketClient(socket).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("with token: %d", resp.StatusCode)
	}
	// The TCP address was bound and announced; the banner is printed before
	// Serve starts, so it is complete once the socket answers.
	if !strings.Contains(h.stdout.String(), "http://127.0.0.1:") {
		t.Fatalf("stdout = %q", h.stdout.String())
	}
	cancel()
	<-done
}

func TestServeUsageAndErrors(t *testing.T) {
	t.Setenv(config.ServeTokenEnvVar, "")
	h := newHarness(t)
	socket := filepath.Join(shortSocketDir(t), "api.sock")

	if code := h.run("serve", "extra"); code != ExitUsage || !strings.Contains(h.stderr.String(), "usage: crux serve") {
		t.Fatalf("extra arg: %d %q", code, h.stderr)
	}
	h.stderr.Reset()
	if code := h.run("serve", "--socket", socket, "--listen", "127.0.0.1:0"); code != ExitUsage || !strings.Contains(h.stderr.String(), "bearer token") {
		t.Fatalf("TCP without token: %d %q", code, h.stderr)
	}
	h.stderr.Reset()
	if code := h.run("serve", "-bogus"); code != ExitUsage {
		t.Fatalf("bad flag: %d", code)
	}
	h.stderr.Reset()
	if code := h.run("serve", "-h"); code != ExitOK {
		t.Fatalf("-h: %d", code)
	}
	h.stderr.Reset()
	if code := h.run("--config", filepath.Join(t.TempDir(), "missing.yaml"), "serve", "--socket", socket); code != ExitError {
		t.Fatalf("missing config: %d %q", code, h.stderr)
	}
	h.stderr.Reset()
	// A socket path that cannot be bound: its parent is a regular file.
	writeFile(t, socket, "")
	if code := h.run("serve", "--socket", filepath.Join(socket, "under-a-file.sock")); code != ExitError || !strings.Contains(h.stderr.String(), "serve:") {
		t.Fatalf("bad socket: %d %q", code, h.stderr)
	}

	// serve is refused over the control socket, like run.
	served := &Env{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	served.defaults()
	if code := served.dispatch(context.Background(), globalOptions{}, "serve", nil, true); code != ExitUsage || !strings.Contains(served.Stderr.(*bytes.Buffer).String(), "not available over the control socket") {
		t.Fatalf("remote serve: %d %q", code, served.Stderr)
	}
}
