package knowledge

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInceptionCampaignArchiveLeaseReviseAndRestore(t *testing.T) {
	dir := t.TempDir()
	engine := NewInceptionEngine(dir, nil, nil)
	state, err := engine.Start("Collaborative Spektacular workspace")
	if err != nil {
		t.Fatalf("start inception: %v", err)
	}
	wikiDir := engine.WikiDir()
	if err := os.MkdirAll(wikiDir, 0o755); err != nil {
		t.Fatalf("mkdir wiki: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wikiDir, "spec.md"), []byte("# Spec\nOriginal"), 0o644); err != nil {
		t.Fatalf("write wiki: %v", err)
	}

	archived, err := engine.ArchiveCurrentCampaign()
	if err != nil {
		t.Fatalf("archive campaign: %v", err)
	}
	if archived.ID != state.IdeaSlug || archived.Engine != "Spec Kit" || archived.Type != "inception" {
		t.Fatalf("archive identity = %+v, want slug %q", archived, state.IdeaSlug)
	}
	if len(archived.WikiFiles) != 1 || archived.WikiFiles[0] != "spec.md" {
		t.Fatalf("archive wiki files = %+v", archived.WikiFiles)
	}

	listed, err := engine.ListCampaignArchives()
	if err != nil {
		t.Fatalf("list archives: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != archived.ID {
		t.Fatalf("listed archives = %+v", listed)
	}

	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	leased, err := engine.LeaseCampaignArchive(archived.ID, "alice", "dashboard", now)
	if err != nil {
		t.Fatalf("lease archive: %v", err)
	}
	if leased.Lease == nil || leased.Lease.Owner != "alice" || leased.Lease.Surface != "dashboard" {
		t.Fatalf("lease = %+v", leased.Lease)
	}
	if _, err := engine.LeaseCampaignArchive(archived.ID, "bob", "dashboard", now.Add(time.Minute)); !errors.Is(err, ErrCampaignLeaseHeld) {
		t.Fatalf("competing lease err = %v, want ErrCampaignLeaseHeld", err)
	}
	if _, err := engine.ReleaseCampaignArchive(archived.ID, "bob", now.Add(2*time.Minute)); !errors.Is(err, ErrCampaignLeaseHeld) {
		t.Fatalf("wrong owner release err = %v, want ErrCampaignLeaseHeld", err)
	}
	released, err := engine.ReleaseCampaignArchive(archived.ID, "alice", now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("release archive: %v", err)
	}
	if released.Lease != nil {
		t.Fatalf("released lease = %+v, want nil", released.Lease)
	}

	revision, err := engine.ReviseCampaignArchive(archived.ID, "carol", now.Add(4*time.Minute))
	if err != nil {
		t.Fatalf("revise archive: %v", err)
	}
	if revision.RevisionOf != archived.ID || revision.Revision != 2 || revision.Lease == nil || revision.Lease.Owner != "carol" {
		t.Fatalf("revision = %+v", revision)
	}
	if revision.State == nil || revision.State.IdeaSlug != revision.ID || revision.State.Phase != PhaseCapture {
		t.Fatalf("revision state = %+v", revision.State)
	}

	if err := os.WriteFile(filepath.Join(wikiDir, "spec.md"), []byte("# Spec\nChanged"), 0o644); err != nil {
		t.Fatalf("change active wiki: %v", err)
	}
	restored, err := engine.RestoreCampaignArchive(archived.ID)
	if err != nil {
		t.Fatalf("restore archive: %v", err)
	}
	if restored.IdeaSlug != archived.ID {
		t.Fatalf("restored slug = %q, want %q", restored.IdeaSlug, archived.ID)
	}
	data, err := os.ReadFile(filepath.Join(wikiDir, "spec.md"))
	if err != nil {
		t.Fatalf("read restored wiki: %v", err)
	}
	if string(data) != "# Spec\nOriginal" {
		t.Fatalf("restored wiki = %q", string(data))
	}
}

func TestInceptionExternalCampaignRevisionDefaults(t *testing.T) {
	engine := NewInceptionEngine(t.TempDir(), nil, nil)
	now := time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC)
	revision, err := engine.ReviseExternalCampaign("Owner/Repo#8687", "Jam Sessions", "owner/repo#8687", "", "", "", []string{"owner/repo"}, now)
	if err != nil {
		t.Fatalf("revise external campaign: %v", err)
	}
	if revision.ID == "" || revision.RevisionOf != "ownerrepo8687" || revision.Revision != 2 {
		t.Fatalf("external revision identity = %+v", revision)
	}
	if revision.Engine != "Spektacular" || revision.Type != "spektacular" {
		t.Fatalf("external revision kind = %s/%s", revision.Engine, revision.Type)
	}
	if revision.Lease == nil || revision.Lease.Owner != "local" || revision.Lease.Surface != "revise" {
		t.Fatalf("external revision lease = %+v", revision.Lease)
	}

	loaded, err := engine.LoadCampaignArchive(revision.ID)
	if err != nil {
		t.Fatalf("load external revision: %v", err)
	}
	if loaded.Title != "Jam Sessions" || len(loaded.Repos) != 1 || loaded.Repos[0] != "owner/repo" {
		t.Fatalf("loaded external revision = %+v", loaded)
	}
}

// TestInceptionCampaignArchiveDelete pins hivecommons/hive#12698: deleting a
// revision removes only its directory (state + wiki snapshot), leaves the
// base archive and the working wiki intact, and clears the live session when
// the deleted id is the active one.
func TestInceptionCampaignArchiveDelete(t *testing.T) {
	dir := t.TempDir()
	engine := NewInceptionEngine(dir, nil, nil)
	state, err := engine.StartBrownfield("https://github.com/hivecommons/hive")
	if err != nil {
		t.Fatalf("start brownfield: %v", err)
	}
	wikiDir := engine.WikiDir()
	if err := os.MkdirAll(wikiDir, 0o755); err != nil {
		t.Fatalf("mkdir wiki: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wikiDir, "scan.md"), []byte("# Scan"), 0o644); err != nil {
		t.Fatalf("write wiki: %v", err)
	}
	if _, err := engine.ArchiveCurrentCampaign(); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if err := engine.Reset(); err != nil {
		t.Fatalf("reset: %v", err)
	}
	revision, err := engine.ReviseCampaignArchive(state.IdeaSlug, "alice", time.Now())
	if err != nil {
		t.Fatalf("revise: %v", err)
	}

	deleted, err := engine.DeleteCampaignArchive(revision.ID)
	if err != nil {
		t.Fatalf("delete revision: %v", err)
	}
	if deleted.ID != revision.ID {
		t.Fatalf("deleted id = %q, want %q", deleted.ID, revision.ID)
	}
	if _, err := os.Stat(filepath.Join(dir, inceptionCampaignsDir, revision.ID)); !os.IsNotExist(err) {
		t.Fatalf("revision directory still present: %v", err)
	}
	if _, err := engine.LoadCampaignArchive(state.IdeaSlug); err != nil {
		t.Fatalf("base archive removed with revision: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wikiDir, "scan.md")); err != nil {
		t.Fatalf("working wiki touched by delete: %v", err)
	}
	if _, err := engine.DeleteCampaignArchive(revision.ID); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("second delete err = %v, want not found", err)
	}
	if _, err := engine.DeleteCampaignArchive("   "); err == nil {
		t.Fatalf("blank id should be rejected")
	}

	// Deleting the live session's campaign clears it too.
	if _, err := engine.RestoreCampaignArchive(state.IdeaSlug); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := engine.DeleteCampaignArchive(state.IdeaSlug); err != nil {
		t.Fatalf("delete active: %v", err)
	}
	if engine.GetState() != nil {
		t.Fatalf("active state should be cleared after deleting its campaign")
	}
	if _, err := os.Stat(filepath.Join(dir, inceptionStateFile)); !os.IsNotExist(err) {
		t.Fatalf("state file still present: %v", err)
	}
	listed, err := engine.ListCampaignArchives()
	if err != nil || len(listed) != 0 {
		t.Fatalf("archives after delete = %+v err=%v, want none", listed, err)
	}
}
