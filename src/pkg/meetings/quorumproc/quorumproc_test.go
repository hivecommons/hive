package quorumproc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
)

// helperEnv selects the fake Quorum behaviour when the test binary is
// re-executed as the child. It is a QUORUM_* name so the supervisor's
// passthrough hands it to the child.
const helperEnv = "QUORUM_TEST_HELPER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		os.Exit(runHelper(mode))
	}
	os.Exit(m.Run())
}

// runHelper is the fake `quorum serve`: it honours QUORUM_LISTEN and
// QUORUM_TOKEN_FILE the way the contract says the real one does.
func runHelper(mode string) int {
	if mode == "exit" {
		fmt.Fprintln(os.Stderr, "fake quorum: exiting")
		return 1
	}
	if mode == "ignore-term" {
		signal.Ignore(syscall.SIGTERM)
	}
	tok, err := os.ReadFile(os.Getenv("QUORUM_TOKEN_FILE"))
	if err != nil {
		return 2
	}
	want := "Bearer " + string(tok)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if mode == "unhealthy" || mode == "ignore-term" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("Authorization") != want {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprintln(os.Stdout, "healthz ok")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	srv := &http.Server{Addr: os.Getenv("QUORUM_LISTEN"), Handler: mux, ReadHeaderTimeout: time.Second}
	if mode == "ignore-term" {
		// Never exit on SIGTERM, so the supervisor must kill after grace.
		_ = srv.ListenAndServe()
		return 0
	}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM)
		<-sig
		_ = srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return 3
	}
	return 0
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func fastOpts(t *testing.T, mode string) (Options, *syncBuf) {
	t.Helper()
	t.Setenv(helperEnv, mode)
	logs := &syncBuf{}
	return Options{
		Listen:             freeAddr(t),
		Command:            []string{os.Args[0]},
		RuntimeDir:         t.TempDir(),
		Logger:             slog.New(slog.NewTextHandler(logs, nil)),
		HealthInterval:     50 * time.Millisecond,
		HealthTimeout:      time.Second,
		StartupTimeout:     5 * time.Second,
		BackoffMin:         10 * time.Millisecond,
		BackoffMax:         40 * time.Millisecond,
		StableAfter:        time.Hour,
		StopGrace:          time.Second,
		UnhealthyThreshold: 2,
	}, logs
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	testutil.Eventually(t, 10*time.Second, cond, "timed out waiting for %s", what)
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("want an error without a listen address")
	}
	if _, err := New(Options{Listen: "127.0.0.1:1"}); err == nil {
		t.Fatal("want an error without a binary")
	}
	if _, err := New(Options{Listen: "127.0.0.1:1", Binary: "/q", RuntimeDir: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("want an error when the runtime dir cannot be created")
	}
}

func TestNewArgvAndDefaults(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Options{Listen: "127.0.0.1:1", Binary: "/opt/quorum.mjs", RuntimeDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	if got := strings.Join(s.argv, " "); got != "node /opt/quorum.mjs serve" {
		t.Fatalf("argv = %q", got)
	}
	if s.opts.HealthInterval != DefaultHealthInterval || s.opts.UnhealthyThreshold != DefaultUnhealthyThreshold ||
		s.opts.BackoffMax != DefaultBackoffMax || s.opts.StopGrace != DefaultStopGrace {
		t.Fatalf("defaults not applied: %+v", s.opts)
	}
	s2, err := New(Options{Listen: "127.0.0.1:1", Binary: "/opt/q.js", Node: "/usr/bin/node22", RuntimeDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Stop()
	if got := strings.Join(s2.argv, " "); got != "/usr/bin/node22 /opt/q.js serve" {
		t.Fatalf("argv = %q", got)
	}
	s3, err := New(Options{Listen: "127.0.0.1:1", Binary: "/usr/local/bin/quorum", RuntimeDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Stop()
	if got := strings.Join(s3.argv, " "); got != "/usr/local/bin/quorum serve" {
		t.Fatalf("argv = %q", got)
	}
}

func TestRuntimeFilesAndEnv(t *testing.T) {
	t.Setenv("QUORUM_CHAT_MAX_CHARS", "1500")
	env := map[string]string{"PATH": "/bin", "HIVE_GITHUB_TOKEN": "ghp_secret", "HTTPS_PROXY": "http://proxy"}
	s, err := New(Options{
		Listen:          "127.0.0.1:8765",
		Binary:          "/q.mjs",
		VexaURL:         "https://vexa",
		VexaAPIKey:      "tenant",
		DiscordToken:    "discord",
		TranscribeURL:   "http://whisper",
		TranscribeToken: "transcribe-secret",
		TTSURL:          "http://kokoro:8880",
		TTSToken:        "tts-secret",
		TTSVoice:        "af_heart",
		RuntimeDir:      t.TempDir(),
		Getenv:          func(k string) string { return env[k] },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Token()) != 2*tokenBytes {
		t.Fatalf("token length %d", len(s.Token()))
	}
	got := map[string]string{}
	for _, kv := range s.Env() {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	if _, leaked := got["HIVE_GITHUB_TOKEN"]; leaked {
		t.Fatal("hive secrets must not reach the child")
	}
	for k, want := range map[string]string{
		"PATH": "/bin", "HTTPS_PROXY": "http://proxy", "QUORUM_LISTEN": "127.0.0.1:8765",
		"QUORUM_VEXA_URL": "https://vexa", "QUORUM_TRANSCRIBE_URL": "http://whisper", "QUORUM_CHAT_MAX_CHARS": "1500",
		"QUORUM_TTS_URL": "http://kokoro:8880", "QUORUM_TTS_VOICE": "af_heart",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	for envKey, want := range map[string]string{"QUORUM_TOKEN_FILE": s.Token(), "QUORUM_VEXA_API_KEY_FILE": "tenant", "QUORUM_DISCORD_TOKEN_FILE": "discord", "QUORUM_TRANSCRIBE_TOKEN_FILE": "transcribe-secret", "QUORUM_TTS_TOKEN_FILE": "tts-secret"} {
		path := got[envKey]
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != secretFileMode {
			t.Fatalf("%s at %s: %v %v", envKey, path, fi, err)
		}
		b, _ := os.ReadFile(path)
		if string(b) != want {
			t.Errorf("%s holds %q", envKey, b)
		}
		if filepath.Dir(path) != s.dir {
			t.Errorf("%s at %s is outside the private runtime dir %s", envKey, path, s.dir)
		}
	}
	for _, kv := range s.Env() {
		if strings.Contains(kv, "transcribe-secret") || strings.Contains(kv, "tts-secret") {
			t.Fatalf("the transcription token must reach the child only through its file, env has %q", kv)
		}
	}
	if s.TokenFile() != got["QUORUM_TOKEN_FILE"] {
		t.Fatal("TokenFile mismatch")
	}
	if fi, _ := os.Stat(s.dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("runtime dir mode %v", fi.Mode().Perm())
	}
	if s.BaseURL() != "http://127.0.0.1:8765" {
		t.Fatal(s.BaseURL())
	}
	s.Stop()
	if _, err := os.Stat(s.dir); !os.IsNotExist(err) {
		t.Fatal("Stop must remove the runtime dir")
	}
}

func TestOptionalEnvOmitted(t *testing.T) {
	s, err := New(Options{Listen: "127.0.0.1:1", Binary: "/q", RuntimeDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	for _, kv := range s.Env() {
		for _, k := range []string{"QUORUM_VEXA_URL=", "QUORUM_VEXA_API_KEY_FILE=", "QUORUM_DISCORD_TOKEN_FILE=", "QUORUM_TRANSCRIBE_URL=", "QUORUM_TRANSCRIBE_TOKEN_FILE=", "QUORUM_TTS_URL=", "QUORUM_TTS_TOKEN_FILE=", "QUORUM_TTS_VOICE="} {
			if strings.HasPrefix(kv, k) {
				t.Errorf("unset option leaked as %s", kv)
			}
		}
	}
}

func TestSuperviseHealthyThenStop(t *testing.T) {
	opts, logs := fastOpts(t, "serve")
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("a second Start must fail")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.WaitHealthy(ctx); err != nil {
		t.Fatalf("WaitHealthy: %v (logs: %s)", err, logs.String())
	}
	if !s.Healthy() || !s.Status().Healthy {
		t.Fatal("want healthy")
	}
	waitFor(t, "child stdout in logs", func() bool { return strings.Contains(logs.String(), "healthz ok") })
	s.Stop()
	s.Stop()
	if s.Healthy() {
		t.Fatal("stopped supervisor must not report healthy")
	}
	if st := s.Status(); st.Restarts != 0 {
		t.Fatalf("restarts = %d", st.Restarts)
	}
}

func TestSuperviseRestartsAfterExit(t *testing.T) {
	opts, logs := fastOpts(t, "exit")
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "restarts", func() bool { return s.Status().Restarts >= 3 })
	if st := s.Status(); !strings.Contains(st.LastErr, "exited") {
		t.Fatalf("last error %q", st.LastErr)
	}
	waitFor(t, "stderr in logs", func() bool { return strings.Contains(logs.String(), "fake quorum: exiting") })
	cancel()
	s.Stop()
	short, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	if err := s.WaitHealthy(short); err == nil {
		t.Fatal("never-healthy child must time out WaitHealthy")
	}
}

func TestSuperviseStartFailure(t *testing.T) {
	opts, _ := fastOpts(t, "serve")
	opts.Command = []string{filepath.Join(t.TempDir(), "no-such-binary")}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "start error", func() bool { return strings.Contains(s.Status().LastErr, "starting quorum") })
	s.Stop()
}

func TestSuperviseStartupTimeout(t *testing.T) {
	opts, _ := fastOpts(t, "unhealthy")
	opts.StartupTimeout = 200 * time.Millisecond
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "startup timeout", func() bool { return strings.Contains(s.Status().LastErr, "not healthy within") })
	s.Stop()
}

func TestSuperviseKillsChildIgnoringSIGTERM(t *testing.T) {
	opts, _ := fastOpts(t, "ignore-term")
	opts.StartupTimeout = 200 * time.Millisecond
	opts.StopGrace = 100 * time.Millisecond
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "kill after grace", func() bool { return s.Status().Restarts >= 1 })
	s.Stop()
}

// TestSuperviseRestartsUnhealthyChild drives the "was healthy, then stopped
// answering" path against an in-process health server whose answer flips.
func TestSuperviseRestartsUnhealthyChild(t *testing.T) {
	var mu sync.Mutex
	status := http.StatusOK
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(status)
	})}
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()

	opts, _ := fastOpts(t, "")
	opts.Listen = l.Addr().String()
	opts.Command = []string{"sleep", "30"}
	opts.StableAfter = time.Millisecond
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.WaitHealthy(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	status = http.StatusInternalServerError
	mu.Unlock()
	waitFor(t, "unhealthy restart", func() bool {
		return s.Status().Restarts >= 1 && strings.Contains(s.Status().LastErr, "health checks")
	})
	mu.Lock()
	status = http.StatusOK
	mu.Unlock()
	waitFor(t, "healthy again", s.Healthy)
	s.Stop()
}

func TestCheckBadURL(t *testing.T) {
	s := &Supervisor{opts: Options{Listen: "bad host:1"}, client: http.DefaultClient}
	if err := s.check(context.Background()); err == nil {
		t.Fatal("want an error for an unparseable URL")
	}
}

func TestLogWriterSplitsAndCaps(t *testing.T) {
	logs := &syncBuf{}
	w := &logWriter{logger: slog.New(slog.NewTextHandler(logs, nil)), stream: "stderr"}
	_, _ = w.Write([]byte("one\ntw"))
	_, _ = w.Write([]byte("o\n\n"))
	_, _ = w.Write(bytes.Repeat([]byte("x"), maxLogLine+1))
	out := logs.String()
	if !strings.Contains(out, "line=one") || !strings.Contains(out, "line=two") || !strings.Contains(out, "xxxx") {
		t.Fatalf("log output %q", out)
	}
	if len(w.buf) != 0 {
		t.Fatal("oversized partial line must be flushed")
	}
}

func TestStopBeforeStart(t *testing.T) {
	s, err := New(Options{Listen: "127.0.0.1:1", Binary: "/q", RuntimeDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s.Stop()
}
