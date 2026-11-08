package climemory

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
// is a directory, its parent for a file store. path is the store as the hive
// listed it with every symlink already resolved (the crux side resolves it
// with EvalSymlinks and keeps it only under the agent's own home), so no
// component of it may be a symlink by the time it is opened. The agent owns
// every directory from its home down and can swap any of them (`.claude`,
// `projects`, the project slug, the store itself) for a symlink after that
// listing; a path-based open would follow it to another agent's store or any
// hive-readable tree and drain, and with truncate empty, it under this
// agent's name. The directories are therefore opened one component at a time
// from the filesystem root, each refused when it is a symlink and checked to
// be the very inode Lstat saw, so the store root is reached without following
// a single symlink.
func openMemoryStore(path string) (*memoryStore, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%w: %s is not an absolute path", errNotMemoryFile, path)
	}
	path = filepath.Clean(path)
	dir, name := filepath.Split(path)
	if name == "" {
		return nil, fmt.Errorf("%w: %s is not a memory store", errNotMemoryFile, path)
	}
	parent, err := openDirNoFollow(filepath.Clean(dir))
	if err != nil {
		return nil, err
	}
	lfi, err := parent.Lstat(name)
	switch {
	case err != nil:
		parent.Close()
		return nil, err
	case lfi.IsDir():
		root, err := openChildDirNoFollow(parent, name, lfi)
		parent.Close()
		if err != nil {
			return nil, err
		}
		return &memoryStore{root: root}, nil
	case lfi.Mode()&os.ModeSymlink != 0:
		// memoryFiles never lists a symlinked store, so one here was swapped
		// in after the listing; refuse it rather than open its parent.
		parent.Close()
		return nil, fmt.Errorf("%w: %s is a symlink", errNotMemoryFile, path)
	default:
		// A single-file store is opened through the directory holding it;
		// open checks the file itself.
		return &memoryStore{root: parent}, nil
	}
}

// openDirNoFollow opens the absolute, clean directory dir by walking it from
// the filesystem root one component at a time with openChildDirNoFollow, so
// a symlink anywhere in dir is refused instead of followed. The returned
// root's Name is dir.
func openDirNoFollow(dir string) (*os.Root, error) {
	vol := filepath.VolumeName(dir)
	root, err := os.OpenRoot(vol + string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	rest := strings.Trim(dir[len(vol):], string(filepath.Separator))
	if rest == "" {
		return root, nil
	}
	for _, name := range strings.Split(rest, string(filepath.Separator)) {
		lfi, err := root.Lstat(name)
		if err != nil {
			root.Close()
			return nil, err
		}
		next, err := openChildDirNoFollow(root, name, lfi)
		root.Close()
		if err != nil {
			return nil, err
		}
		root = next
	}
	return root, nil
}

// openChildDirNoFollow opens name, an entry directly inside parent that
// Lstat reported as lfi, as a root. A symlink or non-directory is refused,
// and the opened directory must be the very inode Lstat saw: os.Root follows
// a symlink that stays inside parent, so a directory swapped for one between
// the two calls would otherwise be opened at its target.
func openChildDirNoFollow(parent *os.Root, name string, lfi os.FileInfo) (*os.Root, error) {
	path := filepath.Join(parent.Name(), name)
	if lfi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s is a symlink", errNotMemoryFile, path)
	}
	if !lfi.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", errNotMemoryFile, path)
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	cfi, err := child.Stat(".")
	if err != nil {
		child.Close()
		return nil, err
	}
	if !os.SameFile(lfi, cfi) {
		child.Close()
		return nil, fmt.Errorf("%w: %s changed while being opened", errNotMemoryFile, path)
	}
	return child, nil
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
