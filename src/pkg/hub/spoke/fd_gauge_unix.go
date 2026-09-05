//go:build unix

package spoke

import "syscall"

// FDSoftLimit returns the RLIMIT_NOFILE soft limit for this process, or 0 when
// it cannot be read. Reported alongside OpenFDCount in the heartbeat so the
// gauge is ulimit-relative — 20k open FDs is an emergency under a 65k limit
// and routine under 1M (#3875).
func FDSoftLimit() uint64 {
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
		return 0
	}
	return uint64(rl.Cur)
}
