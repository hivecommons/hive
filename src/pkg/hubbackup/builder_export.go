package hubbackup

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This file exposes the archive builder so other packages can produce archives
// in the SAME sealed format, with the same SHA-256 manifest and the same
// AES-256-GCM sealing, instead of reimplementing crypto.
//
// pkg/spokebackup is the first consumer: an owner-triggered, per-hive backup
// that includes the bead ledger the fleet-wide hub backup deliberately omits
// (issue #2318). Sharing this builder means Verify and Extract accept both
// archive kinds unchanged, so there is exactly one restore path to trust.
//
// The unexported builder used by Build is untouched; these are thin wrappers.

// Builder accumulates files into a gzipped tar and records their digests.
// Create one with NewBuilder, add content, then call Finish to seal it.
type Builder struct {
	b   *builder
	raw *strings.Builder
}

// NewBuilder returns a Builder ready to accept files.
func NewBuilder() *Builder {
	var raw strings.Builder
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	return &Builder{
		b:   &builder{tw: tw, gz: gz, buf: &raw},
		raw: &raw,
	}
}

// FormatVersion reports the archive format version this build understands, so
// other packages stamp manifests consistently rather than hardcoding a number
// that could drift from Verify's expectation.
func FormatVersion() int { return backupFormatVersion }

// AddBytes writes one in-memory file into the archive and records its digest.
func (x *Builder) AddBytes(name string, mode int64, content []byte) error {
	return x.b.addBytes(name, mode, content)
}

// AddTree recursively copies srcDir into the archive under dstPrefix.
//
// skip, when non-nil, is called with each path relative to srcDir; returning
// true omits that file or (for a directory) prunes the whole subtree. This
// lets a caller apply its own exclusions rather than the hub's fixed set.
func (x *Builder) AddTree(srcDir, dstPrefix string, skip func(rel string) bool, logger *slog.Logger) error {
	return filepath.WalkDir(srcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// A single unreadable path must not abort the whole backup.
			logger.Warn("backup: skipping unreadable path", "path", path, "err", err)
			return nil
		}
		rel, relErr := filepath.Rel(srcDir, path)
		if relErr != nil || rel == "." {
			return nil
		}
		if skip != nil && skip(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			logger.Warn("backup: skipping unreadable file", "path", path, "err", readErr)
			return nil
		}
		mode := int64(0o600)
		if info, infoErr := d.Info(); infoErr == nil {
			mode = int64(info.Mode().Perm())
		}
		return x.b.addBytes(filepath.Join(dstPrefix, rel), mode, content)
	})
}

// Finish writes the manifest, closes the archive and seals it with key.
//
// The manifest is written LAST and its Files list is populated from the
// digests accumulated so far — copying them in is what makes Verify
// meaningful, since an empty Files list would make verification vacuous.
//
// maxBytes, when positive, caps the assembled plaintext so a caller streaming
// to a browser cannot be handed an unbounded archive.
func (x *Builder) Finish(key []byte, man *Manifest, maxBytes int) ([]byte, error) {
	if man == nil {
		return nil, fmt.Errorf("manifest is required")
	}
	man.Files = x.b.files

	manJSON, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	hdr := &tar.Header{Name: manifestName, Mode: 0o644, Size: int64(len(manJSON)), ModTime: time.Now()}
	if err := x.b.tw.WriteHeader(hdr); err != nil {
		return nil, err
	}
	if _, err := x.b.tw.Write(manJSON); err != nil {
		return nil, err
	}
	if err := x.b.tw.Close(); err != nil {
		return nil, fmt.Errorf("close tar: %w", err)
	}
	if err := x.b.gz.Close(); err != nil {
		return nil, fmt.Errorf("close gzip: %w", err)
	}

	plain := x.raw.String()
	if maxBytes > 0 && len(plain) > maxBytes {
		return nil, fmt.Errorf("archive is %d bytes, above the %d-byte cap", len(plain), maxBytes)
	}
	sealed, err := Seal(key, []byte(plain))
	if err != nil {
		return nil, fmt.Errorf("seal archive: %w", err)
	}
	return sealed, nil
}
