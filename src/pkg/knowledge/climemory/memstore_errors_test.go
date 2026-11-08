package climemory

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenMemoryStoreErrors(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	writeFile(t, filepath.Join(real, "MEMORY.md"), "- a fact\n")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable")
	}

	if _, err := openMemoryStore(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing store: err = %v, want ErrNotExist", err)
	}
	if _, err := openMemoryStore(link); !errors.Is(err, errNotMemoryFile) {
		t.Fatalf("symlinked store: err = %v, want errNotMemoryFile", err)
	}
	if _, err := openMemoryStore(filepath.Join(link, "MEMORY.md")); err == nil {
		t.Fatal("file under a symlinked directory should not open as a store")
	}
}

func TestOpenMemoryStoreDirAndFileStore(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "MEMORY.md")
	writeFile(t, file, "- a fact\n")

	for _, path := range []string{dir, file} {
		s, err := openMemoryStore(path)
		if err != nil {
			t.Fatalf("openMemoryStore(%s): %v", path, err)
		}
		b, err := s.read(file)
		if err != nil || string(b) != "- a fact\n" {
			t.Fatalf("read = %q, %v", b, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMemoryStoreCloseNil(t *testing.T) {
	var s *memoryStore
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := (&memoryStore{}).Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryStoreOpenRefusals(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.md")
	writeFile(t, outside, "secret\n")
	big := filepath.Join(dir, "big.md")
	writeFile(t, big, strings.Repeat("x", maxMemoryFileBytes+1))
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "ok.md"), "ok\n")
	escape := filepath.Join(dir, "escape.md")
	symlinks := os.Symlink(outside, escape) == nil

	s, err := openMemoryStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	cases := []struct {
		name string
		path string
	}{
		{"relative path", "ok.md"},
		{"outside the root", outside},
		{"missing file", filepath.Join(dir, "missing.md")},
		{"directory", filepath.Join(dir, "sub")},
		{"over the cap", big},
	}
	if symlinks {
		cases = append(cases, struct{ name, path string }{"symlink out of the root", escape})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := s.open(tc.path, os.O_RDONLY)
			if err == nil {
				f.Close()
				t.Fatal("open succeeded, want errNotMemoryFile")
			}
			if !errors.Is(err, errNotMemoryFile) {
				t.Fatalf("err = %v, want errNotMemoryFile", err)
			}
			if _, err := s.read(tc.path); !errors.Is(err, errNotMemoryFile) {
				t.Fatalf("read err = %v, want errNotMemoryFile", err)
			}
		})
	}

	f, err := s.open(filepath.Join(dir, "ok.md"), os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestMemoryStoreOpenWithoutStore(t *testing.T) {
	var s *memoryStore
	if _, err := s.open("x", os.O_RDONLY); !errors.Is(err, errNotMemoryFile) {
		t.Fatalf("nil store: err = %v", err)
	}
	if _, err := (&memoryStore{}).read("x"); !errors.Is(err, errNotMemoryFile) {
		t.Fatalf("empty store: err = %v", err)
	}
}

func TestReadMemoryFileErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.md")
	writeFile(t, path, strings.Repeat("x", maxMemoryFileBytes+1))
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readMemoryFile(f); !errors.Is(err, errNotMemoryFile) {
		t.Fatalf("oversize: err = %v, want errNotMemoryFile", err)
	}
	f.Close()
	if _, err := readMemoryFile(f); err == nil {
		t.Fatal("reading a closed file should fail")
	}
}
