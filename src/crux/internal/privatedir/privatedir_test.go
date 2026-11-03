package privatedir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	root := t.TempDir()
	private := filepath.Join(root, "private")
	open := filepath.Join(root, "open")
	file := filepath.Join(root, "file")
	link := filepath.Join(root, "link")
	for _, d := range []struct {
		path string
		mode os.FileMode
	}{{private, 0o700}, {open, 0o755}} {
		if err := os.Mkdir(d.path, d.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d.path, d.mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, dir, want string
	}{
		{"private dir", private, ""},
		{"missing", filepath.Join(root, "missing"), "no such file"},
		{"symlink", link, "is a symlink"},
		{"file", file, "is not a directory"},
		{"open mode", open, "readable by others"},
	}
	for _, tc := range cases {
		err := Check(tc.dir, 0o700)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}
