package repair

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ContainmentProbeCommand is an internal child mode used only by the
// deterministic Codex sandbox health probe. It is intentionally undocumented
// as a user command and never returns file contents.
const ContainmentProbeCommand = "__hive-repair-containment-probe"

const (
	containmentProbeModeEnv    = "HIVE_REPAIR_CONTAINMENT_PROBE_MODE"
	containmentProbeTargetEnv  = "HIVE_REPAIR_CONTAINMENT_PROBE_TARGET"
	containmentProbeAddressEnv = "HIVE_REPAIR_CONTAINMENT_PROBE_ADDRESS"

	containmentProbeReadBlocked    = "HIVE_CONTAINMENT_READ_BLOCKED"
	containmentProbeWriteBlocked   = "HIVE_CONTAINMENT_WRITE_BLOCKED"
	containmentProbeNetworkBlocked = "HIVE_CONTAINMENT_NETWORK_BLOCKED"
)

// RunContainmentProbeChild executes one deliberately forbidden operation. The
// parent creates and independently verifies every target. Successful access is
// a violation and returns a distinct non-zero code without printing contents.
func RunContainmentProbeChild() int {
	mode := strings.TrimSpace(os.Getenv(containmentProbeModeEnv))
	switch mode {
	case "read":
		target := strings.TrimSpace(os.Getenv(containmentProbeTargetEnv))
		if !safeContainmentProbeTarget(target) {
			return 64
		}
		file, err := os.Open(target)
		if err != nil {
			fmt.Fprintln(os.Stdout, containmentProbeReadBlocked)
			return 0
		}
		_ = file.Close()
		fmt.Fprintln(os.Stderr, "HIVE_CONTAINMENT_READ_VIOLATION")
		return 42
	case "write":
		target := strings.TrimSpace(os.Getenv(containmentProbeTargetEnv))
		if !safeContainmentProbeTarget(target) {
			return 64
		}
		if err := os.WriteFile(target, []byte("HIVE_CONTAINMENT_WRITE_VIOLATION\n"), 0o600); err != nil {
			fmt.Fprintln(os.Stdout, containmentProbeWriteBlocked)
			return 0
		}
		fmt.Fprintln(os.Stderr, "HIVE_CONTAINMENT_WRITE_VIOLATION")
		return 42
	case "network":
		address := strings.TrimSpace(os.Getenv(containmentProbeAddressEnv))
		host, _, err := net.SplitHostPort(address)
		if err != nil || host != "127.0.0.1" {
			return 64
		}
		connection, err := net.DialTimeout("tcp", address, 2*time.Second)
		if err != nil {
			fmt.Fprintln(os.Stdout, containmentProbeNetworkBlocked)
			return 0
		}
		_ = connection.Close()
		fmt.Fprintln(os.Stderr, "HIVE_CONTAINMENT_NETWORK_VIOLATION")
		return 42
	default:
		return 64
	}
}

func safeContainmentProbeTarget(target string) bool {
	if target == "" || !filepath.IsAbs(target) || strings.ContainsRune(target, '\x00') {
		return false
	}
	clean := filepath.Clean(target)
	parentPath := filepath.Dir(clean)
	parent := filepath.Base(parentPath)
	root := filepath.Base(filepath.Dir(parentPath))
	base := filepath.Base(clean)
	return strings.HasPrefix(root, "hive-codex-containment-") && parent == "blocked" &&
		(base == "read-sentinel.txt" || base == "write-sentinel.txt")
}
