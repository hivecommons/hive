package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteFileAtomicReplacesContentAndMode covers the plain case: an
// existing world-readable file is replaced wholesale and ends up with the
// requested mode, and no temp file is left behind.
func TestWriteFileAtomicReplacesContentAndMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml.runtime")
	if err := os.WriteFile(path, []byte("old: content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("new: content\n"), 0o600); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new: content\n" {
		t.Fatalf("content = %q, want new content", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 0600 (pre-existing 0644 bits must not survive)", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file %s left behind", e.Name())
		}
	}
}

// TestWriteFileAtomicCreatesMissingFile pins that a missing target is
// created (Save's old create fallback) rather than failing on EvalSymlinks.
func TestWriteFileAtomicCreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hive.yaml")
	if err := writeFileAtomic(path, []byte("a: 1\n"), 0o644); err != nil {
		t.Fatalf("writeFileAtomic on missing file: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "a: 1\n" {
		t.Fatalf("content = %q", got)
	}
}

// TestWriteFileAtomicThroughSymlinkKeepsLink is the #17468 constraint at the
// writer level: the rename must land on the link TARGET so the symlink the
// copy-config init container created survives, and the target carries the
// new bytes.
func TestWriteFileAtomicThroughSymlinkKeepsLink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "data", "hive.yaml.runtime")
	link := filepath.Join(dir, "etc", "hive.yaml")
	for _, d := range []string{filepath.Dir(target), filepath.Dir(link)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(link, []byte("new\n"), 0o600); err != nil {
		t.Fatalf("writeFileAtomic through link: %v", err)
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is no longer a symlink — rename replaced the link instead of its target", link)
	}
	if got, _ := os.ReadFile(target); string(got) != "new\n" {
		t.Fatalf("target content = %q, want new", got)
	}
	// Nothing may be left in the link's directory: the temp file must be
	// created next to the TARGET, or the rename would cross directories.
	entries, _ := os.ReadDir(filepath.Dir(link))
	if len(entries) != 1 {
		t.Fatalf("link dir has %d entries, want just the link", len(entries))
	}
}

// TestSaveSurvivesKillMidWrite is the #17862 regression. It cannot SIGKILL
// the test process, so it pins the property that makes a kill harmless:
// at no point during Save does the runtime config path refer to a file
// with fewer bytes than the previous complete config. A temp-file+rename
// writer satisfies this trivially; the old O_TRUNC in-place write fails it
// because the file is observably 0 bytes between truncate and write.
//
// The check is made by racing a reader against repeated Saves and
// asserting it never observes an empty or partial file.
func TestSaveSurvivesKillMidWrite(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "hive.yaml")
	runtimePath := filepath.Join(dir, "hive.yaml.runtime")
	origOverlay, origRuntime := DashboardOverlayFile, RuntimeConfigFile
	DashboardOverlayFile = filepath.Join(dir, "hive.yaml.dashboard")
	RuntimeConfigFile = runtimePath
	t.Cleanup(func() { DashboardOverlayFile, RuntimeConfigFile = origOverlay, origRuntime })

	level := 6
	cfg := &Config{
		SourcePath: cfgPath,
		Project:    ProjectConfig{Org: "testorg", Name: "test"},
		AgentRuntimeConfig: AgentRuntimeConfig{
			Agents: map[string]AgentConfig{"scanner": {Role: "scanner"}},
		},
		ACMMLevel: &level,
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("initial Save: %v", err)
	}
	full, err := os.ReadFile(runtimePath)
	if err != nil || len(full) == 0 {
		t.Fatalf("initial runtime config unreadable: %v", err)
	}

	stop := make(chan struct{})
	bad := make(chan string, 1)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, p := range []string{runtimePath, cfgPath} {
				got, err := os.ReadFile(p)
				if err != nil {
					continue // a rename window may briefly ENOENT on some filesystems; never partial
				}
				if len(got) < len(full) {
					select {
					case bad <- p:
					default:
					}
					return
				}
			}
		}
	}()
	for i := 0; i < 200; i++ {
		if err := cfg.Save(); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
	}
	close(stop)
	select {
	case p := <-bad:
		t.Fatalf("reader observed a truncated/partial config at %s during Save — write is not atomic", p)
	default:
	}
}

func TestWriteFileAtomicMissingDirFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "hive.yaml")
	err := writeFileAtomic(path, []byte("x"), 0o600)
	if err == nil {
		t.Fatal("expected error when target directory does not exist")
	}
	if !strings.Contains(err.Error(), "creating temp file") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWriteFileAtomicResolveErrorIsReported(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "regular")
	if err := os.WriteFile(file, []byte("f"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Using a regular file as a path component makes EvalSymlinks fail with
	// ENOTDIR, which is not ErrNotExist and must surface as a resolve error.
	err := writeFileAtomic(filepath.Join(file, "hive.yaml"), []byte("x"), 0o600)
	if err == nil {
		t.Fatal("expected resolve error")
	}
	if !strings.Contains(err.Error(), "resolving") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWriteFileAtomicDanglingSymlinkIsReplaced(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "hive.yaml")
	if err := os.Symlink(filepath.Join(dir, "missing-target"), link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	if err := writeFileAtomic(link, []byte("fresh"), 0o600); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	got, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fresh" {
		t.Fatalf("got %q", got)
	}
}

func TestWriteFileAtomicRenameFailureFallsBackAndReportsBoth(t *testing.T) {
	dir := t.TempDir()
	// A non-empty directory as the target: rename of a file onto it fails,
	// and the in-place O_TRUNC fallback cannot open a directory for writing.
	target := filepath.Join(dir, "hive.yaml")
	if err := os.MkdirAll(filepath.Join(target, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := writeFileAtomic(target, []byte("x"), 0o600)
	if err == nil {
		t.Fatal("expected rename + fallback failure")
	}
	if !strings.Contains(err.Error(), "in-place fallback also failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".hive.yaml.tmp-") {
			t.Fatalf("temp file %s leaked after failure", e.Name())
		}
	}
}

func TestWriteFileInPlaceWritesAndSetsMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hive.yaml")
	if err := os.WriteFile(path, []byte("old-longer-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileInPlace(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("writeFileInPlace: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("got %q, want truncated rewrite", got)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", st.Mode().Perm())
	}
}

func TestWriteFileInPlaceOpenFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "hive.yaml")
	if err := writeFileInPlace(path, []byte("x"), 0o600); err == nil {
		t.Fatal("expected open failure for missing directory")
	}
}

func TestSyncDirToleratesMissingDir(t *testing.T) {
	syncDir(filepath.Join(t.TempDir(), "nope"))
	syncDir(t.TempDir())
}

func TestSourceConfigModePreservesExistingOrDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte("a: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := sourceConfigMode(path); got != 0o600 {
		t.Fatalf("existing mode = %o, want 600", got)
	}
	if got := sourceConfigMode(filepath.Join(dir, "absent.yaml")); got != 0o644 {
		t.Fatalf("default mode = %o, want 644", got)
	}
}
