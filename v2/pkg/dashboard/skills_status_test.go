package dashboard

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBuildSkills_HonestEmptyWhenUnconfigured proves the "not configured" case
// reports available=false / loaded=0 rather than fabricating a count. A hive
// with no /data/skills must not claim a skill registry it does not have.
func TestBuildSkills_HonestEmptyWhenUnconfigured(t *testing.T) {
	orig := skillsDir
	t.Cleanup(func() { skillsDir = orig })
	skillsDir = filepath.Join(t.TempDir(), "does-not-exist")

	got := buildSkills()
	if got.Available {
		t.Errorf("available = true for a missing skills dir, want false")
	}
	if got.Loaded != 0 {
		t.Errorf("loaded = %d for a missing skills dir, want 0", got.Loaded)
	}
	if got.Dir != "" {
		t.Errorf("dir = %q for a missing skills dir, want empty", got.Dir)
	}
}

// TestBuildSkills_CountsRealSkills is the end-to-end proof that the ported
// skillreg registry actually parses skill files and that the count reaches the
// status payload. Compiling is not the same as working: this asserts a real
// non-zero count from real files on disk, and that a malformed file is
// tolerated (skipped) rather than failing the whole probe.
func TestBuildSkills_CountsRealSkills(t *testing.T) {
	dir := t.TempDir()
	orig := skillsDir
	t.Cleanup(func() { skillsDir = orig })
	skillsDir = dir

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
	// Subdirectories are skipped.
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got := buildSkills()
	if !got.Available {
		t.Fatalf("available = false with a populated skills dir, want true")
	}
	if got.Loaded != 2 {
		t.Errorf("loaded = %d, want 2 (triage.md + review.md; notes.txt and nested/ must not count)", got.Loaded)
	}
	if got.Dir != dir {
		t.Errorf("dir = %q, want %q", got.Dir, dir)
	}
}
