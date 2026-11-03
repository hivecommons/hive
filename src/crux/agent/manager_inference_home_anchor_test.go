package agent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Audit F12 residual: the inference-home ANCHOR directory was never
// symlink-checked. ensureClaudeSettings called os.MkdirAll on a path derived
// from the predictable /tmp/.claude-inference-home-<agent>, and
// tightenInferenceHome then ran path-based os.Chown/os.Chmod over it. All three
// syscalls follow symlinks, so a local UID that pre-planted that name
// redirected the hive's own (root, in the hosted container) privilege onto an
// arbitrary directory. The leaf-file O_NOFOLLOW writers do not cover this —
// they guard files, not the anchor directory.
//
// These tests use the inferenceHomePrefixOverride seam so nothing touches the
// real /tmp.

// inferenceHomeTestPrefix points the per-agent inference HOME at a fresh temp
// dir and returns the root those homes are created in.
func inferenceHomeTestPrefix(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	claudeInferenceHomePrefixForTest(t, filepath.Join(root, "home-"))
	return root
}

// TestF12_EnsureClaudeSettingsRefusesSymlinkedAnchor is the core regression:
// a symlink pre-planted at the anchor must NOT be followed.
func TestF12_EnsureClaudeSettingsRefusesSymlinkedAnchor(t *testing.T) {
	root := inferenceHomeTestPrefix(t)

	victim := filepath.Join(root, "victim")
	if err := os.Mkdir(victim, 0o700); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}

	anchor := inferenceHomePath("scanner")
	if err := os.Symlink(victim, anchor); err != nil {
		t.Fatalf("symlink anchor: %v", err)
	}

	quietManager().ensureClaudeSettings("scanner", 0)

	// The hive must not have created anything through the link.
	if _, err := os.Stat(filepath.Join(victim, ".claude")); err == nil {
		t.Fatal("ensureClaudeSettings created .claude inside the symlink target: anchor symlink was followed (CWE-59 regression)")
	}
	if got := statMode(t, victim); got != 0o700 {
		t.Fatalf("victim mode = %v, want 0700: anchor symlink was followed", got)
	}
	// The anchor itself must still be the planted link, not a replaced dir.
	info, err := os.Lstat(anchor)
	if err != nil {
		t.Fatalf("lstat anchor: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("anchor is no longer a symlink; the guard must refuse, not silently replace")
	}
}

// TestF12_TightenInferenceHomeRefusesSymlinkedAnchor covers the chown/chmod
// half directly: tightenInferenceHome must not act on a linked directory.
func TestF12_TightenInferenceHomeRefusesSymlinkedAnchor(t *testing.T) {
	root := inferenceHomeTestPrefix(t)

	victim := filepath.Join(root, "victim")
	if err := os.Mkdir(victim, 0o700); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}
	anchor := filepath.Join(root, "linked-home")
	if err := os.Symlink(victim, anchor); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// uid>0 so the chown/chmod loop is actually entered. The chown itself is
	// expected to be refused by the OS for an unprivileged test runner; the
	// assertion is on the CHMOD fallback, which would otherwise widen the
	// victim to 0777 through the link.
	quietManager().tightenInferenceHome("scanner", anchor, filepath.Join(anchor, ".claude"), 12345)

	if got := statMode(t, victim); got != 0o700 {
		t.Fatalf("victim mode = %v, want 0700: tightenInferenceHome followed the anchor symlink (CWE-59 regression)", got)
	}
}

// TestF12_MkdirAllNoFollowRefusesSymlinkedIntermediate ensures the guard covers
// every component, not just the final anchor.
func TestF12_MkdirAllNoFollowRefusesSymlinkedIntermediate(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "victim")
	if err := os.Mkdir(victim, 0o700); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}
	if err := os.Symlink(victim, filepath.Join(root, "anchor")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	err := mkdirAllNoFollow(root, filepath.Join(root, "anchor", ".claude"), inferenceHomeDirMode)
	if !errors.Is(err, errInferenceHomeSymlink) {
		t.Fatalf("mkdirAllNoFollow error = %v, want errInferenceHomeSymlink", err)
	}
	if _, statErr := os.Stat(filepath.Join(victim, ".claude")); statErr == nil {
		t.Fatal("created a directory through a symlinked intermediate component")
	}
}

// TestF12_EnsureClaudeSettingsCreatesRealHome is the POSITIVE CONTROL: with no
// symlink planted, the normal path must still create the home, the settings
// directory and the seeded files. A guard that broke this would be worthless.
func TestF12_EnsureClaudeSettingsCreatesRealHome(t *testing.T) {
	inferenceHomeTestPrefix(t)

	quietManager().ensureClaudeSettings("scanner", 0)

	home := inferenceHomePath("scanner")
	info, err := os.Lstat(home)
	if err != nil {
		t.Fatalf("inference home was not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("inference home is not a directory: mode %v", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); err != nil {
		t.Fatalf("settings.json was not seeded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); err != nil {
		t.Fatalf(".claude.json was not seeded: %v", err)
	}
}

// TestF12_TightenInferenceHomeTightensRealDirectory is the second POSITIVE
// CONTROL: permissions must still actually be applied to a real directory.
func TestF12_TightenInferenceHomeTightensRealDirectory(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	settings := filepath.Join(home, ".claude")
	if err := os.MkdirAll(settings, 0o777); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Defeat umask so the starting mode is unambiguously the wide one.
	if err := os.Chmod(home, 0o777); err != nil {
		t.Fatalf("chmod home: %v", err)
	}

	// uid>0 with an unprivileged runner: chown fails, so the loop takes the
	// documented fallback and restores the shared mode. Either way it must
	// have ACTED on the real directory rather than skipping it.
	quietManager().tightenInferenceHome("scanner", home, settings, 12345)

	got := statMode(t, home)
	if got != inferenceHomeSharedDirMode && got != inferenceHomeDirMode {
		t.Fatalf("home mode = %v, want %v or %v: tightenInferenceHome skipped a real directory",
			got, os.FileMode(inferenceHomeSharedDirMode), os.FileMode(inferenceHomeDirMode))
	}
}
