package dashboard

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/turn"
)

func TestSelectTaskPersistsTurnEnvelopeWhenOptedIn(t *testing.T) {
	hub, s := depTestHub(t, nil)
	dir := t.TempDir()
	hub.turnEnvelopeDir = dir
	yes := true
	level := 4
	s.deps.Config.ACMMLevel = &level
	s.deps.Config.Turn = config.TurnConfig{Reentrant: config.ReentrantTurnConfig{Enabled: true}}
	agent := s.deps.Config.Agents["scanner"]
	agent.ReentrantTurn = &yes
	agent.Role = "scanner"
	s.deps.Config.Agents["scanner"] = agent

	conn := depTestConn()
	conn.role = "scanner"
	msg := hub.selectTask(conn)
	if msg == nil || msg.Type != "task_assign" {
		t.Fatalf("selectTask returned %+v, want task_assign", msg)
	}
	if msg.TurnEnvelopeID != msg.TaskID {
		t.Fatalf("TurnEnvelopeID = %q, want task id %q", msg.TurnEnvelopeID, msg.TaskID)
	}
	env, err := (turn.FileStore{Dir: dir}).Load(context.Background(), msg.TurnEnvelopeID)
	if err != nil {
		t.Fatalf("loading persisted envelope: %v", err)
	}
	if env.SessionID != msg.TaskID || env.WorkingRepo != msg.Repo || env.Epoch != msg.TaskGen {
		t.Fatalf("envelope mismatch: %+v for message %+v", env, msg)
	}
	if env.Owner != identityOf(conn) || env.ACMMLevel != level {
		t.Fatalf("envelope owner/ACMM mismatch: owner=%q acmm=%d", env.Owner, env.ACMMLevel)
	}
	if len(env.Messages) != 2 || env.Messages[1].Content != msg.Prompt {
		t.Fatalf("envelope messages did not capture prompt: %+v", env.Messages)
	}
}

func TestSelectTaskDoesNotPersistTurnEnvelopeByDefault(t *testing.T) {
	hub, _ := depTestHub(t, nil)
	dir := t.TempDir()
	hub.turnEnvelopeDir = dir
	msg := hub.selectTask(depTestConn())
	if msg == nil || msg.Type != "task_assign" {
		t.Fatalf("selectTask returned %+v, want task_assign", msg)
	}
	if msg.TurnEnvelopeID != "" {
		t.Fatalf("TurnEnvelopeID = %q, want empty when not opted in", msg.TurnEnvelopeID)
	}
	if matches, err := filepath.Glob(filepath.Join(dir, "*.json")); err != nil || len(matches) != 0 {
		t.Fatalf("turn envelope files = %v, err=%v; want none", matches, err)
	}
}
