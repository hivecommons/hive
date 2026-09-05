package spoke

import "os"

// fdDirs are the per-process file-descriptor listings tried in order:
// /proc/self/fd on Linux (the production container target), /dev/fd as the
// BSD/macOS dev-machine fallback.
var fdDirs = []string{"/proc/self/fd", "/dev/fd"}

// OpenFDCount returns the number of file descriptors this process currently
// holds, or 0 when it cannot be determined. It exists for the heartbeat FD
// gauge (see HeartbeatPayload.OpenFDs, #3875): a leak that self-DoSed spokes
// at 92k FDs was invisible until someone hand-inspected /proc. A directory
// read costs microseconds at healthy FD counts and is paid once per beat.
//
// Callers must treat 0 as UNKNOWN, not as "no descriptors": a live process
// always holds at least the directory handle used for the read itself.
//
// Readdirnames, not os.ReadDir: ReadDir lstats entries whose type the
// filesystem does not report, and on /dev/fd that stats the listing's own
// (already-consumed) descriptor — "bad file descriptor" instead of a count.
// Names alone are all a count needs.
func OpenFDCount() int {
	for _, dir := range fdDirs {
		f, err := os.Open(dir)
		if err != nil {
			continue
		}
		names, err := f.Readdirnames(-1)
		_ = f.Close() // read-only handle
		if err == nil {
			return len(names)
		}
	}
	return 0
}
