package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsAgyCLILog(t *testing.T) {
	cases := map[string]bool{
		"/data/home/.gemini/antigravity-cli/log/cli-2026-10-22.log": true,
		"/data/home/.gemini/antigravity-cli/log/other.txt":          false,
		"/data/home/.gemini/antigravity-cli/cache/cli-1.log":        false,
		"/data/home/.other/antigravity-cli/log/cli-1.log":           false,
	}
	for path, want := range cases {
		if got := isAgyCLILog(path); got != want {
			t.Errorf("isAgyCLILog(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestFixEntryLeavesAgyCLILogMode covers the isAgyCLILog exemption: a
// tight-mode Antigravity CLI log owned by the watcher uid is left alone
// instead of being widened on every tick. It must run as DevUID, otherwise
// the owner guard in fixEntry returns before the chmod arm and the test
// would pass vacuously.
func TestFixEntryLeavesAgyCLILogMode(t *testing.T) {
	if !runningAsDevUID() {
		t.Skipf("chmod arm requires the process to run as DevUID (%d); running as %d", DevUID, os.Getuid())
	}
	dir := filepath.Join(t.TempDir(), ".gemini", "antigravity-cli", "log")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cli-1.log")
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	fixEntry(path, fi, quietLogger())
	if got := statMode(t, path); got != 0o640 {
		t.Errorf("mode = %v, want 0640 untouched", got)
	}
}
