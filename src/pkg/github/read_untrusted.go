package github

import (
	"errors"
	"fmt"
)

// Agent-writable drop-box directories (the request queues in request_dirs.go
// and the token-access spool in token_access_audit.go) accept files of ANY
// type from any agent UID: regular files, but also FIFOs and symlinks. Every
// hive-side reader used a plain os.ReadFile on the dropped name, which gave
// any agent two one-liner attacks (#7576):
//
//   - mkfifo <dir>/x.json — os.ReadFile blocks forever inside open(2) (a FIFO
//     opened O_RDONLY with no writer never returns), silently and permanently
//     stalling the watcher goroutine: no more agent-initiated PRs, issues,
//     merges, reviews, or token-access audit ingestion until restart — and the
//     FIFO survives the restart.
//   - ln -s /dev/zero <dir>/x.json — os.ReadFile follows the link and reads a
//     never-EOF stream into memory, OOM-killing the whole hive process. A
//     symlink to any other hive-readable file launders that file's content
//     into the reader instead.
//
// readUntrustedFile is the only sanctioned way to read a file an agent could
// have dropped: it refuses symlinks, never blocks on FIFOs, verifies AFTER
// opening that the descriptor is a regular file within the size cap, and
// returns the post-open FileInfo so callers attribute ownership to the file
// actually read (see readTokenAccessEvent).

// errDropBoxFileRejected marks a drop-box file that can never become readable
// by retrying: wrong type (FIFO, symlink, device, socket) or over the size
// cap. Callers should quarantine or remove the file instead of rescanning it
// every tick.
var errDropBoxFileRejected = errors.New("drop-box file rejected")

// requestMaxBytes caps one request file in any of the request queues. A real
// request (PR/issue/merge/review JSON, including a long PR body) is well under
// this; the cap only exists to stop a dropped file from becoming a memory
// exhaustion channel.
const requestMaxBytes int64 = 1 << 20

func dropBoxRejectedError(what string) error {
	return fmt.Errorf("%w: %s", errDropBoxFileRejected, what)
}
