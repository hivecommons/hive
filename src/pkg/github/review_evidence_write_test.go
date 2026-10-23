package github

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/evidence"
)

func TestWriteReviewEvidenceCreatesGroupWritableDirs(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "owner", "repo", "42", "abc.json")
	if err := writeReviewEvidence(path, &evidence.Bundle{ID: "x"}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"owner", "owner/repo", "owner/repo/42"} {
		info, err := os.Stat(filepath.Join(root, d))
		if err != nil {
			t.Fatal(err)
		}
		mode := info.Mode() & (os.ModePerm | os.ModeSetgid)
		if mode&0o020 == 0 {
			t.Errorf("%s: mode %v is not group-writable", d, mode)
		}
		if mode&os.ModeSetgid == 0 {
			t.Logf("%s: setgid not preserved on this filesystem", d)
		}
	}
}

func TestWriteReviewEvidenceUnwritableDirReturnsPermissionError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
	dir := filepath.Join(t.TempDir(), "42")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := writeReviewEvidence(filepath.Join(dir, "abc.json"), &evidence.Bundle{ID: "x"})
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("expected permission error, got %v", err)
	}
}
