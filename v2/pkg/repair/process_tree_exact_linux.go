//go:build linux

package repair

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// startExactRepairProcessTree launches one proposal process behind the exact,
// Hive-attested Bubblewrap boundary. Bubblewrap remains PID 1 in the new PID
// namespace and therefore is the kernel-backed reaper for every descendant,
// including children that call setsid, double-fork, or close inherited pipes.
// --die-with-parent also tears the namespace down if Hive itself disappears.
func startExactRepairProcessTree(ctx context.Context, command *exec.Cmd, helper codexProviderFileIdentity) (func() error, error) {
	if command == nil || command.Path == "" || len(command.Args) == 0 {
		return nil, errors.New("exact repair process-tree command is incomplete")
	}
	if command.Dir == "" || !filepath.IsAbs(command.Dir) || strings.ContainsRune(command.Dir, '\x00') {
		return nil, errors.New("exact repair process-tree command requires one absolute neutral working directory")
	}
	if command.SysProcAttr != nil {
		return nil, errors.New("exact repair process-tree command already has process attributes")
	}
	if helper.Path == "" || !filepath.IsAbs(helper.Path) || strings.ContainsRune(helper.Path, '\x00') {
		return nil, errors.New("exact Linux repair process-tree containment requires one sealed Bubblewrap helper")
	}
	actual, err := inspectCodexProviderFile(helper.Path)
	if err != nil {
		return nil, fmt.Errorf("revalidate sealed Linux process-tree helper immediately before launch: %w", err)
	}
	if actual != helper {
		return nil, errors.New("sealed Linux process-tree helper changed immediately before launch")
	}

	payloadPath := command.Path
	payloadArgs := append([]string(nil), command.Args[1:]...)
	command.Path = helper.Path
	command.Args = append([]string{
		helper.Path,
		"--unshare-pid",
		"--die-with-parent",
		"--bind", "/", "/",
		"--dev-bind", "/dev", "/dev",
		"--proc", "/proc",
		"--chdir", command.Dir,
		"--",
		payloadPath,
	}, payloadArgs...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start exact Linux repair process-tree boundary: %w", err)
	}

	// exec.CommandContext owns cancellation of the exact outer Bubblewrap
	// process. Wait returns only after Bubblewrap's PID-namespace reaper exits;
	// at that point the kernel has killed and reaped the namespace descendants.
	return command.Wait, nil
}
