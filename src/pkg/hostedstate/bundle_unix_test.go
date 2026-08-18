//go:build !windows

package hostedstate

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestExportRejectsNonRegularFile(t *testing.T) {
	root := privateStateRoot(t, nil)
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
		t.Skipf("FIFO unavailable: %v", err)
	}
	_, err := Export(ExportOptions{Root: root, Paths: []string{"pipe"}, Metadata: testMetadata(), Secret: testSecret})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("Export() error = %v", err)
	}
}
