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
	if revision.ID != archive.ID || revision.RevisionOf != "" || revision.Revision != 2 {
		t.Fatalf("revision identity = %#v", revision)
	}
	if revision.Lease == nil || revision.Lease.Owner != "local" || revision.Lease.Surface != "revise" {
		t.Fatalf("revision lease = %#v", revision.Lease)
	}
	if revision.State == nil || revision.State.IdeaSlug != archive.ID || revision.State.Phase != PhaseCapture {
		t.Fatalf("revision state = %#v", revision.State)
	}
	copiedWiki := filepath.Join(e.dataDir, inceptionCampaignsDir, archive.ID, inceptionArchiveWiki, "vision.md")
	if got, err := os.ReadFile(copiedWiki); err != nil || string(got) != "# vision" {
		t.Fatalf("revision wiki = %q, %v", got, err)
	}
	zeroNowRevision, err := e.ReviseCampaignArchive(revision.ID, "carol", time.Time{})
	if err != nil {
		t.Fatalf("zero-now ReviseCampaignArchive() error = %v", err)
	}
	if zeroNowRevision.ID != archive.ID || zeroNowRevision.Revision != 3 || zeroNowRevision.Lease.Owner != "carol" {
		t.Fatalf("zero-now revision = %#v", zeroNowRevision)
	}

	second, err := e.ReviseCampaignArchive(archive.ID, "dana", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second ReviseCampaignArchive() error = %v", err)
	}
	if second.ID != archive.ID || second.Revision != 4 || second.Lease.Owner != "dana" {
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
	if external.ID != "External Campaign" || external.RevisionOf != "" || external.Revision != 1 {
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
	second, err := e.ReviseExternalCampaign("External Campaign", "Next", "", "Custom", "research", "dana", nil, time.Time{})
	if err != nil {
		t.Fatalf("second ReviseExternalCampaign() error = %v", err)
	}
	if second.ID != "External Campaign" || second.Revision != 2 || second.Engine != "Custom" || second.Type != "research" || second.Lease.Owner != "dana" {
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

func TestCampaignArchiveListCollapsesStableSpecDuplicates(t *testing.T) {
	e := newTestEngine(t)
	baseTime := time.Date(2026, 9, 26, 0, 24, 27, 0, time.UTC)
	root := InceptionCampaignArchive{
		ID:         "unique-console",
		Engine:     "Spec Kit",
		Type:       "inception",
		ArchivedAt: baseTime,
		State: &InceptionState{
			IdeaText:       "what can I add to console to make it unique",
			IdeaSlug:       "unique-console",
			Phase:          PhaseClarify,
			PhaseChangedAt: &baseTime,
			Answers:        map[string]string{},
		},
	}
	newestTime := baseTime.Add(10 * time.Second)
	dup := root
	dup.ID = "unique-console-rev-2"
	dup.RevisionOf = root.ID
	dup.Revision = 2
	dup.ArchivedAt = newestTime
	dup.State = copyInceptionState(root.State)
	dup.State.IdeaSlug = dup.ID
	dup.State.Phase = PhaseCapture
	dup.State.PhaseChangedAt = &newestTime
	for _, archive := range []*InceptionCampaignArchive{&root, &dup} {
		if err := e.writeArchiveStateLocked(archive); err != nil {
			t.Fatalf("write archive %s: %v", archive.ID, err)
		}
	}

	archives, err := e.ListCampaignArchives()
	if err != nil {
		t.Fatalf("ListCampaignArchives: %v", err)
	}
	if len(archives) != 1 {
		t.Fatalf("archives len = %d, want 1: %#v", len(archives), archives)
	}
	got := archives[0]
	if got.ID != dup.ID || got.RevisionOf != root.ID || got.State == nil || got.State.IdeaSlug != dup.ID || got.State.Phase != PhaseCapture {
		t.Fatalf("deduped archive = %#v", got)
	}
	if len(got.History) < 2 {
		t.Fatalf("deduped archive history = %#v, want merged duplicate history", got.History)
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, inceptionCampaignsDir, dup.ID)); err != nil {
		t.Fatalf("duplicate archive dir was lost: %v", err)
	}
}

func TestCampaignArchiveAdditionalErrorBranches(t *testing.T) {
	e := newTestEngine(t)
	state := &InceptionState{Phase: PhaseComplete, IdeaText: "no wiki", IdeaSlug: "no-wiki", Answers: map[string]string{}, StartedAt: time.Now()}
	if err := e.writeArchiveLocked(&InceptionCampaignArchive{ID: state.IdeaSlug, ArchivedAt: state.StartedAt, State: state}); err != nil {
		t.Fatalf("writeArchiveLocked() error = %v", err)
	}
	if revised, err := e.ReviseCampaignArchive(state.IdeaSlug, "owner", state.StartedAt); err != nil || revised.ID != "no-wiki" {
		t.Fatalf("ReviseCampaignArchive without wiki = %#v, %v", revised, err)
	}

	expiredAt := state.StartedAt.Add(-2 * campaignLeaseTTL)
	archive, err := e.LeaseCampaignArchive(state.IdeaSlug, "owner", "dashboard", expiredAt)
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

func TestCampaignArchiveListPreservesLegacySnapshots(t *testing.T) {
	for _, ids := range [][]string{
		{"spec", "spec-rev-2", "spec-rev-3"},
		{"spec-rev-2", "spec-rev-3"},
		{"spec-rev-3"},
	} {
		t.Run(strings.Join(ids, "+"), func(t *testing.T) {
			e := newTestEngine(t)
			baseTime := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
			for i, id := range ids {
				archive := &InceptionCampaignArchive{
					ID: id, Revision: i + 1, ArchivedAt: baseTime.Add(time.Duration(i) * time.Hour),
					State:     &InceptionState{IdeaSlug: id, IdeaText: id, Phase: PhaseCapture},
					WikiFiles: []string{"snapshot.md"},
				}
				if id != "spec" {
					archive.RevisionOf = "spec"
					if i > 0 {
						archive.RevisionOf = ids[i-1]
					}
				}
				if err := e.writeArchiveStateLocked(archive); err != nil {
					t.Fatal(err)
				}
				wiki := filepath.Join(e.dataDir, inceptionCampaignsDir, id, inceptionArchiveWiki)
				if err := os.MkdirAll(filepath.Join(wiki, "attachments"), 0o755); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"snapshot.md", "attachments/original.txt"} {
					if err := os.WriteFile(filepath.Join(wiki, name), []byte(id+":"+name), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			type snapshot struct {
				Content string
				Mode    os.FileMode
				ModTime time.Time
			}
			readTree := func() map[string]snapshot {
				t.Helper()
				files := map[string]snapshot{}
				err := filepath.Walk(filepath.Join(e.dataDir, inceptionCampaignsDir), func(path string, info os.FileInfo, err error) error {
					if err != nil {
						return err
					}
					var data []byte
					if !info.IsDir() {
						data, err = os.ReadFile(path)
						if err != nil {
							return err
						}
					}
					files[path] = snapshot{string(data), info.Mode(), info.ModTime()}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				return files
			}
			before := readTree()
			newestID := ids[len(ids)-1]
			for attempt := 0; attempt < 3; attempt++ {
				if attempt == 2 {
					e = NewInceptionEngine(e.dataDir, nil, nil)
				}
				archives, err := e.ListCampaignArchives()
				if err != nil {
					t.Fatal(err)
				}
				if after := readTree(); !reflect.DeepEqual(before, after) {
					t.Fatal("listing campaigns modified legacy archive files or directories")
				}
				if len(archives) != 1 || archives[0].ID != newestID || archives[0].State.IdeaSlug != newestID {
					t.Fatalf("list = %#v, want newest snapshot %s", archives, newestID)
				}
				loaded, err := e.LoadCampaignArchive(archives[0].ID)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(loaded.State, archives[0].State) {
					t.Fatal("listed state differs from loaded state")
				}
			}
			if _, err := e.RestoreCampaignArchive(newestID); err != nil {
				t.Fatal(err)
			}
			wiki, err := os.ReadFile(filepath.Join(e.dataDir, inceptionWikiDir, "snapshot.md"))
			if err != nil || string(wiki) != newestID+":snapshot.md" {
				t.Fatalf("restored wiki = %q, %v", wiki, err)
			}
			if _, err := e.ReviseCampaignArchive(newestID, "owner", baseTime.Add(24*time.Hour)); err != nil {
				t.Fatal(err)
			}
			archives, err := e.ListCampaignArchives()
			if err != nil || len(archives) != 1 || archives[0].ID != newestID {
				t.Fatalf("list after revise = %#v, %v", archives, err)
			}
		})
	}
}

func TestRewindExternalCampaignAppendsGenerationInPlace(t *testing.T) {
	e := newTestEngine(t)
	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	if _, err := e.RewindExternalCampaign("", CampaignRewind{}, now); err == nil || !strings.Contains(err.Error(), "campaign id required") {
		t.Fatalf("empty id error = %v", err)
	}
	if _, err := e.RewindExternalCampaign("org/../escape", CampaignRewind{}, now); err == nil || !strings.Contains(err.Error(), "invalid campaign id") {
		t.Fatalf("traversal id error = %v", err)
	}

	rewind := CampaignRewind{
		Title: "Widgets", Source: "org/repo#7", Repos: []string{"org/repo"}, Actor: "bob", Reason: "manual", LastGen: 4,
		Recheck: &CampaignRecheck{Enabled: true, Interval: time.Hour, LastAt: now},
		Drift:   &CampaignDrift{RecheckReason: "manual", PriorRevision: "ignored"},
	}
	first, err := e.RewindExternalCampaign("org/repo#7", rewind, now)
	if err != nil {
		t.Fatalf("first rewind error = %v", err)
	}
	if first.ID != "org/repo#7" || first.Revision != 1 || first.RevisionOf != "" || first.Engine != "Spektacular" || first.Type != "spektacular" ||
		first.RecheckInterval != time.Hour || first.Drift == nil || first.Drift.PriorRevision != "0" {
		t.Fatalf("first rewind = %#v", first)
	}
	if first.Lease == nil || first.Lease.Owner != "bob" || first.Lease.Surface != "revise" {
		t.Fatalf("first rewind lease = %#v", first.Lease)
	}
	if len(first.History) != 1 || first.History[0].LastGen != 4 || first.History[0].Reason != "manual" || first.History[0].Actor != "bob" ||
		!first.History[0].RewoundAt.Equal(now) || first.History[0].Drift == nil || first.History[0].Drift.PriorRevision != "0" {
		t.Fatalf("first generation log = %#v", first.History)
	}

	if _, err := e.RewindExternalCampaign("org/repo#7", rewind, now.Add(time.Minute)); !errors.Is(err, ErrCampaignRewindInFlight) {
		t.Fatalf("same-owner rewind during revise lease error = %v, want in flight", err)
	}
	other := rewind
	other.Actor = "carol"
	if _, err := e.RewindExternalCampaign("org/repo#7", other, now.Add(time.Minute)); !errors.Is(err, ErrCampaignLeaseHeld) {
		t.Fatalf("other-owner rewind error = %v, want lease held", err)
	}
	if _, err := e.ReleaseCampaignArchive("org/repo#7", "bob", now.Add(time.Minute)); err != nil {
		t.Fatalf("release revise lease: %v", err)
	}

	other.Reason, other.LastGen, other.Drift, other.Recheck = "cadence", 9, nil, nil
	second, err := e.RewindExternalCampaign("org/repo#7", other, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("second rewind error = %v", err)
	}
	if second.Revision != 2 || second.Drift != nil || second.Recheck == nil || len(second.History) != 2 ||
		second.History[0].LastGen != 4 || second.History[1].LastGen != 9 || second.History[1].Revision != 1 || second.History[1].Actor != "carol" {
		t.Fatalf("second rewind = %#v", second)
	}

	e.state = &InceptionState{Phase: PhaseCapture, IdeaSlug: "inception-run", IdeaText: "inception run", Answers: map[string]string{}}
	if _, err := e.ArchiveCurrentCampaign(); err != nil {
		t.Fatalf("ArchiveCurrentCampaign() error = %v", err)
	}
	if _, err := e.RewindExternalCampaign("inception-run", rewind, now); err == nil || !strings.Contains(err.Error(), "inception campaign") {
		t.Fatalf("inception rewind error = %v", err)
	}
}
