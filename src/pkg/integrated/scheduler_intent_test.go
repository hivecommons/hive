package integrated

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSchedulerStartIntentIsDurableBoundAndCancelable(t *testing.T) {
	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Config{Repository: "owner/repo", RepositoryID: "123", StateDir: stateDir}); err != nil {
		t.Fatal(err)
	}
	requestedAt := time.Now().UTC().Round(0)
	intent := SchedulerStartIntent{Repository: "owner/repo", StateDir: stateDir, IntervalSeconds: 900, RequestedAt: requestedAt}
	if err := store.SaveSchedulerStartIntent(intent); err != nil {
		t.Fatal(err)
	}
	loaded, exists, err := store.LoadSchedulerStartIntent()
	if err != nil || !exists {
		t.Fatalf("load scheduler start intent: exists=%t err=%v", exists, err)
	}
	if loaded.SchemaVersion != SchedulerStartIntentSchema || loaded.Repository != intent.Repository || loaded.IntervalSeconds != intent.IntervalSeconds || !loaded.RequestedAt.Equal(requestedAt) {
		t.Fatalf("scheduler start intent drifted: %+v", loaded)
	}
	if err := store.SaveSchedulerStartIntent(SchedulerStartIntent{Repository: "other/repo", StateDir: stateDir, IntervalSeconds: 900}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("cross-repository scheduler intent was accepted: %v", err)
	}
	if err := store.DeleteSchedulerStartIntent(); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := store.LoadSchedulerStartIntent(); err != nil || exists {
		t.Fatalf("deleted scheduler intent remained: exists=%t err=%v", exists, err)
	}
}

func TestSchedulerStartIntentRejectsLinkedState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not guaranteed for unprivileged Windows tests")
	}
	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Config{Repository: "owner/repo", RepositoryID: "123", StateDir: stateDir}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(stateDir, "target.json")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(store.Dir(), schedulerStartIntentFile)); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, _, err := store.LoadSchedulerStartIntent(); err == nil || !strings.Contains(err.Error(), "linked or non-regular") {
		t.Fatalf("linked scheduler state was accepted: %v", err)
	}
}
