package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

// ENOEXEC detection (hivecommons/hive#17729).
//
// After the OKE nodes moved to Linux 6.12 (UEK8), the kernel's ELF loader
// rejected the Copilot CLI platform binary — a Bun single-file ET_EXEC — with
// ENOEXEC. Nothing in the launch path said so: the npm shim's sh fell back to
// interpreting the ELF as a script ("copilot: 1: : not found"), the hang
// diagnostic saw no copilot output and timed out, and the restart-storm
// breaker backed every agent off for up to 30 minutes. Fifteen hives, zero
// agents, and the only log line was "copilot hung with no output".
//
// Two things make that loud now. paneShowsExecFormatError recognises the
// kernel's message and the sh-fallback signature in a diagnostic pane so the
// diagnostic fails the agent with a remedy instead of relaunching. And
// PreflightBackendBinaries execs every installed agent CLI once at boot so the
// same breakage is one ERROR line at startup, before any agent is launched.

// execFormatErrorIndicators are the pane signatures of an ELF the kernel
// refused: the libc strerror text, and what dash/bash print when they fall
// back to running the binary as a script (the ELF's PT_INTERP string lands on
// the first "line", so the "command" sh reports is the loader path).
var execFormatErrorIndicators = []string{
	"Exec format error",
	"cannot execute binary file",
	"ld-linux-aarch64.so.1GNU",
	"ld-linux-x86-64.so.2GNU",
}

// paneShowsExecFormatError reports whether a diagnostic pane shows the CLI
// binary being rejected by the kernel rather than failing on its own.
func paneShowsExecFormatError(lines []string) bool {
	for _, line := range lines {
		for _, ind := range execFormatErrorIndicators {
			if strings.Contains(line, ind) {
				return true
			}
		}
		// sh fell back to "running" the ELF as a script and reported the
		// PT_INTERP string (the first printable run in the file) as a
		// missing command.
		if strings.Contains(line, "ld-linux-") && strings.Contains(line, ": not found") {
			return true
		}
	}
	return false
}

// execFormatRemedy is the operator-facing explanation attached to every
// ENOEXEC finding. It names the one thing that fixes it (run the binary
// through ld.so, which the hive image now does via
// bin/install-copilot-exec-shim.sh) so nobody has to rediscover it from a
// pane full of "not found".
const execFormatRemedy = "the kernel refused to exec the CLI binary (ENOEXEC / Exec format error): " +
	"usually a Bun/Node single-file ET_EXEC on a kernel whose ELF loader rejects its segment layout (seen with Linux 6.12 on aarch64). " +
	"Relaunching cannot help. Start it through the dynamic loader instead (`/lib/ld-linux-aarch64.so.1 <binary>` / `/lib64/ld-linux-x86-64.so.2 <binary>`) " +
	"or roll a hive image built with bin/install-copilot-exec-shim.sh — see hivecommons/hive#17729"

// execFormatLastError is what the agent's LastError is set to when the
// diagnostic sees ENOEXEC.
func execFormatLastError(binary string) string {
	return fmt.Sprintf("%s cannot be executed on this kernel: %s", binary, execFormatRemedy)
}

// Preflight pacing. Vars so tests can shrink them.
var (
	binaryPreflightTimeout = 20 * time.Second
)

// BinaryPreflightFinding is one CLI binary the preflight could not start.
type BinaryPreflightFinding struct {
	Backend string
	Binary  string
	Path    string
	// ExecFormat is true when the failure is the kernel refusing the ELF
	// (ENOEXEC) rather than the CLI exiting non-zero on its own.
	ExecFormat bool
	Err        error
	Output     string
}

func (f BinaryPreflightFinding) String() string {
	if f.ExecFormat {
		return fmt.Sprintf("backend %s: %s", f.Backend, execFormatLastError(f.Path))
	}
	return fmt.Sprintf("backend %s: %s --version failed: %v", f.Backend, f.Path, f.Err)
}

// classifyPreflightError decides whether a `--version` failure is the kernel
// refusing the binary. exec.Command surfaces ENOEXEC directly from execve
// (it does not go through a shell), and some CLIs are themselves shims that
// spawn the real binary and print the kernel's message.
func classifyPreflightError(err error, output string) bool {
	if errors.Is(err, syscall.ENOEXEC) {
		return true
	}
	return paneShowsExecFormatError(strings.Split(output, "\n"))
}

// PreflightBackendBinaries runs `<cli> --version` once for every CLI backend
// in use (or every installed CLI backend when backends is empty), with a
// timeout per binary, and returns the ones that could not start. Binaries
// not on PATH are skipped — an absent CLI is a configuration matter the
// launch path already reports; this is about CLIs that are present and
// broken. It never blocks boot: findings are returned for the caller to log.
func PreflightBackendBinaries(ctx context.Context, backends []string) []BinaryPreflightFinding {
	if len(backends) == 0 {
		backends = config.CLIBackends
	}
	seen := map[string]bool{}
	var findings []BinaryPreflightFinding
	for _, backend := range backends {
		binary, err := backendBinaryName(backend)
		if err != nil || seen[binary] {
			continue
		}
		seen[binary] = true
		path, err := exec.LookPath(binary)
		if err != nil {
			continue
		}
		if f, bad := preflightOneBinary(ctx, backend, binary, path); bad {
			findings = append(findings, f)
		}
	}
	return findings
}

func preflightOneBinary(ctx context.Context, backend, binary, path string) (BinaryPreflightFinding, bool) {
	ctx, cancel := context.WithTimeout(ctx, binaryPreflightTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = append(cmd.Environ(), "NO_COLOR=1", "CI=1")
	out, err := cmd.CombinedOutput()
	output := string(out)
	if err == nil {
		return BinaryPreflightFinding{}, false
	}
	execFormat := classifyPreflightError(err, output)
	// A CLI that runs but dislikes `--version` (exit 1, usage text) is not
	// what this preflight is for; only the kernel-level refusal is a finding.
	// A timeout is not either — a slow cold start is the hang diagnostic's job.
	if !execFormat {
		return BinaryPreflightFinding{}, false
	}
	return BinaryPreflightFinding{
		Backend:    backend,
		Binary:     binary,
		Path:       path,
		ExecFormat: true,
		Err:        err,
		Output:     truncateStr(strings.TrimSpace(output), 300),
	}, true
}

// LogBackendBinaryPreflight runs PreflightBackendBinaries and logs each
// finding at ERROR with the remedy. Called once at boot by cmd/hive.
func LogBackendBinaryPreflight(ctx context.Context, logger *slog.Logger, backends []string) []BinaryPreflightFinding {
	findings := PreflightBackendBinaries(ctx, backends)
	for _, f := range findings {
		logger.Error("agent CLI preflight: binary cannot be executed on this kernel",
			"backend", f.Backend, "binary", f.Path, "error", f.Err, "output", f.Output, "remedy", execFormatRemedy)
	}
	return findings
}
