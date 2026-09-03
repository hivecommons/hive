package turn

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestFileStorePersistLoadScrubsAndSanitizesSessionID(t *testing.T) {
	store := FileStore{Dir: t.TempDir()}
	env := SessionEnvelope{
		SessionID: "repo/issue#7",
		Messages: []Message{{
			Role:    RoleUser,
			Content: "token ******",
		}},
	}
	if err := store.Persist(context.Background(), env); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	restored, err := store.Load(context.Background(), env.SessionID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if restored.SessionID != env.SessionID {
		t.Fatalf("SessionID = %q, want %q", restored.SessionID, env.SessionID)
	}
	if strings.Contains(restored.Messages[0].Content, "ghp_") {
		t.Fatalf("persisted envelope was not scrubbed: %q", restored.Messages[0].Content)
	}
}

func TestFileStoreRestartReplayDoesNotDuplicateExternalEffect(t *testing.T) {
	ctx := context.Background()
	store := FileStore{Dir: t.TempDir()}
	env := SessionEnvelope{SessionID: "sess-restart-replay"}
	effect := OpIntent{Kind: OpComment, Repo: "hivecommons/hive", Target: "5799", Body: "phase-2 replay proof"}
	remote := newFakeGitHub()
	exec := &JournaledExecutor{
		Sink:       remote,
		Reconciler: remote,
		Persister:  store,
		Now:        func() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) },
	}
	if _, err := exec.Do(ctx, &env, effect); err != nil {
		t.Fatalf("initial Do: %v", err)
	}
	restarted, err := store.Load(ctx, env.SessionID)
	if err != nil {
		t.Fatalf("Load after restart: %v", err)
	}
	if _, err := exec.Do(ctx, &restarted, effect); err != nil {
		t.Fatalf("replayed Do: %v", err)
	}
	if dups := remote.duplicates(); len(dups) != 0 {
		t.Fatalf("restart replay duplicated external effects: %v", dups)
	}
	if got := remote.calls[effectID(effect)]; got != 1 {
		t.Fatalf("remote perform count = %d, want 1", got)
	}
}
