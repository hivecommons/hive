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
	if revision.ID != archived.ID || revision.RevisionOf != "" || revision.Revision != 2 || revision.Lease == nil || revision.Lease.Owner != "carol" {
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
	if revision.ID != "Owner/Repo#8687" || revision.RevisionOf != "" || revision.Revision != 1 {
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

// Restarting inception with an idea whose slug collides with an earlier
// archived campaign must not silently archive onto — and overwrite the state
// and wiki of — that earlier campaign (hivecommons/hive#10084).
func TestInceptionRestartWithCollidingIdeaDoesNotOverwriteEarlierArchive(t *testing.T) {
	dir := t.TempDir()
	engine := NewInceptionEngine(dir, nil, nil)

	first, err := engine.Start("Build a CLI tool for widgets")
	if err != nil {
		t.Fatalf("start first inception: %v", err)
	}
	wikiDir := engine.WikiDir()
	if err := os.MkdirAll(wikiDir, 0o755); err != nil {
		t.Fatalf("mkdir wiki: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wikiDir, "spec.md"), []byte("# Spec\nFirst campaign"), 0o644); err != nil {
		t.Fatalf("write wiki: %v", err)
	}
	if err := engine.SetQuestions([]Question{{ID: "goal", Text: "Goal?"}}); err != nil {
		t.Fatalf("set questions: %v", err)
	}
	if _, err := engine.ArchiveCurrentCampaign(); err != nil {
		t.Fatalf("archive first campaign: %v", err)
	}
	if err := engine.Reset(); err != nil {
		t.Fatalf("reset: %v", err)
	}

	second, err := engine.Start("build a CLI tool for widgets!")
	if err != nil {
		t.Fatalf("start second inception: %v", err)
	}
	if second.IdeaSlug == first.IdeaSlug {
		t.Fatalf("second slug %q collided with first %q", second.IdeaSlug, first.IdeaSlug)
	}
	if err := engine.Reset(); err != nil {
		t.Fatalf("reset: %v", err)
	}

	archives, err := engine.ListCampaignArchives()
	if err != nil {
		t.Fatalf("list archives: %v", err)
	}
	if len(archives) != 1 {
		t.Fatalf("archives = %+v, want exactly the first campaign preserved", archives)
	}
	preserved, err := engine.LoadCampaignArchive(first.IdeaSlug)
	if err != nil {
		t.Fatalf("load first archive: %v", err)
	}
	if len(preserved.State.Questions) != 1 || preserved.State.Questions[0].ID != "goal" {
		t.Fatalf("first archive state was overwritten: %+v", preserved.State)
	}
	if len(preserved.WikiFiles) != 1 || preserved.WikiFiles[0] != "spec.md" {
		t.Fatalf("first archive wiki files = %+v, want spec.md preserved", preserved.WikiFiles)
	}
	restoredWikiDir := filepath.Join(dir, inceptionCampaignsDir, first.IdeaSlug, inceptionArchiveWiki)
	data, err := os.ReadFile(filepath.Join(restoredWikiDir, "spec.md"))
	if err != nil {
		t.Fatalf("read archived wiki: %v", err)
	}
	if string(data) != "# Spec\nFirst campaign" {
		t.Fatalf("first archive wiki content overwritten: %q", string(data))
	}
}
