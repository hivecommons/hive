package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/claude"
	"github.com/hivecommons/hive/pkg/dashboard"
)

// The Claude CLI path for the one-shot reviewer turns (Let hive decide and
// grounded chat) on a hive that has no OpenAI-compatible reviewer endpoint,
// only the Claude login its agents run on (governor.reviewer_backend, see
// config.ResolveReviewerTurn). Subscription credentials are only ever used
// through the official CLI, never for raw API calls.
//
// The user prompt carries untrusted issue, PR and comment text, so the turn is
// launched so that nothing in it can act:
//
//   - no tools: --tools "" removes every built-in tool, --strict-mcp-config
//     with no --mcp-config loads no MCP server, --disallowedTools "mcp__*"
//     denies any MCP tool on top, and no permission-bypass flag is ever set;
//   - no hooks: --settings {"disableAllHooks":true} (managed policy hooks are
//     the operator's own and are not affected);
//   - no session: no --continue/--resume, --no-session-persistence, a fresh
//     empty HOME and a fresh empty working directory, both removed afterwards;
//   - one turn: --max-turns 1;
//   - no secrets: the environment starts empty and gets only PATH, HOME, the
//     Claude login (CLAUDE_CODE_OAUTH_TOKEN, as headless launches pass it)
//     and, when it runs as its own user, the egress-proxy settings. No GitHub
//     or hive token reaches it;
//   - no argv prompt, and no way for the user prompt to pose as the system
//     prompt: the system prompt replaces Claude Code's own through
//     --system-prompt-file (a file in the turn's private temp tree), and
//     only the user prompt is on stdin. Argv carries flags and that path;
//   - its own user: with per-agent UID isolation on, the CLI runs as the
//     dedicated hive-turn user (provisioned by the entrypoint like the
//     Spektacular hub executor's user), never as the hive's own uid.
//
// Flags are from the Claude Code CLI reference
// (https://code.claude.com/docs/en/cli-reference) and headless guide
// (https://code.claude.com/docs/en/headless) for the version the image pins.

const (
	reviewerCLIBinary = "claude"
	// reviewerCLIMaxConcurrent bounds the CLIs running at once, so a burst
	// of decisions cannot fork many of them.
	reviewerCLIMaxConcurrent = 2
	reviewerCLIWaitDelay     = 2 * time.Second
	reviewerCLIHooksOff      = `{"disableAllHooks":true}`
	// reviewerCLIErrorSnippet caps how much of a failed run's own message an
	// error carries.
	reviewerCLIErrorSnippet = 200
	// reviewerCLISystemFile holds the caller's system prompt, in the turn's
	// temp root (outside the working directory, which stays empty).
	reviewerCLISystemFile = "system-prompt.txt"
)

var errReviewerCLINotSignedIn = errors.New("claude is not signed in on this hive")

// reviewerCLISlots is the process-wide concurrency bound.
var reviewerCLISlots = make(chan struct{}, reviewerCLIMaxConcurrent)

// reviewerCLIRunner runs one reviewer turn through the Claude CLI. Every
// field is a seam the tests replace.
type reviewerCLIRunner struct {
	binary  string
	timeout time.Duration
	// credential is the Claude login the CLI authenticates with; "" means
	// not signed in.
	credential func() string
	// execUser is the su-exec user spec to run as; "" runs as the hive's own
	// uid (no per-agent UID isolation on this deployment).
	execUser func() (string, error)
	// execEnv adds the egress-proxy settings for the run-as user.
	execEnv func([]string) []string
	// runAs wraps argv to run as spec with HOME=home.
	runAs func(argv []string, spec, home string) []string
	// prepare puts the command in its own process group, killed as spec on
	// timeout; reap kills what is left of that group once it exits.
	prepare func(c *exec.Cmd, spec string)
	reap    func(c *exec.Cmd, spec string)
	slots   chan struct{}
}

func newReviewerCLIRunner() *reviewerCLIRunner {
	return &reviewerCLIRunner{
		binary:     reviewerCLIBinary,
		timeout:    delegateDeciderTimeout,
		credential: reviewerClaudeCredential,
		execUser:   dashboard.ReviewerTurnExecUser,
		execEnv:    dashboard.ReviewerTurnExecEnv,
		runAs:      dashboard.ReviewerTurnRunAs,
		prepare:    dashboard.ReviewerTurnConfigureProcess,
		reap:       dashboard.ReviewerTurnReap,
		slots:      reviewerCLISlots,
	}
}

// reviewerClaudeCredential is the hive's Claude login, read from the same
// sources and in the same order as headless agent launches: the operator's
// long-lived token, else the shared dashboard sign-in's access token. It is
// passed to the CLI as CLAUDE_CODE_OAUTH_TOKEN, so the CLI never redeems or
// rotates the fleet's refresh grant. The value is never logged.
func reviewerClaudeCredential() string {
	if tok := claude.OperatorToken(); tok != "" {
		return tok
	}
	return claude.ReadAccessToken(claude.CredentialsPath)
}

// signedIn reports whether the Claude login is present.
func (c *reviewerCLIRunner) signedIn() bool {
	return c != nil && c.credential != nil && c.credential() != ""
}

// reviewerCLIModel maps the resolved reviewer model to the Claude CLI's
// --model form: a LiteLLM-style "anthropic/" prefix is dropped and the
// Copilot CLI's dotted Claude ids are re-spelled. Empty keeps the CLI's own
// default model.
func reviewerCLIModel(model string) string {
	m := strings.TrimPrefix(strings.TrimSpace(model), "anthropic/")
	if m == "" {
		return ""
	}
	return crux.ClaudeLaunchModel(m)
}

// reviewerCLIArgs is the CLI's argv after the binary. No prompt text is in it:
// systemFile is the path of the file holding the system prompt.
func reviewerCLIArgs(model, systemFile string) []string {
	args := []string{"-p", "--output-format", "json",
		"--tools", "", "--disallowedTools", "mcp__*", "--strict-mcp-config",
		"--settings", reviewerCLIHooksOff, "--no-session-persistence", "--max-turns", "1",
		"--system-prompt-file", systemFile}
	if m := reviewerCLIModel(model); m != "" {
		args = append(args, "--model", m)
	}
	return args
}

// writeReviewerCLISystem writes the system prompt into the turn's temp root,
// readable by the run-as user's group only when the CLI runs as its own user.
func writeReviewerCLISystem(root, system string, shared bool) (string, error) {
	path := filepath.Join(root, reviewerCLISystemFile)
	mode := os.FileMode(0o600)
	if shared {
		mode = 0o640
	}
	if err := os.WriteFile(path, []byte(system), 0o600); err != nil {
		return "", err
	}
	return path, os.Chmod(path, mode)
}

// reviewerCLIResult is the part of the CLI's --output-format json reply the
// runner reads.
type reviewerCLIResult struct {
	Type    string  `json:"type"`
	Subtype string  `json:"subtype"`
	IsError bool    `json:"is_error"`
	Result  *string `json:"result"`
}

// run runs one turn and returns the model's reply text.
func (c *reviewerCLIRunner) run(ctx context.Context, profile, system, user, model string) (string, error) {
	cred := c.credential()
	if cred == "" {
		return "", fmt.Errorf("%s: claude cli: %w", profile, errReviewerCLINotSignedIn)
	}
	spec, err := c.execUser()
	if err != nil {
		return "", fmt.Errorf("%s: claude cli: %w", profile, err)
	}
	runCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-runCtx.Done():
		return "", fmt.Errorf("%s: claude cli: waiting for a free slot: %w", profile, runCtx.Err())
	}

	root, work, home, err := reviewerCLIDirs(spec != "")
	if err != nil {
		return "", fmt.Errorf("%s: claude cli: workdir: %w", profile, err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	systemFile, err := writeReviewerCLISystem(root, system, spec != "")
	if err != nil {
		return "", fmt.Errorf("%s: claude cli: system prompt: %w", profile, err)
	}

	argv := append([]string{c.binary}, reviewerCLIArgs(model, systemFile)...)
	env := []string{"HOME=" + home, "DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_OAUTH_TOKEN=" + cred}
	if p, ok := os.LookupEnv("PATH"); ok {
		env = append(env, "PATH="+p)
	}
	if spec != "" {
		argv = c.runAs(argv, spec, home)
		env = c.execEnv(env)
	}
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = work
	cmd.Env = env
	cmd.Stdin = strings.NewReader(user)
	stdout := &reviewerCappedBuffer{max: delegateDeciderMaxReply + 1}
	cmd.Stdout = stdout
	cmd.Stderr = io.Discard
	cmd.WaitDelay = reviewerCLIWaitDelay
	c.prepare(cmd, spec)
	runErr := cmd.Run()
	c.reap(cmd, spec)
	if errors.Is(runErr, exec.ErrWaitDelay) {
		// The CLI exited; only an orphan held the output pipe.
		runErr = nil
	}
	if ctxErr := runCtx.Err(); ctxErr != nil {
		return "", fmt.Errorf("%s: claude cli: %w", profile, ctxErr)
	}
	raw, err := readTurnReply(profile, bytes.NewReader(stdout.buf.Bytes()))
	if err != nil {
		return "", err
	}
	var out reviewerCLIResult
	decodeErr := decodeTurnReply(profile, raw, &out)
	if decodeErr == nil && (out.IsError || (out.Subtype != "" && out.Subtype != "success")) {
		msg := ""
		if out.Result != nil {
			msg = strings.TrimSpace(*out.Result)
			if len(msg) > reviewerCLIErrorSnippet {
				msg = msg[:reviewerCLIErrorSnippet] + "…"
			}
		}
		return "", fmt.Errorf("%s: claude cli run failed (%s): %s", profile, out.Subtype, msg)
	}
	if runErr != nil {
		return "", fmt.Errorf("%s: claude cli: %w", profile, runErr)
	}
	if decodeErr != nil {
		return "", decodeErr
	}
	if out.Result == nil {
		return "", fmt.Errorf("%s: claude cli: reply has no result", profile)
	}
	return *out.Result, nil
}

// reviewerCLIDirs makes the turn's fresh temp tree: root/work (the empty
// working directory) and root/home (the empty HOME). Both are private to the
// hive uid, unless the CLI runs as its own user, which shares the node group:
// then the tree is group-readable and HOME group-writable so that user can
// use it and the hive can still remove what it wrote (the run-as wrapper sets
// umask 002).
func reviewerCLIDirs(shared bool) (root, work, home string, err error) {
	root, err = os.MkdirTemp("", "hive-reviewer-turn-")
	if err != nil {
		return "", "", "", err
	}
	work, home = filepath.Join(root, "work"), filepath.Join(root, "home")
	modes := []struct {
		path   string
		shared os.FileMode
	}{{root, 0o750}, {work, 0o750}, {home, 0o770}}
	for _, d := range modes {
		if d.path != root {
			if err = os.Mkdir(d.path, 0o700); err != nil {
				break
			}
		}
		if shared {
			if err = os.Chmod(d.path, d.shared); err != nil {
				break
			}
		}
	}
	if err != nil {
		_ = os.RemoveAll(root)
		return "", "", "", err
	}
	return root, work, home, nil
}

// readTurnReply reads a turn's raw reply, refusing one larger than
// delegateDeciderMaxReply. Shared by the HTTP and CLI paths.
func readTurnReply(profile string, r io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, delegateDeciderMaxReply+1))
	if err != nil {
		return nil, fmt.Errorf("%s: read response: %w", profile, err)
	}
	if len(raw) > delegateDeciderMaxReply {
		return nil, fmt.Errorf("%s: reply exceeds %d bytes", profile, delegateDeciderMaxReply)
	}
	return raw, nil
}

// decodeTurnReply decodes a turn's JSON envelope (the chat-completions body,
// or the CLI's json output). Shared by the HTTP and CLI paths.
func decodeTurnReply(profile string, raw []byte, v any) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%s: decode response: %w", profile, err)
	}
	return nil
}

// reviewerCappedBuffer keeps the first max bytes written and drops the rest,
// so a runaway CLI cannot grow the hive's memory.
type reviewerCappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *reviewerCappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}
