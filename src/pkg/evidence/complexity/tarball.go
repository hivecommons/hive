package complexity

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MaxExtractBytes caps the uncompressed size ExtractTarGz will write.
const MaxExtractBytes int64 = 2 << 30

// ErrArchiveTooLarge is returned when an archive exceeds MaxExtractBytes.
var ErrArchiveTooLarge = errors.New("complexity: archive exceeds size cap")

// ExtractTarGz extracts a gzip tarball into dest, writing only regular
// files and directories (symlinks and other entries are skipped) and
// refusing paths that escape dest. It returns the archive's single
// top-level directory (GitHub archives wrap the tree in
// "<owner>-<repo>-<sha>/"), or dest itself when there is none.
func ExtractTarGz(r io.Reader, dest string, maxBytes int64) (string, error) {
	if maxBytes <= 0 {
		maxBytes = MaxExtractBytes
	}
	dest, err := filepath.Abs(dest)
	if err != nil {
		return "", err
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var written int64
	tops := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		if name == "." || name == "" {
			continue
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("complexity: archive entry %q escapes destination", hdr.Name)
		}
		target := filepath.Join(dest, name)
		if rel, rerr := filepath.Rel(dest, target); rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("complexity: archive entry %q escapes destination", hdr.Name)
		}
		top := name
		if i := strings.IndexRune(name, filepath.Separator); i >= 0 {
			top = name[:i]
		}
		tops[top] = true
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
		case tar.TypeReg:
			if written += hdr.Size; written > maxBytes {
				return "", ErrArchiveTooLarge
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return "", err
			}
			_, cerr := io.Copy(f, io.LimitReader(tr, hdr.Size))
			if err := f.Close(); err != nil && cerr == nil {
				cerr = err
			}
			if cerr != nil {
				return "", cerr
			}
		}
	}
	if len(tops) == 1 {
		for top := range tops {
			p := filepath.Join(dest, top)
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				return p, nil
			}
		}
	}
	return dest, nil
}
