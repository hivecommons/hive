package beads

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestCrossProcessCreateHelper is not a test: it is the body of the child
// processes spawned by TestCrossProcessCreateLosesNoBeads. Each invocation is
// a stand-in for a burst of `bd create` CLI calls — its own process, its own
// Store per create, exactly like the CLI. It blocks on a start-barrier file
// so every worker's load-modify-persist cycles genuinely overlap.
func TestCrossProcessCreateHelper(t *testing.T) {
	if os.Getenv("BEADS_XPROC_HELPER") != "1" {
		t.Skip("helper for TestCrossProcessCreateLosesNoBeads")
	}
	dir := os.Getenv("BEADS_XPROC_DIR")
	barrier := os.Getenv("BEADS_XPROC_BARRIER")
	for {
		if _, err := os.Stat(barrier); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	count, err := strconv.Atoi(os.Getenv("BEADS_XPROC_COUNT"))
	if err != nil || count < 1 {
		fmt.Fprintln(os.Stderr, "helper: bad BEADS_XPROC_COUNT")
		os.Exit(1)
	}
	prefix := os.Getenv("BEADS_XPROC_TITLE")
	for i := 0; i < count; i++ {
		store, err := NewStore(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "helper NewStore: %v\n", err)
			os.Exit(1)
		}
		if _, err := store.Create(fmt.Sprintf("%s-%03d", prefix, i), TypeTask, PriorityMedium, "xproc", ""); err != nil {
			fmt.Fprintf(os.Stderr, "helper Create: %v\n", err)
			os.Exit(1)
		}
	}
}

// Regression test for #4742: concurrent bd processes lost beads. Each `bd`
// CLI invocation is its own process, so the store's in-process RWMutex never
// serialized them: two parallel creates both snapshotted beads.json, both
// rewrote it whole through the SAME fixed temp name, and either one rename
// failed with ENOENT or the loser's snapshot landed last and silently dropped
// the winner's bead — a create that reported success was absent afterwards.
//
// The test spawns real subprocesses (the test binary re-execed into
// TestCrossProcessCreateHelper) doing one Create each against a shared store
// dir, and asserts every invocation succeeded AND every bead survived.
// Before the flock + unique-temp fix this failed in a handful of runs.
func TestCrossProcessCreateLosesNoBeads(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locating test binary: %v", err)
	}
	dir := t.TempDir()
	barrier := filepath.Join(t.TempDir(), "start")

	const workers = 6
	const perWorker = 15
	cmds := make([]*exec.Cmd, 0, workers)
	for i := 0; i < workers; i++ {
		cmd := exec.Command(exe, "-test.run", "^TestCrossProcessCreateHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(),
			"BEADS_XPROC_HELPER=1",
			"BEADS_XPROC_DIR="+dir,
			"BEADS_XPROC_BARRIER="+barrier,
			fmt.Sprintf("BEADS_XPROC_COUNT=%d", perWorker),
			fmt.Sprintf("BEADS_XPROC_TITLE=xproc bead %02d", i),
		)
		cmds = append(cmds, cmd)
	}
	// Start them all, then release the barrier, so the load-modify-persist
	// cycles genuinely overlap.
	for i, cmd := range cmds {
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatalf("starting helper %d: %v", i, err)
		}
	}
	if err := os.WriteFile(barrier, nil, 0644); err != nil {
		t.Fatalf("releasing barrier: %v", err)
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Errorf("helper %d exited with error: %v", i, err)
		}
	}

	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	beads := store.List(ListFilter{})
	titles := make(map[string]bool, len(beads))
	for _, b := range beads {
		titles[b.Title] = true
	}
	missing := 0
	for i := 0; i < workers; i++ {
		for j := 0; j < perWorker; j++ {
			title := fmt.Sprintf("xproc bead %02d-%03d", i, j)
			if !titles[title] {
				missing++
				t.Errorf("bead %q reported created but is missing from the store", title)
			}
		}
	}
	if want := workers * perWorker; len(beads) != want {
		t.Errorf("store holds %d beads, want %d (%d confirmed lost)", len(beads), want, missing)
	}
}

// Two Store handles on the same directory model two processes: each handle
// snapshots the file at open and rewrites it whole on every mutation. These
// pins are the refreshFromDisk merge rules that keep them from clobbering
// each other.
func TestRefreshMergesConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	a, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("create by one writer survives the other's persist", func(t *testing.T) {
		if _, err := a.Create("from A", TypeTask, PriorityMedium, "a", ""); err != nil {
			t.Fatal(err)
		}
		// b has never seen "from A": its next persist used to overwrite it.
		if _, err := b.Create("from B", TypeTask, PriorityMedium, "b", ""); err != nil {
			t.Fatal(err)
		}
		fresh, err := NewStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, bd := range fresh.List(ListFilter{}) {
			got[bd.Title] = true
		}
		if !got["from A"] || !got["from B"] {
			t.Errorf("store lost a concurrent create: %v", got)
		}
	})

	t.Run("newer update by one writer survives the other's update to a different bead", func(t *testing.T) {
		x, err := a.Create("shared", TypeTask, PriorityMedium, "a", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := b.Reload(); err != nil {
			t.Fatal(err)
		}
		if err := a.SetMetadata(x.ID, "k", "from-a"); err != nil {
			t.Fatal(err)
		}
		// b's stale copy of x lacks the metadata; its own mutation must not
		// roll x back.
		if _, err := b.Create("unrelated", TypeTask, PriorityMedium, "b", ""); err != nil {
			t.Fatal(err)
		}
		fresh, err := NewStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		bead, err := fresh.Get(x.ID)
		if err != nil {
			t.Fatal(err)
		}
		if bead.Metadata["k"] != "from-a" {
			t.Errorf("metadata k = %v, want from-a (stale writer rolled back a newer update)", bead.Metadata["k"])
		}
	})

	t.Run("bead archived by one writer is not resurrected by the other", func(t *testing.T) {
		w, err := a.Create("to archive", TypeTask, PriorityMedium, "a", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Close(w.ID); err != nil {
			t.Fatal(err)
		}
		if err := b.Reload(); err != nil { // b now holds w
			t.Fatal(err)
		}
		if err := a.Archive(w.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Create("after archive", TypeTask, PriorityMedium, "b", ""); err != nil {
			t.Fatal(err)
		}
		fresh, err := NewStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fresh.Get(w.ID); err == nil {
			t.Error("archived bead was resurrected by a stale concurrent writer")
		}
		if !fresh.IsRetired(w.ID) {
			t.Error("archived bead lost its retired mark")
		}
	})
}
