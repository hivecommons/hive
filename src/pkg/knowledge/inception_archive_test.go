package knowledge

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCampaignArchiveRoundTripListAndRestoreWiki(t *testing.T) {
	e := newTestEngine(t)
	state, err := e.Start("Archive the handoff campaign")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	state.Answers["audience"] = "operators"

	wikiDir := e.WikiDir()
	if err := os.MkdirAll(wikiDir, 0o755); err != nil {
		t.Fatalf("mkdir wiki: %v", err)
	}
	files := map[string]string{
		"b.md":       "# second",
		"a.md":       "# first",
		"ignore.txt": "not archived",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(wikiDir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write wiki %s: %v", name, err)
		}
	}

	archive, err := e.ArchiveCurrentCampaign()
	if err != nil {
		t.Fatalf("ArchiveCurrentCampaign() error = %v", err)
	}
	if archive.ID != state.IdeaSlug {
		t.Fatalf("archive ID = %q, want %q", archive.ID, state.IdeaSlug)
	}
	if archive.Engine != "Spec Kit" || archive.Type != "inception" {
		t.Fatalf("archive engine/type = %q/%q", archive.Engine, archive.Type)
	}
	if want := []string{"a.md", "b.md"}; !reflect.DeepEqual(archive.WikiFiles, want) {
		t.Fatalf("archive wiki files = %#v, want %#v", archive.WikiFiles, want)
	}

	loaded, err := e.LoadCampaignArchive(archive.ID)
	if err != nil {
		t.Fatalf("LoadCampaignArchive() error = %v", err)
	}
	if loaded.State == archive.State {
		t.Fatal("loaded archive reused state pointer")
	}
	if loaded.State.Answers["audience"] != "operators" {
		t.Fatalf("loaded answer = %q", loaded.State.Answers["audience"])
	}

	oldTime := archive.ArchivedAt.Add(-time.Hour)
	newTime := archive.ArchivedAt.Add(time.Hour)
	if _, err := e.ReviseExternalCampaign("external beta", "Beta", "speks", "", "", "", []string{"org/repo"}, oldTime); err != nil {
		t.Fatalf("ReviseExternalCampaign(old) error = %v", err)
	}
	if _, err := e.ReviseExternalCampaign("external alpha", "Alpha", "speks", "Custom", "other", "owner", nil, newTime); err != nil {
		t.Fatalf("ReviseExternalCampaign(new) error = %v", err)
	}
	archives, err := e.ListCampaignArchives()
	if err != nil {
		t.Fatalf("ListCampaignArchives() error = %v", err)
	}
	if len(archives) != 3 {
		t.Fatalf("archive count = %d, want 3", len(archives))
	}
	if !archives[0].ArchivedAt.After(archives[1].ArchivedAt) || !archives[1].ArchivedAt.After(archives[2].ArchivedAt) {
		t.Fatalf("archives not sorted newest first: %#v", []time.Time{archives[0].ArchivedAt, archives[1].ArchivedAt, archives[2].ArchivedAt})
	}

	if err := os.WriteFile(filepath.Join(wikiDir, "stale.md"), []byte("stale"), 0o644); err != nil {
		t.Fatalf("write stale wiki: %v", err)
	}
	restored, err := e.RestoreCampaignArchive(archive.ID)
	if err != nil {
		t.Fatalf("RestoreCampaignArchive() error = %v", err)
	}
	if restored.IdeaSlug != archive.ID || restored.Answers["audience"] != "operators" {
		t.Fatalf("restored state = %#v", restored)
	}
	if _, err := os.Stat(filepath.Join(wikiDir, "stale.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale wiki file err = %v, want not exist", err)
	}
	for name, want := range map[string]string{"a.md": "# first", "b.md": "# second"} {
		got, err := os.ReadFile(filepath.Join(wikiDir, name))
		if err != nil {
			t.Fatalf("read restored wiki %s: %v", name, err)
		}
		if string(got) != want {
			t.Fatalf("restored wiki %s = %q, want %q", name, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(wikiDir, "ignore.txt")); err != nil {
		t.Fatalf("non-md wiki file should be left alone: %v", err)
	}

	restored.Answers["audience"] = "mutated"
	if got := e.GetState().Answers["audience"]; got != "operators" {
		t.Fatalf("returned restore state aliases engine state, got %q", got)
	}
}

func TestCampaignArchiveLeaseReleaseAndRevise(t *testing.T) {
	e := newTestEngine(t)
	if _, err := e.Start("Lease and revise campaign"); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	wikiDir := e.WikiDir()
	if err := os.MkdirAll(wikiDir, 0o755); err != nil {
		t.Fatalf("mkdir wiki: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wikiDir, "vision.md"), []byte("# vision"), 0o644); err != nil {
		t.Fatalf("write wiki: %v", err)
	}
	archive, err := e.ArchiveCurrentCampaign()
	if err != nil {
		t.Fatalf("ArchiveCurrentCampaign() error = %v", err)
	}
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	leased, err := e.LeaseCampaignArchive(archive.ID, "alice", "dashboard", now)
	if err != nil {
		t.Fatalf("LeaseCampaignArchive() error = %v", err)
	}
	if leased.Lease == nil || leased.Lease.Owner != "alice" || leased.Lease.Surface != "dashboard" || !leased.Lease.ExpiresAt.Equal(now.Add(campaignLeaseTTL)) {
		t.Fatalf("lease = %#v", leased.Lease)
	}
	if _, err := e.LeaseCampaignArchive(archive.ID, "bob", "dashboard", now.Add(time.Minute)); !errors.Is(err, ErrCampaignLeaseHeld) {
		t.Fatalf("LeaseCampaignArchive by bob error = %v, want ErrCampaignLeaseHeld", err)
	}
	if _, err := e.ReleaseCampaignArchive(archive.ID, "bob", now.Add(time.Minute)); !errors.Is(err, ErrCampaignLeaseHeld) {
		t.Fatalf("ReleaseCampaignArchive by bob error = %v, want ErrCampaignLeaseHeld", err)
	}
	if released, err := e.ReleaseCampaignArchive(archive.ID, "alice", now.Add(time.Minute)); err != nil || released.Lease != nil {
		t.Fatalf("ReleaseCampaignArchive by alice = %#v, %v", released, err)
	}
	if _, err := e.ReleaseCampaignArchive(archive.ID, "alice", now.Add(2*time.Minute)); !errors.Is(err, ErrCampaignNoLease) {
		t.Fatalf("ReleaseCampaignArchive without lease error = %v, want ErrCampaignNoLease", err)
	}

	revision, err := e.ReviseCampaignArchive(archive.ID, "", now)
	if err != nil {
		t.Fatalf("ReviseCampaignArchive() error = %v", err)
	}
	if revision.ID != archive.ID+"-rev-2" || revision.RevisionOf != archive.ID || revision.Revision != 2 {
		t.Fatalf("revision identity = %#v", revision)
	}
	if revision.Lease == nil || revision.Lease.Owner != "local" || revision.Lease.Surface != "revise" {
		t.Fatalf("revision lease = %#v", revision.Lease)
	}
	if revision.State == nil || revision.State.IdeaSlug != revision.ID || revision.State.Phase != PhaseCapture {
		t.Fatalf("revision state = %#v", revision.State)
	}
	copiedWiki := filepath.Join(e.dataDir, inceptionCampaignsDir, revision.ID, inceptionArchiveWiki, "vision.md")
	if got, err := os.ReadFile(copiedWiki); err != nil || string(got) != "# vision" {
		t.Fatalf("revision wiki = %q, %v", got, err)
	}
	zeroNowRevision, err := e.ReviseCampaignArchive(revision.ID, "carol", time.Time{})
	if err != nil {
		t.Fatalf("zero-now ReviseCampaignArchive() error = %v", err)
	}
	if zeroNowRevision.ID != archive.ID+"-rev-3" || zeroNowRevision.Revision != 3 || zeroNowRevision.Lease.Owner != "carol" {
		t.Fatalf("zero-now revision = %#v", zeroNowRevision)
	}

	second, err := e.ReviseCampaignArchive(archive.ID, "dana", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second ReviseCampaignArchive() error = %v", err)
	}
	if second.ID != archive.ID+"-rev-4" || second.Revision != 4 || second.Lease.Owner != "dana" {
		t.Fatalf("second revision = %#v", second)
	}
}

func TestCampaignArchiveErrorsAndExternalValidation(t *testing.T) {
	e := newTestEngine(t)
	if got, err := e.ArchiveCurrentCampaign(); got != nil || err != nil {
		t.Fatalf("ArchiveCurrentCampaign with no state = %#v, %v", got, err)
	}
	archives, err := e.ListCampaignArchives()
	if err != nil {
		t.Fatalf("ListCampaignArchives empty error = %v", err)
	}
	if len(archives) != 0 {
		t.Fatalf("empty archive list = %#v", archives)
	}

	missingIDCalls := []struct {
		name string
		call func() error
	}{
		{"load empty", func() error { _, err := e.LoadCampaignArchive(""); return err }},
		{"lease empty", func() error { _, err := e.LeaseCampaignArchive("", "owner", "", time.Time{}); return err }},
		{"restore empty", func() error { _, err := e.RestoreCampaignArchive(""); return err }},
		{"release empty", func() error { _, err := e.ReleaseCampaignArchive("", "owner", time.Time{}); return err }},
		{"revise empty", func() error { _, err := e.ReviseCampaignArchive("", "owner", time.Time{}); return err }},
	}
	for _, tt := range missingIDCalls {
		if err := tt.call(); err == nil || !strings.Contains(err.Error(), "campaign id required") {
			t.Fatalf("%s error = %v, want campaign id required", tt.name, err)
		}
	}
	if _, err := e.LoadCampaignArchive("does-not-exist"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing archive error = %v, want not found", err)
	}
	if _, err := e.ReviseExternalCampaign("", "title", "source", "engine", "type", "owner", nil, time.Time{}); err == nil || !strings.Contains(err.Error(), "campaign id required") {
		t.Fatalf("ReviseExternalCampaign empty id error = %v", err)
	}

	now := time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC)
	external, err := e.ReviseExternalCampaign(" External Campaign ", " Title ", " Source ", "", "", "", []string{"org/repo"}, now)
	if err != nil {
		t.Fatalf("ReviseExternalCampaign() error = %v", err)
	}
	if external.ID != "external-campaign-rev-2" || external.RevisionOf != "external-campaign" || external.Revision != 2 {
		t.Fatalf("external identity = %#v", external)
	}
	if external.Engine != "Spektacular" || external.Type != "spektacular" || external.Title != "Title" || external.Source != "Source" {
		t.Fatalf("external metadata = %#v", external)
	}
	if external.Lease == nil || external.Lease.Owner != "local" || external.Lease.Surface != "revise" {
		t.Fatalf("external lease = %#v", external.Lease)
	}
	if !reflect.DeepEqual(external.Repos, []string{"org/repo"}) {
		t.Fatalf("external repos = %#v", external.Repos)
	}
	second, err := e.ReviseExternalCampaign("external-campaign", "Next", "", "Custom", "research", "dana", nil, time.Time{})
	if err != nil {
		t.Fatalf("second ReviseExternalCampaign() error = %v", err)
	}
	if second.ID != "external-campaign-rev-3" || second.Engine != "Custom" || second.Type != "research" || second.Lease.Owner != "dana" {
		t.Fatalf("second external = %#v", second)
	}
}

func TestCampaignArchivePreservesMetadataAndSkipsCorruptEntries(t *testing.T) {
	e := newTestEngine(t)
	state := &InceptionState{
		Phase:     PhaseCapture,
		Mode:      InceptionGreenfield,
		IdeaText:  "preserve metadata",
		IdeaSlug:  "preserve-metadata",
		Questions: []Question{{ID: "q1", Text: "Question?", Category: "features"}},
		Answers:   map[string]string{"q1": "answer"},
		FactSlugs: []string{"fact-one"},
		StartedAt: time.Date(2026, 9, 24, 14, 0, 0, 0, time.UTC),
	}
	e.state = state
	first, err := e.ArchiveCurrentCampaign()
	if err != nil {
		t.Fatalf("ArchiveCurrentCampaign() error = %v", err)
	}
	first.Title = "Saved title"
	first.Source = "dashboard"
	first.Repos = []string{"org/repo"}
	first.RevisionOf = "root-campaign"
	first.Revision = 4
	first.Lease = &CampaignLease{Owner: "alice", Surface: "dashboard", AcquiredAt: state.StartedAt, ExpiresAt: state.StartedAt.Add(campaignLeaseTTL)}
	if err := e.writeArchiveStateLocked(first); err != nil {
		t.Fatalf("writeArchiveStateLocked() error = %v", err)
	}

	e.state.IdeaText = "updated body"
	updated, err := e.ArchiveCurrentCampaign()
	if err != nil {
		t.Fatalf("second ArchiveCurrentCampaign() error = %v", err)
	}
	if updated.Title != first.Title || updated.Source != first.Source || updated.RevisionOf != first.RevisionOf || updated.Revision != first.Revision {
		t.Fatalf("metadata not preserved: %#v", updated)
	}
	if updated.Lease == nil || updated.Lease.Owner != "alice" || !reflect.DeepEqual(updated.Repos, first.Repos) {
		t.Fatalf("lease/repos not preserved: %#v", updated)
	}

	root := filepath.Join(e.dataDir, inceptionCampaignsDir)
	if err := os.WriteFile(filepath.Join(root, "not-a-dir"), []byte("ignored"), 0o644); err != nil {
		t.Fatalf("write non-dir archive entry: %v", err)
	}
	corruptDir := filepath.Join(root, "corrupt")
	if err := os.MkdirAll(corruptDir, 0o755); err != nil {
		t.Fatalf("mkdir corrupt archive: %v", err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, inceptionArchiveState), []byte("{"), 0o644); err != nil {
		t.Fatalf("write corrupt archive: %v", err)
	}
	archives, err := e.ListCampaignArchives()
	if err != nil {
		t.Fatalf("ListCampaignArchives() error = %v", err)
	}
	if len(archives) != 1 || archives[0].ID != first.ID {
		t.Fatalf("archives after corrupt skip = %#v", archives)
	}

	if err := e.writeArchiveLocked(nil); err != nil {
		t.Fatalf("writeArchiveLocked(nil) error = %v", err)
	}
	if err := e.writeArchiveLocked(&InceptionCampaignArchive{ID: "missing-state"}); err != nil {
		t.Fatalf("writeArchiveLocked without state error = %v", err)
	}
	if err := e.writeArchiveStateLocked(nil); err != nil {
		t.Fatalf("writeArchiveStateLocked(nil) error = %v", err)
	}
}

func TestCampaignArchiveAdditionalErrorBranches(t *testing.T) {
	e := newTestEngine(t)
	state := &InceptionState{Phase: PhaseComplete, IdeaText: "no wiki", IdeaSlug: "no-wiki", Answers: map[string]string{}, StartedAt: time.Now()}
	if err := e.writeArchiveLocked(&InceptionCampaignArchive{ID: state.IdeaSlug, ArchivedAt: state.StartedAt, State: state}); err != nil {
		t.Fatalf("writeArchiveLocked() error = %v", err)
	}
	if revised, err := e.ReviseCampaignArchive(state.IdeaSlug, "owner", state.StartedAt); err != nil || revised.ID != "no-wiki-rev-2" {
		t.Fatalf("ReviseCampaignArchive without wiki = %#v, %v", revised, err)
	}

	expiredAt := state.StartedAt.Add(-2 * campaignLeaseTTL)
	archive, err := e.LeaseCampaignArchive(state.IdeaSlug, "alice", "dashboard", expiredAt)
	if err != nil {
		t.Fatalf("initial LeaseCampaignArchive() error = %v", err)
	}
	if archive.Lease == nil || !archive.Lease.ExpiresAt.Before(state.StartedAt) {
		t.Fatalf("expected expired lease, got %#v", archive.Lease)
	}
	refreshed, err := e.LeaseCampaignArchive(state.IdeaSlug, "bob", "dashboard", state.StartedAt)
	if err != nil {
		t.Fatalf("expired lease refresh error = %v", err)
	}
	if refreshed.Lease.Owner != "bob" {
		t.Fatalf("expired lease owner = %q, want bob", refreshed.Lease.Owner)
	}
	if _, err := e.ReleaseCampaignArchive(state.IdeaSlug, "bob", state.StartedAt.Add(campaignLeaseTTL+time.Minute)); !errors.Is(err, ErrCampaignNoLease) {
		t.Fatalf("expired release error = %v, want ErrCampaignNoLease", err)
	}

	noState := &InceptionCampaignArchive{ID: "external-only", Engine: "Spektacular", Type: "spektacular", ArchivedAt: state.StartedAt}
	if err := e.writeArchiveStateLocked(noState); err != nil {
		t.Fatalf("write no-state archive: %v", err)
	}
	if _, err := e.RestoreCampaignArchive(noState.ID); err == nil || !strings.Contains(err.Error(), "has no inception state") {
		t.Fatalf("RestoreCampaignArchive no state error = %v", err)
	}

	archiveRoot := filepath.Join(e.dataDir, inceptionCampaignsDir, "id-backfilled")
	if err := os.MkdirAll(archiveRoot, 0o755); err != nil {
		t.Fatalf("mkdir raw archive: %v", err)
	}
	raw := `{"engine":"Spec Kit","type":"inception","archived_at":"2026-09-24T14:00:00Z","state":{"phase":"capture","idea_slug":"id-backfilled","answers":{},"started_at":"2026-09-24T14:00:00Z"}}`
	if err := os.WriteFile(filepath.Join(archiveRoot, inceptionArchiveState), []byte(raw), 0o644); err != nil {
		t.Fatalf("write raw archive: %v", err)
	}
	backfilled, err := e.LoadCampaignArchive("id-backfilled")
	if err != nil {
		t.Fatalf("LoadCampaignArchive backfilled error = %v", err)
	}
	if backfilled.ID != "id-backfilled" {
		t.Fatalf("backfilled ID = %q", backfilled.ID)
	}
}

func TestCampaignArchiveSmallBranches(t *testing.T) {
	e := newTestEngine(t)
	if err := e.restoreArchiveWikiLocked("missing-wiki"); err != nil {
		t.Fatalf("restoreArchiveWikiLocked missing wiki error = %v", err)
	}
	if copyInceptionState(nil) != nil {
		t.Fatal("copyInceptionState(nil) should return nil")
	}
	e.state = &InceptionState{IdeaText: "fallback archive id", Answers: map[string]string{}, StartedAt: time.Now()}
	if got := e.archiveFromStateLocked(time.Unix(123, 0)); got.ID != "inception-fallback-archive-id" {
		t.Fatalf("fallback archive ID = %q", got.ID)
	}
	e.state = &InceptionState{Answers: map[string]string{}, StartedAt: time.Now()}
	if got := e.archiveFromStateLocked(time.Unix(456, 0)); got.ID != "inception" {
		t.Fatalf("empty fallback archive ID = %q", got.ID)
	}

	state := &InceptionState{Phase: PhaseCapture, IdeaText: "default owner", IdeaSlug: "default-owner", Answers: map[string]string{}, StartedAt: time.Now()}
	if err := e.writeArchiveLocked(&InceptionCampaignArchive{ID: state.IdeaSlug, ArchivedAt: state.StartedAt, State: state}); err != nil {
		t.Fatalf("writeArchiveLocked() error = %v", err)
	}
	leased, err := e.LeaseCampaignArchive(state.IdeaSlug, " ", "  surface  ", time.Time{})
	if err != nil {
		t.Fatalf("LeaseCampaignArchive default owner error = %v", err)
	}
	if leased.Lease == nil || leased.Lease.Owner != "local" || leased.Lease.Surface != "surface" || leased.Lease.AcquiredAt.IsZero() {
		t.Fatalf("default lease = %#v", leased.Lease)
	}
	renewed, err := e.LeaseCampaignArchive(state.IdeaSlug, "local", "renewed", leased.Lease.AcquiredAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("same-owner LeaseCampaignArchive error = %v", err)
	}
	if renewed.Lease.Surface != "renewed" {
		t.Fatalf("renewed lease surface = %q", renewed.Lease.Surface)
	}
	if _, err := e.ReleaseCampaignArchive(state.IdeaSlug, " ", time.Time{}); err != nil {
		t.Fatalf("ReleaseCampaignArchive default owner error = %v", err)
	}
}
