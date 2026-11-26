package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTerminalTaskModelRecorded(t *testing.T) {
	for _, outcome := range []string{"complete", "failed"} {
		for _, model := range []string{"gpt-new", ""} {
			t.Run(outcome+"/"+model, func(t *testing.T) {
				s, _ := runsTestServer(t)
				path := filepath.Join(t.TempDir(), "task_runs.jsonl")
				s.contributeHub.taskRunLogFile = path
				conn := &ContributorConnection{
					profile:        &ContributorProfile{ContributorID: "alice", GitHubUsername: "alice", Model: "gpt-old"},
					model:          "gpt-old",
					cliBackend:     "codex",
					currentTask:    &WSTaskAssign{TaskID: "task-1", Kind: "issue", Repo: "myorg/repo1", Number: 42},
					currentTaskGen: 3,
					taskAssignedAt: time.Now().Add(-time.Minute),
				}
				session := &wsSession{h: s.contributeHub, contributor: conn}
				msg := WSMessage{TaskID: "task-1", TaskGen: 3, Model: model, Reason: "failure"}
				if outcome == "complete" {
					session.handleTaskComplete(msg)
				} else {
					session.handleTaskFailed(msg)
				}
				want := model
				if want == "" {
					want = "gpt-old"
				}
				if conn.model != want || conn.profile.Model != want {
					t.Fatalf("connection/profile model = %q/%q, want %q", conn.model, conn.profile.Model, want)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var rec TaskRunRecord
				if err := json.Unmarshal(data, &rec); err != nil {
					t.Fatalf("decode run: %v", err)
				}
				if rec.Model != want {
					t.Fatalf("run model = %q, want %q", rec.Model, want)
				}
			})
		}
	}
}

func TestTerminalTaskModelRejectsUnassignedAndStale(t *testing.T) {
	for _, outcome := range []string{"complete", "failed"} {
		for _, tc := range []struct {
			name, task string
			gen        uint64
		}{{"unassigned", "other", 3}, {"stale", "task-1", 2}} {
			t.Run(outcome+"/"+tc.name, func(t *testing.T) {
				s, _ := runsTestServer(t)
				conn := &ContributorConnection{profile: &ContributorProfile{ContributorID: "alice", GitHubUsername: "alice", Model: "gpt-old"}, model: "gpt-old", currentTask: &WSTaskAssign{TaskID: "task-1"}, currentTaskGen: 3, sawTaskGen: true}
				session := &wsSession{h: s.contributeHub, contributor: conn}
				msg := WSMessage{TaskID: tc.task, TaskGen: tc.gen, Model: "gpt-wrong"}
				if outcome == "complete" {
					session.handleTaskComplete(msg)
				} else {
					session.handleTaskFailed(msg)
				}
				if conn.model != "gpt-old" || conn.profile.Model != "gpt-old" {
					t.Fatalf("unowned task changed model: %q/%q", conn.model, conn.profile.Model)
				}
			})
		}
	}
}
