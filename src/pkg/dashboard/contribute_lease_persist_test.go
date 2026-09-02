package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// contribute_lease_persist_test.go covers kubestellar/hive#5681: a hub restart
// must not make every in-flight contributor task unresumable. Leases lived only
// in memory, so a restart emptied the registry, lookupLease rejected every
// reconnecting relay ("no active lease for this task"), the agent was
// interrupted mid-turn, and the SAME issue was re-assigned to the SAME relay
// seconds later. The registry is now a PVC-backed ledger; these tests exercise
// the restart round-trip and prove persistence grants nothing C4 forbids.

// leasePersistHub builds a hub whose contribute ledgers live in dir, so a
// second hub built over the same dir simulates a hub process restart.
func leasePersistHub(t *testing.T, dir string) *ContributeWSHub {
	t.Helper()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)
	redirectContributeWSDisk(t, dir)
	return covK2HubIn(t, dir)
}

// covK2HubIn is covK2Hub with an explicit dir (covK2Hub only creates a fresh
// temp dir when HIVE_CONTRIBUTORS_DIR is unset, which the caller controls).
func covK2HubIn(t *testing.T, dir string) *ContributeWSHub {
	t.Helper()
	hub, _ := covK2Hub(t)
	if got := hub.leasesFilePath(); filepath.Dir(got) != dir {
		t.Fatalf("lease ledger path %q is not in the test dir %q", got, dir)
	}
	return hub
}

// TestLeasePersist_ResumeSurvivesHubRestart is the core #5681 regression: a
// lease recorded before a restart must still be re-adoptable by the exact
// {identity, task, repo, number, generation} tuple after it, within its window.
func TestLeasePersist_ResumeSurvivesHubRestart(t *testing.T) {
	dir := t.TempDir()

	hub1 := leasePersistHub(t, dir)
	now := time.Now()
	hub1.recordLease("c-restart", "ct-5617", "myorg/repo1", 5617, "contributor", 75, now)

	// "Restart": a brand-new hub over the same ledger dir.
	hub2 := leasePersistHub(t, dir)
	if hub2.lookupLease("c-restart", "ct-5617", "myorg/repo1", 5617, 75, now.Add(time.Minute)) == nil {
		t.Fatalf("#5681: a lease issued before a hub restart could not be re-adopted "+
			"after it — the relay would be told %q and its agent interrupted mid-turn",
			"no active lease for this task")
	}

	// C4 exact-match still applies to a restored lease: wrong gen, task, repo,
	// or identity must all be rejected.
	if hub2.lookupLease("c-restart", "ct-5617", "myorg/repo1", 5617, 74, now.Add(time.Minute)) != nil {
		t.Fatalf("C4: a restored lease matched a stale generation")
	}
	if hub2.lookupLease("c-restart", "ct-other", "myorg/repo1", 5617, 75, now.Add(time.Minute)) != nil {
		t.Fatalf("C4: a restored lease matched a different task id")
	}
	if hub2.lookupLease("c-other", "ct-5617", "myorg/repo1", 5617, 75, now.Add(time.Minute)) != nil {
		t.Fatalf("C4: a restored lease matched a different identity")
	}
}

// TestLeasePersist_RevokedLeaseStaysRevokedAcrossRestart: a lease released
// before the restart is absent from the ledger and must not be resurrected —
// persistence must not reopen the resurrection window C4 closed.
func TestLeasePersist_RevokedLeaseStaysRevokedAcrossRestart(t *testing.T) {
	dir := t.TempDir()

	hub1 := leasePersistHub(t, dir)
	now := time.Now()
	hub1.recordLease("c-done", "ct-done", "myorg/repo1", 7, "contributor", 3, now)
	hub1.revokeLease("c-done", "ct-done")

	hub2 := leasePersistHub(t, dir)
	if hub2.lookupLease("c-done", "ct-done", "myorg/repo1", 7, 3, now.Add(time.Minute)) != nil {
		t.Fatalf("C4: a lease revoked before a restart was resurrected from the ledger")
	}
}

// TestLeasePersist_ExpiredLeaseNotRestored: a lease whose window elapsed while
// the hub was down is dropped at load, exactly as lookupLease would drop it.
func TestLeasePersist_ExpiredLeaseNotRestored(t *testing.T) {
	dir := t.TempDir()

	// Write a ledger with one already-expired lease, as if the hub had been
	// down for longer than leaseTTL.
	records := []taskLeaseRecord{{
		Identity:  "c-stale",
		TaskID:    "ct-stale",
		Repo:      "myorg/repo1",
		Number:    9,
		Tier:      "contributor",
		Gen:       4,
		ExpiresAt: time.Now().Add(-time.Minute),
	}}
	data, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, taskLeasesFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}

	hub := leasePersistHub(t, dir)
	if hub.lookupLease("c-stale", "ct-stale", "myorg/repo1", 9, 4, time.Now()) != nil {
		t.Fatalf("an expired lease was restored from the ledger and re-adopted")
	}
	hub.leaseMu.Lock()
	_, present := hub.leases["c-stale"]
	hub.leaseMu.Unlock()
	if present {
		t.Fatalf("an expired lease was loaded into the registry instead of dropped")
	}
}

// TestLeasePersist_RenewalExtendsWindowAcrossRestart: the #4260 renewal clock
// must survive a restart too — a long-running task renewed past its original
// assignment window is still re-adoptable after a restart.
func TestLeasePersist_RenewalExtendsWindowAcrossRestart(t *testing.T) {
	dir := t.TempDir()

	hub1 := leasePersistHub(t, dir)
	assigned := time.Now().Add(-2 * leaseTTL)
	hub1.recordLease("c-long", "ct-long", "myorg/repo1", 42, "contributor", 9, assigned)
	// The relay kept reporting progress; the last renewal was just now.
	hub1.renewLease("c-long", "ct-long", time.Now())

	hub2 := leasePersistHub(t, dir)
	if hub2.lookupLease("c-long", "ct-long", "myorg/repo1", 42, 9, time.Now().Add(time.Minute)) == nil {
		t.Fatalf("#5681/#4260: a renewed lease did not survive a restart with its "+
			"renewed window — expiry must persist from the LAST renewal (assigned %v ago)",
			2*leaseTTL)
	}
}

// TestLeasePersist_MalformedAndPartialRecordsSkipped: garbage in the ledger
// must not mint leases. Records missing the identity, task, or generation —
// the fields a resume must prove possession of — are skipped at load.
func TestLeasePersist_MalformedAndPartialRecordsSkipped(t *testing.T) {
	dir := t.TempDir()

	future := time.Now().Add(time.Hour)
	records := []taskLeaseRecord{
		{Identity: "", TaskID: "ct-a", Repo: "myorg/repo1", Number: 1, Gen: 2, ExpiresAt: future},
		{Identity: "c-b", TaskID: "", Repo: "myorg/repo1", Number: 2, Gen: 3, ExpiresAt: future},
		{Identity: "c-c", TaskID: "ct-c", Repo: "myorg/repo1", Number: 3, Gen: 0, ExpiresAt: future},
		{Identity: "c-ok", TaskID: "ct-ok", Repo: "myorg/repo1", Number: 4, Gen: 5, ExpiresAt: future},
	}
	data, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, taskLeasesFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}

	hub := leasePersistHub(t, dir)
	hub.leaseMu.Lock()
	count := len(hub.leases)
	hub.leaseMu.Unlock()
	if count != 1 {
		t.Fatalf("expected exactly 1 restored lease, got %d — partial records must be skipped", count)
	}
	if hub.lookupLease("c-ok", "ct-ok", "myorg/repo1", 4, 5, time.Now()) == nil {
		t.Fatalf("the one well-formed lease was not restored")
	}

	// A corrupt (non-JSON) ledger is ignored entirely rather than crashing.
	if err := os.WriteFile(filepath.Join(dir, taskLeasesFileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	hub2 := leasePersistHub(t, dir)
	hub2.leaseMu.Lock()
	count2 := len(hub2.leases)
	hub2.leaseMu.Unlock()
	if count2 != 0 {
		t.Fatalf("a corrupt ledger produced %d leases", count2)
	}
}
