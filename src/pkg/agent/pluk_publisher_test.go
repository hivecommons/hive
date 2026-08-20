package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pluk_publisher_test.go covers kubestellar/hive#4285: the pipe-pane invocation
// that is supposed to produce /var/run/pluk/logs/<session>.jsonl.
//
// `pluk watch` classifies stdin and prints events to stdout; it opens no file.
// tmux pipe-pane feeds the pane into that command's stdin and does nothing with
// its stdout. So the redirect is the only thing that writes the log, and
// --include-raw is the only thing that puts raw_output in it — which is the sole
// event type src/deploy/hive-panes.sh reads. Both were dropped in #1759 when the
// Go `pluk-publish` binary (a publisher, which wrote the file itself) was
// replaced by `pluk watch`, and the log has been empty ever since.

// TestPlukPipePaneCmdRedirectsToTheSessionLog is the core regression. Without a
// redirect the classified events go to a stdout nothing is reading, and every
// consumer of the log directory waits forever on a file that is never created.
func TestPlukPipePaneCmdRedirectsToTheSessionLog(t *testing.T) {
	logFile := plukSessionLogPath(plukRunDir, "hive-brainstorm")
	cmd := plukPipePaneCmd("/usr/bin/pluk", "hive-brainstorm", "claude", logFile)

	if !strings.Contains(cmd, ">>") {
		t.Fatalf("#4285: the pipe-pane command has no redirect, so pluk's events go "+
			"to a stdout nobody reads and the log file is never written: %s", cmd)
	}

	// Appending, never truncating: a re-attach (agent restart, pane respawn)
	// must not blow away the history hive-panes is about to read.
	if strings.Contains(cmd, "> "+shellQuote(logFile)) && !strings.Contains(cmd, ">> "+shellQuote(logFile)) {
		t.Fatalf("the redirect truncates instead of appending: %s", cmd)
	}
	if !strings.HasSuffix(cmd, ">> "+shellQuote(logFile)) {
		t.Fatalf("the command does not end by appending to %s: %s", logFile, cmd)
	}
}

// TestPlukPipePaneCmdRequestsRawOutput pins the second half. `pluk watch`
// defaults includeRaw to false, so without the flag the log exists but contains
// no raw_output at all — and raw_output is the only event hive-panes.sh keeps.
func TestPlukPipePaneCmdRequestsRawOutput(t *testing.T) {
	cmd := plukPipePaneCmd("/usr/bin/pluk", "hive-architect", "codex", "/var/run/pluk/logs/hive-architect.jsonl")

	if !strings.Contains(cmd, "--include-raw") {
		t.Fatalf("#4285: --include-raw missing. pluk watch defaults includeRaw to false, "+
			"so hive-panes.sh — which keeps only raw_output events — would still show "+
			"nothing for every agent: %s", cmd)
	}
	// The subcommand and the CLI selector must survive alongside it.
	if !strings.Contains(cmd, " watch ") {
		t.Fatalf("the watch subcommand is missing: %s", cmd)
	}
	if !strings.Contains(cmd, "--cli="+shellQuote("codex")) {
		t.Fatalf("the --cli selector did not carry the backend: %s", cmd)
	}
}

// TestPlukPipePaneCmdQuotesEveryOperand matters more now than it did before:
// the command already went to a shell, but the redirect target is new and is
// built from the session name. An unquoted name with a space would split the
// redirect and send the log somewhere else entirely.
func TestPlukPipePaneCmdQuotesEveryOperand(t *testing.T) {
	session := "hive-odd name"
	logFile := plukSessionLogPath(plukRunDir, session)
	cmd := plukPipePaneCmd("/opt/my pluk/bin/pluk", session, "claude", logFile)

	for _, operand := range []string{"/opt/my pluk/bin/pluk", session, logFile} {
		if !strings.Contains(cmd, shellQuote(operand)) {
			t.Errorf("operand %q is not shell-quoted in: %s", operand, cmd)
		}
	}

	// A name carrying shell syntax must land inside quotes, not as a second command.
	nasty := "hive-a; touch /tmp/pwned"
	nastyCmd := plukPipePaneCmd("/usr/bin/pluk", nasty, "claude", plukSessionLogPath(plukRunDir, nasty))
	if !strings.Contains(nastyCmd, shellQuote(nasty)) {
		t.Errorf("a session name containing shell syntax was not quoted: %s", nastyCmd)
	}
}

// TestPlukSessionLogPathMatchesItsConsumers pins the path against the two
// readers, which hardcode it independently: inception_watcher.go builds
// "<plukLogDir>/hive-brainstorm.jsonl" and hive-panes.sh globs
// "$LOG_DIR"/hive-*.jsonl. If this drifts, both go quiet without erroring.
func TestPlukSessionLogPathMatchesItsConsumers(t *testing.T) {
	got := plukSessionLogPath(plukRunDir, "hive-brainstorm")
	const want = "/var/run/pluk/logs/hive-brainstorm.jsonl"
	if got != want {
		t.Fatalf("log path = %q, want %q — the inception watcher tails the literal %q",
			got, want, want)
	}

	matched, err := filepath.Match("/var/run/pluk/logs/hive-*.jsonl", got)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if !matched {
		t.Fatalf("%q does not match the hive-*.jsonl glob hive-panes.sh iterates", got)
	}
}

// TestEnsurePlukLogFileIsPeerReadableUnderTightUmask is the reason the file is
// created here at all rather than by the shell's `>>`.
//
// hive-panes is run BY one agent to read every OTHER agent's log, and agent UIDs
// differ. If the pane shell's umask creates the log, a 0077 umask yields a 0600
// file no peer can read — the peer listing silently stays empty and nothing
// reports an error. Pre-creating with an explicit mode makes `>>` append to a
// file that is already group-readable.
func TestEnsurePlukLogFileIsPeerReadableUnderTightUmask(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "pluk")
	if err := ensurePlukRunDirs(runDir); err != nil {
		t.Fatalf("ensure pluk dirs: %v", err)
	}

	oldUmask := setUmask(0o077)
	defer setUmask(oldUmask)

	path, err := ensurePlukLogFile(runDir, "hive-architect")
	if err != nil {
		t.Fatalf("ensure pluk log file: %v", err)
	}
	if want := plukSessionLogPath(runDir, "hive-architect"); path != want {
		t.Fatalf("returned path = %q, want %q", path, want)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != 0o660 {
		t.Fatalf("#4285: log mode = %04o, want 0660. Under a tight umask the shell's "+
			">> would leave this 0600 and hive-panes would read nothing for this agent", got)
	}
	if info.Mode().Perm()&0o040 == 0 {
		t.Errorf("log is not group-readable (%s) — peer agents cannot read it", info.Mode())
	}
	if info.Mode().Perm()&0o007 != 0 {
		t.Errorf("log is accessible to other (%s) — the node group is the intended boundary", info.Mode())
	}
}

// TestEnsurePlukLogFileAppendsRatherThanTruncating: agents restart and panes get
// respawned, so this runs again over a log that already has content. Losing it
// would blank the peer listing every time an agent bounced.
func TestEnsurePlukLogFileAppendsRatherThanTruncating(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "pluk")
	if err := ensurePlukRunDirs(runDir); err != nil {
		t.Fatalf("ensure pluk dirs: %v", err)
	}

	path, err := ensurePlukLogFile(runDir, "hive-brainstorm")
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	const existing = `{"type":"raw_output","data":{"line":"hello"}}` + "\n"
	if err := os.WriteFile(path, []byte(existing), 0o660); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	if _, err := ensurePlukLogFile(runDir, "hive-brainstorm"); err != nil {
		t.Fatalf("second ensure: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != existing {
		t.Fatalf("re-attaching truncated the log: got %q, want %q", got, existing)
	}
}

// TestEnsurePlukLogFileWidensATightExistingLog: O_CREATE's mode is umask-filtered
// and does not apply to a file that already exists, so a log left behind by an
// earlier run — or by the pre-#4285 shell redirect — has to be widened back to
// the shared mode rather than inherited as-is.
func TestEnsurePlukLogFileWidensATightExistingLog(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "pluk")
	if err := ensurePlukRunDirs(runDir); err != nil {
		t.Fatalf("ensure pluk dirs: %v", err)
	}

	path := plukSessionLogPath(runDir, "hive-legacy")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("seed tight log: %v", err)
	}

	if _, err := ensurePlukLogFile(runDir, "hive-legacy"); err != nil {
		t.Fatalf("ensure over existing: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o660 {
		t.Fatalf("a pre-existing 0600 log stayed %04o — peers still cannot read it", got)
	}
}
