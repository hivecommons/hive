// Package cline adapts the official Cline CLI, not an editor extension or SDK.
package cline

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	Subcommand    = "cline-turn"
	ReadyMarker   = "HIVE_CLINE_READY"
	RunningMarker = "HIVE_CLINE_RUNNING"
	// DefaultTimeout bounds each coding turn independently of the scheduler.
	DefaultTimeout = 30 * time.Minute
	probeTimeout   = 10 * time.Second
	cancelGrace    = 5 * time.Second
	maxEventBytes  = 4 << 20
)

// Selection requires an explicit provider; model IDs may themselves contain /.
func Selection(model string) (provider, id string, err error) {
	provider, id, ok := strings.Cut(model, "/")
	if !ok || provider == "" || id == "" || strings.ContainsAny(provider, " \t\r\n") {
		return "", "", errors.New("Cline requires model provider/model (for example openrouter/anthropic/claude-sonnet-4-6); authenticate that provider with cline auth")
	}
	return provider, id, nil
}

// Probe checks capabilities rather than assuming old editor-dependent CLIs work.
func Probe(ctx context.Context, binary string) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--help")
	cmd.Env = workerEnv(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		return errors.New("Cline CLI unavailable: install npm install -g cline@3.0.74 and run cline --help")
	}
	for _, capability := range []string{"--json", "--provider", "--model", "--cwd", "--config", "--data-dir", "--timeout", "--auto-approve", "--thinking"} {
		if !strings.Contains(string(out), capability) {
			return fmt.Errorf("Cline CLI lacks %s: install cline@3.0.74 or a compatible newer CLI (not the legacy editor CLI)", capability)
		}
	}
	return nil
}

// CheckConfig validates the selected credential schema without invoking a
// provider or returning secret bytes to the caller.
func CheckConfig(configDir, model string) error {
	provider, _, err := Selection(model)
	if err != nil {
		return err
	}
	_, err = selectedCredentials(configDir, provider)
	return err
}

type options struct {
	binary, model, effort, promptFile, cwd, configDir, stateDir, stageConfig, providerFile string
	timeout                                                                                time.Duration
	check, exportConfig, providerSecret                                                    bool
}

// Run is shared by hive/cline-turn and standalone crux/cline-turn. Prompts go
// over stdin, never argv; only provider/model names appear in process listings.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	opts := options{}
	fs := flag.NewFlagSet(Subcommand, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.binary, "cline", "cline", "official Cline CLI binary")
	fs.StringVar(&opts.model, "model", "", "provider/model (required)")
	fs.StringVar(&opts.effort, "thinking", "", "none|low|medium|high|xhigh")
	fs.StringVar(&opts.promptFile, "prompt-file", "-", "prompt file, or - for stdin")
	fs.StringVar(&opts.cwd, "cwd", "", "workspace (default current directory)")
	fs.StringVar(&opts.configDir, "config", os.Getenv("HIVE_CLINE_CONFIG_DIR"), "credential source directory (default ~/.cline)")
	fs.StringVar(&opts.stateDir, "state-dir", "", "private state parent (default ~/.local/state/crux/cline)")
	fs.StringVar(&opts.stageConfig, "stage-config", "", "stage only the selected provider in this directory; no inference")
	fs.DurationVar(&opts.timeout, "timeout", DefaultTimeout, "positive task timeout")
	fs.BoolVar(&opts.check, "check", false, "check CLI capabilities and selected provider config without inference")
	fs.BoolVar(&opts.exportConfig, "export-provider-config", false, "write selected provider credential JSON to stdout for Secret generation (sensitive)")
	fs.BoolVar(&opts.providerSecret, "provider-secret", false, "consume HIVE_CLINE_PROVIDERS_B64 from an operator-selected sandbox Job Secret, without a credential source file")
	fs.StringVar(&opts.providerFile, "provider-file", "", "read-only selected-provider file supplied by the scoped Podman capability")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "cline-turn: unexpected positional arguments; supply the prompt on stdin")
		return 2
	}
	if err := run(opts, stdin, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, "cline-turn:", err)
		return 1
	}
	return 0
}

func run(opts options, stdin io.Reader, stdout, stderr io.Writer) error {
	provider, model, err := Selection(opts.model)
	if err != nil {
		return err
	}
	if opts.timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	if opts.effort != "" && !strings.Contains("|none|low|medium|high|xhigh|", "|"+opts.effort+"|") {
		return errors.New("thinking must be none, low, medium, high or xhigh")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	if err := Probe(ctx, opts.binary); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if opts.configDir == "" && !opts.providerSecret && opts.providerFile == "" {
		opts.configDir = filepath.Join(home, ".cline")
	}
	var credentials []byte
	if opts.providerSecret || opts.providerFile != "" {
		if opts.configDir != "" || opts.stageConfig != "" || opts.exportConfig {
			return errors.New("provider-secret cannot be combined with credential source, staging or export options")
		}
		if opts.providerSecret && opts.providerFile != "" {
			return errors.New("provider-file and provider-secret are mutually exclusive")
		}
		if opts.providerSecret {
			credentials, err = selectedProviderSecret(provider)
		} else {
			if opts.cwd == "" {
				opts.cwd, err = os.Getwd()
				if err != nil {
					return err
				}
			}
			opts.cwd, err = filepath.Abs(opts.cwd)
			if err != nil {
				return err
			}
			if err := secretStateOutsideWorkspace(opts.providerFile, opts.cwd); err != nil {
				return err
			}
			var raw []byte
			raw, err = os.ReadFile(opts.providerFile)
			if err == nil {
				credentials, err = narrowCredentials(raw, provider)
			} else {
				return errors.New("cannot read the mounted selected Cline provider")
			}
		}
	} else {
		credentials, err = selectedCredentials(opts.configDir, provider)
	}
	if err != nil {
		return err
	}
	if opts.exportConfig {
		_, err := stdout.Write(credentials)
		return err
	}
	if opts.stageConfig != "" {
		settings := filepath.Join(opts.stageConfig, "data", "settings")
		if err := os.MkdirAll(settings, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(settings, "providers.json"), credentials, 0o600)
	}
	if opts.check {
		fmt.Fprintln(stdout, "Cline CLI capabilities and selected provider config verified; authentication/inference not tested")
		return nil
	}
	if opts.cwd == "" {
		opts.cwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	opts.cwd, err = filepath.Abs(opts.cwd)
	if err != nil {
		return err
	}
	if opts.stateDir == "" {
		opts.stateDir = filepath.Join(home, ".local", "state", "crux", "cline")
	}
	if err := os.MkdirAll(opts.stateDir, 0o700); err != nil {
		return err
	}
	if opts.providerSecret || opts.providerFile != "" {
		if err := secretStateOutsideWorkspace(opts.stateDir, opts.cwd); err != nil {
			return err
		}
	}
	state, err := os.MkdirTemp(opts.stateDir, "turn-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(state)
	settings := filepath.Join(state, "data", "settings")
	hooks := filepath.Join(state, "hooks")
	if err := os.MkdirAll(settings, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(settings, "providers.json"), credentials, 0o600); err != nil {
		return err
	}
	mcp := filepath.Join(settings, "cline_mcp_settings.json")
	if err := os.WriteFile(mcp, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		return err
	}
	if opts.promptFile != "" && opts.promptFile != "-" {
		file, err := os.Open(opts.promptFile)
		if err != nil {
			return errors.New("cannot read Cline prompt file")
		}
		defer file.Close()
		stdin = file
	}
	cmd := exec.CommandContext(ctx, opts.binary, commandArgs(provider, model, opts, state)...)
	cmd.Dir = opts.cwd
	cmd.Stdin = stdin
	cmd.Stderr = stderr
	// Never attach to a shared hub or inherit a host MCP/plugin/hook override.
	cmd.Env = workerEnv(os.Environ())
	cmd.Env = append(cmd.Env, "CLINE_SESSION_BACKEND_MODE=local", "CLINE_MCP_SETTINGS_PATH="+mcp, "CLINE_HOOKS_DIR="+hooks)
	cmd.WaitDelay = cancelGrace
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	finished := make(chan struct{})
	defer close(finished)
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
		go func() {
			timer := time.NewTimer(cancelGrace)
			defer timer.Stop()
			select {
			case <-timer.C:
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			case <-finished:
			}

		}()
		return err
	}
	stream, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return errors.New("could not start Cline CLI")
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	resultErr := renderUsage(stream, stdout, usageFile(opts.stateDir, filepath.Base(state), opts.model, os.Getenv("HIVE_AGENT")))
	if resultErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return fmt.Errorf("Cline task stopped: %w", ctx.Err())
	}
	if waitErr != nil {
		return fmt.Errorf("Cline CLI failed: %w", waitErr)
	}
	return resultErr
}

// Provider auth comes exclusively from the narrowed file, never another
// provider's ambient key/config. Keep proxy CA/auth and toolchain variables.
func workerEnv(environ []string) []string {
	env := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "CLINE_") || strings.HasPrefix(name, "HIVE_CLINE_") || strings.HasPrefix(name, "AWS_") ||
			strings.HasSuffix(name, "_API_KEY") || strings.HasSuffix(name, "_API_TOKEN") ||
			name == "HIVE_CLINE_PROVIDERS_B64" || name == "DATABRICKS_TOKEN" ||
			name == "KILO_AUTH_CONTENT" || name == "KILO_CONFIG_CONTENT" ||
			name == "GOOGLE_APPLICATION_CREDENTIALS" || name == "AZURE_OPENAI_KEY" {
			continue
		}
		env = append(env, entry)
	}
	return env
}

func commandArgs(provider, model string, opts options, state string) []string {
	timeoutSeconds := max(int64(1), int64(opts.timeout/time.Second))
	args := []string{"--json", "--auto-approve", "true", "--provider", provider, "--model", model,
		"--cwd", opts.cwd, "--config", state, "--data-dir", filepath.Join(state, "data"),
		"--timeout", fmt.Sprint(timeoutSeconds)}
	if opts.effort != "" {
		args = append(args, "--thinking", opts.effort)
	}
	return args
}

func selectedCredentials(configDir, provider string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(configDir, "data", "settings", "providers.json"))
	if err != nil {
		return nil, errors.New("Cline provider config missing; run cline auth under the worker's credential source (HIVE_CLINE_CONFIG_DIR)")
	}
	return narrowCredentials(raw, provider)
}

func narrowCredentials(raw []byte, provider string) ([]byte, error) {
	var stored struct {
		Version   int                        `json:"version"`
		Providers map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil || stored.Version != 1 {
		return nil, errors.New("unsupported Cline providers.json schema; reconfigure with cline@3.0.74")
	}
	entry, ok := stored.Providers[provider]
	if !ok {
		return nil, fmt.Errorf("Cline provider %q is not configured; run cline auth for that provider", provider)
	}
	var auth struct {
		TokenSource string                     `json:"tokenSource"`
		Settings    map[string]json.RawMessage `json:"settings"`
	}
	if err := json.Unmarshal(entry, &auth); err != nil || auth.Settings == nil {
		return nil, errors.New("invalid Cline provider config")
	}
	if auth.TokenSource == "oauth" || containsRefreshToken(entry) {
		return nil, errors.New("Cline OAuth refresh synchronization is not supported by isolated workers; configure a dedicated API-key provider with cline auth")
	}
	return json.Marshal(map[string]any{"version": 1, "lastUsedProvider": provider, "modes": map[string]any{},
		"providers": map[string]json.RawMessage{provider: entry}})
}

func render(stream io.Reader, out io.Writer) error {
	return renderUsage(stream, out, nil)
}

func renderUsage(stream io.Reader, out io.Writer, record func(json.RawMessage) error) error {
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 4096), maxEventBytes)
	completed := false
	for scanner.Scan() {
		var event struct {
			Type         string          `json:"type"`
			Text         string          `json:"text"`
			FinishReason string          `json:"finishReason"`
			Usage        json.RawMessage `json:"usage"`
			Event        json.RawMessage `json:"event"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			return errors.New("invalid Cline NDJSON output")
		}
		switch event.Type {
		case "run_result":
			if record != nil && len(event.Usage) != 0 {
				if err := record(event.Usage); err != nil {
					return fmt.Errorf("recording Cline usage: %w", err)
				}
			}
			if event.FinishReason != "completed" {
				return fmt.Errorf("Cline task did not complete (%s)", event.FinishReason)
			}
			fmt.Fprintln(out, event.Text)
			if len(event.Usage) != 0 {
				fmt.Fprintf(out, "CLINE_USAGE %s\n", event.Usage)
			}
			completed = true
		case "error", "run_aborted":
			return errors.New("Cline reported an error or cancellation; inspect the worker's local stderr")
		case "agent_event":
			var agentEvent struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(event.Event, &agentEvent); err != nil {
				return errors.New("invalid Cline agent event")
			}
			if agentEvent.Type == "usage" && record != nil {
				if err := record(event.Event); err != nil {
					return fmt.Errorf("recording Cline usage: %w", err)
				}
			}
			if agentEvent.Type == "error" {
				return errors.New("Cline agent reported an error")
			}
			// Status events keep observation/trajectory live without leaking tool
			// arguments, reasoning, credentials or the original task prompt.
			fmt.Fprintf(out, "%s %s\n", RunningMarker, agentEvent.Type)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("Cline stream: %w", err)
	}
	if !completed {
		return errors.New("Cline exited without a completed run_result")
	}
	return nil
}
