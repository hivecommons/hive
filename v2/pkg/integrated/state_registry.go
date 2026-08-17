package integrated

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const currentStatePointer = "current-state"

// RememberCurrentState durably selects the repository-specific state directory
// used by subsequent Hive CLI and MCP commands that omit --state-dir.
func RememberCurrentState(root, stateDir string) error {
	root = filepath.Clean(strings.TrimSpace(root))
	stateDir = filepath.Clean(strings.TrimSpace(stateDir))
	if root == "." || stateDir == "." || !filepath.IsAbs(root) || !filepath.IsAbs(stateDir) {
		return fmt.Errorf("state registry root and selected state directory must be absolute")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create state registry: %w", err)
	}
	return writeDurableStateFile(filepath.Join(root, currentStatePointer), []byte(stateDir+"\n"))
}

// ForgetCurrentState durably removes the current repository pointer only when
// it still selects stateDir. This lets a completed managed uninstall retire
// its own selection without clearing a different repository selected by a
// concurrent or later setup.
func ForgetCurrentState(root, stateDir string) (bool, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	stateDir = filepath.Clean(strings.TrimSpace(stateDir))
	if root == "." || stateDir == "." || !filepath.IsAbs(root) || !filepath.IsAbs(stateDir) {
		return false, fmt.Errorf("state registry root and selected state directory must be absolute")
	}
	pointer := filepath.Join(root, currentStatePointer)
	data, err := os.ReadFile(pointer)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read current state pointer: %w", err)
	}
	if len(data) > 4096 {
		return false, fmt.Errorf("current state pointer is oversized")
	}
	selected := filepath.Clean(strings.TrimSpace(string(data)))
	if selected == "." || !filepath.IsAbs(selected) {
		return false, fmt.Errorf("current state pointer is invalid")
	}
	if !sameFilesystemPath(selected, stateDir) {
		return false, nil
	}
	if err := durableRemoveStateFile(pointer); err != nil {
		return false, fmt.Errorf("remove current state pointer: %w", err)
	}
	return true, nil
}

// CurrentState reads the durable current repository pointer. It never accepts
// a missing/non-directory target as active state.
func CurrentState(root string) (string, bool, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "." || !filepath.IsAbs(root) {
		return "", false, fmt.Errorf("state registry root must be absolute")
	}
	data, err := os.ReadFile(filepath.Join(root, currentStatePointer))
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read current state pointer: %w", err)
	}
	if len(data) > 4096 {
		return "", false, fmt.Errorf("current state pointer is oversized")
	}
	stateDir := filepath.Clean(strings.TrimSpace(string(data)))
	if stateDir == "." || !filepath.IsAbs(stateDir) {
		return "", false, fmt.Errorf("current state pointer is invalid")
	}
	info, err := os.Stat(stateDir)
	if err != nil || !info.IsDir() {
		return "", false, fmt.Errorf("current state directory %s is unavailable", stateDir)
	}
	return stateDir, true, nil
}
