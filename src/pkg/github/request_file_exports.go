package github

import (
	"errors"
	"time"
)

// The request-file primitives the GitHub relays use, exported for the forge
// relay in cmd/hive (hivecommons/hive#19670) so a forge-only hive reads agent
// request files with the same safety rules. They wrap the unexported helpers
// and change nothing on the GitHub relays.

// ReadRequestFile reads an agent-written request file the way the GitHub
// relays do: no symlinks, FIFOs or other non-regular files, and at most the
// shared request size cap. A file that can never become a valid request
// returns an error for which IsRequestFileRejected is true.
func ReadRequestFile(path string) ([]byte, error) {
	data, _, err := readUntrustedFile(path, requestMaxBytes)
	return data, err
}

// IsRequestFileRejected reports whether err from ReadRequestFile means the
// file is unsafe (symlink, FIFO, oversize) rather than merely gone.
func IsRequestFileRejected(err error) bool {
	return errors.Is(err, errDropBoxFileRejected)
}

// RequestFileOwnerUID returns the UID that owns path, or -1 when it cannot be
// read. It is the forge-resistance anchor the relay authorizers check.
func RequestFileOwnerUID(path string) int {
	return statUID(nil, path)
}

// RequestFileTorn reports whether an unparsable request file is still young
// enough to be a write in progress, which must be left for the next scan.
func RequestFileTorn(path string, now time.Time) bool {
	return !quarantinable(path, now)
}

// InvalidPRHeadReason is why head can never be a pull request's head branch,
// or "" when it can.
func InvalidPRHeadReason(head string) string {
	return invalidHeadReason(head)
}

// WriteRequestResult writes data atomically next to a request, as the
// relays' .result.json files are written.
func WriteRequestResult(path string, data []byte) error {
	return writeRequestFile(path, data)
}
