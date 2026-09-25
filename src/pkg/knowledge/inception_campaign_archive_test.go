package knowledge

import (
	"errors"
	"os"
	"path/filepath"
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
