package dashboard

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestBuildSkills_MissingDirReportsUnavailable proves the "not configured"
// case: a hive with no skills directory at all must report available=false /
// loaded=0 rather than fabricating a count. Dir is also asserted empty on
// this path — buildSkills returns FrontendSkills{Available: false, Loaded: 0}
// with no Dir set when os.Stat fails.
func TestBuildSkills_MissingDirReportsUnavailable(t *testing.T) {
	orig := skillsConventionalDir
	t.Cleanup(func() { skillsConventionalDir = orig })
	skillsConventionalDir = filepath.Join(t.TempDir(), "does-not-exist")

	got := buildSkills()
	if got.Available {
		t.Errorf("Available = true for a missing skills dir, want false")
	}
	if got.Loaded != 0 {
		t.Errorf("Loaded = %d for a missing skills dir, want 0", got.Loaded)
	}
	if got.Dir != "" {
		t.Errorf("Dir = %q for a missing skills dir, want empty", got.Dir)
	}
}

// TestBuildSkills_EmptyDirIsAvailableWithZeroLoaded is the positive control
// for the "directory exists but has nothing in it" case: this must be
// distinguishable from "not configured" — Available true, Loaded 0, and Dir
// populated — because an operator who provisioned an (as yet empty) skills
// directory should see "configured, empty", not "not configured".
func TestBuildSkills_EmptyDirIsAvailableWithZeroLoaded(t *testing.T) {
	dir := t.TempDir()
	orig := skillsConventionalDir
	t.Cleanup(func() { skillsConventionalDir = orig })
	skillsConventionalDir = dir

	got := buildSkills()
	if !got.Available {
		t.Errorf("Available = false for an empty-but-present skills dir, want true")
	}
	if got.Loaded != 0 {
		t.Errorf("Loaded = %d for an empty skills dir, want 0", got.Loaded)
	}
	if got.Dir != dir {
		t.Errorf("Dir = %q, want %q", got.Dir, dir)
	}
}

// TestBuildSkills_PopulatedDirCountsSkillFiles is the end-to-end proof that
// buildSkills actually parses skill files via skillreg and that the count
// reaches the FrontendSkills payload. It also proves non-".md" files are
// ignored and subdirectories are skipped, matching skillreg.Load's documented
// contract.
func TestBuildSkills_PopulatedDirCountsSkillFiles(t *testing.T) {
	dir := t.TempDir()
	orig := skillsConventionalDir
	t.Cleanup(func() { skillsConventionalDir = orig })
	skillsConventionalDir = dir

	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("triage.md", "---\nname: triage\ndescription: triage inbound issues\n---\n\nBody.\n")
	write("review.md", "---\nname: review\ndescription: review a pull request\n---\n\nBody.\n")
	// Not a skill file — must be ignored by extension, not counted.
	write("notes.txt", "not a skill")
	// Subdirectories are skipped, even ones ending in .md-like names.
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got := buildSkills()
	if !got.Available {
		t.Fatalf("Available = false with a populated skills dir, want true")
	}
	if got.Loaded != 2 {
		t.Errorf("Loaded = %d, want 2 (triage.md + review.md; notes.txt and nested/ must not count)", got.Loaded)
	}
	if got.Dir != dir {
		t.Errorf("Dir = %q, want %q", got.Dir, dir)
	}
}

// TestBuildSkills_UnreadableDirReportsUnavailable proves a directory that
// exists but cannot be stat'd (parent lacks execute permission, so the OS
// denies access before the "is it a directory" check ever runs) fails safe:
// buildSkills's os.Stat call errors, which lands in the same Available=false
// / Loaded=0 / Dir-unset branch as a missing directory. This confirms the
// probe never panics or crashes the status builder on a permission error.
func TestBuildSkills_UnreadableDirReportsUnavailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission semantics")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}

	parent := t.TempDir()
	dir := filepath.Join(parent, "locked")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Strip execute permission on the parent so stat-ing a path beneath it
	// (the "locked" dir itself, via its parent's directory entry) is denied.
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatalf("chmod parent: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	orig := skillsConventionalDir
	t.Cleanup(func() { skillsConventionalDir = orig })
	skillsConventionalDir = dir

	got := buildSkills()
	if got.Available {
		t.Errorf("Available = true for an unreadable skills dir, want false")
	}
	if got.Loaded != 0 {
		t.Errorf("Loaded = %d for an unreadable skills dir, want 0", got.Loaded)
	}
	if got.Dir != "" {
		t.Errorf("Dir = %q for an unreadable skills dir, want empty (os.Stat fails before Dir is ever set)", got.Dir)
	}
}
