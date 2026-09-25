package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// A run stage waiting on a taker is held by a server-side identity; leaseTTL
// is sized for a relay working a task, so without a keepalive the pending
// stage — and with it the whole run — aged out of the registry after 30
// minutes with no ready contributor.
func TestKeepPendingStageLeasesAlive_ExtendsSystemIdentitiesOnly(t *testing.T) {
	now := time.Now()
	h := &ContributeWSHub{logger: covBLogger()}
	record := func(identity, task, key, stage string) {
		if err := h.recordLeaseForKeyStage(identity, task, "kubestellar/console", 23725,
			key, "contributor", stage, 6, now.Add(-leaseTTL+time.Minute)); err != nil {
			t.Fatalf("record %s: %v", identity, err)
		}
	}
	record(runAdmissionIdentity, "admit", "kubestellar/console#23725", StageSpec)
	record(runFanoutIdentity, "fanout", "kubestellar/console#23725", StageImplement)
	record(config.DefaultSpektacularHubExecutorIdentity, "exec", "kubestellar/console#23725", StageImplement)
	// A relay on a DIFFERENT run: adopting the same key+stage would (rightly)
	// replace the hub executor's placeholder, which is not what this test is about.
	record("c-relay", "relay", "kubestellar/console#23726", StageImplement)
	// A non-stage relay lease must never be touched either.
	if err := h.recordLeaseForKey("c-plain", "plain", "kubestellar/console", 1, "kubestellar/console#1", "contributor", 2, now.Add(-leaseTTL+time.Minute)); err != nil {
		t.Fatalf("record plain: %v", err)
	}

	if got := h.keepPendingStageLeasesAlive(now); got != 3 {
		t.Fatalf("extended = %d, want 3 (admission, fanout, hub executor)", got)
	}
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	for _, tc := range []struct {
		identity, task string
		extended       bool
	}{
		{runAdmissionIdentity, "admit", true},
		{runFanoutIdentity, "fanout", true},
		{config.DefaultSpektacularHubExecutorIdentity, "exec", true},
		{"c-relay", "relay", false},
		{"c-plain", "plain", false},
	} {
		l := h.leaseForLocked(tc.identity, tc.task)
		if l == nil {
			t.Fatalf("%s lease missing", tc.identity)
		}
		if extended := l.expiresAt.Sub(now) > pendingStageLeaseRenewBefore; extended != tc.extended {
			t.Errorf("%s: expiresAt in %s, extended=%v want %v", tc.identity, l.expiresAt.Sub(now), extended, tc.extended)
		}
	}
}

// Extending is skipped while more than half the window remains so the ledger
// is not rewritten on every 30s cleanup tick.
func TestKeepPendingStageLeasesAlive_SkipsFreshLeases(t *testing.T) {
	now := time.Now()
	h := &ContributeWSHub{logger: covBLogger()}
	if err := h.recordLeaseForKeyStage(runAdmissionIdentity, "admit", "o/r", 1, "o/r#1", "triage", StageSpec, 1, now); err != nil {
		t.Fatal(err)
	}
	if got := h.keepPendingStageLeasesAlive(now); got != 0 {
		t.Fatalf("extended fresh lease: %d", got)
	}
}

// The cleanup order is keepalive THEN prune: a pending stage lease sitting at
// the edge of its window survives the prune that used to remove it.
func TestKeepPendingStageLeasesAlive_SurvivesPrune(t *testing.T) {
	now := time.Now()
	h := &ContributeWSHub{logger: covBLogger()}
	if err := h.recordLeaseForKeyStage(config.DefaultSpektacularHubExecutorIdentity, "exec", "o/r", 1, "o/r#1", "contributor", StageImplement, 6, now.Add(-leaseTTL+time.Second)); err != nil {
		t.Fatal(err)
	}
	later := now.Add(2 * time.Second)
	h.keepPendingStageLeasesAlive(now)
	if dropped := h.pruneExpiredLeases(later); dropped != 0 {
		t.Fatalf("pending implement lease pruned (dropped=%d)", dropped)
	}
}

// A pending stage lease that expired during an outage longer than leaseTTL is
// re-armed on load instead of being dropped; an expired relay lease still is.
func TestLoadLeases_ReArmsExpiredPendingStageLease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-leases.json")
	now := time.Now()
	h := &ContributeWSHub{logger: covBLogger(), persistTaskLedgers: true, taskLeasesFile: path}
	if err := h.recordLeaseForKeyStage(runAdmissionIdentity, "admit", "o/r", 1, "o/r#1", "triage", StageSpec, 1, now); err != nil {
		t.Fatal(err)
	}
	if err := h.recordLeaseForKeyStage("c-relay", "relay", "o/r", 2, "o/r#2", "contributor", StageImplement, 3, now); err != nil {
		t.Fatal(err)
	}
	// Simulate an outage longer than leaseTTL: age every persisted window.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var records []persistedLease
	if err := json.Unmarshal(data, &records); err != nil {
		t.Fatal(err)
	}
	for i := range records {
		records[i].ExpiresAt = now.Add(-leaseTTL)
	}
	aged, _ := json.Marshal(records)
	if err := os.WriteFile(path, aged, 0o600); err != nil {
		t.Fatal(err)
	}

	h2 := &ContributeWSHub{logger: covBLogger(), persistTaskLedgers: true, taskLeasesFile: path}
	h2.loadLeases()
	h2.leaseMu.Lock()
	defer h2.leaseMu.Unlock()
	l := h2.leaseForLocked(runAdmissionIdentity, "admit")
	if l == nil {
		t.Fatal("expired admission stage lease was dropped on load")
	}
	if !l.expiresAt.After(time.Now()) {
		t.Errorf("re-armed lease still expired: %s", l.expiresAt)
	}
	if h2.leaseForLocked("c-relay", "relay") != nil {
		t.Error("expired relay lease was restored")
	}
}
