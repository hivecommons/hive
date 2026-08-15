//go:build linux

package repair

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// startExactRepairProcessTree launches one proposal process behind the exact,
// Hive-attested Bubblewrap boundary. Bubblewrap remains PID 1 in the new PID
// namespace and therefore is the kernel-backed reaper for every descendant,
// including children that call setsid, double-fork, or close inherited pipes.
// --die-with-parent also tears the namespace down if Hive itself disappears.
func startExactRepairProcessTree(ctx context.Context, command *exec.Cmd, helper, capabilityDropHelper codexProviderFileIdentity) (func() error, error) {
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

	// The hosted entrypoint grants Hive ambient CAP_NET_ADMIN so its in-process
	// proxy can mark egress sockets. Linux carries an ambient capability across
	// exec into an otherwise unprivileged Bubblewrap process, and Bubblewrap
	// deliberately refuses that state. Lock this goroutine to the exact thread
	// whose CapAmb value we inspect, then insert the attested setpriv exec shim
	// only when that thread actually needs the capability cleared. setpriv
	// immediately execs Bubblewrap in the same PID, so Bubblewrap remains the
	// outer PID-namespace reaper and process-tree ownership is unchanged.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ambientCapabilities, err := currentLinuxThreadHasAmbientCapabilities()
	if err != nil {
		return nil, fmt.Errorf("inspect ambient capabilities before exact Linux repair launch: %w", err)
	}
	if err := configureExactLinuxProcessTreeCommand(command, helper, capabilityDropHelper, ambientCapabilities); err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start exact Linux repair process-tree boundary: %w", err)
	}

	// exec.CommandContext owns cancellation of the exact outer Bubblewrap
	// process. Wait returns only after Bubblewrap's PID-namespace reaper exits;
	// at that point the kernel has killed and reaped the namespace descendants.
	return command.Wait, nil
}

func configureExactLinuxProcessTreeCommand(command *exec.Cmd, helper, capabilityDropHelper codexProviderFileIdentity, ambientCapabilities bool) error {
	if ambientCapabilities {
		if capabilityDropHelper.Path == "" || !filepath.IsAbs(capabilityDropHelper.Path) || strings.ContainsRune(capabilityDropHelper.Path, '\x00') {
			return errors.New("exact Linux repair process-tree containment requires an attested capability-drop helper when Hive has ambient capabilities")
		}
		actualDropHelper, inspectErr := inspectCodexProviderFile(capabilityDropHelper.Path)
		if inspectErr != nil {
			return fmt.Errorf("revalidate sealed Linux capability-drop helper immediately before launch: %w", inspectErr)
		}
		if actualDropHelper != capabilityDropHelper {
			return errors.New("sealed Linux capability-drop helper changed immediately before launch")
		}
	}

	payloadPath := command.Path
	payloadArgs := append([]string(nil), command.Args[1:]...)
	bubblewrapArgs := append([]string{
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
	if ambientCapabilities {
		command.Path = capabilityDropHelper.Path
		command.Args = append([]string{
			capabilityDropHelper.Path,
			"--inh-caps=-all",
			"--ambient-caps=-all",
			"--",
		}, bubblewrapArgs...)
	} else {
		command.Path = helper.Path
		command.Args = bubblewrapArgs
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

func currentLinuxThreadHasAmbientCapabilities() (bool, error) {
	status, err := os.ReadFile("/proc/thread-self/status")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(status), "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found || name != "CapAmb" {
			continue
		}
		mask, parseErr := strconv.ParseUint(strings.TrimSpace(value), 16, 64)
		if parseErr != nil {
			return false, parseErr
		}
		return mask != 0, nil
	}
	return false, errors.New("CapAmb is absent from /proc/thread-self/status")
}
