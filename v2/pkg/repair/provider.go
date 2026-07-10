package repair

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

type Provider interface {
	Name() string
	Health(ctx context.Context) error
	Run(ctx context.Context, worktree, prompt string) error
}

// CodexProvider invokes the stable non-interactive Codex CLI surface. Prefix
// can pin a packaged launcher, for example: ["--yes",
// "@openai/codex@0.144.1"], while Command is "npx".
type CodexProvider struct {
	Command string
	Prefix  []string
}

func (p CodexProvider) Name() string { return "codex" }

func (p CodexProvider) Health(ctx context.Context) error {
	if strings.TrimSpace(p.Command) == "" {
		return fmt.Errorf("Codex provider command is required")
	}
	command := exec.CommandContext(ctx, p.Command, append(append([]string(nil), p.Prefix...), "login", "status")...)
	command.Env = providerEnvironment()
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return fmt.Errorf("Codex authentication health check failed: %w: %s", err, safeExcerpt(output.String()))
	}
	if !strings.Contains(strings.ToLower(output.String()), "logged in") {
		return fmt.Errorf("Codex authentication health check did not report a logged-in session")
	}
	return nil
}

func (p CodexProvider) Run(ctx context.Context, worktree, prompt string) error {
	args := append([]string(nil), p.Prefix...)
	args = append(args,
		"--ask-for-approval", "never",
		"exec", "--cd", worktree,
		"--sandbox", "workspace-write",
		"--ephemeral", "--ignore-user-config", "--color", "never", "-",
	)
	command := exec.CommandContext(ctx, p.Command, args...)
	command.Dir = worktree
	command.Env = providerEnvironment()
	command.Stdin = strings.NewReader(prompt)
	command.Stdout = io.Discard
	var stderr limitedBuffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("Codex repair run failed: %w: %s", err, safeExcerpt(stderr.String()))
	}
	return nil
}

func providerEnvironment() []string {
	blocked := regexp.MustCompile(`(?i)(^|_)(TOKEN|SECRET|PASSWORD|PRIVATE_KEY|API_KEY)($|_)`)
	result := make([]string, 0, len(os.Environ()))
	for _, pair := range os.Environ() {
		name, _, _ := strings.Cut(pair, "=")
		if blocked.MatchString(name) || strings.EqualFold(name, "GH_TOKEN") || strings.EqualFold(name, "GITHUB_TOKEN") {
			continue
		}
		result = append(result, pair)
	}
	result = append(result, "GIT_TERMINAL_PROMPT=0")
	return result
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(value []byte) (int, error) {
	const limit = 16 << 10
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
	value = providerSecret.ReplaceAllString(value, "[REDACTED]")
	value = strings.TrimSpace(value)
	if len(value) > 2048 {
		value = value[len(value)-2048:]
	}
	return value
}
