// Fixture for hive-exec-shell-dynamic-script. Not compiled: semgrep --test
// parses it and checks each ruleid/ok annotation against the line below it.
package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

const removeScript = `rm -f -- "$1"`

func formatted(branch string) error {
	// ruleid: hive-exec-shell-dynamic-script
	return exec.Command("sh", "-c", fmt.Sprintf("git checkout %s", branch)).Run()
}

func concatenated(ctx context.Context, path string) error {
	// ruleid: hive-exec-shell-dynamic-script
	return exec.CommandContext(ctx, "/bin/bash", "-c", "cat "+path).Run()
}

func builtThenRun(dir string) ([]byte, error) {
	script := fmt.Sprintf("cd %s && make", dir)
	// ruleid: hive-exec-shell-dynamic-script
	return exec.Command("bash", "-lc", script).CombinedOutput()
}

func joined(args []string) error {
	// ruleid: hive-exec-shell-dynamic-script
	return exec.Command("sh", "-c", strings.Join(args, " ")).Run()
}

func throughSuExec(userSpec, path string) error {
	// ruleid: hive-exec-shell-dynamic-script
	return exec.Command("su-exec", userSpec, "sh", "-c", "rm -rf "+path).Run()
}

// The safe shape: a constant script, values as positional parameters.
func positional(userSpec, path string) error {
	// ok: hive-exec-shell-dynamic-script
	return exec.Command("su-exec", userSpec, "sh", "-c", removeScript, "sh", path).Run()
}

func literalScript(path string) error {
	// ok: hive-exec-shell-dynamic-script
	return exec.Command("sh", "-c", `test -d "$1"`, "sh", path).Run()
}

// git -c is a config flag, not a shell.
func gitConfig(helper string) error {
	// ok: hive-exec-shell-dynamic-script
	return exec.Command("git", "-c", "credential.helper="+helper, "fetch").Run()
}

// No shell at all.
func direct(branch string) error {
	// ok: hive-exec-shell-dynamic-script
	return exec.Command("git", "checkout", branch).Run()
}
