package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// N15 (CWE-367/732/20): per-agent control state in the pod-shared /tmp must be
// owner-only and must not follow symlinks.
//
// Two files matter, both at paths derived from the agent NAME (guessable):
//
//   - /tmp/.hive-mode-<name>       — bin/gh-wrapper.sh takes its enforcement mode
//     from this file IN PREFERENCE to the trustworthy env var, so planting it
//     un-gates the victim agent (that half is the N15A fail-closed fix).
//   - /tmp/.hive-bootstrap-<name>.txt — goose is launched with the contents as
//     its first instruction, so planting it injects an attacker-chosen prompt
//     that runs with the victim's credentials at message zero.
//
// Both were written with a plain os.WriteFile at 0o644: world-readable, and
// symlink-following (os.WriteFile passes no O_NOFOLLOW), so a pre-planted link
// redirected the hive's own write to any file the process could reach.
//
// NOTE on scope: an earlier reading of this called the bootstrap path arbitrary
// CODE execution, on the theory that a nested $(...) inside the file would be
// expanded because the launch string is run by a shell. That is wrong — POSIX
// shells expand command substitution once and do not re-scan the result, so the
// content reaches goose as literal text. The finding is prompt injection, which
// these file permissions are the correct fix for.

func TestWriteAgentStateFileIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".hive-mode-victim")

	if err := writeAgentStateFile(path, []byte("ISSUES_ONLY")); err != nil {
		t.Fatalf("writeAgentStateFile: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600 — a group/world-readable control file lets one agent read "+
			"or (with a writable /tmp) steer another", got)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "ISSUES_ONLY" {
		t.Errorf("content = %q err=%v, want ISSUES_ONLY", b, err)
	}
}

// TestWriteAgentStateFileTightensExistingMode covers the upgrade path: a file
// left at 0644 by a previous release must be tightened, not inherited. O_CREATE
// applies its mode only when creating, so without the explicit Chmod the old
// permissive mode would survive every rewrite.
func TestWriteAgentStateFileTightensExistingMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".hive-mode-legacy")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := writeAgentStateFile(path, []byte("ADVISORY")); err != nil {
		t.Fatalf("writeAgentStateFile: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600 — a pre-existing 0644 file must be tightened on rewrite", got)
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

	link := filepath.Join(dir, ".hive-mode-planted")
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

// TestWriteAgentStateFileOverwritesRegularFile guards against over-correction:
// these files are legitimately rewritten on every mode change and relaunch, so
// the hardening must not be O_EXCL.
func TestWriteAgentStateFileOverwritesRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".hive-mode-rewrite")

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
