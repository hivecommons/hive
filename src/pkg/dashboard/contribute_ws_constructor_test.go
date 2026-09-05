package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestNewContributeWSHubNilLoggerLoadsInjectedContributorsDir(t *testing.T) {
	root := t.TempDir()
	contributorsDir := filepath.Join(root, "contributors")
	if err := os.MkdirAll(contributorsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	writeJSON := func(name string, v any) {
		t.Helper()
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(contributorsDir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON("activity.json", []ActivityEntry{{
		Timestamp: now.Format(time.RFC3339),
		Username:  "alice",
		Action:    "joined",
	}})
	writeJSON("completed-tasks.json", map[string]completedTaskRecord{
		"hivecommons/hive#6074": {
			CompletedAt:   now,
			CooldownHours: completedTaskCooldownHours,
			PRURL:         "https://github.com/hivecommons/hive/pull/6074",
		},
	})
	writeJSON("no-pr-streaks.json", map[string]noPRStreakRecord{
		"hivecommons/hive#6074": {Count: 1, LastAt: now},
	})

	server := NewServer(0, nil)
	server.deps = &Dependencies{Config: &config.Config{
		Data: config.DataConfig{AgentsDir: filepath.Join(root, "agent-configs")},
	}}

	hub := NewContributeWSHub(nil, server)
	if hub.logger == nil {
		t.Fatal("logger is nil")
	}
	if hub.completedTasksFile != filepath.Join(contributorsDir, "completed-tasks.json") {
		t.Fatalf("completedTasksFile = %q, want contributors dir under configured data root", hub.completedTasksFile)
	}
	if hub.taskRunLogFile != filepath.Join(contributorsDir, taskRunLogFileName) {
		t.Fatalf("taskRunLogFile = %q, want contributors dir under configured data root", hub.taskRunLogFile)
	}
	if len(hub.activity) != 1 {
		t.Fatalf("activity entries = %d, want 1", len(hub.activity))
	}
	if _, ok := hub.completedTasks["hivecommons/hive#6074"]; !ok {
		t.Fatal("completed task was not restored from injected contributors dir")
	}
	if got := hub.noPRStreaks["hivecommons/hive#6074"].Count; got != 1 {
		t.Fatalf("no-pr streak count = %d, want 1", got)
	}
}
