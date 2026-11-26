package substratebridge

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const kickTestToken = "s3cret"

func kickRequest(t *testing.T, method, url, token string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestKickServerPushRunPull drives one copy-transport kick (#19621): the
// command runs only after the push, sees its env without the token, and
// its changes and exit code come back.
func TestKickServerPushRunPull(t *testing.T) {
	t.Setenv("HIVE_KICK_TEST_TOKEN", kickTestToken)
	work := t.TempDir()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "in.txt"), "hello")
	var packed bytes.Buffer
	if err := Pack(src, &packed); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	k := &KickServer{
		Workspace: work,
		Command:   []string{"sh", "-c", `cat "$0/in.txt" > "$0/out.txt"; echo "token=${HIVE_KICK_TEST_TOKEN:-unset}"; exit 3`, work},
		Token:     kickTestToken,
		TokenEnv:  "HIVE_KICK_TEST_TOKEN",
		Wait:      10 * time.Second,
		Stdout:    &stdout,
	}
	codeCh := make(chan int, 1)
	go func() { codeCh <- k.Serve(context.Background(), ln) }()
	base := "http://" + ln.Addr().String()

	if resp := kickRequest(t, http.MethodGet, base+PathHealth, "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d, want 200 without a token", resp.StatusCode)
	}
	if resp := kickRequest(t, http.MethodPut, base+PathWorkspace, "wrong", packed.Bytes()); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("push with a wrong token = %d, want 401", resp.StatusCode)
	}
	if resp := kickRequest(t, http.MethodGet, base+PathWorkspace, kickTestToken, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("pull before the run = %d, want 409", resp.StatusCode)
	}
	if resp := kickRequest(t, http.MethodPut, base+PathWorkspace, kickTestToken, packed.Bytes()); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("push = %d, want 204", resp.StatusCode)
	}
	var resp *http.Response
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp = kickRequest(t, http.MethodGet, base+PathWorkspace, kickTestToken, nil)
		if resp.StatusCode != http.StatusConflict {
			break
		}
		resp.Body.Close()
		if time.Now().After(deadline) {
			t.Fatal("pull still 409: the run has not finished")
		}
		<-time.After(10 * time.Millisecond)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pull = %d, want 200", resp.StatusCode)
	}
	back := t.TempDir()
	if err := Unpack(resp.Body, back); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got, _ := os.ReadFile(filepath.Join(back, "out.txt")); string(got) != "hello" {
		t.Errorf("pulled out.txt = %q, want the command's write", got)
	}
	if r := kickRequest(t, http.MethodPost, base+PathDone, kickTestToken, nil); r.StatusCode != http.StatusNoContent {
		t.Fatalf("done = %d, want 204", r.StatusCode)
	}
	select {
	case code := <-codeCh:
		if code != 3 {
			t.Errorf("exit code = %d, want the command's 3", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after /done")
	}
	if !strings.Contains(stdout.String(), "token=unset") {
		t.Errorf("command stdout = %q, want the token removed from its env", stdout.String())
	}
}

func TestKickServerGivesUpWithoutAPush(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	k := &KickServer{Workspace: t.TempDir(), Command: []string{"true"}, Token: kickTestToken, Wait: 20 * time.Millisecond}
	if code := k.Serve(context.Background(), ln); code != ExitNoWorkspace {
		t.Errorf("exit code = %d, want ExitNoWorkspace", code)
	}
}

// TestKickServerStopEndsTheCommandAndAllowsThePull (#19694 review): after a
// kick timeout hive stops the command, then pulls what it left behind.
func TestKickServerStopEndsTheCommandAndAllowsThePull(t *testing.T) {
	work := t.TempDir()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "in.txt"), "hello")
	var packed bytes.Buffer
	if err := Pack(src, &packed); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	k := &KickServer{
		Workspace: work,
		Command:   []string{"sh", "-c", `echo partial > "$0/partial.txt"; sleep 60`, work},
		Token:     kickTestToken,
		Wait:      10 * time.Second,
	}
	codeCh := make(chan int, 1)
	go func() { codeCh <- k.Serve(context.Background(), ln) }()
	base := "http://" + ln.Addr().String()
	if resp := kickRequest(t, http.MethodPut, base+PathWorkspace, kickTestToken, packed.Bytes()); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("push = %d, want 204", resp.StatusCode)
	}
	// Wait for the command to write partial.txt before stopping it; stopping
	// right after the push can kill sh before its echo runs (crux#164).
	partialDeadline := time.Now().Add(10 * time.Second)
	for {
		if got, _ := os.ReadFile(filepath.Join(work, "partial.txt")); string(got) == "partial\n" {
			break
		}
		if time.Now().After(partialDeadline) {
			t.Fatal("the command did not write partial.txt before the stop")
		}
		<-time.After(10 * time.Millisecond)
	}
	if resp := kickRequest(t, http.MethodPost, base+PathStop, "wrong", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stop with a wrong token = %d, want 401", resp.StatusCode)
	}
	if resp := kickRequest(t, http.MethodPost, base+PathStop, kickTestToken, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("stop = %d, want 204", resp.StatusCode)
	}
	deadline := time.Now().Add(10 * time.Second)
	var resp *http.Response
	for {
		resp = kickRequest(t, http.MethodGet, base+PathWorkspace, kickTestToken, nil)
		if resp.StatusCode != http.StatusConflict {
			break
		}
		resp.Body.Close()
		if time.Now().After(deadline) {
			t.Fatal("pull still 409 after /stop: the command was not stopped")
		}
		<-time.After(10 * time.Millisecond)
	}
	back := t.TempDir()
	if err := Unpack(resp.Body, back); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got, _ := os.ReadFile(filepath.Join(back, "partial.txt")); string(got) != "partial\n" {
		t.Errorf("pulled partial.txt = %q, want the work done before the stop", got)
	}
	kickRequest(t, http.MethodPost, base+PathDone, kickTestToken, nil)
	select {
	case <-codeCh:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after /done")
	}
}
