package turn

import (
	"context"
	"strings"
	"testing"
)

func TestFileStorePersistLoadScrubsAndSanitizesSessionID(t *testing.T) {
	store := FileStore{Dir: t.TempDir()}
	env := SessionEnvelope{
		SessionID: "repo/issue#7",
		Messages: []Message{{
			Role:    RoleUser,
			Content: "token ghp_1234567890abcdef1234567890abcdef1234",
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
