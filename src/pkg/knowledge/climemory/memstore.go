package climemory

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// errNotMemoryFile marks a listed path that is no longer a regular memory
// file inside its store by the time it is opened. The store is owned by the
// agent and the drain runs with the hive's privileges, so a symlink swapped
// in between memoryFiles and the open must never be followed: by path it
// would read, and TruncateDrained would empty, whatever the agent pointed
// it at.
var errNotMemoryFile = errors.New("cli-memory drain: not a regular memory file in its store")

// memoryStore is one opened Source: the store directory (or the directory
// holding a single-file store) every memory file is opened through, so a
// listed path can only resolve inside it.
type memoryStore struct {
	root *os.Root
}

// openMemoryStore opens the store directory for path — path itself when it
// is a directory, its parent for a file store — and checks that the opened
// directory is the one Lstat sees at that path, so a store directory
// swapped for a symlink since memoryFiles listed it is refused rather than
// followed.
func openMemoryStore(path string) (*memoryStore, error) {
	base := path
	lfi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	// memoryFiles never lists a symlinked store, so one here was swapped in
	// after the listing; refuse it before the file-store fallback below
	// would otherwise open its (real) parent directory and pass SameFile.
	if lfi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s is a symlink", errNotMemoryFile, path)
	}
	if !lfi.IsDir() {
		base = filepath.Dir(path)
		if lfi, err = os.Lstat(base); err != nil {
			return nil, err
		}
	}
	if !lfi.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", errNotMemoryFile, base)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	rfi, err := root.Stat(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	if !os.SameFile(lfi, rfi) {
		root.Close()
		return nil, fmt.Errorf("%w: %s changed while being opened", errNotMemoryFile, base)
	}
	return &memoryStore{root: root}, nil
}

func (s *memoryStore) Close() error {
	if s == nil || s.root == nil {
		return nil
	}
	return s.root.Close()
}

// open opens path, a file memoryFiles listed under this store, inside the
// store root and checks the open descriptor is a regular file within the
// size cap. os.Root only refuses a symlink whose target leaves the root; one
// that stays inside it (another file of the same memory dir, or for a
// single-file store any sibling under its parent such as ~/.gemini/) would
// be followed and fstat would report the target as regular. So the entry is
// Lstat'ed through the root first and refused when it is a symlink, and the
// opened file must be the very inode Lstat saw, which closes the window
// between the two calls. flag is os.O_RDONLY or os.O_RDWR; the caller closes
// the file.
func (s *memoryStore) open(path string, flag int) (*os.File, error) {
	if s == nil || s.root == nil {
		return nil, fmt.Errorf("%w: %s has no open store", errNotMemoryFile, path)
	}
	rel, err := filepath.Rel(s.root.Name(), path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", errNotMemoryFile, path, err)
	}
	lfi, err := s.root.Lstat(rel)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", errNotMemoryFile, path, err)
	}
	if lfi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s is a symlink", errNotMemoryFile, path)
	}
	if !lfi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is %s", errNotMemoryFile, path, lfi.Mode().Type())
	}
	f, err := s.root.OpenFile(rel, flag, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", errNotMemoryFile, path, err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%w: %s is %s", errNotMemoryFile, path, fi.Mode().Type())
	}
	if !os.SameFile(lfi, fi) {
		f.Close()
		return nil, fmt.Errorf("%w: %s changed while being opened", errNotMemoryFile, path)
	}
	if fi.Size() > maxMemoryFileBytes {
		f.Close()
		return nil, fmt.Errorf("%w: %s is %d bytes, over the %d cap", errNotMemoryFile, path, fi.Size(), maxMemoryFileBytes)
	}
	return f, nil
}

// read returns the whole of path opened through the store.
func (s *memoryStore) read(path string) ([]byte, error) {
	f, err := s.open(path, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readMemoryFile(f)
}

// readMemoryFile reads an open memory file, capped even if it grows after
// the size check in open.
func readMemoryFile(f *os.File) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(f, maxMemoryFileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxMemoryFileBytes {
		return nil, fmt.Errorf("%w: %s grew past the %d cap while being read", errNotMemoryFile, f.Name(), maxMemoryFileBytes)
	}
	return data, nil
}
