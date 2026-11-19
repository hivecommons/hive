// Package meetllm is `hive meeting-llm`, the one-shot model command
// Spektacular live mode calls (SPEK_MEETING_LLM=command,
// hivecommons/hive#18291). It reads a prompt on stdin, makes one
// OpenAI-compatible chat completion with the endpoint, key and model hive
// resolved for meetings, prints the reply on stdout, and charges the tokens
// to the meeting.
//
// Everything arrives in the environment hive gives the Spek child, never in
// argv (the key must not show in a process listing):
//
//	HIVE_MEETING_LLM_ENDPOINT     OpenAI-compatible base URL
//	HIVE_MEETING_LLM_MODEL        model name
//	HIVE_MEETING_LLM_API_KEY      bearer key (optional)
//	HIVE_MEETING_LLM_TIMEOUT_MS   per-call timeout
//	HIVE_MEETING_LLM_USAGE_FILE   per-meeting usage ledger (JSON lines)
//	HIVE_MEETING_LLM_MAX_TOKENS   per-meeting token budget (0 = none)
//	HIVE_MEETING_LLM_SLOTS_DIR    directory of in-flight slot lock files
//	HIVE_MEETING_LLM_MAX_IN_FLIGHT concurrent calls across meetings
//	HIVE_MEETING_LLM_BACKEND      agent backend (claude) instead of an endpoint
//	HIVE_MEETING_LLM_CLI_BINARY   that backend's CLI (default: its name)
//	HIVE_MEETING_LLM_CLI_HOME     HOME holding the CLI's sign-in
//	HIVE_MEETING_LLM_CA_BUNDLE    PEM file of private CAs for the endpoint
//
// With HIVE_MEETING_LLM_BACKEND set the call runs that backend's CLI once
// in its no-tools completion profile (cli.go, hivecommons/hive#18517,
// #18549) and no endpoint is needed. The CLI's sign-in variables
// (CLAUDE_CODE_OAUTH_TOKEN, COPILOT_GITHUB_TOKEN, OMP_AUTH_BROKER_*) pass
// through from this environment when set.
//
// Usage ledger: one line per call, {"at":"<RFC 3339>","tokens":N}, appended
// to the meeting's usage file. The live loop sums it for the panel and the
// hive-wide hourly/daily ceilings; this command refuses (exit 1, empty
// stdout, so Spek falls back to its rules) once the per-meeting sum reaches
// the budget.
package meetllm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Subcommand is the hive CLI verb.
const Subcommand = "meeting-llm"

// Environment variable names.
const (
	EnvEndpoint    = "HIVE_MEETING_LLM_ENDPOINT"
	EnvModel       = "HIVE_MEETING_LLM_MODEL"
	EnvAPIKey      = "HIVE_MEETING_LLM_API_KEY"
	EnvTimeoutMS   = "HIVE_MEETING_LLM_TIMEOUT_MS"
	EnvUsageFile   = "HIVE_MEETING_LLM_USAGE_FILE"
	EnvMaxTokens   = "HIVE_MEETING_LLM_MAX_TOKENS"
	EnvSlotsDir    = "HIVE_MEETING_LLM_SLOTS_DIR"
	EnvMaxInFlight = "HIVE_MEETING_LLM_MAX_IN_FLIGHT"
	EnvBackend     = "HIVE_MEETING_LLM_BACKEND"
	EnvCLIBinary   = "HIVE_MEETING_LLM_CLI_BINARY"
	EnvCLIHome     = "HIVE_MEETING_LLM_CLI_HOME"
	EnvCABundle    = "HIVE_MEETING_LLM_CA_BUNDLE"
)

const (
	defaultTimeoutMS = 2500
	// maxPromptBytes bounds the prompt read from stdin.
	maxPromptBytes = 1 << 20
	// maxResponseBytes bounds the model response.
	maxResponseBytes = 1 << 20
	// maxReplyTokens bounds the reply length.
	maxReplyTokens = 512
	// slotPoll is how often a waiting call retries the slot locks.
	slotPoll      = 20 * time.Millisecond
	usageFileMode = 0o600
	slotFileMode  = 0o600
	slotDirMode   = 0o700
)

// ErrBudget is returned when the meeting's token budget is spent.
var ErrBudget = errors.New("meeting-llm: meeting token budget spent")

// Config is the resolved environment.
type Config struct {
	Endpoint    string
	Model       string
	APIKey      string
	Timeout     time.Duration
	UsageFile   string
	MaxTokens   int
	SlotsDir    string
	MaxInFlight int
	// System is an optional system prompt sent before the user prompt. The
	// `hive meeting-llm` command leaves it empty (Spek's prompt arrives
	// whole on stdin); in-process callers such as the meeting persona set it.
	System string
	// Provider is "" (OpenAI-compatible chat completions),
	// ProviderAnthropic (the Anthropic Messages API, in-process callers
	// only; Endpoint then defaults to AnthropicEndpoint) or
	// ProviderAgentBackend (Backend's CLI completion profile).
	Provider string
	// Backend is the agent backend on ProviderAgentBackend.
	Backend string
	// CLIBinary is the backend's CLI; empty runs the backend's name.
	CLIBinary string
	// CLIHome is the HOME the CLI runs with; empty inherits HOME.
	CLIHome string
	// RecycleTurns is how many turns a warm session answers before it is
	// replaced; zero means DefaultRecycleTurns.
	RecycleTurns int
	// Session is the meeting's warm CLI. Nil on ProviderAgentBackend runs
	// one CLI per call. Pool, when set, takes its place.
	Session *CLISession
	// Pool is the meeting's set of warm CLIs, so several turns run at once
	// (hivecommons/hive#18590).
	Pool *CLIPool
	// Gate bounds the meeting's concurrent calls and reserves budget for
	// the ones in flight; nil admits every call (the one-shot command).
	Gate *Gate
	// spent receives the tokens a call charged, for the gate.
	spent *int
	// Getenv reads the environment the CLI inherits from; nil is os.Getenv.
	Getenv func(string) string
	// CABundle is an optional PEM file of private CAs the endpoint is
	// verified against, added to the system roots (`hive meeting-llm`).
	CABundle string
}

func envInt(getenv func(string) string, name string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(getenv(name))); err == nil && n > 0 {
		return n
	}
	return def
}

// FromEnv reads the configuration.
func FromEnv(getenv func(string) string) (Config, error) {
	c := Config{
		Endpoint:    strings.TrimRight(strings.TrimSpace(getenv(EnvEndpoint)), "/"),
		Model:       strings.TrimSpace(getenv(EnvModel)),
		APIKey:      strings.TrimSpace(getenv(EnvAPIKey)),
		Timeout:     time.Duration(envInt(getenv, EnvTimeoutMS, defaultTimeoutMS)) * time.Millisecond,
		UsageFile:   strings.TrimSpace(getenv(EnvUsageFile)),
		MaxTokens:   envInt(getenv, EnvMaxTokens, 0),
		SlotsDir:    strings.TrimSpace(getenv(EnvSlotsDir)),
		MaxInFlight: envInt(getenv, EnvMaxInFlight, 1),
		Backend:     strings.ToLower(strings.TrimSpace(getenv(EnvBackend))),
		CLIBinary:   strings.TrimSpace(getenv(EnvCLIBinary)),
		CLIHome:     strings.TrimSpace(getenv(EnvCLIHome)),
		Getenv:      getenv,
		CABundle:    strings.TrimSpace(getenv(EnvCABundle)),
	}
	if c.Backend != "" {
		c.Provider = ProviderAgentBackend
		p, err := profileFor(c.Backend)
		if err != nil {
			return c, err
		}
		if p.requiresModel && c.Model == "" {
			return c, fmt.Errorf("meeting-llm: %s is required with %s", EnvModel, EnvBackend)
		}
		return c, nil
	}
	if c.Endpoint == "" || c.Model == "" {
		return c, fmt.Errorf("meeting-llm: %s and %s are required", EnvEndpoint, EnvModel)
	}
	return c, nil
}

// UsageTotal sums the tokens recorded in a usage file. A missing file is
// zero.
func UsageTotal(path string) (int, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	total := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var u struct {
			Tokens int `json:"tokens"`
		}
		if json.Unmarshal(sc.Bytes(), &u) == nil && u.Tokens > 0 {
			total += u.Tokens
		}
	}
	return total, sc.Err()
}

// charge records a call's tokens in the ledger and reports them to the
// meeting's gate.
func (c Config) charge(tokens int, now time.Time) error {
	if c.spent != nil && tokens > 0 {
		*c.spent += tokens
	}
	return recordUsage(c.UsageFile, tokens, now)
}

func recordUsage(path string, tokens int, now time.Time) error {
	if path == "" || tokens <= 0 {
		return nil
	}
	line, _ := json.Marshal(map[string]any{"at": now.UTC().Format(time.RFC3339), "tokens": tokens})
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, usageFileMode)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// acquire takes one of n slot locks in dir, waiting until ctx ends. It
// returns the release func.
func acquire(ctx context.Context, dir string, n int) (func(), error) {
	if dir == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(dir, slotDirMode); err != nil {
		return nil, err
	}
	for {
		for i := 0; i < n; i++ {
			f, err := os.OpenFile(filepath.Join(dir, "slot-"+strconv.Itoa(i)), os.O_CREATE|os.O_RDWR, slotFileMode)
			if err != nil {
				return nil, err
			}
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
				return func() {
					_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
					_ = f.Close()
				}, nil
			}
			_ = f.Close()
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("meeting-llm: no free model slot: %w", ctx.Err())
		case <-time.After(slotPoll):
		}
	}
}

type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func chatURL(endpoint string) string {
	if strings.HasSuffix(endpoint, "/chat/completions") {
		return endpoint
	}
	if strings.HasSuffix(endpoint, "/v1") {
		return endpoint + "/chat/completions"
	}
	return endpoint + "/v1/chat/completions"
}

// Complete makes one call and records its usage. It refuses once the
// meeting's budget is spent. With a Gate, the call first waits for one of
// the meeting's places and reserves budget against the calls still in
// flight, so concurrent answers cannot overspend it.
func Complete(ctx context.Context, c Config, hc *http.Client, prompt string) (string, error) {
	used, err := c.used()
	if err != nil {
		return "", err
	}
	if c.MaxTokens > 0 && used >= c.MaxTokens {
		return "", ErrBudget
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	if c.Gate != nil {
		if err := c.Gate.enter(ctx); err != nil {
			return "", err
		}
		defer c.Gate.leave()
		// Calls that finished while this one waited are in the ledger now.
		if used, err = c.used(); err != nil {
			return "", err
		}
		held, err := c.Gate.reserve(used, c.MaxTokens)
		if err != nil {
			return "", err
		}
		var spent int
		c.spent = &spent
		defer func() { c.Gate.settle(held, spent) }()
	}
	return complete(ctx, c, hc, prompt)
}

// used is the meeting's ledger total when it has a budget.
func (c Config) used() (int, error) {
	if c.MaxTokens <= 0 || c.UsageFile == "" {
		return 0, nil
	}
	return UsageTotal(c.UsageFile)
}

// complete is one call once admitted.
func complete(ctx context.Context, c Config, hc *http.Client, prompt string) (string, error) {
	if c.Provider == ProviderAgentBackend && c.Pool != nil {
		return completePool(ctx, c, prompt)
	}
	if c.Provider == ProviderAgentBackend && c.Session != nil {
		return completeSession(ctx, c, prompt)
	}
	release, err := acquire(ctx, c.SlotsDir, c.MaxInFlight)
	if err != nil {
		return "", err
	}
	defer release()
	switch c.Provider {
	case ProviderAnthropic:
		return completeAnthropic(ctx, c, hc, prompt)
	case ProviderAgentBackend:
		return completeCLIOnce(ctx, c, prompt)
	}
	msgs := make([]chatMessage, 0, 2)
	if strings.TrimSpace(c.System) != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: c.System})
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: prompt})
	body, _ := json.Marshal(chatRequest{Model: c.Model, Messages: msgs, MaxTokens: maxReplyTokens})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatURL(c.Endpoint), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("meeting-llm: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("meeting-llm: model returned %d", resp.StatusCode)
	}
	var cr chatResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&cr); err != nil {
		return "", fmt.Errorf("meeting-llm: decoding response: %w", err)
	}
	tokens := cr.Usage.TotalTokens
	if tokens == 0 {
		tokens = cr.Usage.PromptTokens + cr.Usage.CompletionTokens
	}
	if err := c.charge(tokens, time.Now()); err != nil {
		return "", fmt.Errorf("meeting-llm: recording usage: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", errors.New("meeting-llm: model returned no choices")
	}
	return cr.Choices[0].Message.Content, nil
}

// Run is the subcommand entry point. Errors go to stderr without the key;
// stdout carries only the reply.
func Run(_ []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	c, err := FromEnv(getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	prompt, err := io.ReadAll(io.LimitReader(stdin, maxPromptBytes))
	if err != nil {
		fmt.Fprintln(stderr, "meeting-llm: reading prompt:", err)
		return 1
	}
	hc, err := HTTPClient(c.CABundle)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	reply, err := Complete(context.Background(), c, hc, string(prompt))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprint(stdout, reply)
	return 0
}

// HTTPClient is the client for an OpenAI-compatible endpoint: the default
// one, or, with caBundle set, one that also trusts the PEM file's CAs (a
// model gateway's ca_bundle). Verification is never turned off.
func HTTPClient(caBundle string) (*http.Client, error) {
	if strings.TrimSpace(caBundle) == "" {
		return &http.Client{}, nil
	}
	pem, err := os.ReadFile(caBundle)
	if err != nil {
		return nil, fmt.Errorf("meeting-llm: reading CA bundle: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("meeting-llm: CA bundle holds no PEM certificate")
	}
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if def, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = def.Clone()
	}
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: tr}, nil
}
