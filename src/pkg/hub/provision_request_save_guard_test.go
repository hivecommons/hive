package hub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// saveProvisionRequest must apply the same path-traversal guard as
// loadProvisionRequest and deleteProvisionRequest: a Username carrying "..",
// "/" or "\" must be refused, never joined into a filename.
func TestSaveProvisionRequestRejectsTraversalUsernames(t *testing.T) {
	oldDir := provisionRequestsDir
	provisionRequestsDir = t.TempDir()
	t.Cleanup(func() { provisionRequestsDir = oldDir })

	for _, bad := range []string{
		"../escape",
		"..",
		"a/b",
		`a\b`,
		"github:../../etc/cron.d/x",
	} {
		err := saveProvisionRequest(&ProvisionRequest{Username: bad, Status: provisionStatusPending})
		if err == nil {
			t.Errorf("saveProvisionRequest(%q) = nil error, want traversal rejection", bad)
		}
	}

	entries, _ := os.ReadDir(provisionRequestsDir)
	for _, e := range entries {
		t.Errorf("unexpected file written to provision dir: %s", e.Name())
	}
	// Nothing may have escaped the directory either.
	if _, err := os.Stat(filepath.Join(filepath.Dir(provisionRequestsDir), "escape.json")); err == nil {
		t.Error("traversal username escaped the provision requests directory")
	}
}

// A normal username still round-trips.
func TestSaveProvisionRequestAcceptsValidUsername(t *testing.T) {
	oldDir := provisionRequestsDir
	provisionRequestsDir = t.TempDir()
	t.Cleanup(func() { provisionRequestsDir = oldDir })

	pr := &ProvisionRequest{Username: "github:someuser", Status: provisionStatusPending}
	if err := saveProvisionRequest(pr); err != nil {
		t.Fatalf("saveProvisionRequest(valid) = %v", err)
	}
	got := loadProvisionRequest("github:someuser")
	if got == nil || !strings.EqualFold(got.Username, pr.Username) {
		t.Fatalf("round-trip failed: got %+v", got)
	}
}
