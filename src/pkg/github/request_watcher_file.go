package github

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const agentRequestMaxBytes = 1 << 20

// readAgentRequest opens only the ordinary file observed by Lstat and derives
// authorization identity from that same open file. This prevents a
// group-writable request directory from turning symlinks or file replacement
// races into a privileged Hive/App operation.
func readAgentRequest(path string) ([]byte, int, error) {
	observed, err := os.Lstat(path)
	if err != nil {
		return nil, -1, err
	}
	if !observed.Mode().IsRegular() || observed.Mode()&os.ModeSymlink != 0 {
		return nil, -1, fmt.Errorf("request path is not an ordinary file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, -1, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, -1, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(observed, opened) {
		return nil, -1, fmt.Errorf("request file changed while it was being opened")
	}
	data, err := io.ReadAll(io.LimitReader(file, agentRequestMaxBytes+1))
	if err != nil {
		return nil, -1, err
	}
	if len(data) > agentRequestMaxBytes {
		return nil, -1, fmt.Errorf("request exceeds %d-byte limit", agentRequestMaxBytes)
	}
	return data, fileOwnerUID(opened), nil
}

// writeWatcherResult replaces the result path atomically on Unix. In
// particular, it replaces a hostile symlink itself instead of following it
// into Hive's durable state. The Windows fallback is only for local tests,
// where rename-over-existing is not supported by the platform.
func writeWatcherResult(requestPath string, response any) error {
	data, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return err
	}
	output := strings.TrimSuffix(requestPath, ".json") + ".result.json"
	temporary, err := os.CreateTemp(filepath.Dir(output), ".hive-request-result-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, output); err == nil {
		return nil
	} else if runtime.GOOS != "windows" {
		return err
	}
	if err := os.Remove(output); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(temporaryPath, output)
}
