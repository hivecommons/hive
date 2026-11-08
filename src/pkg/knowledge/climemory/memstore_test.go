package climemory

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// swapForSymlink makes listMemoryFiles report path as a memory file and then
// replaces it with a symlink to target, the way an agent that owns the store
// can between the hive's listing and its open.
func swapForSymlink(t *testing.T, path, target string) {
	t.Helper()
	real := memoryFiles
	listMemoryFiles = func(p string) ([]string, error) {
		files, err := real(p)
		if err != nil {
			return nil, err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Skip("symlinks unavailable")
		}
		return files, nil
	}
	t.Cleanup(func() { listMemoryFiles = real })
}

func TestTruncateDrainedNeverFollowsSymlinkSwappedInAfterListing(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "hive.yaml")
	// No "- " bullets: parsed as zero facts, which TruncateDrained counts
	// as fully stored, so a path-based truncate would empty the target.
	writeFile(t, outside, "dashboard:\n  token: not-a-fact\n")
	store := filepath.Join(t.TempDir(), "-data-agents-scanner", "memory")
	victim := filepath.Join(store, "MEMORY.md")
	writeFile(t, victim, "# index\n")
	swapForSymlink(t, victim, outside)

	d := &Drainer{Dir: t.TempDir(), Now: fixedNow}
	truncated, kept, err := d.TruncateDrained([]Source{{CLI: CLIClaude, Path: store}})
	if len(truncated) != 0 {
		t.Fatalf("truncated %v through a symlink", truncated)
	}
	if len(kept) != 1 || kept[0] != victim {
		t.Fatalf("kept = %v, want the swapped path", kept)
	}
	if err == nil || !errors.Is(err, errNotMemoryFile) {
		t.Fatalf("err = %v, want errNotMemoryFile", err)
	}
	if b, _ := os.ReadFile(outside); !strings.Contains(string(b), "not-a-fact") {
		t.Fatalf("symlink target was truncated: %q", b)
	}
}

func TestDrainNeverReadsSymlinkSwappedInAfterListing(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.md")
	writeFile(t, outside, "- The hive's private token is in this line.\n")
	store := filepath.Join(t.TempDir(), "-data-agents-scanner", "memory")
	victim := filepath.Join(store, "MEMORY.md")
	writeFile(t, victim, "- A fact the agent really wrote.\n")
	swapForSymlink(t, victim, outside)

	vault := t.TempDir()
	d := &Drainer{Dir: vault, SpoolDir: t.TempDir(), Now: fixedNow}
	res := d.Drain(Request{Agent: "scanner", Sources: []Source{{CLI: CLIClaude, Path: store}}})
	if res.Drained != 0 || res.Spooled != 0 {
		t.Fatalf("drained %d / spooled %d through a symlink", res.Drained, res.Spooled)
	}
	if len(res.Errors) != 1 || !errors.Is(res.Errors[0], errNotMemoryFile) {
		t.Fatalf("errors = %v, want one errNotMemoryFile", res.Errors)
	}
	entries, _ := filepath.Glob(filepath.Join(vault, filePrefix+"*.md"))
	for _, e := range entries {
		if b, _ := os.ReadFile(e); strings.Contains(string(b), "private token") {
			t.Fatalf("symlink target drained into %s", e)
		}
	}
}

func TestOpenMemoryStoreRefusesSwappedStoreDirectory(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "MEMORY.md"), "x\n")
	home := t.TempDir()
	store := filepath.Join(home, "memory")
	if err := os.Symlink(outside, store); err != nil {
		t.Skip("symlinks unavailable")
	}
	if s, err := openMemoryStore(store); err == nil {
		s.Close()
		t.Fatal("opened a symlinked store directory")
	} else if !errors.Is(err, errNotMemoryFile) {
		t.Fatalf("err = %v, want errNotMemoryFile", err)
	}
}

func TestMemoryStoreOpenKeepsSingleFileStoreWorking(t *testing.T) {
	home := t.TempDir()
	gemini := filepath.Join(home, "GEMINI.md")
	writeFile(t, gemini, "- Single-file stores open through their parent.\n")
	s, err := openMemoryStore(gemini)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b, err := s.read(gemini)
	if err != nil || !strings.Contains(string(b), "Single-file") {
		t.Fatalf("read = %q, %v", b, err)
	}
}
