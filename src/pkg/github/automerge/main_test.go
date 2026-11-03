package automerge

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Default engines must not read or write a deployed hive's preparation state.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "automerge-hermetic-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "automerge TestMain:", err)
		os.Exit(1)
	}
	sharedBasePreparation = NewBasePreparationStore(filepath.Join(dir, "base-preparation"))

	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, "automerge TestMain cleanup:", err)
		code = 1
	}
	os.Exit(code)
}
