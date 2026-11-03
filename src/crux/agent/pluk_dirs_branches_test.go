package agent

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pluk_dirs_branches_test.go covers the pluk_dirs.go branches the setgid-mode
// test leaves untouched: ensurePlukLogFile's create/widen contract and its
// OpenFile error return, and ensurePlukRunDirs' MkdirAll error return and
// repair-on-rerun behavior.

// TestEnsurePlukLogFileCreatesGroupReadableLogDespiteUmask pins the reason the
// file is created in Go rather than left to the pane shell's `>>`: a 0077
// umask must not yield a 0600 log no peer agent can read.
func TestEnsurePlukLogFileCreatesGroupReadableLogDespiteUmask(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "pluk")
	if err := ensurePlukRunDirs(runDir); err != nil {
		t.Fatalf("ensure pluk dirs: %v", err)
	}

	oldUmask := setUmask(0o077)
	defer setUmask(oldUmask)

	path, err := ensurePlukLogFile(runDir, "hive-quality")
	if err != nil {
		t.Fatalf("ensure pluk log file: %v", err)
	}
	if want := plukSessionLogPath(runDir, "hive-quality"); path != want {
		t.Errorf("returned path = %q, want %q", path, want)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != 0o660 {
		t.Errorf("log mode = %04o, want 0660 (umask must not tighten the shared log)", got)
	}
}

// TestEnsurePlukLogFileWidensExistingTightLog pins the second half of the
// contract: an existing log inherited from an earlier run under a tighter
// umask is widened back to the shared mode, and its contents are preserved
// (the open is O_APPEND, never a truncate).
func TestEnsurePlukLogFileWidensExistingTightLog(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "pluk")
	if err := ensurePlukRunDirs(runDir); err != nil {
		t.Fatalf("ensure pluk dirs: %v", err)
	}

	path := plukSessionLogPath(runDir, "hive-guide")
	if err := os.WriteFile(path, []byte("{\"type\":\"raw_output\"}\n"), 0o600); err != nil {
		t.Fatalf("seed tight log: %v", err)
	}

	got, err := ensurePlukLogFile(runDir, "hive-guide")
	if err != nil {
		t.Fatalf("ensure pluk log file: %v", err)
	}
	if got != path {
		t.Errorf("returned path = %q, want %q", got, path)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o660 {
		t.Errorf("existing log mode = %04o, want 0660 (must be widened, not inherited)", perm)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(data) != "{\"type\":\"raw_output\"}\n" {
		t.Errorf("existing log contents changed: %q", data)
	}
}

// TestEnsurePlukLogFileErrorWhenLogsDirMissing covers the OpenFile error
// return: with no logs/ directory the create fails, the error is wrapped
// (fs.ErrNotExist stays reachable through errors.Is), and it names the path.
func TestEnsurePlukLogFileErrorWhenLogsDirMissing(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "pluk") // never created

	path, err := ensurePlukLogFile(runDir, "hive-scanner")
	if err == nil {
		t.Fatalf("ensure pluk log file succeeded without a logs dir, path=%q", path)
	}
	if path != "" {
		t.Errorf("path on error = %q, want empty", path)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error not wrapped (errors.Is fs.ErrNotExist false): %v", err)
	}
	want := plukSessionLogPath(runDir, "hive-scanner")
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the log path %q", err, want)
	}
}

// TestEnsurePlukRunDirsErrorWhenRunDirIsFile covers the MkdirAll error
// return: a regular file squatting on the run dir makes creation fail, the
// error is wrapped, and it names the child dir being created.
func TestEnsurePlukRunDirsErrorWhenRunDirIsFile(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "pluk")
	if err := os.WriteFile(runDir, []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("seed squatting file: %v", err)
	}

	err := ensurePlukRunDirs(runDir)
	if err == nil {
		t.Fatal("ensure pluk dirs succeeded with a file at the run dir path")
	}
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) {
		t.Errorf("error not wrapped (errors.As *fs.PathError false): %v", err)
	}
	if want := filepath.Join(runDir, "logs"); !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the dir being created %q", err, want)
	}
}

// TestEnsurePlukRunDirsRewidensTightenedDirs covers the Chmod-on-existing-dir
// path the create-only test never reaches: dirs left behind by an earlier run
// under a tighter mode are repaired back to 0770+setgid, not inherited.
func TestEnsurePlukRunDirsRewidensTightenedDirs(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "pluk")
	for _, name := range []string{"logs", "commands"} {
		if err := os.MkdirAll(filepath.Join(runDir, name), 0o700); err != nil {
			t.Fatalf("seed tight dir %s: %v", name, err)
		}
	}

	if err := ensurePlukRunDirs(runDir); err != nil {
		t.Fatalf("ensure pluk dirs on existing tree: %v", err)
	}

	for _, name := range []string{"logs", "commands"} {
		path := filepath.Join(runDir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != 0o770 {
			t.Errorf("%s mode = %04o, want 0770 after repair", path, got)
		}
		if info.Mode()&os.ModeSetgid == 0 {
			t.Errorf("%s missing setgid bit after repair: %s", path, info.Mode())
		}
	}
}
