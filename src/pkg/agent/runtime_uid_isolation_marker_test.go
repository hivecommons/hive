package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// These tests pin the fix for the runtime-added-agent launch wedge: the
// entrypoint's UID-isolation migration walk only covers the boot-time roster,
// so an agent added afterwards (AddAgent via the dashboard, or
// ReconcileAgents after an ACMM pack update) allocated a fresh UID whose
// agent-<uid>.ready marker nothing would ever write. awaitUIDIsolation then
// held the launch forever and every governor kick failed with "cannot be
// kicked: it is stopped". Observed live on hosted-kubestellar-console-4vkt:
// the L6 pack's adjudicator was reconciled in 31 minutes after boot and
// stayed down until the marker was written by hand.

// isolationTestUIDMap builds a UIDMap carrying a marker contract rooted in a
// test temp dir, with boot-time markers already published the way the
// entrypoint's walk leaves them.
func isolationTestUIDMap(t *testing.T, revision string) (*UIDMap, string) {
	t.Helper()
	markerDir := t.TempDir()
	u := NewUIDMap()
	u.IsolationMarkerDir = markerDir
	u.IsolationRevision = revision
	if err := os.WriteFile(filepath.Join(markerDir, "home.ready"), []byte(revision+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return u, markerDir
}

func markerContent(t *testing.T, markerDir string, uid int) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(markerDir, "agent-"+strconv.Itoa(uid)+".ready"))
	if err != nil {
		t.Fatalf("marker for uid %d not published: %v", uid, err)
	}
	return string(data)
}

func TestAddAgentPublishesUIDIsolationMarker(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, slog.Default(), ProjectContext{ACMMLevel: 6})
	uidMapPath := filepath.Join(t.TempDir(), "uid-map.json")
	oldPath := UIDMapPath
	UIDMapPath = uidMapPath
	defer func() { UIDMapPath = oldPath }()

	u, markerDir := isolationTestUIDMap(t, "1")
	m.uidMap = u

	m.AddAgent("adjudicator", config.AgentConfig{})

	uid := u.Agents["adjudicator"]
	if uid <= 0 {
		t.Fatalf("expected a runtime UID allocation, got %d", uid)
	}
	got := markerContent(t, markerDir, uid)
	want := "1:" + strconv.Itoa(uid) + "\n"
	if got != want {
		t.Fatalf("marker content = %q, want %q", got, want)
	}

	// The whole point: the launch hold must now clear. awaitUIDIsolation
	// checks the markers before ever sleeping, so a ready contract returns
	// immediately.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	agent := m.agents["adjudicator"]
	if err := m.awaitUIDIsolation(ctx, agent); err != nil {
		t.Fatalf("launch still held after AddAgent published the marker: %v", err)
	}
}

func TestReconcileAgentsPublishesUIDIsolationMarker(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, slog.Default(), ProjectContext{ACMMLevel: 6})
	uidMapPath := filepath.Join(t.TempDir(), "uid-map.json")
	oldPath := UIDMapPath
	UIDMapPath = uidMapPath
	defer func() { UIDMapPath = oldPath }()

	u, markerDir := isolationTestUIDMap(t, "1")
	m.uidMap = u

	added := m.ReconcileAgents(map[string]config.AgentConfig{
		"adjudicator": {},
	})
	if len(added) != 1 || added[0] != "adjudicator" {
		t.Fatalf("expected adjudicator to be added, got %v", added)
	}

	uid := u.Agents["adjudicator"]
	if uid <= 0 {
		t.Fatalf("expected a runtime UID allocation, got %d", uid)
	}
	got := markerContent(t, markerDir, uid)
	want := "1:" + strconv.Itoa(uid) + "\n"
	if got != want {
		t.Fatalf("marker content = %q, want %q", got, want)
	}
}

func TestPublishRuntimeUIDIsolationMarkerIsIdempotent(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, slog.Default(), ProjectContext{ACMMLevel: 6})
	u, markerDir := isolationTestUIDMap(t, "1")
	m.uidMap = u
	uid := u.AllocateUID("adjudicator")

	m.publishRuntimeUIDIsolationMarker("adjudicator")
	marker := filepath.Join(markerDir, "agent-"+strconv.Itoa(uid)+".ready")
	first, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}

	// A second publish with matching contents must not rewrite the file.
	time.Sleep(10 * time.Millisecond)
	m.publishRuntimeUIDIsolationMarker("adjudicator")
	second, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Error("matching marker was rewritten; publish must be idempotent")
	}
}

func TestPublishRuntimeUIDIsolationMarkerLegacyNoContract(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, slog.Default(), ProjectContext{ACMMLevel: 6})
	u := NewUIDMap() // no marker dir / revision: legacy shared-UID deployment
	m.uidMap = u
	u.AllocateUID("adjudicator")

	// Must be a no-op, not a panic or a stray file in the working directory.
	m.publishRuntimeUIDIsolationMarker("adjudicator")
	if _, err := os.Stat("agent-2001.ready"); !os.IsNotExist(err) {
		t.Error("legacy deployment must not write markers")
	}
}
