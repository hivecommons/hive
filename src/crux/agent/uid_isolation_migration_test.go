package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeIsolationMarker(t *testing.T, dir, name, value string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUIDIsolationMarkersReady(t *testing.T) {
	const (
		revision = "1"
		uid      = 2007
	)
	dir := t.TempDir()

	if !uidIsolationMarkersReady("", "", uid) {
		t.Fatal("a legacy UID map with no marker contract must preserve legacy launch behaviour")
	}
	writeIsolationMarker(t, dir, "home.ready", revision)
	if uidIsolationMarkersReady(dir, revision, uid) {
		t.Fatal("home completion alone released the agent before its own tree completed")
	}
	writeIsolationMarker(t, dir, "agent-2007.ready", "1:2006")
	if uidIsolationMarkersReady(dir, revision, uid) {
		t.Fatal("a marker for the wrong target UID released the agent")
	}
	writeIsolationMarker(t, dir, "agent-2007.ready", "1:2007")
	if !uidIsolationMarkersReady(dir, revision, uid) {
		t.Fatal("matching home and agent completion markers did not release the agent")
	}
}

func TestAwaitUIDIsolationHonoursCancellation(t *testing.T) {
	dir := t.TempDir()
	uids := NewUIDMap()
	uids.Agents["quality"] = 2001
	uids.IsolationMarkerDir = dir
	uids.IsolationRevision = "1"
	m := &Manager{
		uidMap: uids,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	agent := &AgentProcess{Name: "quality", UID: 2001}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := m.awaitUIDIsolation(ctx, agent)
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("awaitUIDIsolation() error = %v, want context cancellation while markers are absent", err)
	}
}

func TestAwaitUIDIsolationReturnsForCompletedPass(t *testing.T) {
	dir := t.TempDir()
	writeIsolationMarker(t, dir, "home.ready", "2")
	writeIsolationMarker(t, dir, "agent-2010.ready", "2:2010")
	uids := NewUIDMap()
	uids.Agents["scanner"] = 2010
	uids.IsolationMarkerDir = dir
	uids.IsolationRevision = "2"
	m := &Manager{
		uidMap: uids,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if err := m.awaitUIDIsolation(context.Background(), &AgentProcess{Name: "scanner", UID: 2010}); err != nil {
		t.Fatalf("awaitUIDIsolation() returned error for a completed pass: %v", err)
	}
}
