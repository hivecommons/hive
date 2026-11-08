package climemory

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain points the temp dir at its symlink-free path. openMemoryStore
// refuses every symlinked ancestor, so on macOS, where $TMPDIR sits under
// /var -> /private/var, stores under a raw t.TempDir() could not be opened.
func TestMain(m *testing.M) {
	resolved, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve temp dir: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("TMPDIR", resolved); err != nil {
		fmt.Fprintf(os.Stderr, "set TMPDIR: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
