// Package quorumproc supervises the Quorum child process (hivecommons/hive#18289,
// hivecommons/quorum docs/contract.md, quorum.contract/v1).
//
// Quorum runs inside the hive container as a child of the hive process, the
// same way hive runs the spektacular binary. It listens on loopback only and
// requires a bearer token that hive generates once per boot and hands to the
// child through QUORUM_TOKEN_FILE. Secrets Quorum needs (the Vexa tenant key,
// the optional Discord bot token, the transcription service token and the
// TTS service token) are
// written in plaintext to owner-only files in a private runtime directory
// that is removed on Stop; their durable, keystore-sealed copies stay under
// /data/secrets.
//
// The supervisor starts the child, polls GET /healthz, restarts the child
// with exponential backoff when it exits or stops answering, and stops it on
// shutdown.
package quorumproc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// DefaultNode is the interpreter for a .mjs/.js Quorum bundle.
	DefaultNode = "node"
	// DefaultHealthInterval is how often the supervisor polls /healthz.
	DefaultHealthInterval = 10 * time.Second
	// DefaultHealthTimeout bounds one /healthz request.
	DefaultHealthTimeout = 3 * time.Second
	// DefaultUnhealthyThreshold is how many consecutive failed health checks
	// of a child that was healthy make the supervisor restart it.
	DefaultUnhealthyThreshold = 3
	// DefaultStartupTimeout is how long a new child may take to answer
	// /healthz before it is restarted.
	DefaultStartupTimeout = 60 * time.Second
	// DefaultBackoffMin is the first restart delay.
	DefaultBackoffMin = time.Second
	// DefaultBackoffMax caps the restart delay.
	DefaultBackoffMax = time.Minute
	// DefaultStableAfter is how long a child must stay healthy before the
	// restart backoff resets.
	DefaultStableAfter = 2 * time.Minute
	// DefaultStopGrace is how long a child gets after SIGTERM before it is
	// killed.
	DefaultStopGrace = 10 * time.Second

	tokenBytes      = 32
	secretFileMode  = 0o600
	tokenFileName   = "token"
	vexaKeyFileName = "vexa_api_key"
	discordFileName = "discord_token"
	// transcribeTokenFileName holds the transcription service bearer token
	// (QUORUM_TRANSCRIBE_TOKEN_FILE).
	transcribeTokenFileName = "transcribe_token"
	// ttsTokenFileName holds the TTS service bearer token
	// (QUORUM_TTS_TOKEN_FILE).
	ttsTokenFileName = "tts_token"
)

// passthroughEnv are the parent environment variables the child inherits.
// Everything else (GitHub tokens, hive secrets) is withheld; Quorum gets
// only what the contract names plus what node needs to run and reach the
// network through the pod's proxy.
var passthroughEnv = []string{
	"PATH", "HOME", "TZ", "LANG", "LC_ALL", "TMPDIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "SSL_CERT_DIR",
}

// Options configures a Supervisor.
type Options struct {
	// Binary is the Quorum bundle (.mjs/.js, run through Node) or an
	// executable. Required.
	Binary string
	// Node is the interpreter for a bundle. Empty means DefaultNode.
	Node string
	// Listen is the loopback host:port (QUORUM_LISTEN). Required.
	Listen string
	// VexaURL is QUORUM_VEXA_URL. Empty leaves it unset.
	VexaURL string
	// VexaAPIKey is the plaintext tenant key written to
	// QUORUM_VEXA_API_KEY_FILE. Empty leaves it unset.
	VexaAPIKey string
	// DiscordToken is written to QUORUM_DISCORD_TOKEN_FILE. Empty leaves
	// it unset (Discord tools answer platform_unavailable).
	DiscordToken string
	// TranscribeURL is QUORUM_TRANSCRIBE_URL. Empty leaves it unset.
	TranscribeURL string
	// TranscribeToken is the plaintext transcription service token written
	// to QUORUM_TRANSCRIBE_TOKEN_FILE. Empty leaves it unset.
	TranscribeToken string
	// TTSURL is QUORUM_TTS_URL, the Kokoro service Discord speech uses.
	// Empty leaves it unset.
	TTSURL string
	// TTSToken is the plaintext TTS service token written to
	// QUORUM_TTS_TOKEN_FILE. Empty leaves it unset.
	TTSToken string
	// TTSVoice is QUORUM_TTS_VOICE, the default voice id (the persona's
	// voice). Empty leaves it unset.
	TTSVoice string
	// RuntimeDir is the parent of the private runtime directory. Empty means
	// os.TempDir().
	RuntimeDir string
	// Logger receives lifecycle events and the child's stderr. Nil discards.
	Logger *slog.Logger

	HealthInterval     time.Duration
	HealthTimeout      time.Duration
	UnhealthyThreshold int
	StartupTimeout     time.Duration
	BackoffMin         time.Duration
	BackoffMax         time.Duration
	StableAfter        time.Duration
	StopGrace          time.Duration

	// Command overrides the argv used to start the child (tests).
	Command []string
	// Getenv overrides os.Getenv for the passthrough environment (tests).
	Getenv func(string) string
}

// Supervisor owns one Quorum child.
type Supervisor struct {
	opts   Options
	argv   []string
	token  string
	dir    string
	env    []string
	client *http.Client
	logger *slog.Logger

	mu       sync.Mutex
	healthy  bool
	restarts int
	lastErr  string
	started  bool
	cancel   context.CancelFunc
	done     chan struct{}
	healthCh chan struct{}
}

// Status is a snapshot of the supervisor for the dashboard and logs.
type Status struct {
	Healthy  bool   `json:"healthy"`
	Restarts int    `json:"restarts"`
	LastErr  string `json:"last_error,omitempty"`
	BaseURL  string `json:"base_url"`
}

func durOr(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// New validates opts, generates the per-boot bearer token and writes the
// runtime files. Nothing is started until Start.
func New(opts Options) (*Supervisor, error) {
	if strings.TrimSpace(opts.Listen) == "" {
		return nil, errors.New("quorumproc: listen address is required")
	}
	argv := opts.Command
	if len(argv) == 0 {
		if strings.TrimSpace(opts.Binary) == "" {
			return nil, errors.New("quorumproc: quorum binary is required")
		}
		argv = []string{opts.Binary, "serve"}
		if ext := strings.ToLower(filepath.Ext(opts.Binary)); ext == ".mjs" || ext == ".js" {
			node := opts.Node
			if node == "" {
				node = DefaultNode
			}
			argv = []string{node, opts.Binary, "serve"}
		}
	}
	opts.HealthInterval = durOr(opts.HealthInterval, DefaultHealthInterval)
	opts.HealthTimeout = durOr(opts.HealthTimeout, DefaultHealthTimeout)
	opts.StartupTimeout = durOr(opts.StartupTimeout, DefaultStartupTimeout)
	opts.BackoffMin = durOr(opts.BackoffMin, DefaultBackoffMin)
	opts.BackoffMax = durOr(opts.BackoffMax, DefaultBackoffMax)
	opts.StableAfter = durOr(opts.StableAfter, DefaultStableAfter)
	opts.StopGrace = durOr(opts.StopGrace, DefaultStopGrace)
	if opts.UnhealthyThreshold <= 0 {
		opts.UnhealthyThreshold = DefaultUnhealthyThreshold
	}
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	token, err := newToken()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(opts.RuntimeDir, "hive-quorum-")
	if err != nil {
		return nil, fmt.Errorf("quorumproc: creating runtime dir: %w", err)
	}
	s := &Supervisor{
		opts:     opts,
		argv:     argv,
		token:    token,
		dir:      dir,
		client:   &http.Client{Timeout: opts.HealthTimeout},
		logger:   logger,
		healthCh: make(chan struct{}),
	}
	if err := s.writeRuntimeFiles(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return s, nil
}

func newToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("quorumproc: generating token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// writeRuntimeFiles writes the token and secrets and builds the child env.
func (s *Supervisor) writeRuntimeFiles() error {
	var env []string
	for _, k := range passthroughEnv {
		if v := s.opts.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	// QUORUM_* tunables an operator set on hive (the contract's numeric
	// limits) pass through; the ones below are hive-owned and win.
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "QUORUM_") {
			env = append(env, kv)
		}
	}
	write := func(name, value string) (string, error) {
		path := filepath.Join(s.dir, name)
		if err := os.WriteFile(path, []byte(value), secretFileMode); err != nil {
			return "", fmt.Errorf("quorumproc: writing %s: %w", name, err)
		}
		return path, nil
	}
	tokenPath, err := write(tokenFileName, s.token)
	if err != nil {
		return err
	}
	env = append(env, "QUORUM_LISTEN="+s.opts.Listen, "QUORUM_TOKEN_FILE="+tokenPath)
	if s.opts.VexaURL != "" {
		env = append(env, "QUORUM_VEXA_URL="+s.opts.VexaURL)
	}
	if s.opts.VexaAPIKey != "" {
		p, err := write(vexaKeyFileName, s.opts.VexaAPIKey)
		if err != nil {
			return err
		}
		env = append(env, "QUORUM_VEXA_API_KEY_FILE="+p)
	}
	if s.opts.DiscordToken != "" {
		p, err := write(discordFileName, s.opts.DiscordToken)
		if err != nil {
			return err
		}
		env = append(env, "QUORUM_DISCORD_TOKEN_FILE="+p)
	}
	if s.opts.TranscribeURL != "" {
		env = append(env, "QUORUM_TRANSCRIBE_URL="+s.opts.TranscribeURL)
	}
	if s.opts.TranscribeToken != "" {
		p, err := write(transcribeTokenFileName, s.opts.TranscribeToken)
		if err != nil {
			return err
		}
		env = append(env, "QUORUM_TRANSCRIBE_TOKEN_FILE="+p)
	}
	if s.opts.TTSURL != "" {
		env = append(env, "QUORUM_TTS_URL="+s.opts.TTSURL)
	}
	if s.opts.TTSToken != "" {
		p, err := write(ttsTokenFileName, s.opts.TTSToken)
		if err != nil {
			return err
		}
		env = append(env, "QUORUM_TTS_TOKEN_FILE="+p)
	}
	if s.opts.TTSVoice != "" {
		env = append(env, "QUORUM_TTS_VOICE="+s.opts.TTSVoice)
	}
	s.env = env
	return nil
}

// BaseURL is the Quorum base URL (http://<listen>).
func (s *Supervisor) BaseURL() string { return "http://" + s.opts.Listen }

// Token is the per-boot bearer token. A secret; never log it.
func (s *Supervisor) Token() string { return s.token }

// TokenFile is the path of the 0600 token file.
func (s *Supervisor) TokenFile() string { return filepath.Join(s.dir, tokenFileName) }

// Env returns a copy of the child's environment (tests and diagnostics; it
// holds file paths, never secret values).
func (s *Supervisor) Env() []string { return append([]string(nil), s.env...) }

// Status returns a snapshot.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{Healthy: s.healthy, Restarts: s.restarts, LastErr: s.lastErr, BaseURL: s.BaseURL()}
}

// Healthy reports whether the last health check passed.
func (s *Supervisor) Healthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthy
}

// WaitHealthy blocks until the child first answers /healthz or ctx ends.
func (s *Supervisor) WaitHealthy(ctx context.Context) error {
	select {
	case <-s.healthCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Start launches the supervision loop. It returns at once; the loop runs
// until ctx ends or Stop is called. Start may be called once.
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return errors.New("quorumproc: already started")
	}
	s.started = true
	ctx, s.cancel = context.WithCancel(ctx)
	s.done = make(chan struct{})
	go s.loop(ctx)
	return nil
}

// Stop stops the child and removes the runtime directory. Safe to call more
// than once and before Start.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	_ = os.RemoveAll(s.dir)
}

func (s *Supervisor) setHealthy(ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok && !s.healthy {
		select {
		case <-s.healthCh:
		default:
			close(s.healthCh)
		}
	}
	s.healthy = ok
}

func (s *Supervisor) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErr = err.Error()
}

func (s *Supervisor) loop(ctx context.Context) {
	defer close(s.done)
	backoff := s.opts.BackoffMin
	for {
		healthySince, err := s.runOnce(ctx)
		s.setHealthy(false)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.setErr(err)
		}
		if !healthySince.IsZero() && time.Since(healthySince) >= s.opts.StableAfter {
			backoff = s.opts.BackoffMin
		}
		s.mu.Lock()
		s.restarts++
		s.mu.Unlock()
		s.logger.Warn("[meetings] quorum child stopped; restarting", "error", err, "backoff", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > s.opts.BackoffMax {
			backoff = s.opts.BackoffMax
		}
	}
}

// runOnce starts one child and supervises it until it exits, stops answering
// health checks, or ctx ends. It returns when the child became healthy (zero
// if never) and why it stopped.
func (s *Supervisor) runOnce(ctx context.Context) (time.Time, error) {
	cmd := exec.Command(s.argv[0], s.argv[1:]...)
	cmd.Env = s.env
	cmd.Dir = s.dir
	cmd.Stdout = &logWriter{logger: s.logger, stream: "stdout"}
	cmd.Stderr = &logWriter{logger: s.logger, stream: "stderr"}
	if err := cmd.Start(); err != nil {
		return time.Time{}, fmt.Errorf("starting quorum: %w", err)
	}
	s.logger.Info("[meetings] quorum child started", "pid", cmd.Process.Pid, "listen", s.opts.Listen)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	stop := func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(s.opts.StopGrace):
			_ = cmd.Process.Kill()
			<-exited
		}
	}

	var healthySince time.Time
	failures := 0
	startDeadline := time.Now().Add(s.opts.StartupTimeout)
	tick := time.NewTicker(s.healthPollInterval())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			stop()
			return healthySince, nil
		case err := <-exited:
			return healthySince, fmt.Errorf("quorum exited: %v", err)
		case <-tick.C:
			err := s.check(ctx)
			if err == nil {
				failures = 0
				if healthySince.IsZero() {
					healthySince = time.Now()
					s.logger.Info("[meetings] quorum healthy", "listen", s.opts.Listen)
					tick.Reset(s.opts.HealthInterval)
				}
				s.setHealthy(true)
				continue
			}
			if healthySince.IsZero() {
				if time.Now().After(startDeadline) {
					stop()
					return time.Time{}, fmt.Errorf("quorum not healthy within %s: %w", s.opts.StartupTimeout, err)
				}
				continue
			}
			s.setHealthy(false)
			failures++
			if failures >= s.opts.UnhealthyThreshold {
				stop()
				return healthySince, fmt.Errorf("quorum failed %d health checks: %w", failures, err)
			}
		}
	}
}

// healthPollInterval polls faster while waiting for the first healthy
// answer, so a fresh child is usable quickly.
func (s *Supervisor) healthPollInterval() time.Duration {
	const startupPollDivisor = 10
	return max(s.opts.HealthInterval/startupPollDivisor, time.Millisecond)
}

func (s *Supervisor) check(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL()+"/healthz", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz answered %d", resp.StatusCode)
	}
	return nil
}

// logWriter forwards child output to the logger line by line.
type logWriter struct {
	logger *slog.Logger
	stream string
	mu     sync.Mutex
	buf    []byte
}

// maxLogLine bounds one buffered child output line.
const maxLogLine = 8 << 10

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			if len(w.buf) > maxLogLine {
				w.emit(w.buf)
				w.buf = w.buf[:0]
			}
			return len(p), nil
		}
		w.emit(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
}

func (w *logWriter) emit(line []byte) {
	if s := strings.TrimSpace(string(line)); s != "" {
		w.logger.Info("[meetings] quorum", "stream", w.stream, "line", s)
	}
}
