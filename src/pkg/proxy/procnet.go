package proxy

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// procNetTCPPath / procNetTCP6Path are the kernel socket tables scanned for UID
// attribution. Vars (not consts) so tests can redirect them at temp files.
var (
	procNetTCPPath  = "/proc/net/tcp"
	procNetTCP6Path = "/proc/net/tcp6"
)

// LookupUIDByLocalPort returns the UID of the process owning the socket bound to
// the given local port, checking BOTH /proc/net/tcp (IPv4) and /proc/net/tcp6
// (IPv6). Returns -1 if no match is found.
//
// Both tables must be scanned: an agent connecting to a "127.0.0.1"/"localhost"
// proxy can land its socket in the IPv6 table (as ::1 or an IPv4-mapped ::ffff:
// address), so an IPv4-only lookup silently misses those connections and returns
// no UID — which the proxy treats as an unidentified agent (advisory mode),
// wrongly blocking a write-capable agent's GitHub API calls (e.g. POST /pulls).
func LookupUIDByLocalPort(port int) (int, error) {
	uid, err := lookupUIDByLocalPortFrom(procNetTCPPath, port)
	if err == nil {
		return uid, nil
	}
	// Fall back to the IPv6 table — the socket may be there instead.
	uid6, err6 := lookupUIDByLocalPortFrom(procNetTCP6Path, port)
	if err6 == nil {
		return uid6, nil
	}
	// Neither table had it; return the original (IPv4) error for context.
	return -1, err
}

func lookupUIDByLocalPortFrom(path string, port int) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return -1, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	hexPort := fmt.Sprintf("%04X", port)
	scanner := bufio.NewScanner(f)

	// Skip header line.
	if !scanner.Scan() {
		return -1, fmt.Errorf("empty %s", path)
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}

		// fields[1] is "hex_ip:hex_port"
		localAddr := fields[1]
		colonIdx := strings.LastIndex(localAddr, ":")
		if colonIdx < 0 {
			continue
		}
		localPort := localAddr[colonIdx+1:]
		if !strings.EqualFold(localPort, hexPort) {
			continue
		}

		uid, err := strconv.Atoi(fields[7])
		if err != nil {
			continue
		}
		return uid, nil
	}

	return -1, fmt.Errorf("no socket found for local port %d", port)
}
