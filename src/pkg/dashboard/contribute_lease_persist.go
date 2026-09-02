package dashboard

// contribute_lease_persist.go — persist the C4 task-lease registry across hub
// restarts (kubestellar/hive#5681).
//
// Leases lived only in the hub's memory, so a hub restart (routine under
// self-upgrade rolls) emptied the registry. Every relay that was mid-task kept
// working through the brief disconnect and re-asserted its task on reconnect,
// but lookupLease could no longer match any server-issued lease: the relay was
// told "no active lease for this task", the revoke path interrupted the agent
// mid-turn, and seconds later the SAME issue was re-assigned to the SAME relay
// as a fresh task — ownership was never in question, only the record of it.
//
// The registry is now a PVC-backed ledger in the same contributors dir as the
// cooldown/streak/verdict ledgers, written on every lease mutation (record,
// renew, revoke) and loaded at hub construction with expired entries dropped.
// Persistence changes NOTHING about what a lease grants: a restored lease is
// byte-for-byte the {identity, task, repo, number, tier, generation, expiry}
// tuple the hub itself issued, so the C4 exact-match contract and the #2568
// generation fence apply to it unchanged, and a lease revoked before the
// restart is absent from the file and stays absent.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// taskLeasesFileName is the on-disk ledger for server-issued task leases
// (#5681). The full path comes from taskLeasesPath(), which honours
// HIVE_CONTRIBUTORS_DIR exactly as the other contribute ledgers do — that is
// what lets the persistence round-trip tests point it at a temp dir.
const taskLeasesFileName = "task-leases.json"

func taskLeasesPath() string {
	return filepath.Join(getContributorsDir(), taskLeasesFileName)
}

// taskLeaseRecord is the JSON shape of one persisted lease. It carries exactly
// the fields lookupLease matches on plus the expiry — nothing is reconstructed
// at load time.
type taskLeaseRecord struct {
	Identity  string    `json:"identity"`
	TaskID    string    `json:"task_id"`
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Tier      string    `json:"tier"`
	Gen       uint64    `json:"gen"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (h *ContributeWSHub) leasesFilePath() string {
	if h != nil && h.leasesFile != "" {
		return h.leasesFile
	}
	return taskLeasesPath()
}

// loadLeases restores the lease registry from disk at hub construction
// (#5681), dropping entries that expired while the hub was down or that are
// malformed. A restored lease behaves identically to one recordLease minted in
// this process: it is renewed by task_progress, revoked on every release path,
// and expires on the same leaseTTL clock.
func (h *ContributeWSHub) loadLeases() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	data, err := os.ReadFile(h.leasesFilePath())
	if err != nil {
		return
	}
	var records []taskLeaseRecord
	if json.Unmarshal(data, &records) != nil {
		return
	}
	now := time.Now()
	restored := 0
	h.leaseMu.Lock()
	if h.leases == nil {
		h.leases = make(map[string]*taskLease)
	}
	for _, rec := range records {
		if rec.Identity == "" || rec.TaskID == "" || rec.Gen == 0 {
			continue
		}
		if now.After(rec.ExpiresAt) {
			continue
		}
		h.leases[rec.Identity] = &taskLease{
			identity:  rec.Identity,
			taskID:    rec.TaskID,
			repo:      rec.Repo,
			number:    rec.Number,
			tier:      rec.Tier,
			gen:       rec.Gen,
			expiresAt: rec.ExpiresAt,
		}
		restored++
	}
	h.leaseMu.Unlock()
	if restored > 0 {
		h.logger.Info("[contribute-ws] restored task leases", "count", restored)
	}
}

// saveLeases writes the current lease registry to its ledger atomically
// (tmp + rename, like every other contribute ledger). Called after every
// lease mutation; expired entries are skipped so the file cannot accumulate
// stale claims.
func (h *ContributeWSHub) saveLeases() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	now := time.Now()
	h.leaseMu.Lock()
	records := make([]taskLeaseRecord, 0, len(h.leases))
	for _, l := range h.leases {
		if now.After(l.expiresAt) {
			continue
		}
		records = append(records, taskLeaseRecord{
			Identity:  l.identity,
			TaskID:    l.taskID,
			Repo:      l.repo,
			Number:    l.number,
			Tier:      l.tier,
			Gen:       l.gen,
			ExpiresAt: l.expiresAt,
		})
	}
	h.leaseMu.Unlock()
	data, err := json.Marshal(records)
	if err != nil {
		h.logger.Warn("[contribute-ws] task leases marshal failed", "error", err)
		return
	}
	path := h.leasesFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.logger.Warn("[contribute-ws] task leases directory creation failed", "error", err)
		return
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		h.logger.Warn("[contribute-ws] task leases write failed", "error", err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		h.logger.Warn("[contribute-ws] task leases rename failed", "error", err)
	}
}
