package knowledge

import (
	"testing"
	"time"
)

func TestExternalCampaignIdentityDoesNotCollide(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	now := time.Now()
	ids := []string{"myorg/repo1#8450", "myorg/repo#18450", "myorgrepo18450", "Owner/Repo.Name!spec:plan", "External Campaign", "external-campaign", "MyOrg/repo#18450"}
	for _, id := range ids {
		archive, err := e.ReviseExternalCampaign(id, id, id, "", "", "alice", nil, now)
		if err != nil {
			t.Fatal(err)
		}
		if archive.ID != id || archive.Revision != 1 {
			t.Fatalf("archive = %+v, want %q revision 1", archive, id)
		}
	}
	// Reopen the store: identity must survive both listing and direct lookup.
	e = NewInceptionEngine(dir, nil, nil)
	archives, err := e.ListCampaignArchives()
	if err != nil || len(archives) != len(ids) {
		t.Fatalf("archives = %+v, err = %v", archives, err)
	}
	for _, id := range ids {
		archive, err := e.LoadCampaignArchive(id)
		if err != nil || archive.ID != id || archive.Title != id || archive.Source != id {
			t.Fatalf("load %q = %+v, err = %v", id, archive, err)
		}
		revision, err := e.ReviseExternalCampaign(id, "changed", id, "", "", "alice", nil, now.Add(time.Minute))
		if err != nil || revision.ID != id || revision.Revision != 1 {
			t.Fatalf("repeat revise %q = %+v, err = %v", id, revision, err)
		}
		if _, err := e.ReleaseCampaignArchive(id, "alice", now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		revision, err = e.ReviseExternalCampaign(id, "changed", id, "", "", "alice", nil, now.Add(2*time.Minute))
		if err != nil || revision.Revision != 2 || len(revision.History) != 1 {
			t.Fatalf("next revise %q = %+v, err = %v", id, revision, err)
		}
	}
}

func TestExternalCampaignCannotOverwriteInception(t *testing.T) {
	e := newTestEngine(t)
	state, err := e.Start("Protected inception campaign")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.ArchiveCurrentCampaign(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ReviseExternalCampaign(state.IdeaSlug, "external", "run", "Spektacular", "spektacular", "alice", nil, time.Now()); err == nil {
		t.Fatal("external revise overwrote inception")
	}
	archive, err := e.LoadCampaignArchive(state.IdeaSlug)
	if err != nil || archive.Type != "inception" || archive.State == nil || archive.Lease != nil {
		t.Fatalf("inception changed: %+v, err = %v", archive, err)
	}
}
