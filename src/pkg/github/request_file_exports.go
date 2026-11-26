package github

import (
	"errors"
	"log/slog"
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

// ReservedLabelForAgents reports whether label is hive-controlled (hold and
// gate labels, approved-direction, needs-human, blocked, hive/* and stage/*,
// and autoMergeLabel) and so may not be added or removed by an agent. It is
// the check the GitHub label relay applies, without that relay's narrow
// per-label allowances, so a caller using it alone fails closed.
func ReservedLabelForAgents(label, autoMergeLabel string) bool {
	return reservedLabelForAgents(label, autoMergeLabel)
}

// EnsureRequestDir creates one agent-facing request queue with the drop-box
// mode every relay uses (group-writable, setgid, sticky). It returns false
// when the directory cannot be created.
func EnsureRequestDir(logger *slog.Logger, kind, dir string) bool {
	return ensureRequestDir(logger, kind, dir)
}

// ValidPushBranchName reports whether branch is a name the push-branch relay
// will put in a refspec (hivecommons/hive#19916).
func ValidPushBranchName(branch string) bool {
	return validPushBranchName(branch)
}

// ValidatePushBranchDir checks the agent-named checkout of a push-branch
// request: absolute, an existing directory, and owned by fileUID when
// fileUID > 0.
func ValidatePushBranchDir(dir string, fileUID int) error {
	return validatePushBranchDir(dir, fileUID)
}

// PushBranchGitEnv is the environment the push-branch relay runs git with in
// an agent-owned checkout: no prompts, hooks, fsmonitor or credential
// helpers from the checkout, and token as a Basic auth extraHeader scoped to
// remote. An empty token adds no header.
func PushBranchGitEnv(dir, remote, token string) []string {
	header := ""
	if token != "" {
		header = gitBasicAuthHeader(token)
	}
	return pushBranchGitEnv(dir, remote, header)
}

// FirstOutputLines returns the first n non-empty lines of a command's output,
// as the relays quote git's diagnostics.
func FirstOutputLines(output string, n int) string {
	return firstOutputLines(output, n)
}
