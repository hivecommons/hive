package inbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// TestEmailRecentMailLog (hivecommons/hive#18699): the log keeps the last
// MaxMailEvents events, newest first, survives a restart, holds only short
// tokens, and is not mistaken for a spooled item.
func TestEmailRecentMailLog(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "items")
	now := time.Date(2026, 11, 17, 13, 5, 0, 0, time.UTC)
	e := &EmailSource{Dir: dir, Now: func() time.Time { return now }}
	if evs := e.RecentMail(); evs == nil || len(evs) != 0 {
		t.Fatalf("empty log: %#v", evs)
	}
	const extra = 5
	for i := 0; i < MaxMailEvents+extra; i++ {
		if err := e.RecordMail(MailEvent{Outcome: MailRejected, Reason: wire.ReasonUnsupported, Code: 550, Files: i}); err != nil {
			t.Fatal(err)
		}
	}
	evs := (&EmailSource{Dir: dir}).RecentMail()
	if len(evs) != MaxMailEvents || evs[0].Files != MaxMailEvents+extra-1 || evs[len(evs)-1].Files != extra || !evs[0].At.Equal(now) {
		t.Fatalf("%d events, first %+v, last %+v", len(evs), evs[0], evs[len(evs)-1])
	}

	// Free text is reduced to tokens and bounded.
	long := strings.Repeat("x", 400)
	skipped := make([]wire.SkippedFile, wire.MaxSkipped+extra)
	for i := range skipped {
		skipped[i] = wire.SkippedFile{Ext: "PpTx<script>", Reason: wire.SkipType}
	}
	at := now.Add(-time.Hour)
	if err := e.RecordMail(MailEvent{At: at, Outcome: "weird", Kind: "Files!", Reason: "Subject: secret " + long, SenderDomain: "Ada <ada@Example.COM>",
		Bytes: -1, Files: -1, Code: -1, Skipped: skipped}); err != nil {
		t.Fatal(err)
	}
	ev := e.RecentMail()[0]
	if ev.Outcome != MailRejected || ev.Kind != "files" || len(ev.Reason) != maxMailReason || strings.ContainsAny(ev.Reason, " :") ||
		ev.SenderDomain != "adaadaexample.com" || ev.Bytes != 0 || ev.Files != 0 || ev.Code != 0 || !ev.At.Equal(at) {
		t.Fatalf("cleaned event %+v", ev)
	}
	if len(ev.Skipped) != wire.MaxSkipped || ev.Skipped[0] != (wire.SkippedFile{Ext: "pptxscript", Reason: wire.SkipType}) {
		t.Fatalf("cleaned skipped %+v", ev.Skipped)
	}
	if got := SenderDomainOf("Ada@Mail.Example.com"); got != "mail.example.com" {
		t.Fatalf("SenderDomainOf: %q", got)
	}

	// The log file is not an item, and a damaged log reads as empty.
	if items, err := e.List(t.Context()); err != nil || len(items) != 0 {
		t.Fatalf("the log is listed as an item: %v %+v", err, items)
	}
	if err := os.WriteFile(filepath.Join(dir, emailLogFile), []byte("{not json"), emailFileMode); err != nil {
		t.Fatal(err)
	}
	if evs := e.RecentMail(); len(evs) != 0 {
		t.Fatalf("damaged log: %+v", evs)
	}
	if err := e.RecordMail(MailEvent{Outcome: MailAccepted, Kind: "files", Files: 1}); err != nil || len(e.RecentMail()) != 1 {
		t.Fatalf("record over a damaged log: %v", err)
	}

	// A spool directory that cannot be created is an error.
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, emailFileMode); err != nil {
		t.Fatal(err)
	}
	if err := (&EmailSource{Dir: filepath.Join(blocked, "items")}).RecordMail(MailEvent{Outcome: MailAccepted}); err == nil {
		t.Fatal("recorded into a blocked spool")
	}
}

// TestEmailMailEventItems (hivecommons/hive#18744): an event keeps only
// well-formed item ids, a bounded number of them, and PendingItems lists
// those still in the spool and still awaiting approval.
func TestEmailMailEventItems(t *testing.T) {
	e := &EmailSource{Dir: filepath.Join(t.TempDir(), "items")}
	md := wire.Attachment{Name: "n.md", Kind: wire.AttachMarkdown, ContentType: "text/markdown", Data: []byte("# hi")}
	add := func(messageID string) string {
		t.Helper()
		items, err := e.Add(wire.Delivery{Kind: wire.KindFiles, MessageID: messageID, Recipient: "hive@acme.example",
			Files: &wire.Files{Title: "notes", Attachments: []wire.Attachment{md}}}, false)
		if err != nil || len(items) != 1 {
			t.Fatalf("add: %v %+v", err, items)
		}
		return items[0].ID
	}
	kept, approved, declined := add("m-1"), add("m-2"), add("m-3")
	ids := []string{kept, "../../etc/passwd", approved, "<script>", declined}
	for i := 0; i < maxMailItems; i++ {
		ids = append(ids, kept)
	}
	if err := e.RecordMail(MailEvent{Outcome: MailAwaitingApproval, Items: ids}); err != nil {
		t.Fatal(err)
	}
	ev := e.RecentMail()[0]
	if len(ev.Items) != maxMailItems || ev.Items[0] != kept || ev.Items[1] != approved || ev.Items[2] != declined {
		t.Fatalf("items %v", ev.Items)
	}
	if _, err := e.Approve(approved, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := e.Decline(declined); err != nil {
		t.Fatal(err)
	}
	if got := e.PendingItems([]string{kept, approved, declined, "../x", strings.Repeat("a", len(kept))}); len(got) != 1 || got[0] != kept {
		t.Fatalf("pending %v", got)
	}
}
