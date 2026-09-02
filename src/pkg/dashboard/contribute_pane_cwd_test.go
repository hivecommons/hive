package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A contributor's agy backend died ~30s after every task delivery, exiting 2,
// with no crash, no log and no signal. The cause was not agy, the relay or the
// prompt: the tmux SERVER had been running since a Go test created it hours
// earlier, holding a working directory inside a nested clone — v2/pkg/agent,
// orphaned when the repo renamed v2/ -> src/. Every pane that server forked
// inherited the deleted cwd ("shell-init: error retrieving current directory"),
// and agy refuses to run without a resolvable one. claude/codex/goose tolerate
// it, which is why it looked backend-specific.
//
// `tmux new-session -c <valid path>` does NOT rescue this: on a server whose own
// cwd is gone, the pane is still forked into the deleted directory. Only a cd in
// the launch command itself is reliable, so these tests pin the cd — reading the
// Justfile (and, for the container entrypoint below, contributor-agent.sh)
// rather than restating them, so neither spawn site can regress quietly.
//
// The Justfile's local-mode recipe and contributor-relay.sh's relaunchCLI() were
// fixed first; contributor-agent.sh — the DEFAULT docker/container-mode
// entrypoint (`just contribute-hive` defaults to mode="docker") — launches the
// CLI the identical way and is just as exposed, so it gets the identical fix and
// the identical style of regression test.

func fileSource(t *testing.T, relPath string) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", relPath)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func justfileSource(t *testing.T) string {
	t.Helper()
	return fileSource(t, "Justfile")
}

// contributeHiveLaunchBlock returns the lines of contribute-hive that create the
// tmux session and type the CLI launch command into it.
func contributeHiveLaunchBlock(t *testing.T) string {
	t.Helper()
	src := justfileSource(t)
	start := strings.Index(src, "Create tmux session with the CLI")
	if start < 0 {
		t.Fatal("tmux session creation block not found in the Justfile")
	}
	end := strings.Index(src[start:], "# Start the relay")
	if end < 0 {
		t.Fatal("end of the session creation block not found in the Justfile")
	}
	return src[start : start+end]
}

// contributorAgentLaunchBlock returns the lines of contributor-agent.sh (the
// container entrypoint) that create the tmux session and type the CLI launch
// command into it — the container-mode counterpart of contributeHiveLaunchBlock.
func contributorAgentLaunchBlock(t *testing.T) string {
	t.Helper()
	src := fileSource(t, "bin/contributor-agent.sh")
	start := strings.Index(src, "Create the tmux session for the agent")
	if start < 0 {
		t.Fatal("tmux session creation block not found in contributor-agent.sh")
	}
	end := strings.Index(src[start:], "Auto-dismiss startup prompts")
	if end < 0 {
		t.Fatal("end of the session creation block not found in contributor-agent.sh")
	}
	return src[start : start+end]
}

// requireCdBeforeCmd asserts the send-keys line launching $CMD in block cds
// first, so the CLI starts in a real directory whatever cwd the tmux server
// forked the pane into.
func requireCdBeforeCmd(t *testing.T, block, label string) {
	t.Helper()
	sendKeys := ""
	for _, line := range strings.Split(block, "\n") {
		if strings.Contains(line, "send-keys") && (strings.Contains(line, "$CMD") || strings.Contains(line, "$AGENT_LAUNCH_CMD")) {
			sendKeys = line
			break
		}
	}
	if sendKeys == "" {
		t.Fatalf("%s: no send-keys line launching the CLI found in the block", label)
	}
	if !strings.Contains(sendKeys, "cd ") {
		t.Errorf("%s: the CLI launch must cd into a durable directory first — a tmux server can hand the pane a "+
			"DELETED cwd, and a backend that needs a resolvable one then dies seconds into its first task: %s",
			label, strings.TrimSpace(sendKeys))
	}
}

func TestContributeHivePinsPaneWorkingDirectory(t *testing.T) {
	block := contributeHiveLaunchBlock(t)

	// The load-bearing half: the launch command itself must cd first, so the CLI
	// starts in a real directory whatever cwd the tmux server hands the pane.
	requireCdBeforeCmd(t, block, "Justfile contribute-hive")

	// Defense in depth, correct on a healthy server (but not sufficient alone).
	if !strings.Contains(block, "new-session") || !strings.Contains(block, "-c ") {
		t.Error("new-session should also pass -c so a healthy server starts the pane in the right directory")
	}

	// A poisoned server must be reported, not silently worked around: without a
	// message, the next person debugs an unexplained backend exit for hours.
	if !strings.Contains(block, "pane_current_path") {
		t.Error("the recipe must check the pane's working directory and warn when it is gone")
	}
	if !strings.Contains(block, "tmux kill-server") {
		t.Error("the warning must tell the contributor how to fix a poisoned tmux server")
	}
}

// TestContributorAgentPinsPaneWorkingDirectory covers the DEFAULT docker/
// container-mode spawn site (bin/contributor-agent.sh), which launches the CLI
// into the exact same kind of tmux pane as the Justfile's local-mode recipe but
// was originally missed by the #4046 fix: it relied on `-c "$HIVE_WORKSPACE_DIR"`
// alone, which this suite's sibling test already demonstrates is not sufficient
// once the tmux server's own cwd is gone.
//
// It must also NOT cd into $HIVE_WORKSPACE_DIR itself: that is the very
// directory agents clone per-task repos into (contribute_ws.go's assignment
// prompt), so pinning the launch there would recreate the trap the first time
// that directory is removed and recreated under a still-running server. The cd
// target must be a directory the task lifecycle never deletes or recreates —
// $HOME, the container's own home directory.
func TestContributorAgentPinsPaneWorkingDirectory(t *testing.T) {
	block := contributorAgentLaunchBlock(t)

	requireCdBeforeCmd(t, block, "contributor-agent.sh")

	if strings.Contains(block, `cd $(printf %q "$HIVE_WORKSPACE_DIR")`) {
		t.Error("the launch must not cd into $HIVE_WORKSPACE_DIR itself — that is the per-task clone directory " +
			"the task lifecycle can delete and recreate, which would reintroduce the #4046 trap")
	}

	// Defense in depth, correct on a healthy server (but not sufficient alone).
	if !strings.Contains(block, "new-session") || !strings.Contains(block, "-c ") {
		t.Error("new-session should also pass -c so a healthy server starts the pane in the right directory")
	}
}

func TestContributeHiveExportsLaunchCommandForRelayRelaunch(t *testing.T) {
	block := contributeHiveLaunchBlock(t)
	if !strings.Contains(block, "export AGENT_LAUNCH_CMD=") {
		t.Fatal("local contribute-hive must export the exact launch command for contributor-relay.sh relaunches")
	}
	if !strings.Contains(block, "$AGENT_LAUNCH_CMD") {
		t.Fatal("the first local launch must use the same AGENT_LAUNCH_CMD value the relay will reuse")
	}
	if !strings.Contains(block, "PERM_FLAG") || !strings.Contains(block, "LITELLM_ENV") {
		t.Fatal("AGENT_LAUNCH_CMD must include the resolved permission flags and backend environment prefix")
	}
}
