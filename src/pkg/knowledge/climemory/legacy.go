package climemory

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// LegacyAgent is the agent the one-shot drain of the shared home's Claude
// memory store attributes its entries to (`agent:shared`, #17058). Before
// #17039 every agent wrote into that one tree, so no single agent owns a fact
// found there.
const LegacyAgent = "shared"

// LegacyClaudeSources lists the Claude auto-memory directories
// <projectsDir>/*/memory of the shared home #17039 left behind, in name
// order. A symlinked project or memory directory is skipped; a missing
// projectsDir yields none.
func LegacyClaudeSources(projectsDir string) ([]Source, error) {
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("cli-memory legacy drain: listing %s: %w", projectsDir, err)
	}
	var out []Source
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mem := filepath.Join(projectsDir, e.Name(), "memory")
		info, err := os.Lstat(mem)
		if err != nil || !info.IsDir() {
			continue
		}
		out = append(out, Source{CLI: CLIClaude, Path: mem})
	}
	return out, nil
}

// TruncateDrained empties every memory file under sources whose facts are
// all recorded in the vault (by memhash, in any lifecycle state). A file
// holding a fact the vault lacks — a write failed, the per-drain cap was
// hit — is left as it is and reported in kept, so truncation never loses a
// fact. A non-empty file with no fact to drain (a Claude index of links)
// counts as stored. Already-empty files are in neither list. Each file is
// read and truncated through one descriptor opened inside its store, so a
// path the agent swapped for a symlink since the listing is kept, not
// followed.
func (d *Drainer) TruncateDrained(sources []Source) (truncated, kept []string, err error) {
	if d == nil || d.Dir == "" {
		return nil, nil, errors.New("cli-memory drain: no target directory")
	}
	seen, err := existingHashes(d.Dir)
	if err != nil {
		return nil, nil, err
	}
	var errs []error
	for _, src := range sources {
		files, err := listMemoryFiles(src.Path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if len(files) == 0 {
			continue
		}
		store, err := openMemoryStore(src.Path)
		if err != nil {
			errs = append(errs, fmt.Errorf("cli-memory drain: opening %s: %w", src.Path, err))
			kept = append(kept, files...)
			continue
		}
		for _, file := range files {
			t, k, err := truncateIfStored(store, src.CLI, file, seen)
			if err != nil {
				errs = append(errs, err)
			}
			truncated, kept = append(truncated, t...), append(kept, k...)
		}
		store.Close()
	}
	return truncated, kept, errors.Join(errs...)
}

func truncateIfStored(store *memoryStore, cli, file string, seen map[string]string) (truncated, kept []string, err error) {
	f, err := store.open(file, os.O_RDWR)
	if err != nil {
		return nil, []string{file}, fmt.Errorf("cli-memory drain: opening %s: %w", file, err)
	}
	defer f.Close()
	content, err := readMemoryFile(f)
	if err != nil {
		return nil, []string{file}, fmt.Errorf("cli-memory drain: reading %s: %w", file, err)
	}
	if len(content) == 0 {
		return nil, nil, nil
	}
	if !allStored(ParseFor(cli, string(content)), seen) {
		return nil, []string{file}, nil
	}
	if err := f.Truncate(0); err != nil {
		return nil, []string{file}, fmt.Errorf("cli-memory drain: truncating %s: %w", file, err)
	}
	return []string{file}, nil, nil
}

func allStored(facts []Fact, seen map[string]string) bool {
	for _, f := range facts {
		if _, ok := seen[Hash(f.Body)]; !ok {
			return false
		}
	}
	return true
}
