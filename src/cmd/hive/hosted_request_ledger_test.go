package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHostedRequestLedgerCompactsTerminalsAndPreservesOneRecoverableActiveRequest(t *testing.T) {
	root := t.TempDir()
	started := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	for index := 0; index < hostedRequestRetainedTerminals+4; index++ {
		id := hostedRequestID(index)
		prepared, err := beginHostedRequest(root, id, "cycle", uint64(index+1), 1, started.Add(time.Duration(index)*time.Minute))
		if err != nil || !prepared.New {
			t.Fatalf("begin request %d: prepared=%+v err=%v", index, prepared, err)
		}
		if err := completeHostedRequest(root, id, "cycle", true, uint64(index+1), 1, started.Add(time.Duration(index)*time.Minute+time.Second)); err != nil {
			t.Fatalf("complete request %d: %v", index, err)
		}
	}
	ledger, err := loadHostedRequestLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Requests) != hostedRequestRetainedTerminals {
		t.Fatalf("terminal ledger was not compacted: %d", len(ledger.Requests))
	}
	if ledger.Requests[0].ID == hostedRequestID(0) {
		t.Fatal("oldest terminal request survived bounded compaction")
	}

	activeID := "cycle-active"
	if prepared, err := beginHostedRequest(root, activeID, "cycle", 1000, 1, started.Add(20*time.Hour)); err != nil || !prepared.New {
		t.Fatalf("begin active request: prepared=%+v err=%v", prepared, err)
	}
	if _, err := beginHostedRequest(root, "cycle-older-controller", "cycle", 999, 1, started.Add(20*time.Hour+time.Minute)); err == nil {
		t.Fatal("older controller was allowed to supersede an active request")
	}
	resumed, err := beginHostedRequest(root, activeID, "cycle", 1001, 2, started.Add(20*time.Hour+2*time.Minute))
	if err != nil || !resumed.Resume {
		t.Fatalf("exact active request was not recoverable: prepared=%+v err=%v", resumed, err)
	}
	newer, err := beginHostedRequest(root, "cycle-new-controller", "cycle", 1002, 1, started.Add(20*time.Hour+3*time.Minute))
	if err != nil || !newer.New {
		t.Fatalf("new serialized controller did not retire abandoned active state: prepared=%+v err=%v", newer, err)
	}
	ledger, err = loadHostedRequestLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, record := range ledger.Requests {
		if record.Status == "started" {
			active++
			if record.ID != "cycle-new-controller" {
				t.Fatalf("wrong request remained active: %+v", record)
			}
		}
	}
	if active != 1 {
		t.Fatalf("expected exactly one active request, got %d", active)
	}
}

func TestHostedRequestLedgerReplaysTerminalFailureWithoutExecution(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 16, 13, 0, 0, 0, time.UTC)
	if prepared, err := beginHostedRequest(root, "pause-failed", "pause", 20, 1, now); err != nil || !prepared.New {
		t.Fatalf("begin failed request: prepared=%+v err=%v", prepared, err)
	}
	if err := completeHostedRequest(root, "pause-failed", "pause", false, 20, 1, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	replayed, err := beginHostedRequest(root, "pause-failed", "pause", 21, 1, now.Add(time.Minute))
	if err != nil || !replayed.Duplicate || replayed.Status != "failed" || replayed.New || replayed.Resume {
		t.Fatalf("terminal failure was not replayed safely: prepared=%+v err=%v", replayed, err)
	}
	if _, err := beginHostedRequest(root, "pause-failed", "resume", 21, 1, now.Add(time.Minute)); err == nil {
		t.Fatal("request ID was rebound to another operation")
	}
}

func TestHostedRequestLedgerMigratesLegacyStartedAndCompletedRecords(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "integrated")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 16, 14, 0, 0, 0, time.UTC)
	legacy := map[string]any{
		"schema_version": hostedRequestLegacySchema,
		"requests": []map[string]any{
			{"id": "legacy-complete", "operation": "cycle", "status": "completed", "started_at": now.Add(-2 * time.Minute), "completed_at": now.Add(-time.Minute)},
			{"id": "legacy-active", "operation": "cycle", "status": "started", "started_at": now},
		},
	}
	data, _ := json.Marshal(legacy)
	path := filepath.Join(directory, "hosted-requests.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err := beginHostedRequest(root, "legacy-active", "cycle", 30, 1, now.Add(time.Minute))
	if err != nil || !prepared.Resume {
		t.Fatalf("legacy active request was not resumed deterministically: prepared=%+v err=%v", prepared, err)
	}
	if err := completeHostedRequest(root, "legacy-active", "cycle", true, 30, 1, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	ledger, err := loadHostedRequestLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	if ledger.SchemaVersion != hostedRequestSchema || len(ledger.Requests) != 2 || ledger.Requests[0].Status != "succeeded" || !ledger.Requests[0].MigratedLegacy {
		t.Fatalf("legacy ledger migration lost its explicit provenance: %+v", ledger)
	}
}

func hostedRequestID(index int) string {
	return "cycle-retained-" + time.Unix(int64(index), 0).UTC().Format("20060102T150405")
}
