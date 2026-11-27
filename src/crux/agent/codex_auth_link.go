package agent

import (
	"errors"
	"os/exec"
)

// codexAuthKeptExitCode is the exit status codexAuthLinkScript uses to report
// that it left a non-link file in place.
const codexAuthKeptExitCode = 3

// codexAuthLinkScript links $2 to $1 ONLY when $2 is absent or already a
// symlink (dangling or not). Anything else at $2, a regular file above all, is
// left untouched and reported with codexAuthKeptExitCode.
//
// Why not a bare `ln -sfn` (#20002): `codex logout` removes the per-agent
// symlink, and the next `codex login` in the agent's terminal (CODEX_HOME is
// the per-agent home) then writes a REGULAR auth.json there. `ln -sfn` deleted
// that file on the next agent start with no copy, losing the new login. The
// script never reads the file; it only tests what kind of path it is.
const codexAuthLinkScript = `if [ -L "$2" ] || [ ! -e "$2" ]; then exec ln -sfn "$1" "$2"; fi; exit 3`

// codexAuthLinkCommand builds the command that runs codexAuthLinkScript as the
// agent user (the manager cannot write into the agent-owned CODEX_HOME). It is
// a variable so tests can run the same script without su-exec on synthetic
// files.
var codexAuthLinkCommand = func(userSpec, shared, link string) *exec.Cmd {
	return exec.Command("su-exec", userSpec, "sh", "-c", codexAuthLinkScript, "sh", shared, link)
}

// linkCodexAuth runs a codexAuthLinkScript command. kept is true when a
// non-link file was already at the link path and was left in place.
func linkCodexAuth(cmd *exec.Cmd) (kept bool, err error) {
	output, err := cmd.CombinedOutput()
	if err == nil {
		return false, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == codexAuthKeptExitCode {
		return true, nil
	}
	return false, outputErr("linking codex auth", err, output)
}
