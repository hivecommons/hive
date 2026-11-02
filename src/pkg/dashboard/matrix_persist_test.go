package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/sentinel"
)

// The backlog survives a restart (#16276): a new source on the same path
// serves the persisted lines, continues the sequence counter so a kiosk
// never sees seq go backwards, and treats a re-observed head as already
// seen.
func TestMatrixSourcePersistRestore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "matrix-lines.json")
	m, clk := newTestMatrixSource(t, matrixTestConfig())
	m.persistPath = path
	m.restore() // no file yet is the normal first boot
	if m.Latest() != 0 || len(m.ring) != 0 {
		t.Fatalf("missing file restored seq=%d lines=%d", m.Latest(), len(m.ring))
	}
	file := sentinel.File{Path: "a.go", Patch: matrixPatch("one", "two")}
	if got := m.ingest(matrixObs(1, "h1", file)); got != 2 {
		t.Fatalf("ingest = %d, want 2", got)
	}
	m.persist()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("persisted file: %v", err)
	}
	var f matrixPersistFile
	if err := json.Unmarshal(raw, &f); err != nil || f.Seq != 2 || len(f.Lines) != 2 || f.Lines[1].Line != "two" {
		t.Fatalf("persisted %+v err=%v", f, err)
	}

	restored, _ := newTestMatrixSource(t, matrixTestConfig())
	restored.now = clk.now
	restored.persistPath = path
	restored.restore()
	lines, latest := restored.Since(0, 10)
	if latest != 2 || len(lines) != 2 || lines[0].Line != "one" || lines[1].Seq != 2 {
		t.Fatalf("restored latest=%d lines=%+v", latest, lines)
	}
	if got := restored.ingest(matrixObs(1, "h1", file)); got != 0 {
		t.Fatalf("re-observed head after restart added %d lines", got)
	}
	if got := restored.ingest(matrixObs(2, "h", sentinel.File{Path: "b.go", Patch: matrixPatch("three")})); got != 1 {
		t.Fatalf("new PR after restart added %d", got)
	}
	if restored.Latest() != 3 {
		t.Fatalf("sequence did not continue: latest=%d", restored.Latest())
	}

	// A source with no path never touches disk.
	plain, _ := newTestMatrixSource(t, matrixTestConfig())
	plain.ingest(matrixObs(1, "h1", file))
	plain.persist()
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("unexpected files: %v", entries)
	}
}

func TestMatrixSourcePersistCapsAndTolerates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "matrix-lines.json")
	m, _ := newTestMatrixSource(t, matrixTestConfig())
	m.persistPath = path
	// More distinct lines than the persist cap, spread over PRs so the
	// per-head cap does not bite.
	total := matrixPersistLines + 50
	for pr := 0; pr*100 < total; pr++ {
		var ls []string
		for i := 0; i < 100 && pr*100+i < total; i++ {
			ls = append(ls, "line"+strconv.Itoa(pr*100+i))
		}
		m.ingest(matrixObs(pr+1, "h", sentinel.File{Path: "a.go", Patch: matrixPatch(ls...)}))
	}
	if m.Latest() != uint64(total) {
		t.Fatalf("latest=%d want %d", m.Latest(), total)
	}
	m.persist()
	restored, _ := newTestMatrixSource(t, matrixTestConfig())
	restored.persistPath = path
	restored.restore()
	lines, latest := restored.Since(0, matrixBacklogCap)
	if latest != uint64(total) || len(lines) != matrixPersistLines || lines[0].Seq != uint64(total-matrixPersistLines+1) {
		t.Fatalf("restored latest=%d n=%d first=%d", latest, len(lines), lines[0].Seq)
	}

	// Disabling the matrix empties the file too, so a later re-enable does
	// not resurrect lines captured under the old config.
	m.setConfig(config.MatrixConfig{})
	m.reset()
	empty, _ := newTestMatrixSource(t, matrixTestConfig())
	empty.persistPath = path
	empty.restore()
	if n := len(empty.ring); n != 0 || empty.Latest() != uint64(total) {
		t.Fatalf("after reset restored %d lines, seq %d", n, empty.Latest())
	}

	// Garbage on disk is logged and ignored, never fatal.
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad, _ := newTestMatrixSource(t, matrixTestConfig())
	bad.persistPath = path
	bad.restore()
	if bad.Latest() != 0 || len(bad.ring) != 0 {
		t.Fatalf("malformed file restored seq=%d lines=%d", bad.Latest(), len(bad.ring))
	}
}

// Server.SetMatrixPersistPath hands the path to the source MatrixObserver
// builds, and a source built first still picks the backlog up when it has
// nothing of its own.
func TestServerSetMatrixPersistPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "matrix-lines.json")
	seed := matrixPersistFile{Seq: 7, Lines: []matrixLine{{Seq: 7, Repo: matrixTestRepo, PR: 1, Head: "h", Path: "a.go", Line: "kept", At: time.Now().UTC()}}}
	raw, _ := json.Marshal(seed)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	s.SetMatrixPersistPath(path)
	if s.MatrixObserver(matrixTestConfig()) == nil {
		t.Fatal("no observer")
	}
	t.Cleanup(s.matrix.close)
	if lines, latest := s.matrix.Since(0, 10); latest != 7 || len(lines) != 1 || lines[0].Line != "kept" {
		t.Fatalf("observer-built source: latest=%d lines=%+v", latest, lines)
	}

	late := &Server{}
	if late.MatrixObserver(matrixTestConfig()) == nil {
		t.Fatal("no observer")
	}
	t.Cleanup(late.matrix.close)
	late.SetMatrixPersistPath(path)
	if latest := late.matrix.Latest(); latest != 7 {
		t.Fatalf("late path on an empty source: latest=%d, want 7", latest)
	}
	var nilSrv *Server
	nilSrv.SetMatrixPersistPath(path)
}
