package repair

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"unicode/utf8"
)

type ProviderResult struct {
	Summary string
	Output  string
}

type Provider interface {
	Name() string
	Health(ctx context.Context) error
	Run(ctx context.Context, worktree, prompt string) (ProviderResult, error)
}

// ProviderRunError records whether the provider process was successfully
// launched. Once launched, a timeout or non-zero exit is an ambiguous model
// invocation and must consume exactly one bounded model ordinal. A launch
// failure is infrastructure and can resume the same uncounted ordinal.
type ProviderRunError struct {
	Launched bool
	Cause    error
}

func (e *ProviderRunError) Error() string { return e.Cause.Error() }
func (e *ProviderRunError) Unwrap() error { return e.Cause }

func providerRunWasLaunched(err error) bool {
	var runErr *ProviderRunError
	if errors.As(err, &runErr) {
		return runErr.Launched
	}
	// Third-party Provider implementations cannot prove a failure happened
	// before launch. Fail closed by charging an invocation that may have run.
	return true
}

func sanitizeProviderRunError(err error) error {
	if err == nil {
		return nil
	}
	rule, confidence := classifyRepairSourceSecret([]byte(err.Error()))
	if confidence == repairSourceSecretNone {
		return err
	}
	return &ProviderRunError{
		Launched: providerRunWasLaunched(err),
		Cause:    fmt.Errorf("provider returned an unsafe error matching %s rule %s", repairSourceSecretRulesetVersion, rule),
	}
}

// CodexProvider invokes a reviewed native Codex executable through its stable
// non-interactive surface. Script and package-runner wrappers are deliberately
// unsupported because Hive cannot seal the eventual executable across Health
// and Run. Prefix is limited to native Codex options.
type CodexProvider struct {
	Command   string
	Prefix    []string
	CodexHome string

	runtimeEnvironment []string
}

// Codex can gain tool surfaces independently of the shell sandbox. Keep this
// fail-closed list explicit and append every override after Prefix. Unknown or
// unavailable feature names must make Codex fail before the model runs rather
// than silently restoring a tool surface.
var codexDisabledToolFeatures = []string{
	"apply_patch_freeform",
	"apply_patch_streaming_events",
	"shell_tool",
	"unified_exec",
	"artifact",
	"apps",
	"auth_elicitation",
	"browser_use",
	"browser_use_external",
	"browser_use_full_cdp_access",
	"computer_use",
	"in_app_browser",
	"code_mode_host",
	"code_mode",
	"code_mode_only",
	"current_time_reminder",
	"default_mode_request_user_input",
	"deferred_executor",
	"enable_mcp_apps",
	"enable_fanout",
	"goals",
	"guardian_approval",
	"js_repl",
	"js_repl_tools_only",
	"memories",
	"multi_agent",
	"multi_agent_v2",
	"plugins",
	"request_permissions_tool",
	"remote_plugin",
	"remote_control",
	"realtime_conversation",
	"shell_snapshot",
	"skill_mcp_dependency_install",
	"tool_suggest",
	"tool_call_mcp_elicitation",
	"standalone_web_search",
	"search_tool",
	"web_search_cached",
	"web_search_request",
	"tool_search",
	"network_proxy",
	"workspace_dependencies",
	"hooks",
	"image_generation",
	"steer",
	"token_budget",
	"unavailable_dummy_tools",
	"undo",
}

func (p CodexProvider) Name() string { return "codex" }

func (p CodexProvider) Health(ctx context.Context) error {
	attestation, err := p.healthAttestation(ctx)
	if err != nil {
		// A failed no-model check must never leave an older authorization for
		// this provider configuration runnable. The specialist child path uses
		// an exact per-request authorization ID; this also keeps the legacy
		// Provider interface fail-closed when callers perform health-only probes.
		forgetCodexProviderAttestation(p)
		return err
	}
	rememberCodexProviderAttestation(p, attestation)
	return nil
}

// healthAttestation performs every no-model check and returns one exact,
// unshared authorization. Callers either enqueue it for the legacy Provider
// API or bind it directly to one child-dispatch reservation.
func (p CodexProvider) healthAttestation(ctx context.Context) (*codexProviderIdentityAttestation, error) {
	if strings.TrimSpace(p.Command) == "" {
		return nil, fmt.Errorf("Codex provider command is required")
	}
	attestation, err := prepareCodexProviderAttestation(p)
	if err != nil {
		return nil, fmt.Errorf("Codex executable identity health check failed before model accounting: %w", err)
	}
	defer attestation.cleanupSeal()
	privateRuntime, err := prepareCodexPrivateRuntime(attestation, p.Command, "")
	if err != nil {
		return nil, fmt.Errorf("prepare private Codex health runtime: %w", err)
	}
	defer privateRuntime.cleanup()
	securedProvider := attestation.provider()
	securedProvider.runtimeEnvironment = privateRuntime.environment
	isolatedCWD, err := os.MkdirTemp("", "hive-codex-provider-health-")
	if err != nil {
		return nil, fmt.Errorf("create isolated Codex health directory: %w", err)
	}
	defer os.RemoveAll(isolatedCWD)
	if err := securedProvider.verifyReviewedCapabilities(ctx, isolatedCWD); err != nil {
		return nil, fmt.Errorf("Codex capability health check failed before model accounting: %w", err)
	}
	// Parse and resolve every mandatory security option without starting a
	// model. `exec --help` is insufficient because Codex exits before validating
	// feature names. An existing directory cannot be read as a JSON output
	// schema, so the exact expected schema-read failure proves configuration was
	// resolved and then stops execution before a model request.
	preflightStop, err := os.MkdirTemp("", "hive-codex-preflight-schema-")
	if err != nil {
		return nil, fmt.Errorf("create Codex tool-isolation preflight stop: %w", err)
	}
	defer os.Remove(preflightStop)
	preflightArgs := securedProvider.securedExecArgs(isolatedCWD, "")
	preflightArgs = append(preflightArgs, "--output-schema", preflightStop, "Hive security-option parse preflight; this prompt must never run.")
	preflight := exec.CommandContext(ctx, securedProvider.Command, preflightArgs...)
	preflight.Dir = isolatedCWD
	preflight.Env = securedProvider.commandEnvironment()
	preflight.Stdin = strings.NewReader("")
	var preflightOutput limitedBuffer
	preflight.Stdout, preflight.Stderr = &preflightOutput, &preflightOutput
	preflightErr := preflight.Run()
	preflightText := preflightOutput.String()
	if preflightErr == nil || !strings.Contains(preflightText, "Failed to read output schema file") || !strings.Contains(preflightText, preflightStop) {
		if preflightErr == nil {
			preflightErr = fmt.Errorf("Codex unexpectedly passed the intentional pre-model stop")
		}
		return nil, fmt.Errorf("Codex tool-isolation preflight failed; install the packaged Codex version that supports every mandatory --disable, --ignore-user-config, --ignore-rules, --config, and --strict-config option: %w: %s", preflightErr, safeExcerpt(preflightText))
	}
	// Keep the deterministic no-model runtime and containment checks before the
	// operator-specific login check. This lets release CI prove the exact native
	// artifact and active platform sandbox without storing model credentials,
	// while Health still refuses to authorize Run until login succeeds.
	if err := verifyCodexPlatformContainmentWithEnvironment(ctx, securedProvider.Command, securedProvider.commandEnvironment()); err != nil {
		return nil, fmt.Errorf("Codex no-model platform containment health check failed before model accounting: %w", err)
	}
	if attestation.Auth.Path == "" && !codexProviderIsTestExecutable(p.Command) {
		return nil, fmt.Errorf("Codex authentication health check failed: no bounded auth.json is available for a private child runtime")
	}
	command := exec.CommandContext(ctx, securedProvider.Command, append(append([]string(nil), securedProvider.Prefix...), "login", "status")...)
	command.Dir = isolatedCWD
	command.Env = securedProvider.commandEnvironment()
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("Codex authentication health check failed: %w: %s", err, safeExcerpt(output.String()))
	}
	if !strings.Contains(strings.ToLower(output.String()), "logged in") {
		return nil, fmt.Errorf("Codex authentication health check did not report a logged-in session")
	}
	if err := attestation.revalidate(ctx); err != nil {
		return nil, fmt.Errorf("Codex executable identity changed during Health: %w", err)
	}
	return attestation.authorizationCopy(), nil
}

func (p CodexProvider) Run(ctx context.Context, _ string, prompt string) (ProviderResult, error) {
	return p.runAttested(ctx, prompt, false)
}

// runStructuredReleaseTest exercises the exact production launch path while
// retaining Codex JSONL events for release-gating assertions. It remains
// unexported and is used only by authenticated cross-platform release tests.
func (p CodexProvider) runStructuredReleaseTest(ctx context.Context, prompt string) (ProviderResult, error) {
	return p.runAttested(ctx, prompt, true)
}

func (p CodexProvider) runAttested(ctx context.Context, prompt string, structured bool) (ProviderResult, error) {
	attestation, err := consumeCodexProviderAttestation(p)
	if err != nil {
		return ProviderResult{}, &ProviderRunError{Launched: false, Cause: err}
	}
	process, err := startCodexAttestedProcess(ctx, p, attestation, prompt, structured, "")
	if err != nil {
		return ProviderResult{}, &ProviderRunError{Launched: false, Cause: err}
	}
	return process.WaitProvider()
}

func (p CodexProvider) securedExecArgs(worktree, terminalArgument string) []string {
	return p.securedExecArgsInternal(worktree, terminalArgument, true)
}

func (p CodexProvider) securedExecArgsInternal(worktree, terminalArgument string, disableTools bool) []string {
	args := append([]string(nil), p.Prefix...)
	// Prefix contains only validated native Codex options and is operator
	// configurable. Keep every security override after it so a prefix that attempts an
	// `--enable` cannot restore filesystem, command, browser, app, or agent tools.
	if disableTools {
		for _, feature := range codexDisabledToolFeatures {
			args = append(args, "--disable", feature)
		}
	}
	args = append(args,
		"--ask-for-approval", "never",
		"exec",
	)
	if strings.TrimSpace(worktree) != "" {
		args = append(args, "--cd", worktree)
	}
	args = append(args,
		"--skip-git-repo-check",
		"--ephemeral", "--ignore-user-config", "--ignore-rules",
	)
	args = append(args, codexNoFilesPermissionArgs()...)
	args = append(args,
		"--config", `web_search="disabled"`,
		"--config", "project_doc_max_bytes=0",
		"--config", "project_doc_fallback_filenames=[]",
		"--strict-config", "--color", "never",
	)
	if terminalArgument != "" {
		args = append(args, terminalArgument)
	}
	return args
}

func codexNoFilesPermissionArgs() []string {
	args := []string{
		"--config", `default_permissions="hive_repair_no_files"`,
		"--config", `permissions.hive_repair_no_files.filesystem.:minimal="read"`,
		"--config", `permissions.hive_repair_no_files.network.enabled=false`,
	}
	if runtime.GOOS == "windows" {
		// Native Windows does not activate filesystem permission profiles unless a
		// Windows sandbox backend is selected. The unelevated backend fails closed
		// for restricted read-only shell requests instead of falling back to the
		// unsandboxed legacy read-only behavior.
		args = append(args, "--config", `windows.sandbox="unelevated"`)
	}
	return args
}

func providerEnvironment() []string {
	result := make([]string, 0, len(os.Environ()))
	for _, pair := range os.Environ() {
		name, _, _ := strings.Cut(pair, "=")
		upperName := strings.ToUpper(strings.TrimSpace(name))
		if blockedEnvironmentName.MatchString(name) || strings.EqualFold(name, "GH_TOKEN") || strings.EqualFold(name, "GITHUB_TOKEN") ||
			strings.HasPrefix(upperName, "HIVE_HOSTED_") || strings.EqualFold(name, "HIVE_RELEASE_CODEX_AUTH_JSON_B64") {
			continue
		}
		result = append(result, pair)
	}
	result = append(result, "GIT_TERMINAL_PROMPT=0")
	return result
}

var blockedEnvironmentName = regexp.MustCompile(`(?i)(^|_)(TOKEN|SECRET|PASSWORD|PRIVATE_KEY|API_KEY)($|_)`)

type limitedBuffer struct {
	mu    sync.Mutex
	head  bytes.Buffer
	tail  []byte
	total int
}

func (b *limitedBuffer) Write(value []byte) (int, error) {
	const limit = 16 << 10
	const headLimit = limit / 2
	const tailLimit = limit - headLimit
	original := len(value)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total += original
	if b.head.Len() < headLimit {
		remaining := headLimit - b.head.Len()
		written := min(len(value), remaining)
		_, _ = b.head.Write(value[:written])
		value = value[written:]
	}
	if len(value) > 0 {
		if len(value) > tailLimit {
			value = value[len(value)-tailLimit:]
		}
		b.tail = append(b.tail, value...)
		if len(b.tail) > tailLimit {
			b.tail = append([]byte(nil), b.tail[len(b.tail)-tailLimit:]...)
		}
	}
	return original, nil
}

func (b *limitedBuffer) String() string {
	const limit = 16 << 10
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.total <= limit {
		return b.head.String() + string(b.tail)
	}
	return b.head.String() + "\n...[bounded output omitted]...\n" + string(b.tail)
}

type providerOutputBuffer struct{ bytes.Buffer }

func (b *providerOutputBuffer) Write(value []byte) (int, error) {
	const limit = 256 << 10
	original := len(value)
	if b.Len() < limit {
		remaining := limit - b.Len()
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = b.Buffer.Write(value)
	}
	return original, nil
}

var providerSecret = regexp.MustCompile(`(?i)(github_pat_[A-Za-z0-9_]{10,}|gh[pousr]_[A-Za-z0-9]{10,}|sk-[A-Za-z0-9_-]{10,})`)

func safeExcerpt(value string) string {
	// Retry audit transaction IDs bind this exact bounded value. Canonicalize
	// invalid UTF-8 before redaction and only cut at rune boundaries so JSON
	// round-trips cannot replace a partial rune and change the durable hash.
	value = strings.ToValidUTF8(value, "\uFFFD")
	value = safeProviderOutput(value)
	value = strings.TrimSpace(value)
	const limit = 8 << 10
	if len(value) > limit {
		const marker = "\n...[bounded excerpt]...\n"
		contentBudget := limit - len(marker)
		prefixEnd := contentBudget / 2
		for prefixEnd > 0 && !utf8.RuneStart(value[prefixEnd]) {
			prefixEnd--
		}
		suffixStart := len(value) - (contentBudget - contentBudget/2)
		for suffixStart < len(value) && !utf8.RuneStart(value[suffixStart]) {
			suffixStart++
		}
		value = value[:prefixEnd] + marker + value[suffixStart:]
	}
	return value
}

func safeProviderOutput(value string) string {
	if rule, confidence := classifyRepairSourceSecret([]byte(value)); confidence != repairSourceSecretNone {
		return fmt.Sprintf("[REDACTED: %s/%s]", repairSourceSecretRulesetVersion, rule)
	}
	return strings.TrimSpace(providerSecret.ReplaceAllString(value, "[REDACTED]"))
}
