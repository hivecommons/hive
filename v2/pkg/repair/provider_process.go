package repair

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	codexStdoutHardLimit           = 256 << 10
	codexStderrDiagnosticHardLimit = 64 << 10
	codexHumanPromptHeaderLimit    = 16 << 10
	codexHumanBannerPrefix         = "OpenAI Codex v0.144.1\n--------\n"
	codexHumanPromptMarker         = "--------\nuser\n"
)

// Human-readable `codex exec` writes the input prompt and its final response
// to stderr as progress output. Keep that transport bounded, but account for
// the two payloads that Codex is expected to mirror before reserving a fixed
// diagnostic allowance. The authoritative response on stdout retains its
// independent hard limit.
func codexStderrCaptureLimit(prompt string) int {
	return len(prompt) + codexStdoutHardLimit + codexStderrDiagnosticHardLimit
}

type codexHardLimitBuffer struct {
	mu       sync.Mutex
	value    bytes.Buffer
	limit    int
	overflow bool
	cancel   context.CancelFunc
}

func (b *codexHardLimitBuffer) Write(value []byte) (int, error) {
	original := len(value)
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - b.value.Len()
	if remaining > 0 {
		written := len(value)
		if written > remaining {
			written = remaining
		}
		_, _ = b.value.Write(value[:written])
	}
	if original > remaining {
		b.overflow = true
		if b.cancel != nil {
			b.cancel()
		}
	}
	return original, nil
}

func (b *codexHardLimitBuffer) snapshot() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.value.Bytes()...), b.overflow
}

type codexRunningProcess struct {
	pid                int
	providerSHA256     string
	wait               func() error
	cancel             context.CancelFunc
	commandContext     context.Context
	stdout             *codexHardLimitBuffer
	stderr             *codexHardLimitBuffer
	stdoutRead         *os.File
	stderrRead         *os.File
	stdoutDrain        <-chan error
	stderrDrain        <-chan error
	privateRuntime     *codexPrivateRuntime
	sealed             *codexProviderIdentityAttestation
	invocationRoot     string
	cwd                string
	sandboxAlias       string
	ownedRoot          bool
	structured         bool
	expectedPromptEcho string
	waitOnce           sync.Once
	result             ProviderResult
	err                error
	reapErr            error
}

func startCodexAttestedProcess(ctx context.Context, provider CodexProvider, attestation *codexProviderIdentityAttestation, prompt string, structured bool, root string) (*codexRunningProcess, error) {
	if ctx == nil {
		return nil, errors.New("Codex invocation requires a context")
	}
	if attestation == nil {
		return nil, errors.New("Codex invocation requires an exact health attestation")
	}
	if err := attestation.revalidateSource(ctx); err != nil {
		return nil, fmt.Errorf("Codex executable identity pre-launch check failed: %w", err)
	}
	sealed, err := attestation.sealForRun()
	if err != nil {
		return nil, fmt.Errorf("seal Codex executable for model invocation: %w", err)
	}
	cleanupSealed := true
	defer func() {
		if cleanupSealed {
			_ = sealed.cleanupSeal()
		}
	}()
	// Capture caller intent before an ordinary invocation replaces an empty root
	// with its private temporary directory. Only the Manager-owned specialist
	// proposal path supplies a root and selects the exact outer process tree.
	exactProposalProcessTree := codexProposalProcessTreeRequested(root)
	ownedRoot := false
	if root == "" {
		root, err = os.MkdirTemp("", "hive-codex-provider-run-")
		ownedRoot = true
	} else {
		root = filepath.Clean(root)
		if !filepath.IsAbs(root) {
			return nil, errors.New("Codex neutral invocation root must be absolute")
		}
	}
	if err != nil {
		return nil, fmt.Errorf("create neutral Codex invocation root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		if ownedRoot {
			_ = os.RemoveAll(root)
		}
		return nil, err
	}
	cwd := filepath.Join(root, "cwd")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		if ownedRoot {
			_ = os.RemoveAll(root)
		}
		return nil, err
	}
	privateRuntime, err := prepareCodexPrivateRuntime(sealed, provider.Command, root)
	if err != nil {
		_ = os.Remove(cwd)
		if ownedRoot {
			_ = os.RemoveAll(root)
		}
		return nil, err
	}
	securedProvider := sealed.provider()
	securedProvider.runtimeEnvironment = privateRuntime.environment
	sandboxAlias, err := createCodexLinuxSandboxAlias(securedProvider.Command, filepath.Dir(securedProvider.Command))
	if err != nil {
		_ = privateRuntime.cleanup()
		_ = os.Remove(cwd)
		if ownedRoot {
			_ = os.RemoveAll(root)
		}
		return nil, err
	}
	args := securedProvider.securedExecArgsInternal(cwd, "-", !structured)
	if structured || sandboxAlias != "" {
		filesystemProfile := `permissions.hive_repair_no_files.filesystem={":minimal"="read",` + strconv.Quote(filepath.ToSlash(cwd)) + `="read"`
		if sandboxAlias != "" {
			filesystemProfile += `,` + strconv.Quote(filepath.ToSlash(filepath.Dir(securedProvider.Command))) + `="read"`
		}
		filesystemProfile += `}`
		terminal := args[len(args)-1]
		args = append(args[:len(args)-1], "--config", filesystemProfile)
		if structured {
			args = append(args, "--json")
		}
		args = append(args, terminal)
	}
	commandCtx, cancel := context.WithCancel(ctx)
	command := exec.CommandContext(commandCtx, securedProvider.Command, args...)
	command.Dir = cwd
	command.Env = securedProvider.commandEnvironment()
	if sandboxAlias != "" {
		command.Env = prependProviderExecutablePath(command.Env, filepath.Dir(sandboxAlias))
	}
	command.Stdin = strings.NewReader(prompt)
	stdout := &codexHardLimitBuffer{limit: codexStdoutHardLimit, cancel: cancel}
	stderrLimit := codexStderrDiagnosticHardLimit
	if !structured {
		stderrLimit = codexStderrCaptureLimit(prompt)
	}
	stderr := &codexHardLimitBuffer{limit: stderrLimit, cancel: cancel}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		cancel()
		_ = os.Remove(sandboxAlias)
		_ = privateRuntime.cleanup()
		_ = os.Remove(cwd)
		if ownedRoot {
			_ = os.RemoveAll(root)
		}
		return nil, fmt.Errorf("create Codex stdout pipe: %w", err)
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		cancel()
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()
		_ = os.Remove(sandboxAlias)
		_ = privateRuntime.cleanup()
		_ = os.Remove(cwd)
		if ownedRoot {
			_ = os.RemoveAll(root)
		}
		return nil, fmt.Errorf("create Codex stderr pipe: %w", err)
	}
	command.Stdout, command.Stderr = stdoutWrite, stderrWrite
	stdoutDrain := make(chan error, 1)
	stderrDrain := make(chan error, 1)
	go func() { _, copyErr := io.Copy(stdout, stdoutRead); stdoutDrain <- copyErr }()
	go func() { _, copyErr := io.Copy(stderr, stderrRead); stderrDrain <- copyErr }()
	if err := sealed.revalidate(ctx); err != nil {
		cancel()
		_ = stdoutWrite.Close()
		_ = stderrWrite.Close()
		_ = stdoutRead.Close()
		_ = stderrRead.Close()
		_ = os.Remove(sandboxAlias)
		_ = privateRuntime.cleanup()
		_ = os.Remove(cwd)
		if ownedRoot {
			_ = os.RemoveAll(root)
		}
		return nil, fmt.Errorf("Codex executable identity changed immediately before launch: %w", err)
	}
	var wait func() error
	if exactProposalProcessTree && !(runtime.GOOS == "linux" && codexProviderIsTestExecutable(provider.Command)) {
		// The governed proposal child needs a kernel-backed exact descendant
		// fence. Production Linux uses the attested/sealed Bubblewrap PID
		// namespace; Windows uses a kill-on-close Job Object. Native test
		// providers model Codex directly and retain the ordinary test boundary.
		wait, err = startExactRepairProcessTree(commandCtx, command, sealed.SealedContainmentHelper)
	} else {
		wait, err = startRepairProcessTree(commandCtx, command)
	}
	_ = stdoutWrite.Close()
	_ = stderrWrite.Close()
	if err != nil {
		cancel()
		_ = stdoutRead.Close()
		_ = stderrRead.Close()
		_ = os.Remove(sandboxAlias)
		_ = privateRuntime.cleanup()
		_ = os.Remove(cwd)
		if ownedRoot {
			_ = os.RemoveAll(root)
		}
		return nil, err
	}
	cleanupSealed = false
	expectedPromptEcho := ""
	if !structured {
		expectedPromptEcho = prompt
	}
	return &codexRunningProcess{
		pid: command.Process.Pid, providerSHA256: attestation.IdentitySHA256,
		wait: wait, cancel: cancel, stdout: stdout, stderr: stderr,
		stdoutRead: stdoutRead, stderrRead: stderrRead, stdoutDrain: stdoutDrain, stderrDrain: stderrDrain,
		commandContext: commandCtx,
		privateRuntime: privateRuntime, sealed: sealed, invocationRoot: root,
		cwd: cwd, sandboxAlias: sandboxAlias, ownedRoot: ownedRoot, structured: structured,
		expectedPromptEcho: expectedPromptEcho,
	}, nil
}

func codexProposalProcessTreeRequested(explicitRoot string) bool {
	return explicitRoot != ""
}

func (process *codexRunningProcess) PID() int { return process.pid }

func (process *codexRunningProcess) ProviderSHA256() string { return process.providerSHA256 }

func (process *codexRunningProcess) WaitProvider() (ProviderResult, error) {
	process.waitOnce.Do(func() {
		waitErr := process.wait()
		process.cancel()
		drainErr := errors.Join(
			finishCodexOutputDrain(process.stdoutRead, process.stdoutDrain, "stdout"),
			finishCodexOutputDrain(process.stderrRead, process.stderrDrain, "stderr"),
		)
		if isPatchEngineInfrastructureFailure(waitErr) {
			process.reapErr = errors.Join(process.reapErr, waitErr)
		}
		// A bounded output drain is also part of the descendant-liveness
		// proof: retained inherited handles mean process-tree cleanup was not
		// demonstrated even if the provider leader already exited.
		process.reapErr = errors.Join(process.reapErr, drainErr)
		stdout, stdoutOverflow := process.stdout.snapshot()
		stderr, stderrOverflow := process.stderr.snapshot()
		expectedPromptEcho := process.expectedPromptEcho
		process.expectedPromptEcho = ""
		cleanupErr := errors.Join(drainErr, process.cleanup())
		if stdoutOverflow || stderrOverflow {
			process.err = &ProviderRunError{Launched: true, Cause: errors.Join(errors.New("Codex output exceeded its hard cap and the child was canceled"), cleanupErr)}
			return
		}
		combined := codexOutputForSecretClassification(stdout, stderr, expectedPromptEcho, process.structured)
		if rule, confidence := classifyRepairSourceSecret(combined); confidence != repairSourceSecretNone {
			process.err = &ProviderRunError{Launched: true, Cause: errors.Join(fmt.Errorf("Codex repair run emitted unsafe output matching %s rule %s", repairSourceSecretRulesetVersion, rule), cleanupErr)}
			return
		}
		output := safeProviderOutput(string(stdout))
		process.result = ProviderResult{Summary: safeExcerpt(output), Output: output}
		if waitErr != nil {
			process.result = ProviderResult{}
			waitCause := error(waitErr)
			if process.commandContext != nil && process.commandContext.Err() != nil {
				waitCause = errors.Join(process.commandContext.Err(), waitErr)
			}
			process.err = &ProviderRunError{Launched: true, Cause: errors.Join(fmt.Errorf("Codex repair run failed: %w: %s", waitCause, safeExcerpt(string(stderr))), cleanupErr)}
			return
		}
		if cleanupErr != nil {
			process.result = ProviderResult{}
			process.err = &ProviderRunError{Launched: true, Cause: cleanupErr}
		}
	})
	return process.result, process.err
}

// codexOutputForSecretClassification excludes the byte-identical input prompt
// only from the reviewed initial human-output frame emitted by pinned Codex
// 0.144.1. That frame is transport input, not provider output. Authoritative
// stdout and every other stderr byte remain fail-closed inputs to the
// output-secret classifier. Structured mode has no reviewed prompt echo and
// therefore receives no exemption.
func codexOutputForSecretClassification(stdout, stderr []byte, expectedPromptEcho string, structured bool) []byte {
	if !structured && expectedPromptEcho != "" {
		prompt := []byte(expectedPromptEcho)
		if offset, ok := codexHumanPromptEchoOffset(stderr, prompt); ok {
			withoutEcho := make([]byte, 0, len(stderr)-len(prompt))
			withoutEcho = append(withoutEcho, stderr[:offset]...)
			withoutEcho = append(withoutEcho, stderr[offset+len(prompt):]...)
			stderr = withoutEcho
		}
	}
	combined := make([]byte, 0, len(stdout)+1+len(stderr))
	combined = append(combined, stdout...)
	combined = append(combined, '\n')
	return append(combined, stderr...)
}

func codexHumanPromptEchoOffset(stderr, prompt []byte) (int, bool) {
	if len(prompt) == 0 || !bytes.HasPrefix(stderr, []byte(codexHumanBannerPrefix)) {
		return 0, false
	}
	headerEnd := len(stderr)
	if headerEnd > codexHumanPromptHeaderLimit {
		headerEnd = codexHumanPromptHeaderLimit
	}
	markerOffset := bytes.Index(stderr[len(codexHumanBannerPrefix):headerEnd], []byte(codexHumanPromptMarker))
	if markerOffset < 0 {
		return 0, false
	}
	promptOffset := len(codexHumanBannerPrefix) + markerOffset + len(codexHumanPromptMarker)
	if promptOffset >= len(stderr) || len(prompt) > len(stderr)-promptOffset-1 {
		return 0, false
	}
	promptEnd := promptOffset + len(prompt)
	if !bytes.Equal(stderr[promptOffset:promptEnd], prompt) || stderr[promptEnd] != '\n' {
		return 0, false
	}
	return promptOffset, true
}

func (process *codexRunningProcess) ReapError() error {
	if process == nil {
		return errors.New("Codex process is unavailable")
	}
	_, _ = process.WaitProvider()
	return process.reapErr
}

func finishCodexOutputDrain(reader *os.File, done <-chan error, name string) error {
	if reader == nil || done == nil {
		return nil
	}
	select {
	case err := <-done:
		_ = reader.Close()
		if err != nil && !errors.Is(err, os.ErrClosed) {
			return fmt.Errorf("drain Codex %s: %w", name, err)
		}
		return nil
	case <-time.After(3 * time.Second):
		_ = reader.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		return fmt.Errorf("Codex descendants retained inherited %s handles after bounded process-tree termination", name)
	}
}

func (process *codexRunningProcess) cleanup() error {
	var result error
	if process.sandboxAlias != "" {
		if err := os.Remove(process.sandboxAlias); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	if process.privateRuntime != nil {
		result = errors.Join(result, process.privateRuntime.cleanup())
	}
	if process.cwd != "" {
		result = errors.Join(result, cleanupCodexNeutralCWD(process.cwd))
	}
	if process.sealed != nil {
		result = errors.Join(result, process.sealed.cleanupSeal())
	}
	if process.ownedRoot && process.invocationRoot != "" {
		if err := os.Remove(process.invocationRoot); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func cleanupCodexNeutralCWD(cwd string) error {
	cwd = filepath.Clean(cwd)
	if filepath.Base(cwd) != "cwd" || !filepath.IsAbs(cwd) {
		return errors.New("refuse unsafe Codex neutral cwd cleanup")
	}
	info, err := os.Lstat(cwd)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refuse replaced Codex neutral cwd cleanup")
	}
	if err := rejectCodexProviderLinkedPath(cwd); err != nil {
		return fmt.Errorf("refuse linked Codex neutral cwd cleanup: %w", err)
	}
	entries, err := os.ReadDir(cwd)
	if err != nil {
		return err
	}
	var violation error
	if len(entries) != 0 {
		violation = errors.New("contained Codex process created unexpected neutral-cwd files")
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(cwd, entry.Name())); err != nil {
			violation = errors.Join(violation, err)
		}
	}
	if err := os.Remove(cwd); err != nil && !errors.Is(err, os.ErrNotExist) {
		violation = errors.Join(violation, err)
	}
	return violation
}

var _ = runtime.GOOS
