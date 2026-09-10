package hub

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSaveLoopFlushWarnsOnUnwritableRegistry covers saveLoopFlush's
// warn-and-continue error branch: a failed periodic save must neither panic
// nor kill the loop — the next requestSave retries. The success half is
// exercised by every test that joins the save loop; the failure half was not.
func TestSaveLoopFlushWarnsOnUnwritableRegistry(t *testing.T) {
	srv := newHubServerForTest(t)

	// Point this server's registry at a file inside a directory that does not
	// exist, so the temp-file write in saveRegistryNow reliably fails.
	srv.registryPath = filepath.Join(t.TempDir(), "no-such-dir", "hub-registry.json")

	// Must return normally despite the write error.
	srv.saveLoopFlush()

	if _, err := os.Stat(srv.registryPath); !os.IsNotExist(err) {
		t.Errorf("registry file unexpectedly present or unexpected stat error: %v", err)
	}
}
