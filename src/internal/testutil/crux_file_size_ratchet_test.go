package testutil

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file is the file-size ratchet for the Crux mirror (hivecommons/hive#15193).
// src/crux is published to hivecommons/crux by src/scripts/crux-publish.sh, and
// crux gates every commit with .github/scripts/check-file-size-ratchet.sh, but
// its .github/ is kept, not synced, so hive CI never measured those limits and
// growth merged green here and turned red only downstream, where PRs are
// refused. This test applies the same rule to src/crux so it fails in hive first:
//
//   - a non-test .go file listed in crux_file_size_ratchet.txt may not grow
//     past its recorded line count;
//   - an unlisted non-test .go file may not exceed cruxFileSizeLimit lines.
//
// Lines are counted like crux's `wc -l`: the number of newline bytes. Paths are
// relative to src/crux, which is crux's module root.
//
// crux_file_size_ratchet.txt is a verbatim copy of crux's
// .github/go-file-size-ratchet.txt ("<lines> <path>" per line, # comments and
// blank lines ignored). Never raise a number or add a file to it: split the
// file instead. Shrinking is always allowed, so a stale entry only needs
// lowering when crux's copy is next refreshed.

const (
	cruxFileSizeLimit    = 800
	cruxFileSizeBaseline = "crux_file_size_ratchet.txt"
)

// cruxFileSizeRoot is the Crux module root, relative to this package's
// directory (src/internal/testutil).
var cruxFileSizeRoot = filepath.Join("..", "..", "crux")

// TestCruxFileSizeRatchet fails when a non-test Go file under src/crux is over
// its baseline, or over cruxFileSizeLimit when it is not in the baseline.
func TestCruxFileSizeRatchet(t *testing.T) {
	baseline := loadCruxFileSizeBaseline(t)
	sizes := measureCruxFileSizes(t)
	if len(sizes) == 0 {
		t.Fatalf("no non-test .go files found under %s — is the test running from src/internal/testutil?", cruxFileSizeRoot)
	}

	var failures []string
	for _, path := range sortedFileSizePaths(sizes) {
		got := sizes[path]
		if want, listed := baseline[path]; listed {
			if got > want {
				failures = append(failures, fmt.Sprintf(
					"%s has %d lines, over its ratchet baseline of %d.", path, got, want))
			}
			continue
		}
		if got > cruxFileSizeLimit {
			failures = append(failures, fmt.Sprintf(
				"%s has %d lines, over the %d-line limit for non-test Go files (not in the baseline).",
				path, got, cruxFileSizeLimit))
		}
	}

	if len(failures) > 0 {
		t.Fatalf("Crux file-size ratchet (hivecommons/hive#15193, mirrored from crux's check-file-size-ratchet.sh):\n\n%s\n\n"+
			"Split the file or move code out instead of growing it; new files are not added to "+
			"internal/testutil/%s.",
			strings.Join(failures, "\n"), cruxFileSizeBaseline)
	}
}

// loadCruxFileSizeBaseline parses the vendored baseline, failing on a
// malformed or duplicate entry the way crux's gate does.
func loadCruxFileSizeBaseline(t *testing.T) map[string]int {
	t.Helper()
	data, err := os.ReadFile(cruxFileSizeBaseline)
	if err != nil {
		t.Fatalf("reading %s: %v", cruxFileSizeBaseline, err)
	}
	baseline := map[string]int{}
	for i, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) != 2 {
			t.Fatalf("%s:%d: malformed entry (want \"<lines> <path>\"): %s", cruxFileSizeBaseline, i+1, line)
		}
		n, err := strconv.Atoi(fields[0])
		if err != nil || n < 0 {
			t.Fatalf("%s:%d: malformed entry (want \"<lines> <path>\"): %s", cruxFileSizeBaseline, i+1, line)
		}
		if _, dup := baseline[fields[1]]; dup {
			t.Fatalf("%s:%d: duplicate entry for %s", cruxFileSizeBaseline, i+1, fields[1])
		}
		baseline[fields[1]] = n
	}
	return baseline
}

// measureCruxFileSizes returns the line count of every non-test Go file under
// src/crux, keyed by its slash path relative to the module root.
func measureCruxFileSizes(t *testing.T) map[string]int {
	t.Helper()
	sizes := map[string]int{}
	err := filepath.WalkDir(cruxFileSizeRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(cruxFileSizeRoot, path)
		if err != nil {
			return err
		}
		sizes[filepath.ToSlash(rel)] = bytes.Count(data, []byte{'\n'})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", cruxFileSizeRoot, err)
	}
	return sizes
}

func sortedFileSizePaths(sizes map[string]int) []string {
	paths := make([]string, 0, len(sizes))
	for p := range sizes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}
