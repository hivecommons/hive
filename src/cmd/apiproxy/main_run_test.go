package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests exercise main() itself — flag wiring, the event-log encoder
// closure (the body-redaction contract), and the fail-closed startup exits —
// rather than the already-tested env helpers.
//
// The happy paths run main() in-process on a goroutine so its statements are
// visible to the coverage profile; the log.Fatalf lanes re-exec the test
// binary (the same pattern as cmd/hive-backup) so os.Exit is observable.

// freeLoopbackPort reserves an ephemeral loopback port and releases it for
// main() to bind. The tiny reuse race is acceptable in tests.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// startMain runs main() on a goroutine with the given argv and waits until the
// listener accepts connections. The goroutine leaks for the remainder of the
// test binary's life — ListenAndServe has no shutdown lane — which is why each
// caller uses its own port. flag.CommandLine is replaced because the test
// binary's own flags have already been parsed on the real one.
func startMain(t *testing.T, args ...string) {
	t.Helper()
	oldArgs := os.Args
	oldFlags := flag.CommandLine
	t.Cleanup(func() {
		os.Args = oldArgs
		flag.CommandLine = oldFlags
	})
	os.Args = append([]string{"apiproxy"}, args...)
	flag.CommandLine = flag.NewFlagSet("apiproxy", flag.ExitOnError)
	go main()
}

func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("proxy never started listening on %s", addr)
}

// logEntry mirrors the anonymous struct main()'s handler encodes.
type logEntry struct {
	Timestamp string          `json:"ts"`
	Agent     string          `json:"agent"`
	Direction string          `json:"direction"`
	Method    string          `json:"method"`
	Path      string          `json:"path"`
	Status    int             `json:"status"`
	Model     string          `json:"model"`
	SSEType   string          `json:"sse_type"`
	BodySize  int             `json:"body_size"`
	Body      json.RawMessage `json:"body"`
}

func readLogEntries(t *testing.T, path string) []logEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening event log: %v", err)
	}
	defer func() { _ = f.Close() }()
	var entries []logEntry
	dec := json.NewDecoder(f)
	for {
		var e logEntry
		if err := dec.Decode(&e); err == io.EOF {
			return entries
		} else if err != nil {
			t.Fatalf("decoding event log: %v", err)
		}
		entries = append(entries, e)
	}
}

// waitForEntries polls the log file until at least n entries are present; the
// SSE handler emits from a goroutine after the client has read the stream.
func waitForEntries(t *testing.T, path string, n int) []logEntry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries := readLogEntries(t, path)
		if len(entries) >= n || time.Now().After(deadline) {
			return entries
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMainServesAndRedactsNonSSEBodiesInFileLog(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"claude-test","content":[{"type":"text","text":"secret completion"}]}`)
	}))
	defer upstream.Close()

	t.Setenv("PROXY_AUTH_TOKEN", "gate-token")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-upstream")

	logPath := filepath.Join(t.TempDir(), "events.log")
	port := freeLoopbackPort(t)
	startMain(t,
		"-port", fmt.Sprint(port),
		"-upstream", upstream.URL,
		"-log", logPath,
	)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	waitForListener(t, addr)

	reqBody := `{"model":"claude-test","messages":[{"role":"user","content":"secret prompt"}]}`
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/messages", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer gate-token")
	req.Header.Set("X-Hive-Agent", "quality")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxied request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "secret completion") {
		t.Fatalf("upstream body not relayed to client: %s", body)
	}

	entries := waitForEntries(t, logPath, 2)
	if len(entries) != 2 {
		t.Fatalf("got %d log entries, want request+response: %+v", len(entries), entries)
	}

	reqEvt, respEvt := entries[0], entries[1]
	if reqEvt.Direction != "request" || respEvt.Direction != "response" {
		t.Fatalf("directions = %q, %q; want request, response", reqEvt.Direction, respEvt.Direction)
	}
	if reqEvt.Method != http.MethodPost || reqEvt.Path != "/v1/messages" {
		t.Errorf("request entry = %s %s, want POST /v1/messages", reqEvt.Method, reqEvt.Path)
	}
	if reqEvt.Agent != "quality" {
		t.Errorf("request agent = %q, want X-Hive-Agent value", reqEvt.Agent)
	}
	if reqEvt.Model != "claude-test" || respEvt.Model != "claude-test" {
		t.Errorf("models = %q, %q; want claude-test in both", reqEvt.Model, respEvt.Model)
	}
	if respEvt.Status != http.StatusOK {
		t.Errorf("response status = %d, want 200", respEvt.Status)
	}
	if _, err := time.Parse(time.RFC3339, reqEvt.Timestamp); err != nil {
		t.Errorf("timestamp %q is not RFC3339: %v", reqEvt.Timestamp, err)
	}

	// The redaction contract: non-SSE entries carry body_size only — the
	// message content (prompts, completions) must never land in the log file.
	for _, e := range entries {
		if len(e.Body) != 0 {
			t.Errorf("%s entry logged a body (%s); non-SSE bodies must be size-only", e.Direction, e.Body)
		}
		if e.BodySize == 0 {
			t.Errorf("%s entry body_size = 0, want the observed payload size", e.Direction)
		}
	}
}

func TestMainWritesSSEEventBodiesToStdoutLog(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"model":"claude-sse","usage":{"input_tokens":3}}}`,
		``,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n") + "\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, stream)
	}))
	defer upstream.Close()

	t.Setenv("PROXY_AUTH_TOKEN", "gate-token")
	t.Setenv("ANTHROPIC_API_KEY", "")

	// Cover the stdout lane of main(): swap os.Stdout for a file and read the
	// encoder's output back from it.
	stdoutPath := filepath.Join(t.TempDir(), "stdout.log")
	f, err := os.Create(stdoutPath)
	if err != nil {
		t.Fatalf("creating stdout capture file: %v", err)
	}
	oldStdout := os.Stdout
	os.Stdout = f
	t.Cleanup(func() {
		os.Stdout = oldStdout
		_ = f.Close()
	})

	port := freeLoopbackPort(t)
	startMain(t,
		"-port", fmt.Sprint(port),
		"-upstream", upstream.URL,
	)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	waitForListener(t, addr)
	// main() has read os.Stdout by now; later tests' output must not land in
	// the capture file, but the leaked server goroutine keeps its encoder.
	os.Stdout = oldStdout

	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/messages", strings.NewReader(`{"model":"claude-sse"}`))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("X-Api-Key", "gate-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxied request failed: %v", err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("draining SSE stream: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// request + message_start + message_delta + message_stop (content deltas
	// are aggregated into the stop summary, not logged individually).
	entries := waitForEntries(t, stdoutPath, 4)
	bySSEType := map[string]logEntry{}
	for _, e := range entries {
		bySSEType[e.SSEType] = e
	}

	for _, typ := range []string{"message_start", "message_delta", "message_stop"} {
		e, ok := bySSEType[typ]
		if !ok {
			t.Fatalf("no %s entry in stdout log; got %+v", typ, entries)
		}
		if e.Direction != "sse" {
			t.Errorf("%s direction = %q, want sse", typ, e.Direction)
		}
		// SSE entries are the one lane where main()'s handler includes the
		// body: they carry usage/summary metadata rather than raw payloads.
		if len(e.Body) == 0 {
			t.Errorf("%s entry has no body; SSE events must include their data payload", typ)
		}
		if e.Model != "claude-sse" {
			t.Errorf("%s model = %q, want claude-sse", typ, e.Model)
		}
	}
	if stop := bySSEType["message_stop"]; !strings.Contains(string(stop.Body), "stream_complete") {
		t.Errorf("message_stop body = %s, want stream_complete summary", stop.Body)
	}
	if reqEvt, ok := bySSEType[""]; !ok || reqEvt.Direction != "request" {
		t.Errorf("missing plain request entry in stdout log: %+v", entries)
	} else if len(reqEvt.Body) != 0 {
		t.Errorf("request entry logged a body (%s); must be size-only", reqEvt.Body)
	}
}

// --- fatal-exit contract (re-exec the test binary so os.Exit is observable) ---

const mainArgsSep = "\x1f"

// TestHelperRunMain is not a real test: it becomes the apiproxy process when
// re-exec'd by runMainExpectingFatal.
func TestHelperRunMain(t *testing.T) {
	if os.Getenv("APIPROXY_TEST_RUN_MAIN") != "1" {
		t.Skip("helper process for exit-code tests")
	}
	args := []string{"apiproxy"}
	if raw := os.Getenv("APIPROXY_TEST_ARGS"); raw != "" {
		args = append(args, strings.Split(raw, mainArgsSep)...)
	}
	os.Args = args
	flag.CommandLine = flag.NewFlagSet("apiproxy", flag.ExitOnError)
	main()
}

func runMainExpectingFatal(t *testing.T, env map[string]string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", "TestHelperRunMain")
	cmd.Env = append(os.Environ(),
		"APIPROXY_TEST_RUN_MAIN=1",
		"APIPROXY_TEST_ARGS="+strings.Join(args, mainArgsSep),
		// No ambient credentials leak in; tests that need them set them.
		"PROXY_AUTH_TOKEN=",
		"ANTHROPIC_API_KEY=",
	)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), string(out)
	}
	t.Fatalf("re-exec failed to run: %v\n%s", err, out)
	return -1, ""
}

// The fail-closed security contract: without PROXY_AUTH_TOKEN the process must
// refuse to start, not fall back to an unauthenticated relay.
func TestMainExitsWhenClientAuthTokenMissing(t *testing.T) {
	code, out := runMainExpectingFatal(t,
		map[string]string{"ANTHROPIC_API_KEY": "sk-ant"},
		"-port", "0",
	)
	if code == 0 {
		t.Fatalf("main exited 0 without PROXY_AUTH_TOKEN; must fail closed\n%s", out)
	}
	if !strings.Contains(out, "PROXY_AUTH_TOKEN") {
		t.Errorf("fatal output %q must name the missing variable", out)
	}
	if !strings.Contains(out, "refusing to start") {
		t.Errorf("fatal output %q must state the refusal", out)
	}
}

func TestMainExitsOnUnwritableLogFile(t *testing.T) {
	code, out := runMainExpectingFatal(t,
		map[string]string{"PROXY_AUTH_TOKEN": "gate"},
		"-port", "0", "-log", t.TempDir(), // a directory is not openable as a file
	)
	if code == 0 {
		t.Fatalf("main exited 0 with unwritable -log path\n%s", out)
	}
	if !strings.Contains(out, "failed to open log file") {
		t.Errorf("fatal output %q must report the log-file failure", out)
	}
}

func TestMainExitsOnInvalidUpstreamURL(t *testing.T) {
	code, out := runMainExpectingFatal(t,
		map[string]string{"PROXY_AUTH_TOKEN": "gate"},
		"-port", "0", "-upstream", "://not-a-url",
	)
	if code == 0 {
		t.Fatalf("main exited 0 with invalid -upstream URL\n%s", out)
	}
	if !strings.Contains(out, "failed to create proxy") {
		t.Errorf("fatal output %q must report the proxy construction failure", out)
	}
}

func TestMainExitsWhenListenAddressUnavailable(t *testing.T) {
	// Hold the port so main()'s ListenAndServe fails immediately.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving port: %v", err)
	}
	defer func() { _ = l.Close() }()
	port := l.Addr().(*net.TCPAddr).Port

	code, out := runMainExpectingFatal(t,
		map[string]string{"PROXY_AUTH_TOKEN": "gate"},
		"-port", fmt.Sprint(port),
	)
	if code == 0 {
		t.Fatalf("main exited 0 when its port was already bound\n%s", out)
	}
	if !strings.Contains(out, "server error") {
		t.Errorf("fatal output %q must report the listen failure", out)
	}
}
