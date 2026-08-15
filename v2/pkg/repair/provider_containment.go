package repair

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const windowsRestrictedReadDenial = "Restricted read-only access requires the elevated Windows sandbox backend"

// verifyCodexPlatformContainment runs no model request. It asks Codex's own
// sandbox subcommand to contain a Hive child that deliberately attempts read,
// write, and loopback-network operations. Health cannot authorize Run unless
// every operation is observably denied and all parent-side invariants hold.
func verifyCodexPlatformContainment(ctx context.Context, codexCommand string) error {
	base := cleanCodexEnvironment(codexCommand, os.TempDir(), os.TempDir(), os.TempDir(), os.TempDir(), os.TempDir(), os.TempDir(), os.TempDir(), os.TempDir())
	return verifyCodexPlatformContainmentWithEnvironment(ctx, codexCommand, base)
}

func verifyCodexPlatformContainmentWithEnvironment(ctx context.Context, codexCommand string, environment []string) error {
	return verifyCodexPlatformContainmentWithExactProcessTree(ctx, codexCommand, environment, codexProviderFileIdentity{}, codexProviderFileIdentity{})
}

// verifyCodexPlatformContainmentWithExactProcessTree exercises Codex's real
// inner sandbox underneath the same sealed outer process-tree boundary used by
// governed proposal children. This is a no-model health check: a host whose
// user/PID namespace or AppArmor policy rejects nested Bubblewrap fails before
// a specialist authorization can be issued.
func verifyCodexPlatformContainmentWithExactProcessTree(ctx context.Context, codexCommand string, environment []string, helper, capabilityDropHelper codexProviderFileIdentity) error {
	root, err := os.MkdirTemp("", "hive-codex-containment-")
	if err != nil {
		return fmt.Errorf("create Codex containment probe root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		_ = os.Remove(root)
		return fmt.Errorf("protect Codex containment probe root: %w", err)
	}
	binRoot := filepath.Join(root, "bin")
	blockedRoot := filepath.Join(root, "blocked")
	if err := os.Mkdir(binRoot, 0o700); err != nil {
		return fmt.Errorf("create Codex containment probe bin: %w", err)
	}
	if err := os.Mkdir(blockedRoot, 0o700); err != nil {
		return fmt.Errorf("create Codex containment blocked root: %w", err)
	}
	readPath := filepath.Join(blockedRoot, "read-sentinel.txt")
	writePath := filepath.Join(blockedRoot, "write-sentinel.txt")
	probeExtension := ""
	if runtime.GOOS == "windows" {
		probeExtension = ".exe"
	}
	probePath := filepath.Join(binRoot, "hive-containment-probe"+probeExtension)
	sandboxAlias, err := createCodexLinuxSandboxAlias(codexCommand, binRoot)
	if err != nil {
		return err
	}
	defer cleanupCodexContainmentProbe(root, binRoot, blockedRoot, readPath, writePath, probePath, sandboxAlias)

	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		return fmt.Errorf("create Codex containment nonce: %w", err)
	}
	readValue := []byte("HIVE_CONTAINMENT_READ_" + hex.EncodeToString(nonceBytes))
	writeValue := []byte("HIVE_CONTAINMENT_WRITE_" + hex.EncodeToString(nonceBytes))
	if err := os.WriteFile(readPath, readValue, 0o600); err != nil {
		return fmt.Errorf("write Codex read sentinel: %w", err)
	}
	if err := os.WriteFile(writePath, writeValue, 0o600); err != nil {
		return fmt.Errorf("write Codex write sentinel: %w", err)
	}
	if err := copyCurrentExecutableForContainmentProbe(probePath); err != nil {
		return err
	}

	if err := runCodexContainmentProbe(ctx, codexCommand, binRoot, probePath, "read", readPath, "", containmentProbeReadBlocked, environment, helper, capabilityDropHelper); err != nil {
		return err
	}
	if actual, err := os.ReadFile(readPath); err != nil || string(actual) != string(readValue) {
		return fmt.Errorf("Codex containment read probe changed or removed its sentinel")
	}
	if err := runCodexContainmentProbe(ctx, codexCommand, binRoot, probePath, "write", writePath, "", containmentProbeWriteBlocked, environment, helper, capabilityDropHelper); err != nil {
		return err
	}
	if actual, err := os.ReadFile(writePath); err != nil || string(actual) != string(writeValue) {
		return fmt.Errorf("Codex containment write probe modified its sentinel")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("create Codex containment loopback listener: %w", err)
	}
	defer listener.Close()
	if err := runCodexContainmentProbe(ctx, codexCommand, binRoot, probePath, "network", "", listener.Addr().String(), containmentProbeNetworkBlocked, environment, helper, capabilityDropHelper); err != nil {
		return err
	}
	if tcp, ok := listener.(*net.TCPListener); ok {
		_ = tcp.SetDeadline(time.Now().Add(100 * time.Millisecond))
	}
	connection, acceptErr := listener.Accept()
	if acceptErr == nil {
		_ = connection.Close()
		return fmt.Errorf("Codex containment network probe reached the parent listener")
	}
	return nil
}

func runCodexContainmentProbe(ctx context.Context, codexCommand, cwd, probePath, mode, target, address, expectedMarker string, environment []string, helper, capabilityDropHelper codexProviderFileIdentity) error {
	args := []string{"sandbox", "-C", cwd}
	args = append(args, codexNoFilesPermissionArgs()...)
	args = append(args,
		"-P", "hive_repair_no_files",
		"--sandbox-state-readable-root", cwd,
	)
	if runtime.GOOS == "linux" {
		args = append(args, "--sandbox-state-readable-root", filepath.Dir(codexCommand))
	}
	args = append(args,
		"--sandbox-state-disable-network",
		"--", probePath, ContainmentProbeCommand,
	)
	command := exec.CommandContext(ctx, codexCommand, args...)
	command.Dir = cwd
	command.Env = containmentProbeEnvironment(mode, target, address, cwd, environment)
	var stdout, stderr limitedBuffer
	command.Stdout, command.Stderr = &stdout, &stderr
	var err error
	if helper.Path != "" {
		wait, startErr := startExactRepairProcessTree(ctx, command, helper, capabilityDropHelper)
		if startErr != nil {
			err = startErr
		} else {
			err = wait()
		}
	} else {
		err = command.Run()
	}
	output := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
	if strings.Contains(output, "HIVE_CONTAINMENT_") && !strings.Contains(output, expectedMarker) {
		return fmt.Errorf("Codex %s containment probe reported an access violation", mode)
	}
	if runtime.GOOS == "windows" {
		if err == nil || !strings.Contains(output, windowsRestrictedReadDenial) || strings.Contains(output, expectedMarker) {
			return fmt.Errorf("Codex %s containment probe did not produce the exact reviewed Windows denial: %s", mode, containmentProbeFailureDetail(err, output))
		}
		return nil
	}
	if err != nil || strings.TrimSpace(stdout.String()) != expectedMarker {
		return codexContainmentProbeDeniedError(runtime.GOOS, mode, err, output)
	}
	return nil
}

func codexContainmentProbeDeniedError(platform, mode string, err error, output string) error {
	detail := containmentProbeFailureDetail(err, output)
	if platform == "linux" && strings.Contains(strings.ToLower(output), "bubblewrap is unavailable") {
		return fmt.Errorf("Codex Linux containment requires bwrap on PATH; install the open-source bubblewrap package with the host package manager and rerun Hive setup: %s", detail)
	}
	lowerOutput := strings.ToLower(output)
	if platform == "linux" && strings.Contains(lowerOutput, "operation not permitted") && (strings.Contains(lowerOutput, "failed rtm_newaddr") || strings.Contains(lowerOutput, "creating new namespace failed")) {
		return fmt.Errorf("Codex Linux containment found bwrap, but the host policy blocked its user or network namespace; on Ubuntu install apparmor-profiles and load /usr/share/apparmor/extra-profiles/bwrap-userns-restrict with apparmor_parser, then rerun Hive setup: %s", detail)
	}
	return fmt.Errorf("Codex %s containment probe was not denied by the active no-files profile: %s", mode, detail)
}

func containmentProbeFailureDetail(err error, output string) string {
	detail := safeExcerpt(output)
	if err == nil {
		return detail
	}
	if detail == "" {
		return err.Error()
	}
	return err.Error() + ": " + detail
}

func containmentProbeEnvironment(mode, target, address, runtimeRoot string, environment []string) []string {
	result := make([]string, 0, len(environment)+3)
	for _, pair := range environment {
		name, _, _ := strings.Cut(pair, "=")
		if name == containmentProbeModeEnv || name == containmentProbeTargetEnv || name == containmentProbeAddressEnv {
			continue
		}
		result = append(result, pair)
	}
	result = append(result,
		containmentProbeModeEnv+"="+mode,
		containmentProbeTargetEnv+"="+target,
		containmentProbeAddressEnv+"="+address,
	)
	if runtime.GOOS == "linux" {
		result = prependProviderExecutablePath(result, runtimeRoot)
	}
	return result
}

func createCodexLinuxSandboxAlias(codexCommand, root string) (string, error) {
	if runtime.GOOS != "linux" {
		return "", nil
	}
	source, err := os.Lstat(codexCommand)
	if err != nil || !source.Mode().IsRegular() || source.Mode()&os.ModeSymlink != 0 || source.Size() <= 0 || source.Size() > maxCodexProviderExecutableBytes {
		return "", fmt.Errorf("Codex Linux sandbox alias source is not the bounded sealed executable")
	}
	alias := filepath.Join(root, "codex-linux-sandbox")
	if err := os.Link(codexCommand, alias); err != nil {
		return "", fmt.Errorf("create Hive-owned Codex Linux sandbox alias: %w", err)
	}
	linked, err := os.Lstat(alias)
	if err != nil || !linked.Mode().IsRegular() || linked.Mode()&os.ModeSymlink != 0 || !os.SameFile(source, linked) {
		_ = os.Remove(alias)
		return "", fmt.Errorf("verify Hive-owned Codex Linux sandbox alias")
	}
	return alias, nil
}

func prependProviderExecutablePath(environment []string, root string) []string {
	result := append([]string(nil), environment...)
	for index, pair := range result {
		name, value, found := strings.Cut(pair, "=")
		if found && strings.EqualFold(name, "PATH") {
			result[index] = name + "=" + root + string(os.PathListSeparator) + value
			return result
		}
	}
	return append(result, "PATH="+root)
}

func copyCurrentExecutableForContainmentProbe(destination string) error {
	sourcePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve Hive containment probe executable: %w", err)
	}
	sourceInfo, err := os.Lstat(sourcePath)
	if err != nil || !sourceInfo.Mode().IsRegular() || sourceInfo.Mode()&os.ModeSymlink != 0 || sourceInfo.Size() <= 0 || sourceInfo.Size() > maxCodexProviderExecutableBytes {
		return fmt.Errorf("Hive containment probe executable is not a bounded ordinary file")
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open Hive containment probe executable: %w", err)
	}
	defer source.Close()
	destinationFile, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o500)
	if err != nil {
		return fmt.Errorf("create Hive containment probe executable: %w", err)
	}
	written, copyErr := io.Copy(destinationFile, io.LimitReader(source, maxCodexProviderExecutableBytes+1))
	syncErr := destinationFile.Sync()
	closeErr := destinationFile.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil || written != sourceInfo.Size() {
		_ = os.Remove(destination)
		return fmt.Errorf("copy stable Hive containment probe executable")
	}
	return nil
}

func cleanupCodexContainmentProbe(root, binRoot, blockedRoot string, paths ...string) {
	root = filepath.Clean(root)
	if !strings.HasPrefix(filepath.Base(root), "hive-codex-containment-") {
		return
	}
	for _, candidate := range paths {
		candidate = filepath.Clean(candidate)
		if filepath.Dir(candidate) == binRoot || filepath.Dir(candidate) == blockedRoot {
			_ = os.Remove(candidate)
		}
	}
	_ = os.Remove(binRoot)
	_ = os.Remove(blockedRoot)
	_ = os.Remove(root)
}
