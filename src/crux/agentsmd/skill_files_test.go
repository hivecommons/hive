package agentsmd

import (
	"os"
	"path/filepath"
	"testing"
)

// A repo may keep standalone skills/ files with no AGENTS.md at all; the
// skills must still load, with an empty body (hivecommons/hive#20009).
func TestParse_SkillsDirWithoutAgentsMd(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, SkillsDirName, "go-testing.md"), "Prefer t.TempDir.\n")

	cfg, err := Parse(dir, nil)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if cfg.Body != "" {
		t.Errorf("Body = %q, want empty with no AGENTS.md", cfg.Body)
	}
	if got := cfg.Skills["go-testing"]; got != "Prefer t.TempDir." {
		t.Errorf("skills/go-testing.md = %q, want it loaded without AGENTS.md", got)
	}
}

// Front matter in a standalone skill file is metadata, not instructions; it
// must not reach the agent prompt.
func TestParse_SkillFileFrontMatterStripped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, AgentsFileName), "# Title\n")
	writeFile(t, filepath.Join(dir, SkillsDirName, "flat.md"), "---\nname: flat\ndescription: d\n---\nFLAT BODY\n")

	cfg, err := Parse(dir, nil)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if got := cfg.Skills["flat"]; got != "FLAT BODY" {
		t.Errorf("skills/flat.md = %q, want front matter stripped", got)
	}
}

// A committed symlink in skills/ must never make Hive read a file from outside
// the repo root into a prompt, whether the link is one file or the directory.
func TestParse_SkillSymlinksRefused(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "host-file.md")
	writeFile(t, secret, "HOST FILE CONTENT")

	t.Run("file", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, AgentsFileName), "# Title\n")
		writeFile(t, filepath.Join(dir, SkillsDirName, "ok.md"), "fine")
		if err := os.Symlink(secret, filepath.Join(dir, SkillsDirName, "link.md")); err != nil {
			t.Skipf("symlinks unsupported: %v", err)
		}
		cfg, _ := Parse(dir, nil)
		if _, ok := cfg.Skills["link"]; ok {
			t.Errorf("symlinked skill file was loaded: %q", cfg.Skills["link"])
		}
		if cfg.Skills["ok"] != "fine" {
			t.Errorf("regular skill next to a symlink = %q, want it still loaded", cfg.Skills["ok"])
		}
	})

	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(dir, SkillsDirName)); err != nil {
			t.Skipf("symlinks unsupported: %v", err)
		}
		cfg, _ := Parse(dir, nil)
		if len(cfg.Skills) != 0 {
			t.Errorf("symlinked skills/ directory was read: %v", cfg.Skills)
		}
	})
}
