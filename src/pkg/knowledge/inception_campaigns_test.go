package knowledge

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCampaignWiki(t *testing.T, e *InceptionEngine, files map[string]string) {
	t.Helper()
	wikiDir := filepath.Join(e.dataDir, inceptionWikiDir)
	if err := os.MkdirAll(wikiDir, 0o755); err != nil {
		t.Fatalf("mkdir wiki: %v", err)
	}
	for name, body := range files {
		path := filepath.Join(wikiDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir wiki parent: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write wiki %s: %v", name, err)
		}
	}
}

func readCampaignFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestCampaignArchiveLifecycleLeaseReviseAndRestore(t *testing.T) {
	e := newTestEngine(t)
	state, err := e.Start("Durable campaign archive flow")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := e.SetQuestions([]Question{{ID: "goal", Text: "Goal?"}}); err != nil {
		t.Fatalf("SetQuestions: %v", err)
	}
	if _, err := e.SubmitAnswers(map[string]string{"goal": "restore wiki"}); err != nil {
		t.Fatalf("SubmitAnswers: %v", err)
	}
	writeCampaignWiki(t, e, map[string]string{
		"brief.md":       "original brief",
		"notes.md":       "original notes",
		"ignored.txt":    "not archived",
		"nested/skip.md": "nested file is skipped",
	})

	archive, err := e.ArchiveCurrentCampaign()
	if err != nil {
		t.Fatalf("ArchiveCurrentCampaign: %v", err)
	}
	if archive == nil {
		t.Fatal("ArchiveCurrentCampaign returned nil archive")
	}
	if archive.ID != state.IdeaSlug {
		t.Fatalf("archive ID = %q, want %q", archive.ID, state.IdeaSlug)
	}
	if archive.Engine != "Spec Kit" || archive.Type != "inception" {
		t.Fatalf("archive engine/type = %q/%q", archive.Engine, archive.Type)
	}
	if got, want := strings.Join(archive.WikiFiles, ","), "brief.md,notes.md"; got != want {
		t.Fatalf("archive wiki files = %q, want %q", got, want)
	}

	archives, err := e.ListCampaignArchives()
	if err != nil {
		t.Fatalf("ListCampaignArchives: %v", err)
	}
	if len(archives) != 1 || archives[0].ID != archive.ID {
		t.Fatalf("archives = %+v, want one archive %q", archives, archive.ID)
	}

	loaded, err := e.LoadCampaignArchive(archive.ID)
	if err != nil {
		t.Fatalf("LoadCampaignArchive: %v", err)
	}
	loaded.State.Answers["goal"] = "mutated copy"
	reloaded, err := e.LoadCampaignArchive(archive.ID)
	if err != nil {
		t.Fatalf("reload after copy mutation: %v", err)
	}
	if reloaded.State.Answers["goal"] != "restore wiki" {
		t.Fatalf("archive state was aliased through load: %q", reloaded.State.Answers["goal"])
	}

	now := time.Date(2026, 9, 24, 18, 0, 0, 0, time.UTC)
	leased, err := e.LeaseCampaignArchive(archive.ID, "alice", "resume", now)
	if err != nil {
		t.Fatalf("LeaseCampaignArchive alice: %v", err)
	}
	if leased.Lease == nil || leased.Lease.Owner != "alice" || leased.Lease.Surface != "resume" || !leased.Lease.ExpiresAt.Equal(now.Add(campaignLeaseTTL)) {
		t.Fatalf("lease = %+v", leased.Lease)
	}
	if _, err := e.LeaseCampaignArchive(archive.ID, "bob", "resume", now.Add(time.Hour)); !errors.Is(err, ErrCampaignLeaseHeld) {
		t.Fatalf("conflicting lease err = %v, want ErrCampaignLeaseHeld", err)
	}
	if _, err := e.ReleaseCampaignArchive(archive.ID, "bob", now.Add(time.Hour)); !errors.Is(err, ErrCampaignLeaseHeld) {
		t.Fatalf("conflicting release err = %v, want ErrCampaignLeaseHeld", err)
	}
	refreshed, err := e.LeaseCampaignArchive(archive.ID, "alice", "resume-again", now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("LeaseCampaignArchive refresh: %v", err)
	}
	if refreshed.Lease.Surface != "resume-again" {
		t.Fatalf("refreshed surface = %q", refreshed.Lease.Surface)
	}
	released, err := e.ReleaseCampaignArchive(archive.ID, "alice", now.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("ReleaseCampaignArchive alice: %v", err)
	}
	if released.Lease != nil {
		t.Fatalf("release left lease = %+v", released.Lease)
	}
	if _, err := e.ReleaseCampaignArchive(archive.ID, "alice", now.Add(4*time.Hour)); !errors.Is(err, ErrCampaignNoLease) {
		t.Fatalf("release without lease err = %v, want ErrCampaignNoLease", err)
	}

	revision, err := e.ReviseCampaignArchive(archive.ID, "carol", now.Add(5*time.Hour))
	if err != nil {
		t.Fatalf("ReviseCampaignArchive: %v", err)
	}
	if revision.ID != archive.ID+"-rev-2" || revision.RevisionOf != archive.ID || revision.Revision != 2 {
		t.Fatalf("revision linkage = id:%q of:%q rev:%d", revision.ID, revision.RevisionOf, revision.Revision)
	}
	if revision.Lease == nil || revision.Lease.Owner != "carol" || revision.Lease.Surface != "revise" {
		t.Fatalf("revision lease = %+v", revision.Lease)
	}
	if revision.State.IdeaSlug != revision.ID || revision.State.Phase != PhaseCapture {
		t.Fatalf("revision state = slug:%q phase:%q", revision.State.IdeaSlug, revision.State.Phase)
	}
	revisionWiki := filepath.Join(e.dataDir, inceptionCampaignsDir, revision.ID, inceptionArchiveWiki, "brief.md")
	if got := readCampaignFile(t, revisionWiki); got != "original brief" {
		t.Fatalf("revision wiki brief = %q", got)
	}

	if err := e.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	writeCampaignWiki(t, e, map[string]string{
		"brief.md": "changed brief",
		"stale.md": "remove on restore",
	})
	restored, err := e.RestoreCampaignArchive(revision.ID)
	if err != nil {
		t.Fatalf("RestoreCampaignArchive: %v", err)
	}
	if restored.IdeaSlug != revision.ID || restored.Answers["goal"] != "restore wiki" {
		t.Fatalf("restored state = %+v", restored)
	}
	wikiDir := filepath.Join(e.dataDir, inceptionWikiDir)
	if got := readCampaignFile(t, filepath.Join(wikiDir, "brief.md")); got != "original brief" {
		t.Fatalf("restored brief = %q", got)
	}
	if _, err := os.Stat(filepath.Join(wikiDir, "stale.md")); !os.IsNotExist(err) {
		t.Fatalf("stale wiki file err = %v, want not exist", err)
	}
}

func TestCampaignArchiveExternalReviseAndErrorPaths(t *testing.T) {
	e := newTestEngine(t)
	if archives, err := e.ListCampaignArchives(); err != nil || len(archives) != 0 {
		t.Fatalf("empty ListCampaignArchives = len %d err %v", len(archives), err)
	}
	if archive, err := e.ArchiveCurrentCampaign(); err != nil || archive != nil {
		t.Fatalf("nil-state ArchiveCurrentCampaign = archive %+v err %v", archive, err)
	}

	missingID := "missing-campaign"
	for name, run := range map[string]func() error{
		"load missing":    func() error { _, err := e.LoadCampaignArchive(missingID); return err },
		"lease missing":   func() error { _, err := e.LeaseCampaignArchive(missingID, "alice", "resume", time.Time{}); return err },
		"release missing": func() error { _, err := e.ReleaseCampaignArchive(missingID, "alice", time.Time{}); return err },
		"revise missing":  func() error { _, err := e.ReviseCampaignArchive(missingID, "alice", time.Time{}); return err },
		"restore missing": func() error { _, err := e.RestoreCampaignArchive(missingID); return err },
		"bad id":          func() error { _, err := e.LoadCampaignArchive("../.."); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(); err == nil {
				t.Fatal("expected error")
			}
		})
	}

	outside := filepath.Join(e.dataDir, "escaped", inceptionArchiveState)
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		t.Fatalf("mkdir escaped fixture: %v", err)
	}
	if err := os.WriteFile(outside, []byte(`{"id":"escaped"}`), 0o644); err != nil {
		t.Fatalf("write escaped fixture: %v", err)
	}
	if _, err := e.LoadCampaignArchive("../escaped"); err == nil {
		t.Fatal("path traversal load unexpectedly found archive outside campaigns dir")
	}

	now := time.Date(2026, 9, 24, 19, 0, 0, 0, time.UTC)
	external, err := e.ReviseExternalCampaign("External Campaign", "External Title", "https://example.invalid/campaign", "", "", "", []string{"hivecommons/hive"}, now)
	if err != nil {
		t.Fatalf("ReviseExternalCampaign: %v", err)
	}
	if external.ID != "external-campaign-rev-2" || external.RevisionOf != "external-campaign" || external.Revision != 2 {
		t.Fatalf("external revision linkage = id:%q of:%q rev:%d", external.ID, external.RevisionOf, external.Revision)
	}
	if external.Engine != "Spektacular" || external.Type != "spektacular" || external.Lease == nil || external.Lease.Owner != "local" {
		t.Fatalf("external defaults/lease = engine:%q type:%q lease:%+v", external.Engine, external.Type, external.Lease)
	}
	loaded, err := e.LoadCampaignArchive(external.ID)
	if err != nil {
		t.Fatalf("load external revision: %v", err)
	}
	if loaded.Title != "External Title" || loaded.Source != "https://example.invalid/campaign" || len(loaded.Repos) != 1 || loaded.Repos[0] != "hivecommons/hive" {
		t.Fatalf("external round trip = %+v", loaded)
	}
	if _, err := e.RestoreCampaignArchive(external.ID); err == nil || !strings.Contains(err.Error(), "no inception state") {
		t.Fatalf("restore external err = %v, want no inception state", err)
	}

	second, err := e.ReviseExternalCampaign("External Campaign", "External Title 2", "source", "Other", "custom", "dana", nil, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("second ReviseExternalCampaign: %v", err)
	}
	if second.ID != "external-campaign-rev-3" || second.Engine != "Other" || second.Type != "custom" || second.Lease.Owner != "dana" {
		t.Fatalf("second external revision = %+v", second)
	}
	if _, err := e.ReviseExternalCampaign("../..", "bad", "", "", "", "", nil, now); err == nil {
		t.Fatal("ReviseExternalCampaign accepted path traversal/empty id")
	}
}

func TestCampaignArchiveExpiredLeaseAndArchiveFallbackID(t *testing.T) {
	e := newTestEngine(t)
	now := time.Date(2026, 9, 24, 20, 0, 0, 0, time.UTC)
	e.state = &InceptionState{Phase: PhaseCapture, IdeaText: "!!!", Answers: map[string]string{}, StartedAt: now}
	archive := e.archiveFromStateLocked(now)
	if archive.ID != "inception" {
		t.Fatalf("sanitized archive id = %q, want inception", archive.ID)
	}
	if err := e.writeArchiveLocked(archive); err != nil {
		t.Fatalf("writeArchiveLocked fallback: %v", err)
	}
	leased, err := e.LeaseCampaignArchive(archive.ID, "alice", "resume", now)
	if err != nil {
		t.Fatalf("LeaseCampaignArchive: %v", err)
	}
	if leased.Lease == nil {
		t.Fatal("expected lease")
	}
	released, err := e.ReleaseCampaignArchive(archive.ID, "bob", now.Add(2*campaignLeaseTTL))
	if !errors.Is(err, ErrCampaignNoLease) {
		t.Fatalf("expired release err = %v, want ErrCampaignNoLease", err)
	}
	if released != nil {
		t.Fatalf("expired release archive = %+v, want nil", released)
	}
	loaded, err := e.LoadCampaignArchive(archive.ID)
	if err != nil {
		t.Fatalf("load after expired release: %v", err)
	}
	if loaded.Lease != nil {
		t.Fatalf("expired release did not clear lease: %+v", loaded.Lease)
	}
}
