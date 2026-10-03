//go:build linux

package dashboard

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// spekHubDropCapsCommand wraps the stage CLI so it does not inherit the hive
// process's inheritable/ambient capabilities (the entrypoint raises
// NET_ADMIN into both). It fails closed: if the capability sets cannot be
// read, or they are non-empty and setpriv is unavailable, the stage is not
// launched.
func spekHubDropCapsCommand(cmd []string) ([]string, error) {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return nil, fmt.Errorf("reading process capabilities: %w", err)
	}
	return spekHubDropCapsCommandFor(cmd, string(status), exec.LookPath)
}

func spekHubDropCapsCommandFor(cmd []string, status string, lookPath func(string) (string, error)) ([]string, error) {
	held, err := spekHubInheritableCapsHeld(status)
	if err != nil {
		return nil, err
	}
	if !held {
		return cmd, nil
	}
	setpriv, err := lookPath("setpriv")
	if err != nil {
		return nil, fmt.Errorf("hive holds inheritable capabilities and setpriv is unavailable to clear them for the agent CLI: %w", err)
	}
	wrapped := []string{setpriv, "--inh-caps=-all", "--ambient-caps=-all", "--no-new-privs", "--"}
	return append(wrapped, cmd...), nil
}

func spekHubInheritableCapsHeld(status string) (bool, error) {
	seen := 0
	held := false
	for _, line := range strings.Split(status, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || (key != "CapInh" && key != "CapAmb") {
			continue
		}
		caps, err := strconv.ParseUint(strings.TrimSpace(value), 16, 64)
		if err != nil {
			return false, fmt.Errorf("parsing %s: %w", key, err)
		}
		seen++
		if caps != 0 {
			held = true
		}
	}
	if seen != 2 {
		return false, errors.New("process capability sets not found in /proc/self/status")
	}
	return held, nil
}
