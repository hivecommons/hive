package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxSpecialistCodexAuthBytes = 2 << 20

func prepareSpecialistCodexHome(sourceHome, workDir string) (string, error) {
	if sourceHome != strings.TrimSpace(sourceHome) || sourceHome == "" || !filepath.IsAbs(sourceHome) {
		return "", errors.New("specialist Codex home must be one absolute authenticated directory")
	}
	authSource := filepath.Join(filepath.Clean(sourceHome), "auth.json")
	auth, err := readStableSpecialistAuth(authSource)
	if err != nil {
		return "", fmt.Errorf("read authenticated specialist Codex home: %w", err)
	}
	parent := filepath.Join(workDir, ".codex-homes")
	if err := secureDirectory(parent); err != nil {
		return "", fmt.Errorf("secure specialist Codex home root: %w", err)
	}
	home, err := os.MkdirTemp(parent, "manager-")
	if err != nil {
		return "", fmt.Errorf("create isolated specialist Codex home: %w", err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		_ = os.Remove(home)
		return "", fmt.Errorf("secure isolated specialist Codex home: %w", err)
	}
	if err := atomicWriteFile(filepath.Join(home, "auth.json"), auth, false); err != nil {
		_ = os.RemoveAll(home)
		return "", fmt.Errorf("copy specialist Codex authentication: %w", err)
	}
	return home, nil
}

func readStableSpecialistAuth(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil || before == nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() <= 1 || before.Size() > maxSpecialistCodexAuthBytes {
		return nil, errors.New("Codex auth.json is not a bounded ordinary file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxSpecialistCodexAuthBytes+1))
	opened, statErr := file.Stat()
	closeErr := file.Close()
	after, lstatErr := os.Lstat(path)
	if readErr != nil || statErr != nil || closeErr != nil || lstatErr != nil || int64(len(data)) != before.Size() || len(data) > maxSpecialistCodexAuthBytes || !os.SameFile(before, opened) || !os.SameFile(before, after) || after.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("Codex auth.json changed while copying")
	}
	if !json.Valid(data) {
		return nil, errors.New("Codex auth.json is not valid JSON")
	}
	return data, nil
}

func cleanupSpecialistCodexHome(home string) error {
	if home == "" {
		return nil
	}
	home = filepath.Clean(home)
	if !strings.HasPrefix(filepath.Base(home), "manager-") || filepath.Base(filepath.Dir(home)) != ".codex-homes" {
		return errors.New("refuse unsafe specialist Codex home cleanup")
	}
	info, err := os.Lstat(home)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	reparse, reparseErr := specialistProviderPathIsReparsePoint(home)
	if reparseErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || reparse {
		return errors.New("refuse linked or replaced specialist Codex home cleanup")
	}
	// RemoveAll does not follow directory symlinks. The exact randomized root
	// was created by Hive beneath its private work directory and passed the
	// final-component link/reparse check above.
	return os.RemoveAll(home)
}
