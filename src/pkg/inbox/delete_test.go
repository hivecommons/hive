package inbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// An upload's delete removes the stored file and the item; the Knowledge
// fact stays unless the caller asks for it to go.
func TestDeleteItemUploadKeepsKnowledgeByDefault(t *testing.T) {
	e := newUploadEnv(t, config.InboxConfig{})
	ctx := context.Background()
	it, err := e.svc.AddUpload(ctx, UploadInput{Filename: "notes.md", Data: readUploadFixture(t, "notes.md"), AddedBy: "ana@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Store.AppendShare(it.Key(), ShareRecord{Destination: DestDibs, Actor: "ana", StartedAt: time.Now(), Status: StatusFiled}); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.DeleteItem(ctx, UploadType, it.ID, DeleteOptions{Actor: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.StoredRemoved || res.KnowledgeRemoved || !res.KnowledgeKept || res.SharesKept != 1 {
		t.Fatalf("result = %+v", res)
	}
	if _, err := e.src.Store.Get(it.ID); !errors.Is(err, ErrUploadNotFound) {
		t.Fatalf("upload still stored: %v", err)
	}
	if _, err := os.Stat(it.FactPath); err != nil {
		t.Fatalf("the Knowledge fact must stay: %v", err)
	}
	items, err := e.svc.Items(ctx)
	if err != nil || len(items) != 0 {
		t.Fatalf("Items after delete = %+v, %v", items, err)
	}
	if !e.svc.Store.Deleted(it.Key()) || len(e.svc.Store.History(it.Key())) != 0 {
		t.Fatal("no tombstone, or the share history survived")
	}
	if _, err := e.svc.DeleteItem(ctx, UploadType, it.ID, DeleteOptions{Actor: "bob"}); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("second delete = %v, want ErrItemNotFound", err)
	}
}

func TestDeleteItemUploadRemovesKnowledgeWhenAsked(t *testing.T) {
	e := newUploadEnv(t, config.InboxConfig{})
	ctx := context.Background()
	it, err := e.svc.AddUpload(ctx, UploadInput{Filename: "notes.md", Data: readUploadFixture(t, "notes.md"), AddedBy: "ana@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.DeleteItem(ctx, UploadType, it.ID, DeleteOptions{Actor: "ana", RemoveKnowledge: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.StoredRemoved || !res.KnowledgeRemoved || res.KnowledgeKept {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(it.FactPath); !os.IsNotExist(err) {
		t.Fatalf("fact still there: %v", err)
	}
}

// A synced item keeps coming from its source; the tombstone keeps it out of
// the Inbox, across a restart too, and its fact is never removed.
func TestDeleteItemSyncedSourceIsTombstoned(t *testing.T) {
	fact := filepath.Join(t.TempDir(), "plaud-rec-1.md")
	if err := os.WriteFile(fact, []byte("---\nsource: plaud\n---\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{items: []Item{
		{Source: "plaud", ID: "rec-1", Title: "one", FactSlug: "plaud-rec-1", FactPath: fact},
		{Source: "plaud", ID: "rec-2", Title: "two"},
	}}
	svc := newTestService(t, config.InboxConfig{}, src)
	ctx := context.Background()
	res, err := svc.DeleteItem(ctx, "plaud", "rec-1", DeleteOptions{Actor: "ana", RemoveKnowledge: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.StoredRemoved || res.KnowledgeRemoved || !res.KnowledgeKept {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(fact); err != nil {
		t.Fatalf("a synced source's fact must stay: %v", err)
	}
	// The source still lists rec-1 (the next sync): it stays out.
	src.items = append([]Item(nil), src.items...)
	items, err := svc.Items(ctx)
	if err != nil || len(items) != 1 || items[0].ID != "rec-2" {
		t.Fatalf("Items = %+v, %v", items, err)
	}
	// A late route outcome for the deleted item must not clear the tombstone.
	if _, err := svc.Store.Set(Key("plaud", "rec-1"), DestDibs, StatusFiled, Result{}, ""); err != nil {
		t.Fatal(err)
	}
	reopened := NewStore(svc.Store.Path)
	if !reopened.Deleted(Key("plaud", "rec-1")) || reopened.Deleted(Key("plaud", "rec-2")) {
		t.Fatal("tombstone not persisted")
	}
	if !reopened.Seen(Key("plaud", "rec-1")) {
		t.Fatal("a deleted item must stay seen so auto-routing never fires on it")
	}
}

func TestDeleteItemEmailRemovesFileAndGatesApprovalQueue(t *testing.T) {
	e, _ := newEmailSource(t)
	svc := NewService(config.InboxConfig{}, NewStore(filepath.Join(t.TempDir(), "inbox.json")), nil, nil, nil)
	svc.EnableEmail(e)
	ctx := context.Background()
	trusted, err := e.Add(emailDelivery("m1@example.com", att("notes.md", wire.AttachMarkdown, "text/markdown", emailMD)), true)
	if err != nil || len(trusted) != 1 {
		t.Fatalf("add: %v %v", trusted, err)
	}
	waiting, err := e.Add(emailDelivery("m2@example.com", att("more.md", wire.AttachMarkdown, "text/markdown", emailMD)), false)
	if err != nil || len(waiting) != 1 {
		t.Fatalf("add: %v %v", waiting, err)
	}
	if _, err := svc.DeleteItem(ctx, EmailType, waiting[0].ID, DeleteOptions{Actor: "bob"}); !errors.Is(err, ErrItemOwnerOnly) {
		t.Fatalf("read-write delete of a waiting item = %v, want ErrItemOwnerOnly", err)
	}
	if _, err := svc.DeleteItem(ctx, EmailType, waiting[0].ID, DeleteOptions{Actor: "owner", AsOwner: true}); err != nil {
		t.Fatal(err)
	}
	res, err := svc.DeleteItem(ctx, EmailType, trusted[0].ID, DeleteOptions{Actor: "bob"})
	if err != nil || !res.StoredRemoved || res.KnowledgeKept || res.KnowledgeRemoved {
		t.Fatalf("delete = %+v, %v", res, err)
	}
	if _, err := os.Stat(e.itemDir(trusted[0].ID)); !os.IsNotExist(err) {
		t.Fatalf("mailed-in file still stored: %v", err)
	}
	if items, err := svc.Items(ctx); err != nil || len(items) != 0 {
		t.Fatalf("Items = %+v, %v", items, err)
	}
}

// A hive recording keeps its rule (the recorder or an owner) and, by
// default, its Knowledge fact with the machine transcript.
func TestDeleteItemRecordingKeepsFactAndRecorderRule(t *testing.T) {
	svc, src, _ := newRecordingService(t, enabledRecording())
	withTranscription(t, svc, config.InboxTranscription{Enabled: true, ConsentAcknowledged: true})
	m := recordAndFinalize(t, svc, "ana", RecordingOptions{})
	svc.Wait()
	ctx := context.Background()
	if _, err := svc.DeleteItem(ctx, RecordingType, m.ID, DeleteOptions{Actor: "bob"}); !errors.Is(err, ErrRecordingNotYours) {
		t.Fatalf("someone else's delete = %v, want ErrRecordingNotYours", err)
	}
	var audits []RecordingAudit
	res, err := svc.DeleteItem(ctx, RecordingType, m.ID, DeleteOptions{Actor: "ana", Recording: RecordingOptions{Actor: "ana", Audit: func(a RecordingAudit) { audits = append(audits, a) }}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.StoredRemoved || res.KnowledgeRemoved || !res.KnowledgeKept {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(src.factPath(m.ID)); err != nil {
		t.Fatalf("the Knowledge fact must stay: %v", err)
	}
	if _, ok := src.Store.Get(m.ID); ok {
		t.Fatal("recording metadata survived")
	}
	if _, ok := svc.transcriptStore().Get(RecordingType, m.ID); ok {
		t.Fatal("transcript record survived")
	}
	if len(audits) != 1 || audits[0].Action != "inbox_recording_delete" {
		t.Fatalf("audits = %+v", audits)
	}
}

func TestDeleteItemRefusedWhileTranscribing(t *testing.T) {
	e := newUploadEnv(t, config.InboxConfig{})
	ctx := context.Background()
	it, err := e.svc.AddUpload(ctx, UploadInput{Filename: "notes.md", Data: readUploadFixture(t, "notes.md"), AddedBy: "ana@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	e.svc.transcripts = NewTranscriptStore(filepath.Join(t.TempDir(), "transcripts"))
	if err := e.svc.transcripts.Put(TranscriptRecord{Source: UploadType, ID: it.ID, Status: TranscriptionRunning}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.DeleteItem(ctx, UploadType, it.ID, DeleteOptions{Actor: "ana"}); !errors.Is(err, ErrItemTranscribing) {
		t.Fatalf("delete while transcribing = %v, want ErrItemTranscribing", err)
	}
	if _, err := e.src.Store.Get(it.ID); err != nil {
		t.Fatalf("a refused delete must leave the upload: %v", err)
	}
	if err := e.src.Store.Delete("not-an-id"); !errors.Is(err, ErrUploadNotFound) {
		t.Fatalf("Delete of a malformed id = %v", err)
	}
}

func TestStoredLocallyAndKnowledgeRemovable(t *testing.T) {
	for src, want := range map[string]bool{RecordingType: true, UploadType: true, EmailType: true, LiveMeetingType: false, "plaud": false} {
		if StoredLocally(src) != want {
			t.Errorf("StoredLocally(%q) = %v", src, !want)
		}
	}
	if KnowledgeRemovable(Item{Source: UploadType}) || !KnowledgeRemovable(Item{Source: UploadType, FactPath: "/x.md"}) || KnowledgeRemovable(Item{Source: "plaud", FactPath: "/x.md"}) {
		t.Fatal("KnowledgeRemovable")
	}
}
