package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/live"
)

func TestMeetingsInboxReader(t *testing.T) {
	store, err := meetings.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	q := &stubQuorum{events: make(chan meetings.Event)}
	mgr, err := meetings.NewManager(meetings.Options{Store: store, Quorum: q})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	ended := now.Add(time.Hour)
	save := func(m meetings.Meeting) {
		t.Helper()
		if err := store.Save(m); err != nil {
			t.Fatal(err)
		}
	}
	save(meetings.Meeting{ID: "mtg-done", NativeID: "q1", Platform: "google_meet", Title: "Standup", State: meetings.StateEnded, CreatedAt: now, LiveAt: &now, EndedAt: &ended})
	save(meetings.Meeting{ID: "mtg-live", State: meetings.StateLive, CreatedAt: now})
	save(meetings.Meeting{ID: "mtg-empty", State: meetings.StateEnded, CreatedAt: now, EndedAt: &ended})
	save(meetings.Meeting{ID: "mtg-notes", State: meetings.StateFailed, CreatedAt: now})
	save(meetings.Meeting{ID: "mtg-bad", State: meetings.StateEnded, CreatedAt: now})
	_ = store.AppendTranscript("mtg-done", meetings.Event{Type: meetings.EventSegment, Speaker: &meetings.Person{Name: "Ana"}, Text: "ship friday", Final: true})
	_ = store.AppendTranscript("mtg-done", meetings.Event{Type: meetings.EventChat, Author: &meetings.Person{Name: "Bo"}, Text: "agreed"})
	_ = store.AppendTranscript("mtg-done", meetings.Event{Type: meetings.EventChat, Text: "anon"})
	_ = store.SaveDoc("mtg-done", live.StateDoc, live.State{Notes: map[string][]string{live.SectionSummary: {"short"}, live.SectionDecisions: {"ship"}}})
	_ = store.SaveDoc("mtg-notes", live.StateDoc, live.State{Notes: map[string][]string{live.SectionNotes: {"n"}}})
	// A side document that does not decode makes the meeting unreadable.
	_ = os.WriteFile(filepath.Join(store.Dir(), "mtg-bad", live.StateDoc), []byte("{"), 0o600)

	mi := &meetingsInbox{mgr: mgr, store: store}
	list, err := mi.EndedMeetings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range list {
		ids[m.ID] = true
	}
	if len(list) != 2 || !ids["mtg-done"] || !ids["mtg-notes"] {
		t.Fatalf("ended meetings %+v", list)
	}
	got, err := mi.ReadMeeting(context.Background(), "mtg-done")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Ana: ship friday", "Bo (chat): agreed", "unknown (chat): anon"} {
		if !strings.Contains(got.Transcript, want) {
			t.Errorf("transcript missing %q:\n%s", want, got.Transcript)
		}
	}
	if got.Summary != "short" || !strings.Contains(got.Notes, "## Decisions") || !got.StartedAt.Equal(now) || !got.EndedAt.Equal(ended) {
		t.Fatalf("meeting %+v", got)
	}
	if !strings.HasPrefix(got.Notes, "Notes hive took in") {
		t.Fatalf("header without a persona name: %q", got.Notes)
	}
	// The notes header names the persona (hivecommons/hive#18393).
	named := &meetingsInbox{mgr: mgr, store: store, name: "Betty"}
	if b, err := named.ReadMeeting(context.Background(), "mtg-done"); err != nil || !strings.HasPrefix(b.Notes, "Notes Betty took in") {
		t.Fatalf("persona header %q, %v", b.Notes, err)
	}
	if _, err := mi.ReadMeeting(context.Background(), "mtg-none"); err == nil {
		t.Fatal("want not found")
	}
	if err := mi.HandOff(context.Background(), "mtg-done"); err != nil {
		t.Fatal(err)
	}
	if m, _ := mgr.Get("mtg-done"); m.State != meetings.StateHandedOff {
		t.Fatalf("state %s", m.State)
	}
	// A transcript that cannot be read hides the meeting.
	_ = os.MkdirAll(filepath.Join(store.Dir(), "mtg-notes", "transcript.jsonl", "x"), 0o700)
	if _, err := mi.ReadMeeting(context.Background(), "mtg-notes"); err == nil {
		t.Fatal("want a transcript read error")
	}
	_ = os.RemoveAll(store.Dir())
	if _, err := mi.EndedMeetings(context.Background()); err == nil {
		t.Fatal("want a list error")
	}
}

func TestWireMeetingsInbox(t *testing.T) {
	store, _ := meetings.NewStore(t.TempDir())
	mgr, _ := meetings.NewManager(meetings.Options{Store: store, Quorum: &stubQuorum{events: make(chan meetings.Event)}})
	defer mgr.Close()
	b := &boot{cfg: &config.Config{}}
	b.wireMeetingsInbox(mgr, store)
	if b.meetingsInboxSrc == nil {
		t.Fatal("inbox source not set")
	}
	if mi, ok := b.meetingsInboxSrc.Reader.(*meetingsInbox); !ok || mi.name != "Betty" {
		t.Fatalf("inbox reader %+v", b.meetingsInboxSrc.Reader)
	}
	off := false
	cfg := config.MeetingsPersonaConfig{Enabled: &off}
	if meetingsAttendeeName(cfg.Resolve()) != "hive" || meetingsAttendeeName(config.MeetingsPersonaConfig{}.Resolve()) != "Betty" {
		t.Fatal("meetingsAttendeeName")
	}
	srcs := b.inboxSources(t.TempDir(), nil, nil)
	if len(srcs) != 1 || srcs[0].Type() != "meeting" {
		t.Fatalf("sources %+v", srcs)
	}
}
