package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const maxSpecialistProviderBytes = 512 << 20

type specialistProviderIdentity struct {
	Path     string
	SHA256   string
	Bytes    int64
	SealRoot string
}

func sealSpecialistProvider(command, workDir string) (*specialistProviderIdentity, error) {
	if command != strings.TrimSpace(command) || command == "" || containsControlCharacter(command) {
		return nil, errors.New("specialist codex executable must be one exact native path")
	}
	if !filepath.IsAbs(command) {
		return nil, errors.New("specialist codex executable must be an absolute path")
	}
	source, err := inspectSpecialistProvider(command)
	if err != nil {
		return nil, fmt.Errorf("inspect specialist codex executable: %w", err)
	}
	if !specialistProviderIsNative(source.Path) {
		return nil, errors.New("specialist codex executable must be a native binary, not a script or package-runner wrapper")
	}
	sealParent := filepath.Join(workDir, ".provider-seals")
	if err := secureDirectory(sealParent); err != nil {
		return nil, fmt.Errorf("secure specialist provider seal root: %w", err)
	}
	sealRoot, err := os.MkdirTemp(sealParent, "manager-")
	if err != nil {
		return nil, fmt.Errorf("create specialist provider seal: %w", err)
	}
	if err := os.Chmod(sealRoot, 0o700); err != nil {
		_ = os.Remove(sealRoot)
		return nil, fmt.Errorf("secure specialist provider seal: %w", err)
	}
	destination := filepath.Join(sealRoot, "codex-"+source.SHA256+filepath.Ext(source.Path))
	if err := copySpecialistProvider(source.Path, destination); err != nil {
		_ = os.RemoveAll(sealRoot)
		return nil, err
	}
	sealed, err := inspectSpecialistProvider(destination)
	if err != nil || sealed.SHA256 != source.SHA256 || sealed.Bytes != source.Bytes {
		_ = os.RemoveAll(sealRoot)
		return nil, errors.New("sealed specialist codex executable does not match its source")
	}
	sealed.SealRoot = sealRoot
	return sealed, nil
}

func copySpecialistProvider(sourcePath, destination string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open specialist codex executable: %w", err)
	}
	defer source.Close()
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".codex-*.tmp")
	if err != nil {
		return fmt.Errorf("create specialist codex seal: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(0o500); err != nil {
		cleanup()
		return fmt.Errorf("secure temporary specialist codex seal: %w", err)
	}
	written, copyErr := io.Copy(temporary, io.LimitReader(source, maxSpecialistProviderBytes+1))
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil || written <= 0 || written > maxSpecialistProviderBytes {
		_ = os.Remove(temporaryPath)
		return errors.New("write bounded specialist codex seal")
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("activate specialist codex seal: %w", err)
	}
	if err := os.Chmod(destination, 0o500); err != nil {
		_ = os.Remove(destination)
		return fmt.Errorf("secure specialist codex seal: %w", err)
	}
	return nil
}

func inspectSpecialistProvider(candidate string) (*specialistProviderIdentity, error) {
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return nil, err
	}
	absolute = filepath.Clean(absolute)
	if err := rejectSpecialistProviderLinkedPath(absolute); err != nil {
		return nil, err
	}
	before, err := os.Lstat(absolute)
	if err != nil || before == nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() <= 0 || before.Size() > maxSpecialistProviderBytes {
		return nil, errors.New("path is not a bounded ordinary file")
	}
	if runtime.GOOS != "windows" && before.Mode().Perm()&0o111 == 0 {
		return nil, errors.New("native specialist codex executable is not executable")
	}
	file, err := os.Open(absolute)
	if err != nil {
		return nil, err
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(hasher, io.LimitReader(file, maxSpecialistProviderBytes+1))
	opened, statErr := file.Stat()
	closeErr := file.Close()
	after, lstatErr := os.Lstat(absolute)
	if copyErr != nil || statErr != nil || closeErr != nil || lstatErr != nil || written != before.Size() || written > maxSpecialistProviderBytes || !os.SameFile(before, opened) || !os.SameFile(before, after) || after.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("specialist codex executable changed while hashing")
	}
	return &specialistProviderIdentity{Path: absolute, SHA256: hex.EncodeToString(hasher.Sum(nil)), Bytes: written}, nil
}

func revalidateSpecialistProvider(expected *specialistProviderIdentity) error {
	if expected == nil || expected.Path == "" || expected.SHA256 == "" || expected.Bytes <= 0 {
		return errors.New("specialist codex executable is not bound")
	}
	actual, err := inspectSpecialistProvider(expected.Path)
	if err != nil || actual.SHA256 != expected.SHA256 || actual.Bytes != expected.Bytes {
		return errors.New("sealed specialist codex executable changed before launch")
	}
	return nil
}

func cleanupSpecialistProvider(identity *specialistProviderIdentity) error {
	if identity == nil {
		return nil
	}
	root := filepath.Clean(identity.SealRoot)
	executable := filepath.Clean(identity.Path)
	if root == "." || executable == "." || filepath.Dir(executable) != root || !strings.HasPrefix(filepath.Base(root), "manager-") || filepath.Base(filepath.Dir(root)) != ".provider-seals" {
		return errors.New("refuse unsafe specialist provider seal cleanup")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	reparse, reparseErr := specialistProviderPathIsReparsePoint(root)
	if reparseErr != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || reparse {
		return errors.New("refuse linked or replaced specialist provider seal cleanup")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(executable) || entries[0].Type()&os.ModeSymlink != 0 {
		return errors.New("refuse specialist provider seal cleanup with unexpected inventory")
	}
	if err := revalidateSpecialistProvider(identity); err != nil {
		return err
	}
	if err := os.Remove(executable); err != nil {
		return err
	}
	return os.Remove(root)
}

func rejectSpecialistProviderLinkedPath(absolute string) error {
	volume := filepath.VolumeName(absolute)
	remainder := strings.TrimPrefix(absolute, volume)
	remainder = strings.TrimLeft(remainder, string(filepath.Separator))
	cursor := volume + string(filepath.Separator)
	if volume == "" {
		cursor = string(filepath.Separator)
	}
	for _, component := range strings.Split(remainder, string(filepath.Separator)) {
		if component == "" {
			continue
		}
		cursor = filepath.Join(cursor, component)
		info, err := os.Lstat(cursor)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("path resolves through a symbolic link")
		}
		reparse, err := specialistProviderPathIsReparsePoint(cursor)
		if err != nil {
			return err
		}
		if reparse {
			return errors.New("path resolves through a reparse point")
		}
	}
	return nil
}

func specialistProviderIsNative(path string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Ext(path), ".exe")
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	header := make([]byte, 2)
	_, _ = io.ReadFull(file, header)
	return string(header) != "#!"
}

func containsControlCharacter(value string) bool {
	for _, character := range value {
		if character == 0 || character == '\n' || character == '\r' || character == '\t' {
			return true
		}
	}
	return false
}
