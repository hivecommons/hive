package nous

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestExperimentStoreIndependentOfCampaignStore(t *testing.T) {
	dir := t.TempDir()
	campaignPath := filepath.Join(dir, DefaultFileName)
	ledgerPath := filepath.Join(dir, "experiments.json")
	campaign, _, err := NewStore(campaignPath).Ensure("campaign question", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	want := []Experiment{exampleExperiment()}
	if err := NewExperimentStore(ledgerPath).Save(want); err != nil {
		t.Fatal(err)
	}
	gotCampaign, ok := NewStore(campaignPath).Active()
	if !ok || !reflect.DeepEqual(gotCampaign, campaign) {
		t.Fatalf("ledger save changed campaign: got %+v, active=%v", gotCampaign, ok)
	}
	if err := NewStore(campaignPath).SetQuestion("updated campaign question"); err != nil {
		t.Fatal(err)
	}
	got, err := NewExperimentStore(ledgerPath).Load()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("campaign update changed ledger: got %#v, error %v", got, err)
	}
}

func TestExperimentStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nous", "experiments.json")
	store := NewExperimentStore(path)
	got, err := store.Load()
	if err != nil || len(got) != 0 {
		t.Fatalf("missing ledger: %v %v", got, err)
	}
	e := exampleExperiment()
	addResult(&e)
	want := []Experiment{e}
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err = NewExperimentStore(path).Load()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip: got %#v, error %v", got, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("ledger permissions: %v", info.Mode())
	}
	// Load is detached from both earlier loads and the input to Save.
	got[0].Cohorts[0].HiveIDs[0] = "modified"
	again, err := store.Load()
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("load aliases state: %v", err)
	}
	if err := store.Save(nil); err != nil {
		t.Fatal(err)
	}
	got, err = store.Load()
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty ledger: %v %v", got, err)
	}
}

func TestSaveRejectsInvalidStateWithoutReplacingLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "experiments.json")
	store := NewExperimentStore(path)
	e := exampleExperiment()
	if err := store.Save([]Experiment{e}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	invalid := e
	invalid.Window.BaselineHours = 0
	for _, experiments := range [][]Experiment{{invalid}, {e, e}} {
		if err := store.Save(experiments); err == nil {
			t.Fatal("accepted invalid ledger")
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Fatal("invalid save replaced ledger")
		}
	}
}

func TestLoadRejectsCorruptionAndInvalidRecords(t *testing.T) {
	e := exampleExperiment()
	valid, err := json.Marshal(stateFile{Version: storeVersion, Experiments: []Experiment{e, e}})
	if err != nil {
		t.Fatal(err)
	}
	invalid := exampleExperiment()
	invalid.Subject.Kind = "repo"
	invalidJSON, err := json.Marshal(stateFile{Version: storeVersion, Experiments: []Experiment{invalid}})
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"", "null", "{}", "{", `{"version":2,"experiments":[]}`, string(valid), string(invalidJSON)} {
		t.Run(data, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger.json")
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := NewExperimentStore(path).Load(); err == nil {
				t.Fatal("accepted invalid ledger")
			}
		})
	}
}

func TestExperimentStoreIOFailures(t *testing.T) {
	dir := t.TempDir()
	// Reading a directory is not a missing ledger.
	if _, err := NewExperimentStore(dir).Load(); err == nil {
		t.Fatal("ignored read error")
	}
	// A regular file cannot be used as the ledger's parent directory.
	parent := filepath.Join(dir, "file")
	if err := os.WriteFile(parent, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewExperimentStore(filepath.Join(parent, "ledger.json")).Save(nil); err == nil {
		t.Fatal("ignored mkdir failure")
	}
	// Rename onto a directory must fail and clean up its temporary file.
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := NewExperimentStore(target).Save(nil); err == nil {
		t.Fatal("ignored rename failure")
	}
	temps, err := filepath.Glob(filepath.Join(dir, ".nous-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("leaked temporary files: %v", temps)
	}
}

func TestExperimentStoreConcurrentIO(t *testing.T) {
	store := NewExperimentStore(filepath.Join(t.TempDir(), "ledger.json"))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 10; n++ {
				if err := store.Save([]Experiment{exampleExperiment()}); err != nil {
					t.Error(err)
					return
				}
				if _, err := store.Load(); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
