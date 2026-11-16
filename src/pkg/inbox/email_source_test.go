package inbox

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/mailin/wire"
	"github.com/hivecommons/hive/pkg/transcribe"
)

var (
	emailMD    = []byte("# Notes\n\nShip it.\n")
	emailPNG   = []byte("\x89PNG\r\n\x1a\nfake")
	emailAudio = []byte("ID3\x04fake-mp3")
)

func emailDelivery(msgID string, files ...wire.Attachment) wire.Delivery {
	return wire.Delivery{ID: "d-" + msgID, Kind: wire.KindFiles, Recipient: "hive@acme.example", MessageID: msgID,
		Auth: wire.Auth{From: "ada@example.com", Aligned: true}, Files: &wire.Files{Title: "Tuesday notes", Attachments: files}}
}

func att(name, kind, ct string, data []byte) wire.Attachment {
	return wire.Attachment{Name: name, Kind: kind, ContentType: ct, Size: len(data), Data: data}
}

func newEmailSource(t *testing.T) (*EmailSource, *time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	return &EmailSource{Dir: filepath.Join(t.TempDir(), "email", "items"), Now: func() time.Time { return now }}, &now
}

func TestEmailSourceAddListFetch(t *testing.T) {
	e, _ := newEmailSource(t)
	if e.Type() != EmailType || !e.Builtin() {
		t.Fatal("type")
	}
	if items, err := e.List(context.Background()); err != nil || len(items) != 0 {
		t.Fatalf("empty spool: %v %v", items, err)
	}
	d := emailDelivery("m1@example.com",
		att("notes.md", wire.AttachMarkdown, "text/markdown", emailMD),
		att("shot.png", wire.AttachImage, "image/png", emailPNG),
		att("memo.mp3", wire.AttachAudio, "audio/mpeg", emailAudio),
		att("empty.md", wire.AttachMarkdown, "text/markdown", nil))
	items, err := e.Add(d, true)
	if err != nil || len(items) != 3 {
		t.Fatalf("add: %d %v", len(items), err)
	}
	md := items[0]
	if md.Source != EmailType || md.AwaitingApproval || md.Sender != "ada@example.com" || md.Title != "Tuesday notes · notes.md" || !strings.Contains(md.Excerpt, "Ship it") || md.Pending {
		t.Fatalf("markdown item %+v", md)
	}
	if !items[2].Pending || items[2].UploadKind != wire.AttachAudio {
		t.Fatalf("audio item %+v", items[2])
	}
	// A retried delivery of the same mail lands on the same items.
	again, err := e.Add(d, false)
	if err != nil || len(again) != 3 || again[0].ID != md.ID || again[0].AwaitingApproval {
		t.Fatalf("retry: %+v %v", again, err)
	}
	listed, _ := e.List(context.Background())
	if len(listed) != 3 {
		t.Fatalf("listed %d", len(listed))
	}

	c, err := e.Fetch(context.Background(), md)
	if err != nil || !strings.Contains(c.Markdown, "Ship it") || !strings.Contains(c.Markdown, "DMARC aligned") || c.Provenance != "email://"+md.ID+"/notes.md" {
		t.Fatalf("markdown content %+v %v", c, err)
	}
	if c, _ := e.Fetch(context.Background(), items[1]); !strings.Contains(c.Markdown, "An image") {
		t.Fatalf("image content %+v", c)
	}
	if c, _ := e.Fetch(context.Background(), items[2]); !c.Pending {
		t.Fatalf("audio content %+v", c)
	}
	if _, err := e.Fetch(context.Background(), Item{ID: "../../etc"}); !errors.Is(err, ErrEmailNotFound) {
		t.Fatalf("bad id: %v", err)
	}
	if _, err := e.Fetch(context.Background(), Item{ID: strings.Repeat("a", emailIDHexLen)}); !errors.Is(err, ErrEmailNotFound) {
		t.Fatalf("missing id: %v", err)
	}

	// Files on disk are private.
	st, err := os.Stat(filepath.Join(e.Dir, md.ID, emailDataFile))
	if err != nil || st.Mode().Perm() != emailFileMode {
		t.Fatalf("data mode %v %v", st, err)
	}

	// Nothing to add.
	if out, err := e.Add(wire.Delivery{}, true); err != nil || out != nil {
		t.Fatalf("no files: %v %v", out, err)
	}
}

func TestEmailSourceApprovalAndDecline(t *testing.T) {
	e, now := newEmailSource(t)
	d := emailDelivery("m2", att("a.md", wire.AttachMarkdown, "text/markdown", emailMD))
	d.Files.Title = ""
	d.Auth = wire.Auth{From: "stranger@example.org"}
	items, err := e.Add(d, false)
	if err != nil || !items[0].AwaitingApproval || items[0].Title != "a.md" {
		t.Fatalf("untrusted add %+v %v", items, err)
	}
	if c, _ := e.Fetch(context.Background(), items[0]); !strings.Contains(c.Markdown, "not authenticated") {
		t.Fatalf("untrusted content %q", c.Markdown)
	}
	ok, err := e.Approve(items[0].ID, "owner")
	if err != nil || ok.AwaitingApproval {
		t.Fatalf("approve %+v %v", ok, err)
	}
	if again, err := e.Approve(items[0].ID, "owner"); err != nil || again.AwaitingApproval {
		t.Fatalf("approve twice %+v %v", again, err)
	}
	if _, err := e.Approve("nope", "owner"); !errors.Is(err, ErrEmailNotFound) {
		t.Fatalf("approve bad id: %v", err)
	}
	if _, err := e.Approve(strings.Repeat("b", emailIDHexLen), "owner"); !errors.Is(err, ErrEmailNotFound) {
		t.Fatalf("approve missing: %v", err)
	}

	// Declining deletes; unapproved items expire.
	d2 := emailDelivery("m3", att("b.png", wire.AttachImage, "image/png", emailPNG))
	pending, _ := e.Add(d2, false)
	if err := e.Decline(pending[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := e.Decline(pending[0].ID); !errors.Is(err, ErrEmailNotFound) {
		t.Fatalf("decline twice: %v", err)
	}
	if err := e.Decline("x"); !errors.Is(err, ErrEmailNotFound) {
		t.Fatalf("decline bad id: %v", err)
	}
	stale, _ := e.Add(emailDelivery("m4", att("c.png", wire.AttachImage, "image/png", emailPNG)), false)
	*now = now.Add(EmailPendingRetention + time.Hour)
	_, _ = e.Add(emailDelivery("m5", att("d.md", wire.AttachMarkdown, "text/markdown", emailMD)), true)
	if _, err := e.readMeta(stale[0].ID); !errors.Is(err, ErrEmailNotFound) {
		t.Fatalf("stale pending item kept: %v", err)
	}
	if _, err := e.readMeta(items[0].ID); err != nil {
		t.Fatalf("approved item pruned: %v", err)
	}
}

func TestEmailSourceQuotaAndOpen(t *testing.T) {
	e, _ := newEmailSource(t)
	e.QuotaBytes = int64(len(emailMD) + 1)
	if _, err := e.Add(emailDelivery("q1", att("a.md", wire.AttachMarkdown, "text/markdown", emailMD)), true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Add(emailDelivery("q2", att("b.md", wire.AttachMarkdown, "text/markdown", emailMD)), true); !errors.Is(err, ErrEmailQuota) {
		t.Fatalf("quota: %v", err)
	}
	if (&EmailSource{}).quota() != DefaultEmailQuotaBytes {
		t.Fatal("default quota")
	}

	e2, _ := newEmailSource(t)
	items, _ := e2.Add(emailDelivery("o1", att("v.webm", wire.AttachAudio, "audio/webm", emailAudio), att("p.png", wire.AttachImage, "image/png", emailPNG)), true)
	la, err := e2.OpenAudio(context.Background(), items[0])
	if err != nil || la.MIME != "audio/webm" || la.Size != int64(len(emailAudio)) || la.Name != "v.webm" {
		t.Fatalf("open audio %+v %v", la, err)
	}
	b, _ := io.ReadAll(la.File)
	_ = la.File.Close()
	if string(b) != string(emailAudio) {
		t.Fatal("audio bytes")
	}
	if _, err := e2.OpenAudio(context.Background(), items[1]); !errors.Is(err, ErrNoAudio) {
		t.Fatalf("image as audio: %v", err)
	}
	if _, err := e2.OpenAudio(context.Background(), Item{ID: "zz"}); err == nil {
		t.Fatal("bad id opened")
	}
	if _, err := e2.OpenAudio(context.Background(), Item{ID: strings.Repeat("d", emailIDHexLen)}); err == nil {
		t.Fatal("bad id opened")
	}
	if _, _, err := e2.Open(strings.Repeat("c", emailIDHexLen)); !errors.Is(err, ErrEmailNotFound) {
		t.Fatalf("open missing: %v", err)
	}
	// A wav keeps its MIME by extension.
	items, _ = e2.Add(emailDelivery("o2", att("r.wav", wire.AttachAudio, "audio/wave", emailAudio)), true)
	if la, _ := e2.OpenAudio(context.Background(), items[0]); la.MIME != "audio/wav" {
		t.Fatalf("wav mime %q", la.MIME)
	}
	// An unknown audio extension falls back to the sniffed type.
	items, _ = e2.Add(emailDelivery("o3", att("noext", wire.AttachAudio, "audio/ogg", emailAudio)), true)
	if la, _ := e2.OpenAudio(context.Background(), items[0]); la.MIME != "audio/ogg" {
		t.Fatalf("fallback mime %q", la.MIME)
	}
	// A corrupt meta file is skipped by List.
	if err := os.WriteFile(filepath.Join(e2.Dir, items[0].ID, emailMetaFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	listed, _ := e2.List(context.Background())
	for _, it := range listed {
		if it.ID == items[0].ID {
			t.Fatal("corrupt item listed")
		}
	}
}

func TestEmailItemsGatedInService(t *testing.T) {
	e, _ := newEmailSource(t)
	dest := &contentDest{}
	svc := NewService(config.InboxConfig{Rules: []config.InboxRule{{Source: EmailType, Tag: EmailType, Destination: DestDibs}}}, NewStore(filepath.Join(t.TempDir(), "inbox.json")), nil, []Destination{dest}, nil)
	svc.Run = func(fn func()) { fn() }
	svc.EnableEmail(e)
	if svc.Email() != e || !svc.HasSource(EmailType) {
		t.Fatal("email source not enabled")
	}
	svc.SetSources(nil)
	if !svc.HasSource(EmailType) {
		t.Fatal("SetSources dropped the email source")
	}
	pending, _ := e.Add(emailDelivery("g1", att("x.md", wire.AttachMarkdown, "text/markdown", emailMD)), false)
	ctx := context.Background()
	it, ok, _ := svc.Find(ctx, EmailType, pending[0].ID)
	if !ok || !it.AwaitingApproval {
		t.Fatalf("find %+v", it)
	}
	if _, err := svc.Route(ctx, it, Request{Destination: DestDibs}); !errors.Is(err, ErrAwaitingApproval) {
		t.Fatalf("route an unapproved item: %v", err)
	}
	if n, _ := svc.Sweep(ctx); n != 0 || svc.Store.Seen(it.Key()) {
		t.Fatalf("sweep routed or marked an unapproved item: %d", n)
	}
	if _, err := e.Approve(it.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if n, _ := svc.Sweep(ctx); n != 1 {
		t.Fatalf("approved item not auto-routed: %d", n)
	}
	svc.Wait()
	if !strings.Contains(dest.got.Markdown, "Ship it") {
		t.Fatalf("routed content %+v", dest.got)
	}
	// Dismissing ("Knowledge only") stays allowed while awaiting approval.
	other, _ := e.Add(emailDelivery("g2", att("y.md", wire.AttachMarkdown, "text/markdown", emailMD)), false)
	oit, _, _ := svc.Find(ctx, EmailType, other[0].ID)
	if _, err := svc.Route(ctx, oit, Request{Destination: DestDismiss}); err != nil {
		t.Fatalf("dismiss an unapproved item: %v", err)
	}
	var nilSvc Service
	if nilSvc.Email() != nil {
		t.Fatal("zero service has an email source")
	}
}

func TestEmailAudioTranscribesFromTheSpool(t *testing.T) {
	e, _ := newEmailSource(t)
	svc := NewService(config.InboxConfig{Transcription: activeTx()}, NewStore(filepath.Join(t.TempDir(), "inbox.json")), nil, nil, nil)
	svc.Run = func(fn func()) { fn() }
	store := NewTranscriptStore(filepath.Join(t.TempDir(), "transcripts"))
	svc.EnableTranscription(store)
	fake := &transcribe.Fake{Result: transcribe.Result{Segments: []transcribe.Segment{{Text: "hello from email"}}, Duration: time.Second}}
	svc.NewTranscriber = func(config.InboxTranscription) (transcribe.Transcriber, error) { return fake, nil }
	svc.TranscribeBackoff = func(int) time.Duration { return 0 }
	svc.EnableEmail(e)
	ctx := context.Background()

	pending, _ := e.Add(emailDelivery("a1", att("memo.m4a", wire.AttachAudio, "audio/mp4", emailAudio)), false)
	it, _, _ := svc.Find(ctx, EmailType, pending[0].ID)
	if it.Transcribable {
		t.Fatal("an unapproved recording must not be transcribable")
	}
	if _, err := e.Approve(it.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	it, _, _ = svc.Find(ctx, EmailType, it.ID)
	if !it.Transcribable {
		t.Fatalf("approved audio not transcribable: %+v", it)
	}
	if _, err := svc.Transcribe(ctx, it, TranscribeOptions{}); err != nil {
		t.Fatal(err)
	}
	rec, ok := store.Get(EmailType, it.ID)
	if !ok || rec.Status != TranscriptionDone || !strings.Contains(rec.Text, "hello from email") || fake.Bytes() != int64(len(emailAudio)) {
		t.Fatalf("transcript %+v", rec)
	}
	c, err := svc.Content(ctx, it)
	if err != nil || !strings.Contains(c.Transcript, "hello from email") {
		t.Fatalf("content %+v %v", c, err)
	}

	// Markdown has no audio.
	md, _ := e.Add(emailDelivery("a3", att("n.md", wire.AttachMarkdown, "text/markdown", emailMD)), true)
	mit, _, _ := svc.Find(ctx, EmailType, md[0].ID)
	if svc.hasAudio(mit) || mit.Transcribable {
		t.Fatal("markdown has no audio")
	}
}
