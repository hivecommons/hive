package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestObserveSpecialistTaskResponseBindsExactPrivateCodexTurn(t *testing.T) {
	manager, request := newSpecialistResponseTestManager(t)
	completedAt := time.Now().UTC().Truncate(time.Second)
	response := `{"schema_version":"hive.specialist-model-result.v1","status":"blocked"}`
	writeSpecialistCodexRollout(t, manager.workDir, "manager-crash-left", "one", filepath.Join(manager.workDir, "quality"), request.Message, response, completedAt, true)

	got, err := manager.ObserveSpecialistTaskResponse(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != SpecialistTaskResponseSchema || got.WorkOrderID != request.TaskID || got.Specialist != request.Specialist || got.SessionID != request.SessionID || got.Response != response || got.CompletedAt != completedAt {
		t.Fatalf("response = %#v", got)
	}
	if !digestPattern.MatchString(got.DispatchSHA256) || !digestPattern.MatchString(got.ResponseSHA256) || got.ProviderSHA256 != manager.specialistProvider.SHA256 || got.TurnID == "" {
		t.Fatalf("response identities = %#v", got)
	}
}

func TestObserveSpecialistTaskResponseRejectsWrongRoleDuplicateAndUnsafeJournal(t *testing.T) {
	t.Run("wrong role cwd", func(t *testing.T) {
		manager, request := newSpecialistResponseTestManager(t)
		writeSpecialistCodexRollout(t, manager.workDir, "manager-one", "wrong-cwd", filepath.Join(manager.workDir, "scanner"), request.Message, `{}`, time.Now().UTC().Truncate(time.Second), true)
		_, err := manager.ObserveSpecialistTaskResponse(context.Background(), request)
		if err == nil || !strings.Contains(err.Error(), "selected role work directory") {
			t.Fatalf("wrong cwd error = %v", err)
		}
	})

	t.Run("duplicate exact delivery", func(t *testing.T) {
		manager, request := newSpecialistResponseTestManager(t)
		when := time.Now().UTC().Truncate(time.Second)
		for _, name := range []string{"one", "two"} {
			writeSpecialistCodexRollout(t, manager.workDir, "manager-old", name, filepath.Join(manager.workDir, "quality"), request.Message, `{}`, when, true)
		}
		_, err := manager.ObserveSpecialistTaskResponse(context.Background(), request)
		if err == nil || !strings.Contains(err.Error(), "more than one Codex turn") {
			t.Fatalf("duplicate error = %v", err)
		}
	})

	t.Run("linked session tree", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("ordinary symlink creation is not consistently available")
		}
		manager, request := newSpecialistResponseTestManager(t)
		home := filepath.Join(manager.workDir, ".codex-homes", "manager-linked")
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), filepath.Join(home, "sessions")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		_, err := manager.ObserveSpecialistTaskResponse(context.Background(), request)
		if err == nil || !strings.Contains(err.Error(), "linked or non-directory ancestor") {
			t.Fatalf("linked journal error = %v", err)
		}
	})

	t.Run("unrelated linked Codex temp entry is ignored", func(t *testing.T) {
		manager, request := newSpecialistResponseTestManager(t)
		completedAt := time.Now().UTC().Truncate(time.Second)
		writeSpecialistCodexRollout(t, manager.workDir, "manager-one", "ordinary", filepath.Join(manager.workDir, "quality"), request.Message, `{"status":"blocked"}`, completedAt, true)
		linkedParent := filepath.Join(manager.workDir, ".codex-homes", "manager-one", "tmp", "arg0", "codex-probe")
		if err := os.MkdirAll(linkedParent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), filepath.Join(linkedParent, "codex-linux-sandbox")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}

		got, err := manager.ObserveSpecialistTaskResponse(context.Background(), request)
		if err != nil {
			t.Fatalf("observe ordinary rollout with unrelated Codex temp symlink: %v", err)
		}
		if got.Response != `{"status":"blocked"}` || got.CompletedAt != completedAt {
			t.Fatalf("response = %#v", got)
		}
	})
}

func TestObserveSpecialistTaskResponseRetriesPartialFinalRecord(t *testing.T) {
	manager, request := newSpecialistResponseTestManager(t)
	path := writeSpecialistCodexRollout(t, manager.workDir, "manager-one", "partial", filepath.Join(manager.workDir, "quality"), request.Message, `{"status":"blocked"}`, time.Now().UTC().Truncate(time.Second), false)
	if _, err := manager.ObserveSpecialistTaskResponse(context.Background(), request); !errors.Is(err, ErrSpecialistTaskResponsePending) {
		t.Fatalf("partial observation error = %v", err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ObserveSpecialistTaskResponse(context.Background(), request); err != nil {
		t.Fatalf("completed observation error = %v", err)
	}
}

func newSpecialistResponseTestManager(t *testing.T) (*Manager, SpecialistTaskResponseRequest) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "specialists")
	roleDir := filepath.Join(root, "quality")
	if err := os.MkdirAll(filepath.Join(root, ".codex-homes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(roleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sessionID := "hive-quality-response-test"
	providerPath := filepath.Join(root, "sealed-codex")
	providerBody := "sealed specialist provider"
	if err := os.WriteFile(providerPath, []byte(providerBody), 0o500); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{
		workDir: root,
		agents: map[string]*AgentProcess{
			"quality": {Name: "quality", tmuxSession: sessionID},
		},
		specialistLeases:   map[string]string{},
		specialistProvider: &specialistProviderIdentity{Path: providerPath, SHA256: sha256HexString(providerBody), Bytes: int64(len(providerBody))},
	}
	return manager, SpecialistTaskResponseRequest{
		TaskID: "swo-" + strings.Repeat("a", 64), Specialist: SpecialistQuality,
		SessionID: sessionID, Message: "exact inline specialist work order",
	}
}

func writeSpecialistCodexRollout(t *testing.T, root, managerName, rolloutName, cwd, message, response string, completedAt time.Time, finalNewline bool) string {
	t.Helper()
	directory := filepath.Join(root, ".codex-homes", managerName, "sessions", "2026", "07", "16")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	turnID := "019f6a3e-fb36-7b90-80b0-" + rolloutName + "abcd"
	if !safeIdentityPattern.MatchString(turnID) {
		turnID = "turn-" + rolloutName
	}
	dispatchedAt := completedAt.Add(-time.Second)
	records := []map[string]any{
		{"timestamp": dispatchedAt.Format(time.RFC3339Nano), "type": "session_meta", "payload": map[string]any{"cwd": cwd}},
		{"timestamp": dispatchedAt.Format(time.RFC3339Nano), "type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": turnID}},
		{"timestamp": dispatchedAt.Format(time.RFC3339Nano), "type": "response_item", "payload": map[string]any{
			"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": message}},
			"internal_chat_message_metadata_passthrough": map[string]any{"turn_id": turnID},
		}},
		{"timestamp": completedAt.Format(time.RFC3339Nano), "type": "event_msg", "payload": map[string]any{
			"type": "task_complete", "turn_id": turnID, "completed_at": completedAt.Unix(), "last_agent_message": response,
		}},
	}
	var content strings.Builder
	for index, record := range records {
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		content.Write(encoded)
		if index < len(records)-1 || finalNewline {
			content.WriteByte('\n')
		}
	}
	path := filepath.Join(directory, "rollout-"+rolloutName+".jsonl")
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
