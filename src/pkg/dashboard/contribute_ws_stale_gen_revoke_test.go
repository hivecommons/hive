package dashboard

import (
	"encoding/json"
	"testing"
	"time"
)

// hivecommons/hive#16218: a relay echoing a stale generation must be told to
// release the task, otherwise it re-pings every tick and each ping logs a WARN.
func TestTaskProgressStaleGenerationSendsTaskRevoke(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	conn, reg := authWithModel(t, s, ts, "stalerevoker", "claude-sonnet-5", "")
	defer conn.Close()

	const taskID = "ct-16218-a"
	holder := mustConn(t, s.contributeHub, reg["contributor_id"])
	holder.mu.Lock()
	holder.currentTask = &WSTaskAssign{TaskID: taskID, Kind: "issue", Repo: "myorg/repo1", Number: 3}
	holder.currentTaskGen = 9
	holder.mu.Unlock()

	conn.WriteJSON(WSMessage{Type: "task_progress", TaskID: taskID, TaskGen: 4, Status: "working"})

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("no task_revoke received for a stale-generation task_progress: %v", err)
		}
		var m WSMessage
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if m.Type == "task_revoke" && m.TaskID == taskID {
			return
		}
	}
}
