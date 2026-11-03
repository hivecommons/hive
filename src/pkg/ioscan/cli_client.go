package ioscan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

const (
	// DefaultCLIClassifierTimeout bounds one CLI classifier call when the
	// caller passes no timeout.
	DefaultCLIClassifierTimeout = 60 * time.Second
	cliClassifierMaxOutput      = 1 << 20
	cliClassifierWaitDelay      = 2 * time.Second
)

// cliToollessArgs holds, per subscription CLI backend, the binary and the
// non-interactive arguments that launch it with every tool disabled. The
// agent launcher's headless command (agent.HeadlessPromptCommand) cannot be
// reused here: it pre-approves every tool (--allow-all,
// --dangerously-skip-permissions, --dangerously-bypass-approvals-and-sandbox),
// which is exactly what the classifier must never do. pkg/agent also imports
// this package, so it cannot be called from here without an import cycle.
//
// Only backends whose CLI can disable all built-in tools and all MCP servers
// from the command line are listed. codex and gemini expose no flag that
// removes every tool (their non-interactive modes still run read-only
// shell/file tools), so SupportsToolless reports false for them.
//
// promptViaArg says how the rendered prompt reaches the CLI: false pipes it
// on stdin (claude -p with no text reads the prompt from stdin); true appends
// it as the value of promptFlag (copilot ignores stdin in prompt mode). The
// prompt is bounded by DefaultClassifierMaxInputChars plus the fixed system
// text, far below any argv limit.
var cliToollessArgs = map[string]struct {
	binary       string
	args         []string
	promptViaArg bool
	promptFlag   string
}{
	"copilot": {
		binary: "copilot",
		// --available-tools with a name that matches no tool leaves the model
		// with no tool at all (#14499): the CLI logs "Disabled tools: bash,
		// create, edit, glob, grep, view, web_fetch, ..." and every MCP server
		// reports disabled. An empty list is ignored by the CLI, so the
		// sentinel name is required. --disable-builtin-mcps additionally
		// turns off the GitHub MCP servers, --no-custom-instructions keeps
		// AGENTS.md and friends out of the context, --no-ask-user removes the
		// interactive ask tool, --no-auto-update keeps the run offline apart
		// from the model call, and -s prints the reply with no stats.
		// --allow-all-tools is never passed: with no tools available the
		// model has nothing to be approved for.
		args: []string{"-s", "--no-color", "--available-tools", cliCopilotNoToolSentinel,
			"--disable-builtin-mcps", "--no-custom-instructions", "--no-ask-user", "--no-auto-update"},
		promptViaArg: true,
		promptFlag:   "-p",
	},
	"claude": {
		binary: "claude",
		// --tools "" disables every built-in tool; --strict-mcp-config with no
		// --mcp-config loads no MCP server; no permission-bypass flag is set.
		// --settings with disableAllHooks overrides the same key in the
		// user's settings.json for this run, so hooks configured in the
		// host's ~/.claude (UserPromptSubmit, Stop, ...) do not run against
		// the text being classified (#14062). Login, env and model keys in
		// that file still load. Hooks from managed (policy) settings are the
		// operator's own and are not affected.
		args: []string{"-p", "--output-format", "text", "--tools", "", "--strict-mcp-config",
			"--settings", cliClaudeHooksOffSettings},
	},
}

// cliClaudeHooksOffSettings is the inline settings JSON passed to claude so
// no user, project, local or plugin hook runs in the classifier session.
const cliClaudeHooksOffSettings = `{"disableAllHooks":true}`

// cliCopilotNoToolSentinel is the tool allowlist passed to copilot. It names
// no real tool, so the allowlist is empty and the model is launched with no
// tool, built-in or MCP.
const cliCopilotNoToolSentinel = "hive-ioscan-no-tools"

// cliEnvAllowlist is the complete environment a classifier CLI receives:
// enough to locate the binary and its own subscription login, and nothing
// else, so hive tokens (GH_TOKEN, GITHUB_TOKEN, HIVE_GITHUB_TOKEN, ...) never
// reach a process that reads untrusted text. copilot's own login lives in
// COPILOT_HOME (default ~/.copilot); COPILOT_GITHUB_TOKEN is its dedicated
// override. GH_TOKEN and GITHUB_TOKEN, which copilot also honours, are
// deliberately absent: they are the hive's tokens.
var cliEnvAllowlist = map[string][]string{
	"":        {"PATH", "HOME", "LANG", "LC_ALL", "TERM", "XDG_CONFIG_HOME"},
	"claude":  {"CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN"},
	"copilot": {"COPILOT_HOME", "COPILOT_GITHUB_TOKEN", "COPILOT_GH_HOST", "GH_HOST", "NO_COLOR"},
}

// SupportsToolless reports whether backend can be launched as a classifier
// CLI with every tool disabled. Boot must refuse backends that cannot.
func SupportsToolless(backend string) bool {
	_, ok := cliToollessArgs[strings.TrimSpace(backend)]
	return ok
}

// ToollessBackends lists the backends SupportsToolless accepts, sorted, for
// log lines and error messages.
func ToollessBackends() []string {
	out := make([]string, 0, len(cliToollessArgs))
	for b := range cliToollessArgs {
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

// CLIChatClient is a ChatClient that runs the classifier prompt through a
// subscription CLI backend launched tool-less, in an empty temporary working
// directory, with a scrubbed environment.
type CLIChatClient struct {
	backend string
	model   string
	timeout time.Duration
	binary  string
}

// NewCLIChatClient constructs a CLI-backed classifier client. It refuses
// backends that cannot be launched tool-less.
func NewCLIChatClient(backend, model string, timeout time.Duration) (*CLIChatClient, error) {
	backend = strings.TrimSpace(backend)
	spec, ok := cliToollessArgs[backend]
	if !ok {
		return nil, fmt.Errorf("ioscan classifier: backend %q cannot be launched without tools", backend)
	}
	if timeout <= 0 {
		timeout = DefaultCLIClassifierTimeout
	}
	return &CLIChatClient{backend: backend, model: strings.TrimSpace(model), timeout: timeout, binary: spec.binary}, nil
}

// NewCLIClassifier builds the classifier boot installs for
// ioscan.classifier.backend: the tool-less CLI client behind the standard
// prompt and verdict parsing, capped at dailyQuota calls per UTC day. Wrap
// the result in NewCachedClassifier so cache hits do not spend the quota.
func NewCLIClassifier(backend, model string, dailyQuota int, now func() time.Time) (*DailyQuotaClassifier, error) {
	client, err := NewCLIChatClient(backend, model, 0)
	if err != nil {
		return nil, err
	}
	llm, err := NewLLMClassifier(LLMClassifierConfig{Client: client})
	if err != nil {
		return nil, err
	}
	return NewDailyQuotaClassifier(llm, dailyQuota, now), nil
}

// argv returns the launch command. The prompt is appended as a flag value
// only for backends that do not read it from stdin; Complete pipes it on
// stdin otherwise. Either way the untrusted text is delivered as data, never
// interpreted by a shell: exec.Command passes argv verbatim.
func (c *CLIChatClient) argv(prompt string) []string {
	spec := cliToollessArgs[c.backend]
	argv := append([]string{c.binary}, spec.args...)
	if c.model != "" {
		argv = append(argv, "--model", c.model)
	}
	if spec.promptViaArg {
		argv = append(argv, spec.promptFlag, prompt)
	}
	return argv
}

func (c *CLIChatClient) promptViaArg() bool {
	return cliToollessArgs[c.backend].promptViaArg
}

func (c *CLIChatClient) env() []string {
	var env []string
	for _, key := range append(append([]string{}, cliEnvAllowlist[""]...), cliEnvAllowlist[c.backend]...) {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	return env
}

// Complete runs the CLI once with the rendered prompt (on stdin, or as the
// prompt flag's value for CLIs that do not read stdin) and returns the
// single JSON verdict object it printed. Timeouts, nonzero exits, and
// replies that are not exactly one valid verdict are errors.
func (c *CLIChatClient) Complete(ctx context.Context, messages []chatMessage, _ int) (string, error) {
	dir, err := os.MkdirTemp("", "hive-ioscan-cli-")
	if err != nil {
		return "", fmt.Errorf("classifier cli: workdir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	runCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	prompt := renderCLIPrompt(messages)
	argv := c.argv(prompt)
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = c.env()
	if c.promptViaArg() {
		cmd.Stdin = strings.NewReader("")
	} else {
		cmd.Stdin = strings.NewReader(prompt)
	}
	stdout := &cappedBuffer{max: cliClassifierMaxOutput}
	cmd.Stdout = stdout
	cmd.Stderr = &cappedBuffer{max: cliClassifierMaxOutput}
	cmd.WaitDelay = cliClassifierWaitDelay
	runErr := cmd.Run()
	if ctxErr := runCtx.Err(); ctxErr != nil {
		return "", fmt.Errorf("classifier cli %s: %w", c.backend, ctxErr)
	}
	if runErr != nil {
		return "", fmt.Errorf("classifier cli %s: %w", c.backend, runErr)
	}
	verdict, err := extractSingleJSONObject(stdout.String())
	if err != nil {
		return "", fmt.Errorf("classifier cli %s: %w", c.backend, err)
	}
	if _, err := ParseInjectionScore(verdict); err != nil {
		return "", fmt.Errorf("classifier cli %s: %w", c.backend, err)
	}
	return verdict, nil
}

func renderCLIPrompt(messages []chatMessage) string {
	var b strings.Builder
	for i, m := range messages {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(strings.ToUpper(m.Role))
		b.WriteString(":\n")
		b.WriteString(m.Content)
	}
	b.WriteString("\n")
	return b.String()
}

// extractSingleJSONObject returns the only JSON object in s. Surrounding
// banners and markdown fences are ignored; zero or several objects is an error.
func extractSingleJSONObject(s string) (string, error) {
	var found []string
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			continue
		}
		found = append(found, string(raw))
		i += int(dec.InputOffset()) - 1
	}
	switch len(found) {
	case 0:
		return "", errors.New("no JSON verdict in classifier reply")
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("classifier reply has %d JSON objects, want exactly one", len(found))
	}
}

type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }
