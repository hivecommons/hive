package keystore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Secret files.
//
// WriteFile and ReadFile are the two calls every writer and reader of a
// secret file on the data volume goes through. WriteFile seals the plaintext
// with the active provider (or writes it as is in legacy mode) and replaces
// the file atomically at an owner-only mode. ReadFile returns the plaintext
// whatever the file holds — a sealed envelope, or the plaintext a
// pre-keystore hive wrote — and, when a provider is active and the file is
// under a writable root, re-seals the file in place if it was plaintext or
// sealed under a retired key generation. That rewrite-on-read is how an
// existing hive converges onto encryption without a migration step, and how a
// rotation converges onto the new generation.

const (
	// DefaultFileMode is the mode WriteFile gives a new secret file.
	DefaultFileMode fs.FileMode = 0o600
	// MaxFileBytes bounds a secret file read. Secrets are short; a file over
	// this size is not one, and never gets decrypted or rewritten.
	MaxFileBytes = 1 << 20
)

// ErrFileTooLarge is returned by ReadFile for a file over MaxFileBytes.
var ErrFileTooLarge = fmt.Errorf("keystore: secret file is larger than %d bytes", MaxFileBytes)

// rewriteRoots are the directories under which ReadFile may re-seal a file
// in place. The Kubernetes Secret mount at /secrets is read-only and operator
// managed, so it is deliberately absent; only the data volume converges.
var (
	rewriteRootsMu sync.RWMutex
	rewriteRoots   = []string{"/data"}
	// rewriteWarned remembers paths whose rewrite failed, so a read-only or
	// misowned file is reported once rather than on every read.
	rewriteWarned sync.Map
)

// SetRewriteRootsForTest replaces the writable roots and returns a restore
// function. Tests point it at a t.TempDir().
func SetRewriteRootsForTest(roots ...string) func() {
	rewriteRootsMu.Lock()
	prev := rewriteRoots
	rewriteRoots = roots
	rewriteRootsMu.Unlock()
	return func() {
		rewriteRootsMu.Lock()
		rewriteRoots = prev
		rewriteRootsMu.Unlock()
	}
}

// Rewritable reports whether path lies under a writable root.
func Rewritable(path string) bool {
	clean := filepath.Clean(path)
	rewriteRootsMu.RLock()
	defer rewriteRootsMu.RUnlock()
	for _, root := range rewriteRoots {
		root = filepath.Clean(root)
		if strings.HasPrefix(clean, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// FileAAD is the additional authenticated data a secret file is sealed with:
// its base name. A ciphertext copied from one file into another's slot fails
// to open, so a volume-level attacker cannot swap the backup key for a
// gateway key, say, and still be served by code expecting the latter. The
// directory is left out so a secrets directory can be moved or restored
// elsewhere and still open.
func FileAAD(path string) []byte {
	return []byte("file:" + filepath.Base(path))
}

// WriteFile seals plaintext for path and replaces the file atomically with
// mode DefaultFileMode. The parent directory must exist: every caller owns
// its directory's creation and mode (the secrets dir is 0710 so agent uids
// can traverse it, the owner identity dir is 0700), and a write into a
// missing directory is the error it always was.
func WriteFile(ctx context.Context, path string, plaintext []byte) error {
	return WriteFileMode(ctx, path, plaintext, DefaultFileMode)
}

// WriteFileMode is WriteFile at an explicit mode, for the few files other
// uids read by design (the Bob key is group-readable).
func WriteFileMode(ctx context.Context, path string, plaintext []byte, mode fs.FileMode) error {
	body := plaintext
	if p := Active(); p != nil {
		sealed, err := Seal(ctx, p, plaintext, FileAAD(path))
		if err != nil {
			return err
		}
		body = []byte(sealed + "\n")
	}
	return replaceFile(path, body, mode)
}

// ReadFile returns the plaintext held by path, opening an envelope or passing
// legacy plaintext through, and re-seals the file when that converges it (see
// the package notes above). A missing file is reported with fs.ErrNotExist so
// callers keep their "not configured" branches.
func ReadFile(ctx context.Context, path string) ([]byte, error) {
	raw, err := readCapped(path)
	if err != nil {
		return nil, err
	}
	if !IsEnvelope(raw) {
		if p := Active(); p != nil && Rewritable(path) {
			reseal(ctx, path, raw, "plaintext")
		}
		return raw, nil
	}
	p := Active()
	pt, stale, err := Open(ctx, p, string(raw), FileAAD(path))
	if err != nil {
		if errors.Is(err, ErrNoProvider) {
			if ierr := InitError(); ierr != nil {
				err = fmt.Errorf("%w: %w", err, ierr)
			}
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if stale && Rewritable(path) {
		reseal(ctx, path, pt, "retired key generation")
	}
	return pt, nil
}

// ReadString is ReadFile with the result trimmed of surrounding whitespace,
// the shape every single-value key file is read in.
func ReadString(ctx context.Context, path string) (string, error) {
	b, err := ReadFile(ctx, path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// reseal rewrites path with plaintext sealed under the current generation,
// keeping the file's mode. Best effort: a failure is logged once per path and
// the read still succeeds, because converging the file on disk is never worth
// failing the caller that only wanted the value.
func reseal(ctx context.Context, path string, plaintext []byte, why string) {
	mode := DefaultFileMode
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := WriteFileMode(ctx, path, plaintext, mode); err != nil {
		if _, seen := rewriteWarned.LoadOrStore(path, true); !seen {
			slog.Default().Warn("keystore: could not re-seal secret file in place; it stays as it was",
				"path", path, "was", why, "error", err.Error())
		}
		return
	}
	slog.Default().Info("keystore: re-sealed secret file under the current key generation", "path", path, "was", why)
}

// MigrateDir reads every regular file directly under dir through ReadFile so
// each one converges onto the active provider and generation. It is the boot
// sweep for files nothing reads often (the backup key). Hidden temp files are
// skipped. Returns how many files were visited; a missing dir is not an
// error. Does nothing without an active provider.
func MigrateDir(ctx context.Context, dir string) (int, error) {
	if Active() == nil {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	var errs []error
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		n++
		if _, err := ReadFile(ctx, filepath.Join(dir, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return n, errors.Join(errs...)
}

// readCapped reads at most MaxFileBytes of path.
func readCapped(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size() > MaxFileBytes {
		return nil, fmt.Errorf("%s: %w", path, ErrFileTooLarge)
	}
	return os.ReadFile(path)
}

// replaceFile writes body to a temp file beside path at mode and renames it
// over path, so a reader sees the old file or the new one, never a partial
// write, and the secret is never briefly readable by anyone else.
func replaceFile(path string, body []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		return cleanup(err)
	}
	if _, err := tmp.Write(body); err != nil {
		return cleanup(err)
	}
	if err := tmp.Sync(); err != nil {
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
