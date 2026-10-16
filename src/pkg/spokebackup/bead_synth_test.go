package spokebackup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/hubbackup"
)

func TestBeadSynthVaultBackupRestore(t *testing.T) {
	dir := seedSpoke(t)
	t.Setenv(EnvDataDir, dir)
	fact := filepath.Join("vaults", beadSynthPrefix, "stable-fact.md")
	body := "---\ntitle: Stable fact\n---\nPrivate learned knowledge.\n"
	mustWrite(t, filepath.Join(dir, fact), body)
	// Only the reserved vault's flat markdown files belong in this archive.
	excluded := []string{
		"vaults/imported/other.md",
		"vaults/bead-synth-wiki-other/fact.md",
		"vaults/bead-synth-wiki/attachment.bin",
		"vaults/bead-synth-wiki/nested/fact.md",
		"vaults/bead-synth-wiki/linked.md",
	}
	for _, rel := range excluded[:len(excluded)-1] {
		mustWrite(t, filepath.Join(dir, rel), "excluded")
	}
	if err := os.Symlink(filepath.Join(dir, "gh-app-key.pem"), filepath.Join(dir, excluded[len(excluded)-1])); err != nil {
		t.Fatal(err)
	}

	// No publishing config is present: backup must not depend on opting in.
	res, err := Build(testKey(), "hive-abc123", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	extracted := t.TempDir()
	if _, err := hubbackup.Extract(testKey(), res.Sealed, extracted); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, filepath.Join(extracted, beadSynthPrefix, "stable-fact.md")); got != body {
		t.Fatalf("archived fact = %q, want %q", got, body)
	}
	entries, err := os.ReadDir(filepath.Join(extracted, beadSynthPrefix))
	if err != nil || len(entries) != 1 {
		t.Fatalf("vault archive entries = %v, err = %v; want only stable-fact.md", entries, err)
	}

	dest := t.TempDir()
	restoreInto(t, res.Sealed, dest, RestoreOptions{})
	if got := mustRead(t, filepath.Join(dest, fact)); got != body {
		t.Fatalf("restored fact = %q, want %q", got, body)
	}
	info, err := os.Stat(filepath.Join(dest, fact))
	if err != nil {
		t.Fatal(err)
	}
	// Extract writes through os.WriteFile, so the restored mode is
	// beadSynthFactMode less the process umask; compare against a file
	// written the same way rather than the literal mode.
	probe := filepath.Join(t.TempDir(), "probe.md")
	if err := os.WriteFile(probe, []byte("x"), beadSynthFactMode); err != nil {
		t.Fatal(err)
	}
	probeInfo, err := os.Stat(probe)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), probeInfo.Mode().Perm(); got != want {
		t.Fatalf("fact restored with permissions %o, want %o (the synthesizer's own mode under this umask)", got, want)
	}
	for _, rel := range excluded {
		if _, err := os.Lstat(filepath.Join(dest, rel)); !os.IsNotExist(err) {
			t.Errorf("excluded file %s restored: %v", rel, err)
		}
	}
}

func TestBeadSynthVaultDoesNotFollowDirectorySymlinks(t *testing.T) {
	for _, boundary := range []string{"vaults", filepath.Join("vaults", beadSynthPrefix)} {
		t.Run(boundary, func(t *testing.T) {
			dir := seedSpoke(t)
			t.Setenv(EnvDataDir, dir)
			outside := t.TempDir()
			mustWrite(t, filepath.Join(outside, "secret.md"), "not a fact")
			mustWrite(t, filepath.Join(outside, beadSynthPrefix, "secret.md"), "not a fact")
			link := filepath.Join(dir, boundary)
			if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			res, err := Build(testKey(), "hive-abc123", testLogger())
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := res.Skipped[filepath.Join("vaults", beadSynthPrefix)]; !ok {
				t.Fatal("unsafe vault directory not reported")
			}
			dest := t.TempDir()
			if _, err := hubbackup.Extract(testKey(), res.Sealed, dest); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dest, beadSynthPrefix)); !os.IsNotExist(err) {
				t.Fatalf("symlinked directory captured: %v", err)
			}
		})
	}
}
