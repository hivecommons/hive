package sharedfile

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func leftovers(t *testing.T, target string) []string {
	t.Helper()
	m, err := filepath.Glob(target + ".*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func modeOf(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}

func TestReplace_WritesAtModeAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.json")
	if err := Replace(target, []byte("one"), 0o660, Options{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != "one" {
		t.Errorf("content = %q", got)
	}
	if m := modeOf(t, target); !m.IsRegular() || m.Perm() != 0o660 {
		t.Errorf("mode = %v, want regular 0660", m)
	}
	// Replacing an existing regular file works and keeps the new mode.
	if err := Replace(target, []byte("two"), 0o640, Options{PreserveOwner: true}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != "two" {
		t.Errorf("content after replace = %q", got)
	}
	if m := modeOf(t, target); m.Perm() != 0o640 {
		t.Errorf("mode after replace = %v", m)
	}
	if l := leftovers(t, target); len(l) != 0 {
		t.Errorf("temporary files left: %v", l)
	}
}

// Two writes never share a temporary name, and the name is not derivable
// from the target alone.
func TestReplace_TempNameIsUnpredictable(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "f")
	var names []string
	old := chownFile
	chownFile = func(f *os.File, uid, gid int) error {
		names = append(names, filepath.Base(f.Name()))
		return nil
	}
	t.Cleanup(func() { chownFile = old })
	if err := os.WriteFile(target, []byte("seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := Replace(target, []byte("x"), 0o600, Options{PreserveOwner: true}); err != nil {
			t.Fatal(err)
		}
	}
	if len(names) != 2 || names[0] == names[1] {
		t.Fatalf("temp names = %v, want two distinct", names)
	}
	for _, n := range names {
		if !strings.HasPrefix(n, "f.") || !strings.HasSuffix(n, ".tmp") || len(n) != len("f.")+16+len(".tmp") {
			t.Errorf("temp name %q is not <base>.<16 hex>.tmp", n)
		}
	}
	// The sentinel a fixed-name writer would use is never created.
	if _, err := os.Lstat(target + ".tmp"); !errors.Is(err, fs.ErrNotExist) {
		t.Error("a predictable <target>.tmp exists")
	}
}

func TestReplace_RefusesSymlinkAtTarget(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "cred")
	if err := os.Symlink(victim, target); err != nil {
		t.Fatal(err)
	}
	err := Replace(target, []byte("evil"), 0o660, Options{PreserveOwner: true})
	if !errors.Is(err, ErrUnsafeLocation) {
		t.Fatalf("err = %v, want ErrUnsafeLocation", err)
	}
	if got, _ := os.ReadFile(victim); string(got) != "keep" {
		t.Errorf("victim rewritten: %q", got)
	}
	if m := modeOf(t, victim); m.Perm() != 0o600 {
		t.Errorf("victim mode changed: %v", m)
	}
	if m := modeOf(t, target); m&fs.ModeSymlink == 0 {
		t.Error("symlink at target was replaced")
	}
	if l := leftovers(t, target); len(l) != 0 {
		t.Errorf("temporary files left: %v", l)
	}
}

func TestReplace_RefusesUnsafeDirectory(t *testing.T) {
	real := t.TempDir()
	// A symlink where the directory should be.
	linkDir := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(real, linkDir); err != nil {
		t.Fatal(err)
	}
	if err := Replace(filepath.Join(linkDir, "f"), []byte("x"), 0o600, Options{}); !errors.Is(err, ErrUnsafeLocation) {
		t.Errorf("symlinked dir: err = %v, want ErrUnsafeLocation", err)
	}
	if entries, _ := os.ReadDir(real); len(entries) != 0 {
		t.Errorf("wrote through the directory symlink: %v", entries)
	}
	// A missing directory.
	if err := Replace(filepath.Join(real, "nope", "f"), []byte("x"), 0o600, Options{}); !errors.Is(err, ErrUnsafeLocation) {
		t.Errorf("missing dir: err = %v", err)
	}
	// A world-writable directory.
	ww := filepath.Join(t.TempDir(), "ww")
	if err := os.Mkdir(ww, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ww, 0o1777); err != nil {
		t.Fatal(err)
	}
	if err := Replace(filepath.Join(ww, "f"), []byte("x"), 0o600, Options{}); !errors.Is(err, ErrUnsafeLocation) {
		t.Errorf("world-writable dir: err = %v", err)
	}
	// A file where the directory should be.
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Replace(filepath.Join(plain, "f"), []byte("x"), 0o600, Options{}); !errors.Is(err, ErrUnsafeLocation) {
		t.Errorf("file as dir: err = %v", err)
	}
	// A directory where the target should be.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Replace(filepath.Join(dir, "d"), []byte("x"), 0o600, Options{}); !errors.Is(err, ErrUnsafeLocation) {
		t.Errorf("dir as target: err = %v", err)
	}
}

func TestReplace_ChownFailureIsBestEffortAndVerified(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "f")
	if err := os.WriteFile(target, []byte("seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := chownFile
	chownFile = func(*os.File, int, int) error { return errors.New("EPERM") }
	t.Cleanup(func() { chownFile = old })
	if err := Replace(target, []byte("new"), 0o660, Options{PreserveOwner: true}); err != nil {
		t.Fatalf("a failed chown must not fail the write: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "new" {
		t.Errorf("content = %q", got)
	}
	// A chown that "succeeds" but leaves the wrong owner is caught by fstat.
	chownFile = func(*os.File, int, int) error { return nil }
	victimOwner := filepath.Join(dir, "g")
	if err := os.WriteFile(victimOwner, []byte("seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Pretend the previous file belonged to someone else by faking prev.
	prev := fakeInfo{FileInfo: must(os.Lstat(victimOwner)), uid: 12345, gid: 12345}
	f, tmp, err := createTemp(victimOwner)
	if err != nil {
		t.Fatal(err)
	}
	err = fill(f, []byte("x"), 0o660, prev, Options{PreserveOwner: true})
	_ = f.Close()
	_ = os.Remove(tmp)
	if err == nil || !strings.Contains(err.Error(), "owned by") {
		t.Errorf("owner mismatch not detected: %v", err)
	}
}

type fakeInfo struct {
	os.FileInfo
	uid, gid uint32
}

func (f fakeInfo) Sys() any {
	st, _ := f.FileInfo.Sys().(*syscall.Stat_t)
	cp := *st
	cp.Uid, cp.Gid = f.uid, f.gid
	return &cp
}

func must(info os.FileInfo, err error) os.FileInfo {
	if err != nil {
		panic(err)
	}
	return info
}

func TestCreateTemp_SkipsTakenNames(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "f")
	f, tmp, err := createTemp(target)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if m := modeOf(t, tmp); m.Perm() != 0o600 {
		t.Errorf("temp mode = %v, want 0600", m)
	}
	// A second create beside the first gets another name.
	g, tmp2, err := createTemp(target)
	if err != nil {
		t.Fatal(err)
	}
	_ = g.Close()
	if tmp == tmp2 {
		t.Error("same temp name twice")
	}
}

func TestReadNoFollow(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadNoFollow(p, 64)
	if err != nil || !bytes.Equal(got, []byte("hello")) {
		t.Errorf("read = %q, %v", got, err)
	}
	if _, err := ReadNoFollow(p, 4); !errors.Is(err, ErrTooLarge) {
		t.Errorf("over cap: err = %v", err)
	}
	link := filepath.Join(dir, "l")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadNoFollow(link, 64); !errors.Is(err, ErrNotRegular) {
		t.Errorf("symlink: err = %v, want ErrNotRegular", err)
	}
	if _, err := ReadNoFollow(dir, 64); !errors.Is(err, ErrNotRegular) {
		t.Errorf("directory: err = %v, want ErrNotRegular", err)
	}
	if _, err := ReadNoFollow(filepath.Join(dir, "missing"), 64); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: err = %v", err)
	}
	if os.Getuid() != 0 {
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadNoFollow(p, 64); !errors.Is(err, fs.ErrPermission) {
			t.Errorf("unreadable: err = %v, want ErrPermission", err)
		}
	}
}

func TestChmod(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	prev, changed, err := Chmod(path, 0o660)
	if err != nil || !changed || prev != 0o600 {
		t.Fatalf("Chmod = (%o, %v, %v)", prev, changed, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o660 {
		t.Fatalf("mode = %v", info.Mode())
	}
	if _, changed, err := Chmod(path, 0o660); err != nil || changed {
		t.Fatalf("already matching: changed=%v err=%v", changed, err)
	}
	if _, _, err := Chmod(filepath.Join(dir, "absent"), 0o660); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("absent: %v", err)
	}

	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Chmod(link, 0o666); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("symlink: %v, want ErrNotRegular", err)
	}
	if info, _ := os.Stat(victim); info.Mode().Perm() != 0o600 {
		t.Fatalf("victim mode changed through the symlink: %v", info.Mode())
	}
	if _, _, err := Chmod(dir, 0o700); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("directory: %v, want ErrNotRegular", err)
	}
}

func TestCheckLocation(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "f")
	if info, err := CheckLocation(target); err != nil || info != nil {
		t.Fatalf("absent: (%v, %v)", info, err)
	}
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := CheckLocation(target); err != nil || info == nil || !info.Mode().IsRegular() {
		t.Fatalf("regular: (%v, %v)", info, err)
	}
	if os.Geteuid() != 0 {
		// A directory that cannot be searched: the target's Lstat fails with
		// something other than "not exist", and that is refused too.
		closed := filepath.Join(dir, "closed")
		if err := os.Mkdir(closed, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(closed, 0o700) })
		if _, err := CheckLocation(filepath.Join(closed, "f")); !errors.Is(err, ErrUnsafeLocation) {
			t.Errorf("unsearchable dir: %v", err)
		}
	}
}

func TestCreateTemp_RandomFailures(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "f")
	old := randRead
	t.Cleanup(func() { randRead = old })

	randRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	if _, _, err := createTemp(target); err == nil || !strings.Contains(err.Error(), "no entropy") {
		t.Errorf("entropy failure: %v", err)
	}
	if left := leftovers(t, target); len(left) != 0 {
		t.Errorf("leftovers: %v", left)
	}

	// Every name the generator can produce is already taken: refused after
	// createAttempts, never written through the existing file.
	randRead = func(b []byte) (int, error) {
		for i := range b {
			b[i] = 0xAB
		}
		return len(b), nil
	}
	taken := target + "." + strings.Repeat("ab", 8) + ".tmp"
	if err := os.WriteFile(taken, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Replace(target, []byte("new"), 0o600, Options{}); err == nil || !strings.Contains(err.Error(), "no free temporary name") {
		t.Errorf("all names taken: %v", err)
	}
	if got, _ := os.ReadFile(taken); string(got) != "keep" {
		t.Errorf("the taken name was written through: %q", got)
	}
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("target appeared: %v", err)
	}
}

func TestReplace_FailuresLeaveNoTemp(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is not stopped by directory modes")
	}
	old := chownFile
	t.Cleanup(func() { chownFile = old })

	// Cannot create the temporary: the directory is read-only.
	ro := t.TempDir()
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	if err := Replace(filepath.Join(ro, "f"), []byte("x"), 0o600, Options{}); err == nil || errors.Is(err, ErrUnsafeLocation) {
		t.Errorf("read-only dir: err = %v, want a create error", err)
	}

	// The written file fails verification (mode changed under us).
	dir := t.TempDir()
	target := filepath.Join(dir, "f")
	if err := os.WriteFile(target, []byte("seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	chownFile = func(f *os.File, _, _ int) error { return f.Chmod(0o644) }
	if err := Replace(target, []byte("new"), 0o660, Options{PreserveOwner: true}); err == nil || !strings.Contains(err.Error(), "written file is mode") {
		t.Errorf("mode drift: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "seed" {
		t.Errorf("target replaced despite failed verification: %q", got)
	}
	if left := leftovers(t, target); len(left) != 0 {
		t.Errorf("leftovers after failed fill: %v", left)
	}

	// The flush fails: no rename, no leftover.
	oldSync := syncFile
	syncFile = func(*os.File) error { return errors.New("EIO") }
	err := Replace(target, []byte("new"), 0o660, Options{})
	syncFile = oldSync
	if err == nil || !strings.Contains(err.Error(), "EIO") {
		t.Errorf("sync failure: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "seed" {
		t.Errorf("target replaced despite failed sync: %q", got)
	}
	if left := leftovers(t, target); len(left) != 0 {
		t.Errorf("leftovers after failed sync: %v", left)
	}

	// The temporary gained a second name while being written.
	chownFile = func(f *os.File, _, _ int) error {
		hl := f.Name() + ".hl"
		t.Cleanup(func() { _ = os.Remove(hl) })
		return os.Link(f.Name(), hl)
	}
	if err := Replace(target, []byte("new"), 0o660, Options{PreserveOwner: true}); err == nil || !strings.Contains(err.Error(), "links, want 1") {
		t.Errorf("extra link: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "seed" {
		t.Errorf("target replaced despite extra link: %q", got)
	}

	// The location changes while the temporary is being written.
	chownFile = func(*os.File, int, int) error {
		_ = os.Remove(target)
		return os.Symlink("/nonexistent", target)
	}
	if err := Replace(target, []byte("new"), 0o660, Options{PreserveOwner: true}); !errors.Is(err, ErrUnsafeLocation) {
		t.Errorf("location changed mid-write: %v", err)
	}
	if info, err := os.Lstat(target); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("the planted link was replaced: %v %v", info, err)
	}
	if left := leftovers(t, target); len(left) != 0 {
		t.Errorf("leftovers after refused rename: %v", left)
	}
	_ = os.Remove(target)
	if err := os.WriteFile(target, []byte("seed"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The rename itself fails: the directory went read-only after the
	// temporary was created.
	chownFile = func(*os.File, int, int) error { return os.Chmod(dir, 0o500) }
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	err = Replace(target, []byte("new"), 0o660, Options{PreserveOwner: true})
	_ = os.Chmod(dir, 0o700)
	if err == nil || errors.Is(err, ErrUnsafeLocation) {
		t.Errorf("rename failure: err = %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "seed" {
		t.Errorf("target changed on failed rename: %q", got)
	}
	// The temporary could not be removed from the read-only directory at
	// the time; it is the one leftover this path can produce, and it is
	// removable now.
	for _, l := range leftovers(t, target) {
		_ = os.Remove(l)
	}
}
