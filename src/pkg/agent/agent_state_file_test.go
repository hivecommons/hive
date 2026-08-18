package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// N15 (CWE-367/732/20): per-agent control state in the pod-shared /tmp must be
// owner-WRITE-only and must not follow symlinks. It must remain world-readable,
// because its consumers run as the per-agent UID, not the hive uid — see
// agentStateFileMode and #3679/#3881/#3882.
//
// Two files matter, both at paths derived from the agent NAME (guessable):
//
//   - /tmp/.hive-mode-<name>       — bin/gh-wrapper.sh takes its enforcement mode
//     from this file. It is Hive-owned, target-agent-group-readable, and not
//     writable by any agent.
//   - /tmp/.hive-bootstrap-<name>.txt — goose is launched with the contents as
//     its first instruction, so planting it injects an attacker-chosen prompt
//     that runs with the victim's credentials at message zero.
//
// Both were written with a plain os.WriteFile at 0o644, symlink-following
// (os.WriteFile passes no O_NOFOLLOW), so a pre-planted link redirected the
// hive's own write to any file the process could reach. The 0644 itself was
// not the hole: /tmp is sticky, so an agent UID cannot replace a hive-owned
// file there, and world-read is REQUIRED — gh-wrapper.sh, git-credential-hive.sh
// and goose's `--text "$(cat ...)"` all read these files as the agent UID. The
// original N15 fix tightened to 0600 and locked every agent out of gh and git
// push (#3679/#3881); the mode is now 0644 with the symlink hardening kept.
//
// NOTE on scope: an earlier reading of this called the bootstrap path arbitrary
// CODE execution, on the theory that a nested $(...) inside the file would be
// expanded because the launch string is run by a shell. That is wrong — POSIX
// shells expand command substitution once and do not re-scan the result, so the
// content reaches goose as literal text. The finding is prompt injection, which
// these file permissions are the correct fix for.

// TestWriteAgentStateFileIsOwnerWriteWorldRead pins the mode: only the hive uid
// may write (no group/other write bits — that is what would let one agent steer
// another), and everyone may read (the per-agent UID that consumes the file is
// not the hive uid). The write must be exact regardless of the caller's umask.
func TestWriteAgentStateFileIsOwnerWriteWorldRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".hive-bootstrap-victim.txt")

	restore := syscall.Umask(0o077) // the restrictive umask that produced 0600 in prod (#3882)
	defer syscall.Umask(restore)

	if err := writeAgentStateFile(path, []byte("ISSUES_ONLY")); err != nil {
		t.Fatalf("writeAgentStateFile: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %o, want 644 — group/world WRITE lets one agent steer another, "+
			"but the per-agent UID must be able to READ its own mode/bootstrap file", got)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "ISSUES_ONLY" {
		t.Errorf("content = %q err=%v, want ISSUES_ONLY", b, err)
	}
}

// TestWriteAgentStateFileRepairsExistingMode covers the upgrade path in both
// directions: a file left 0600 by the #3172 build (agent-unreadable) must be
// widened, and a file left group/world-writable must be tightened. O_CREATE
// applies its mode only when creating, so without the explicit Chmod the old
// mode would survive every rewrite.
func TestWriteAgentStateFileRepairsExistingMode(t *testing.T) {
	for _, seed := range []os.FileMode{0o600, 0o666} {
		dir := t.TempDir()
		path := filepath.Join(dir, ".hive-mode-legacy")
		if err := os.WriteFile(path, []byte("old"), seed); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := os.Chmod(path, seed); err != nil {
			t.Fatalf("seed chmod: %v", err)
		}

		if err := writeAgentStateFile(path, []byte("ADVISORY")); err != nil {
			t.Fatalf("writeAgentStateFile: %v", err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if got := fi.Mode().Perm(); got != 0o644 {
			t.Errorf("seed %o: mode = %o, want 644 — a pre-existing file must be repaired on rewrite", seed, got)
		}
	}
}

// TestWriteAgentStateFileRefusesSymlink is the core of N15: a symlink planted at
// the predictable path must make the write FAIL, not silently redirect it.
func TestWriteAgentStateFileRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim-target")
	if err := os.WriteFile(victim, []byte("ORIGINAL"), 0o600); err != nil {
		t.Fatalf("seed victim: %v", err)
	}

	link := filepath.Join(dir, ".hive-bootstrap-planted.txt")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := writeAgentStateFile(link, []byte("ISSUES_PRS_MERGE"))
	if err == nil {
		t.Fatal("writing through a planted symlink SUCCEEDED — O_NOFOLLOW is not in effect; " +
			"an attacker can redirect the hive's own write to an arbitrary file")
	}

	// The symlink target must be untouched.
	b, readErr := os.ReadFile(victim)
	if readErr != nil {
		t.Fatalf("read victim: %v", readErr)
	}
	if string(b) != "ORIGINAL" {
		t.Fatalf("symlink target was CLOBBERED (now %q) — the write followed the link", b)
	}
}

// TestWriteAgentControlFileChmodsViaDescriptor pins the #3175 fix at source
// level: the mode-tightening step must go through the open descriptor
// (f.Chmod), never through the pathname after Close. O_NOFOLLOW only proves
// the path is not a symlink at OPEN time; a path-based os.Chmod issued in the
// close→chmod window can be redirected by swapping the pathname for a symlink
// in shared /tmp. The runtime tests above are the positive control that the
// tightening still happens; this asserts HOW it happens, which no runtime
// probe can observe reliably (the race window is microseconds).
func TestWriteAgentControlFileChmodsViaDescriptor(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(file), "manager.go"))
	if err != nil {
		t.Fatalf("read manager.go: %v", err)
	}
	text := string(body)
	start := strings.Index(text, "func writeAgentControlFile(")
	if start < 0 {
		t.Fatal("writeAgentControlFile not found in manager.go")
	}
	end := strings.Index(text[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not delimit writeAgentControlFile body")
	}
	fn := text[start : start+end]
	if !strings.Contains(fn, "f.Chmod(mode)") {
		t.Fatal("writeAgentControlFile must apply the requested mode via the open descriptor (f.Chmod) — " +
			"see #3175: a path-based chmod after Close can be symlink-swapped")
	}
	if strings.Contains(fn, "os.Chmod(") {
		t.Fatal("writeAgentControlFile must not use path-based os.Chmod — " +
			"the close→chmod window is symlink-swappable in shared /tmp (#3175)")
	}
}

// TestWriteAgentStateFileOverwritesRegularFile guards against over-correction:
// these files are legitimately rewritten on every mode change and relaunch, so
// the hardening must not be O_EXCL.
func TestWriteAgentStateFileOverwritesRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".hive-bootstrap-rewrite.txt")

	if err := writeAgentStateFile(path, []byte("ISSUES_ONLY")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := writeAgentStateFile(path, []byte("NO_GITHUB")); err != nil {
		t.Fatalf("rewrite must succeed (mode changes rewrite this file): %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "NO_GITHUB" {
		t.Fatalf("content = %q err=%v, want NO_GITHUB (truncating rewrite)", b, err)
	}
}

func TestWriteAgentModeFileIsGroupReadableButNotWritable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".hive-mode-quality")
	if err := writeAgentModeFile(path, []byte("ISSUES_AND_PRS")); err != nil {
		t.Fatalf("writeAgentModeFile: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o640 {
		t.Fatalf("mode = %o, want 640 so isolated agent UIDs can read policy while only Hive can write it", got)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		t.Fatalf("mode file is group/world writable: %o", fi.Mode().Perm())
	}
}

func TestWriteAgentModeFileRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("ORIGINAL"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".hive-mode-quality")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := writeAgentModeFile(link, []byte("ISSUES_AND_PRS")); err == nil {
		t.Fatal("writing a mode through a planted symlink succeeded")
	}
	if got, err := os.ReadFile(victim); err != nil || string(got) != "ORIGINAL" {
		t.Fatalf("symlink target changed: content=%q err=%v", got, err)
	}
}

func TestWriteModeFileForAgentRejectsUnknownGroup(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may chown to arbitrary numeric groups")
	}
	path := filepath.Join(t.TempDir(), ".hive-mode-quality")
	if err := writeModeFileForAgent(path, []byte("ISSUES_AND_PRS"), 2147483646); err == nil {
		t.Fatal("isolated mode writer accepted an unavailable agent group")
	}
}
