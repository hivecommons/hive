package chat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/persona"
)

func openTestPersonaStore(t *testing.T, path string) *FilePersonaStore {
	t.Helper()
	store, err := OpenFilePersonaStore(path)
	if err != nil {
		t.Fatalf("OpenFilePersonaStore: %v", err)
	}
	return store
}

// TestFilePersonaStoreSurvivesRestart is the #9175 scenario: a user runs
// `!persona setup`, learning state accrues, and the process restarts. A fresh
// service over a reopened store must still see the persona, its pending
// suggestion, and its adjustment history.
func TestFilePersonaStoreSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "chat-personas.json")
	s := NewService(&recordingBackend{}, Config{
		AllowedUsers: []string{"uid:owner"},
		PersonaStore: openTestPersonaStore(t, path).ForTransport("test"),
	}, discardLogger())
	ctx := context.WithValue(context.Background(), commandAuthorContextKey{}, "uid")
	if _, err := s.cmdPersona(ctx, "setup"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	for _, answer := range []string{"outcomes", "standard", "include receipts"} {
		if !s.handlePendingPersonaReply(context.Background(), makeMsg("1", answer, false), answer) {
			t.Fatalf("persona answer %q was not consumed", answer)
		}
	}
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	cfg := persona.LearningConfig{Enabled: true, Threshold: 2}
	record, _, err := s.getPersona(ctx, "uid")
	if err != nil {
		t.Fatalf("getPersona: %v", err)
	}
	for i := 0; i < cfg.Threshold; i++ {
		if record, err = record.RecordSignal(persona.SignalExpanded, now, cfg); err != nil {
			t.Fatalf("RecordSignal: %v", err)
		}
	}
	if err := s.putPersona(ctx, "uid", record); err != nil {
		t.Fatalf("putPersona: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("persona file not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != personaStoreFileMode {
		t.Fatalf("persona file mode = %v, want %v", perm, personaStoreFileMode)
	}

	restarted := NewService(&recordingBackend{}, Config{
		AllowedUsers: []string{"uid:owner"},
		PersonaStore: openTestPersonaStore(t, path).ForTransport("test"),
	}, discardLogger())
	got, err := restarted.cmdPersona(ctx, "show")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	for _, want := range []string{"depth: outcomes", "summary_length: standard", "notes: include receipts", "suggestions: 1 pending"} {
		if !strings.Contains(got, want) {
			t.Fatalf("show after restart = %q, missing %q", got, want)
		}
	}
}

func TestFilePersonaStoreKeepsTransportsApart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat-personas.json")
	store := openTestPersonaStore(t, path)
	ctx := context.Background()
	if err := store.ForTransport("slack").PutPersona(ctx, "42", persona.Record{Depth: persona.DepthTechnical}); err != nil {
		t.Fatalf("put slack: %v", err)
	}
	if err := store.ForTransport("telegram").PutPersona(ctx, "42", persona.Record{Depth: persona.DepthOutcomes}); err != nil {
		t.Fatalf("put telegram: %v", err)
	}

	reopened := openTestPersonaStore(t, path)
	for transport, want := range map[string]string{"slack": persona.DepthTechnical, "telegram": persona.DepthOutcomes} {
		got, ok, err := reopened.ForTransport(transport).GetPersona(ctx, "42")
		if err != nil || !ok || got.Depth != want {
			t.Fatalf("%s persona for 42 = %#v (ok=%v, err=%v), want depth %q", transport, got, ok, err, want)
		}
	}
	if _, ok, _ := reopened.ForTransport("discord").GetPersona(ctx, "42"); ok {
		t.Fatal("author 42 on discord must not inherit another transport's persona")
	}
}

func TestOpenFilePersonaStoreRejectsMalformedFileWithoutClobbering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat-personas.json")
	for name, body := range map[string]string{
		"not json":        "{not json",
		"unknown version": `{"version":99,"personas":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenFilePersonaStore(path); err == nil {
				t.Fatal("OpenFilePersonaStore accepted a malformed file")
			}
			raw, err := os.ReadFile(path)
			if err != nil || string(raw) != body {
				t.Fatalf("malformed file changed on open: %q, %v", raw, err)
			}
		})
	}
}

// TestFilePersonaStoreFailedWriteKeepsPreviousRecord: when the file cannot be
// replaced, PutPersona reports the error and reads keep returning what is on
// disk, so the chat reply "Failed to save persona" is not contradicted by a
// later `!persona show`.
func TestFilePersonaStoreFailedWriteKeepsPreviousRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat-personas.json")
	store := openTestPersonaStore(t, path).ForTransport("slack")
	ctx := context.Background()
	if err := store.PutPersona(ctx, "u1", persona.Record{Depth: persona.DepthOutcomes}); err != nil {
		t.Fatalf("initial put: %v", err)
	}
	// A non-empty directory at the file path makes the atomic rename fail.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "blocker"), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := store.PutPersona(ctx, "u1", persona.Record{Depth: persona.DepthTechnical}); err == nil {
		t.Fatal("PutPersona succeeded although the file could not be replaced")
	}
	if got, ok, _ := store.GetPersona(ctx, "u1"); !ok || got.Depth != persona.DepthOutcomes {
		t.Fatalf("after failed update = %#v (ok=%v), want previous outcomes record", got, ok)
	}
	if err := store.PutPersona(ctx, "u2", persona.Record{Depth: persona.DepthTechnical}); err == nil {
		t.Fatal("PutPersona for a new author succeeded although the file could not be replaced")
	}
	if _, ok, _ := store.GetPersona(ctx, "u2"); ok {
		t.Fatal("failed first write for u2 left a record in memory")
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp"))
	if len(matches) != 0 {
		t.Fatalf("temp files left behind: %v", matches)
	}
}
