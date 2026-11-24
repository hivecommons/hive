package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/logscrub"
	"github.com/hivecommons/hive/crux/pushbroker"
	"github.com/hivecommons/hive/crux/sandbox"
)

// publishVerifyDirective matches an HTML-comment block of the form
//
//	<!-- hive:publish.verify
//	uv run pytest --collect-only -q
//	-->
//
// One shell command per line; blank lines and `#` comments are ignored.
//
// The block is recognised so the manager can say it saw one, but it is never
// executed: a rendered kick message interpolates issue and PR titles,
// knowledge facts and delegated prompts, none of which may choose the
// commands that run against the agent's tree before it is pushed. Verify
// commands are read from sandbox.publish.verify config only.
var publishVerifyDirective = regexp.MustCompile(`(?s)<!--\s*hive:publish\.verify\s*\n(.*?)-->`)

// parsePublishVerifyDirective extracts the commands a directive lists. ok is
// true when a directive is present, even if it lists no commands.
func parsePublishVerifyDirective(message string) (cmds []string, ok bool) {
	m := publishVerifyDirective.FindStringSubmatch(message)
	if m == nil {
		return nil, false
	}
	for _, line := range strings.Split(m[1], "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cmds = append(cmds, line)
	}
	return cmds, true
}

const (
	// sandboxVerifyTailMax bounds the verify output quoted on the card and in
	// the audit record; the full output lands in the kick log archive.
	sandboxVerifyTailMax = 600
	// sandboxVerifyOutputMax bounds what one verify command's combined output
	// keeps in memory and in the kick log.
	sandboxVerifyOutputMax      = 64 << 10
	defaultSandboxVerifyTimeout = 15 * time.Minute
)

// runPublishVerify runs spec.PublishVerify in order and returns the
// withhold reason for the first failure, or "" when every command passed
// (or none were declared). Results are appended to res.Verify either way.
func (e *SandboxExecutor) runPublishVerify(ctx context.Context, spec SandboxKickSpec, workspace, baseSHA string, res *SandboxKickResult) string {
	if len(spec.PublishVerify) == 0 {
		return ""
	}
	changed := e.changedFiles(ctx, workspace, baseSHA)
	env := map[string]string{
		"HIVE_CHANGED_FILES": strings.Join(changed, "\n"),
		"HIVE_CHANGED_TESTS": strings.Join(changedTestFiles(changed), "\n"),
		"HIVE_BASE_SHA":      baseSHA,
	}
	timeout := spec.VerifyTimeout
	if timeout <= 0 {
		timeout = defaultSandboxVerifyTimeout
	}
	for i, cmd := range spec.PublishVerify {
		cmd = strings.TrimSpace(cmd)
		if cmd == "" {
			continue
		}
		vres := SandboxVerifyResult{Command: cmd}
		vctx, cancel := context.WithTimeout(ctx, timeout)
		sres, err := e.launcher().Run(vctx, sandbox.LaunchSpec{
			Image:          spec.Image,
			Name:           "hive-" + sanitizeBranchPart(spec.Agent) + "-verify",
			Workspace:      workspace,
			WorkspaceMount: sandbox.DefaultWorkspaceMount,
			WorkDir:        sandbox.DefaultWorkspaceMount,
			Command:        []string{"sh", "-lc", cmd},
			Env:            env,
			EnvAllowlist:   spec.EnvAllowlist,
			NetworkMode:    spec.NetworkMode,
			PidsLimit:      spec.PidsLimit,
		})
		vres.TimedOut = errors.Is(vctx.Err(), context.DeadlineExceeded)
		cancel()
		out := logscrub.ScrubString(sres.Stdout, logscrub.WithMarkers())
		if sres.Stderr != "" {
			out += "\n" + logscrub.ScrubString(sres.Stderr, logscrub.WithMarkers())
		}
		vres.Output = tailRunes(strings.TrimSpace(out), sandboxVerifyOutputMax)
		vres.ExitCode = sres.ExitCode
		if err != nil {
			vres.Err = err.Error()
		}
		res.Verify = append(res.Verify, vres)
		if vres.Passed() {
			continue
		}
		reason := fmt.Sprintf("verify failed (%d/%d): %s", i+1, len(spec.PublishVerify), cmd)
		switch {
		case vres.TimedOut:
			reason += fmt.Sprintf(" timed out after %s", timeout)
		case vres.Err != "":
			reason += ": " + vres.Err
		default:
			reason += fmt.Sprintf(" (exit %d)", vres.ExitCode)
		}
		if tail := tailRunes(vres.Output, sandboxVerifyTailMax); tail != "" {
			reason += "\n" + tail
		}
		return reason
	}
	return ""
}

// changedFiles lists paths touched by the run's commits (base..HEAD). An
// error yields nil so the verify commands still run, with empty env.
func (e *SandboxExecutor) changedFiles(ctx context.Context, workspace, baseSHA string) []string {
	out, err := e.runner().Run(ctx, workspace, pushbroker.PushEnv(os.Environ()), "git", "diff", "--name-only", "--diff-filter=AMR", baseSHA, "HEAD")
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files
}

// changedTestFiles keeps the pytest/go/js-style test files out of a changed
// list so a verify command can run exactly the tests the run added.
func changedTestFiles(files []string) []string {
	var tests []string
	for _, f := range files {
		base := filepath.Base(f)
		switch {
		case strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"),
			strings.HasSuffix(base, "_test.py"),
			strings.HasSuffix(base, "_test.go"),
			strings.HasSuffix(base, ".test.js"), strings.HasSuffix(base, ".test.ts"),
			strings.HasSuffix(base, ".spec.js"), strings.HasSuffix(base, ".spec.ts"):
			tests = append(tests, f)
		}
	}
	return tests
}
