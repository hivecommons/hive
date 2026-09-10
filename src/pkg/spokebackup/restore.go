package spokebackup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/hivecommons/hive/pkg/hubbackup"
)

const maxHiveIDBytes = 4096

// RestoreOptions controls application of a spoke backup archive to a data dir.
type RestoreOptions struct {
	// DestDir is the target spoke data directory, usually /data.
	DestDir string

	// DryRun validates and plans the restore without writing any files.
	DryRun bool

	// Force permits replacing a destination hive-id that differs from the
	// archive. Without it, restores only proceed into a fresh destination or
	// one already carrying the same hive-id.
	Force bool
}

// RestoreResult describes a validated or applied spoke restore.
type RestoreResult struct {
	Manifest       *hubbackup.Manifest
	ArchiveHiveID  string
	ExistingHiveID string
	FilesPlanned   []string
	FilesWritten   int
	DryRun         bool
	Forced         bool
}

type restoreEntry struct {
	targetRel string
	content   []byte
	mode      os.FileMode
}

type restoreOwner struct {
	uid int
	gid int
	ok  bool
}

// Restore decrypts and verifies a pkg/spokebackup archive, maps spoke/* to the
// destination data-dir root and beads/* to destination/beads/*, and refuses to
// overwrite a different live hive-id unless Force is set.
func Restore(key, sealed []byte, opts RestoreOptions) (*RestoreResult, error) {
	if strings.TrimSpace(opts.DestDir) == "" {
		return nil, fmt.Errorf("restore requires a destination data directory")
	}
	man, err := hubbackup.Verify(key, sealed)
	if err != nil {
		return nil, err
	}
	entries, archiveHiveID, err := readRestoreEntries(key, sealed, man)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("archive contains no spoke state to restore")
	}

	existingHiveID, err := readExistingHiveID(opts.DestDir)
	if err != nil {
		return nil, err
	}
	if archiveHiveID != "" && existingHiveID != "" && archiveHiveID != existingHiveID && !opts.Force {
		return nil, fmt.Errorf("refusing to restore hive-id %q over existing hive-id %q without -force",
			archiveHiveID, existingHiveID)
	}
	if archiveHiveID == "" {
		return nil, fmt.Errorf("refusing to restore archive with no spoke/hive-id; use extract for partial manual recovery")
	}

	files := make([]string, 0, len(entries))
	for _, e := range entries {
		files = append(files, filepath.ToSlash(e.targetRel))
	}
	sort.Strings(files)
	res := &RestoreResult{
		Manifest:       man,
		ArchiveHiveID:  archiveHiveID,
		ExistingHiveID: existingHiveID,
		FilesPlanned:   files,
		DryRun:         opts.DryRun,
		Forced:         opts.Force,
	}
	if opts.DryRun {
		return res, nil
	}
	destAbs, owner, err := prepareRestoreRoot(opts.DestDir)
	if err != nil {
		return nil, err
	}

	for _, e := range entries {
		out := filepath.Join(destAbs, e.targetRel)
		if err := mkdirSafeParent(destAbs, e.targetRel, owner); err != nil {
			return nil, fmt.Errorf("create parent for %s: %w", e.targetRel, err)
		}
		if err := writeFileNoFollow(out, e.content, e.mode, owner); err != nil {
			return nil, fmt.Errorf("write %s: %w", e.targetRel, err)
		}
		res.FilesWritten++
	}
	return res, nil
}

func readExistingHiveID(destDir string) (string, error) {
	path := filepath.Join(destDir, hiveIDFile)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("stat existing hive-id: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("existing hive-id %s is a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("existing hive-id %s is not a regular file", path)
	}
	if info.Size() > maxHiveIDBytes {
		return "", fmt.Errorf("existing hive-id %s exceeds the %d-byte limit", path, maxHiveIDBytes)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("open existing hive-id: %w", err)
	}
	defer func() { _ = f.Close() }()
	content, err := io.ReadAll(io.LimitReader(f, maxHiveIDBytes+1))
	if err != nil {
		return "", fmt.Errorf("read existing hive-id: %w", err)
	}
	if len(content) > maxHiveIDBytes {
		return "", fmt.Errorf("existing hive-id %s exceeds the %d-byte limit", path, maxHiveIDBytes)
	}
	return strings.TrimSpace(string(content)), nil
}

func readRestoreEntries(key, sealed []byte, man *hubbackup.Manifest) ([]restoreEntry, string, error) {
	want := map[string]string{}
	for _, f := range man.Files {
		want[f.Path] = f.SHA256
	}

	plain, err := hubbackup.Open(key, sealed)
	if err != nil {
		return nil, "", err
	}
	gz, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		return nil, "", fmt.Errorf("gunzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	var entries []restoreEntry
	var archiveHiveID string
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("read tar: %w", err)
		}
		if hdr.FileInfo().IsDir() {
			continue
		}
		content, err := readRestoreMember(tr, hdr.Name)
		if err != nil {
			return nil, "", err
		}
		if hdr.Name == "MANIFEST.json" {
			continue
		}
		targetRel, ok := restoreTarget(hdr.Name)
		if !ok {
			return nil, "", fmt.Errorf("archive path %q is not part of a spoke backup", hdr.Name)
		}
		wantSum := want[hdr.Name]
		if wantSum == "" {
			return nil, "", fmt.Errorf("archive path %q is not listed in MANIFEST.json", hdr.Name)
		}
		sum := sha256.Sum256(content)
		if got := hex.EncodeToString(sum[:]); got != wantSum {
			return nil, "", fmt.Errorf("checksum mismatch for %s", hdr.Name)
		}
		if filepath.ToSlash(targetRel) == hiveIDFile {
			archiveHiveID = strings.TrimSpace(string(content))
		}
		entries = append(entries, restoreEntry{
			targetRel: targetRel,
			content:   content,
			mode:      safeRestoreMode(hdr.Mode),
		})
	}
	return entries, archiveHiveID, nil
}

func restoreTarget(name string) (string, bool) {
	clean := filepath.ToSlash(filepath.Clean(name))
	if clean == "." || strings.HasPrefix(clean, "../") || clean == ".." || filepath.IsAbs(clean) {
		return "", false
	}
	switch {
	case strings.HasPrefix(clean, spokePrefix+"/"):
		rel := strings.TrimPrefix(clean, spokePrefix+"/")
		if rel == "" || rel == "." || strings.HasPrefix(rel, "../") {
			return "", false
		}
		return filepath.FromSlash(rel), true
	case strings.HasPrefix(clean, beadsPrefix+"/"):
		rel := strings.TrimPrefix(clean, beadsPrefix+"/")
		if rel == "" || rel == "." || strings.HasPrefix(rel, "../") {
			return "", false
		}
		return filepath.Join(beadsSubdir, filepath.FromSlash(rel)), true
	default:
		return "", false
	}
}

func readRestoreMember(r io.Reader, name string) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(r, MaxArchiveBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > MaxArchiveBytes {
		return nil, fmt.Errorf("archive member %q exceeds the %d-byte restore limit", name, int64(MaxArchiveBytes))
	}
	return content, nil
}

func prepareRestoreRoot(destDir string) (string, restoreOwner, error) {
	destAbs, err := filepath.Abs(destDir)
	if err != nil {
		return "", restoreOwner{}, err
	}
	info, err := os.Lstat(destAbs)
	if err != nil {
		return "", restoreOwner{}, fmt.Errorf("stat destination data directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", restoreOwner{}, fmt.Errorf("destination data directory %s is a symlink", destAbs)
	}
	if !info.IsDir() {
		return "", restoreOwner{}, fmt.Errorf("destination data directory %s is not a directory", destAbs)
	}
	return destAbs, ownerFromInfo(info), nil
}

func mkdirSafeParent(destAbs, targetRel string, owner restoreOwner) error {
	if err := rejectRelativeEscape(destAbs, targetRel); err != nil {
		return err
	}
	outAbs, err := filepath.Abs(filepath.Join(destAbs, targetRel))
	if err != nil {
		return err
	}
	parent := filepath.Dir(outAbs)
	parentRel, err := filepath.Rel(destAbs, parent)
	if err != nil {
		return err
	}
	cur := destAbs
	if parentRel == "." {
		return rejectFinalSymlink(outAbs)
	}
	for _, part := range strings.Split(parentRel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		next := filepath.Join(cur, part)
		info, err := os.Lstat(next)
		if err != nil {
			if os.IsNotExist(err) {
				if err := os.Mkdir(next, 0o755); err != nil {
					return err
				}
				if err := chownIfRoot(next, owner); err != nil {
					return err
				}
				cur = next
				continue
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink", next)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", next)
		}
		cur = next
	}
	return rejectFinalSymlink(outAbs)
}

func rejectRelativeEscape(destAbs, targetRel string) error {
	outAbs, err := filepath.Abs(filepath.Join(destAbs, targetRel))
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(destAbs, outAbs)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("target escapes destination")
	}
	return nil
}

func rejectFinalSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}
	return nil
}

func writeFileNoFollow(path string, content []byte, mode os.FileMode, owner restoreOwner) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(content)
	if writeErr != nil {
		_ = f.Close()
		return writeErr
	}
	if err := chownFileIfRoot(f, owner); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func ownerFromInfo(info os.FileInfo) restoreOwner {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return restoreOwner{}
	}
	return restoreOwner{uid: int(st.Uid), gid: int(st.Gid), ok: true}
}

func chownIfRoot(path string, owner restoreOwner) error {
	if !owner.ok || os.Geteuid() != 0 {
		return nil
	}
	return os.Chown(path, owner.uid, owner.gid)
}

func chownFileIfRoot(f *os.File, owner restoreOwner) error {
	if !owner.ok || os.Geteuid() != 0 {
		return nil
	}
	return f.Chown(owner.uid, owner.gid)
}

func safeRestoreMode(mode int64) os.FileMode {
	m := os.FileMode(mode) & 0o755
	if m == 0 {
		return 0o600
	}
	return m
}
