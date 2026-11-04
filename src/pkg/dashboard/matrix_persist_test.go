package dashboard

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/sentinel"
)

// The superseded persistent line history must never re-enter either feed.
// Existing files are left for the operator to remove, rather than silently
// deleting their data as part of a dashboard config or software upgrade.
func TestMatrixCodeHistoryIsMemoryOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "matrix-lines.json")
	seed := []byte(`{"seq":7,"lines":[{"seq":7,"repo":"org/repo","pr":1,"line":"old retained code"}]}`)
	if err := os.WriteFile(path, seed, 0600); err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	s.SetMatrixPersistPath(path)
	observe := s.MatrixObserver(matrixTestConfig())
	defer func() { s.matrix.close(); <-s.matrix.done }()
	if s.matrix.Latest() != 0 || len(s.matrix.checkpoints.since(0, 200).Regions) != 0 {
		t.Fatal("legacy persistent code restored")
	}
	observe(matrixObs(1, "h", sentinel.File{Path: "a.go", Patch: checkpointPatch("fresh checkpoint")}))
	deadline := time.After(2 * time.Second)
	for s.matrix.Latest() == 0 {
		select {
		case <-deadline:
			t.Fatal("observer did not ingest")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	s.SetMatrixPersistPath(path)
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(seed) {
		t.Fatal("legacy history file changed")
	}
	var nilServer *Server
	nilServer.SetMatrixPersistPath(path)
}
