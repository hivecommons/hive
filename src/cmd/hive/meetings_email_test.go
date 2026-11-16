package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/mailin/wire"
	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/schedule"
)

func emailSchedule(t *testing.T) *meetingsSchedule {
	t.Helper()
	dir := t.TempDir()
	store, err := meetings.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := meetings.NewManager(meetings.Options{Store: store, Quorum: &stubQuorum{events: make(chan meetings.Event, 1)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	cfg := &config.Config{}
	cfg.Meetings.DataDir = dir
	svc, stop := wireMeetingsSchedule(cfg, mgr, nil)
	if svc == nil {
		t.Fatal("schedule not wired")
	}
	t.Cleanup(stop)
	return svc
}

func mailInvite(uid, organizer string, seq int, start time.Time) wire.Invite {
	return wire.Invite{
		UID: uid, Sequence: seq, Method: "REQUEST", Summary: "Planning",
		Organizer: &wire.Person{Email: organizer, Name: "Org"}, Attendees: []wire.Person{{Email: "hive@acme.example"}},
		Start: start, End: start.Add(30 * time.Minute), JoinURL: "https://zoom.us/j/123456789?pwd=abc",
		Platform: "zoom", NativeMeetingID: "123456789", Passcode: "abc",
	}
}

func mailDelivery(from string, aligned bool, invs ...wire.Invite) wire.Delivery {
	return wire.Delivery{ID: "d", Kind: wire.KindInvite, Auth: wire.Auth{From: from, Aligned: aligned}, Invites: invs}
}

func mailResults(t *testing.T, out any) []inviteResult {
	t.Helper()
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("result %T", out)
	}
	r, ok := m["results"].([]inviteResult)
	if !ok {
		t.Fatalf("results %T", m["results"])
	}
	return r
}

func TestIngestInvitesTrustAndApproval(t *testing.T) {
	svc := emailSchedule(t)
	ctx := context.Background()
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)

	out, err := svc.IngestInvites(ctx, mailDelivery("ada@example.com", true, mailInvite("u-trusted", "ada@example.com", 0, start)), true)
	if err != nil {
		t.Fatal(err)
	}
	if r := mailResults(t, out); r[0].Status != string(schedule.StatusScheduled) || r[0].EventID == "" {
		t.Fatalf("trusted invite: %+v", r)
	}
	ev, err := svc.sched.Get(schedule.EventID("u-trusted", nil))
	if err != nil || ev.Source != schedule.SourceEmail || ev.Passcode != "abc" || ev.Platform != "zoom" || ev.InvitedBy != "ada@example.com" || len(ev.Attendees) != 1 {
		t.Fatalf("stored %+v %v", ev, err)
	}

	out, _ = svc.IngestInvites(ctx, mailDelivery("stranger@example.org", true, mailInvite("u-outsider", "stranger@example.org", 0, start)), false)
	if r := mailResults(t, out); r[0].Status != string(schedule.StatusAwaitingApproval) {
		t.Fatalf("untrusted invite: %+v", r)
	}

	// The same SEQUENCE again: unchanged.
	out, _ = svc.IngestInvites(ctx, mailDelivery("ada@example.com", true, mailInvite("u-trusted", "ada@example.com", 0, start)), true)
	if r := mailResults(t, out); r[0].Status != inviteUnchanged {
		t.Fatalf("stale sequence: %+v", r)
	}
	// A higher SEQUENCE from the authenticated organizer moves it.
	out, _ = svc.IngestInvites(ctx, mailDelivery("Ada@example.com", true, mailInvite("u-trusted", "ada@example.com", 1, start.Add(time.Hour))), true)
	if r := mailResults(t, out); r[0].Status != string(schedule.StatusScheduled) {
		t.Fatalf("update: %+v", r)
	}
	if ev, _ := svc.sched.Get(schedule.EventID("u-trusted", nil)); !ev.Start.Equal(start.Add(time.Hour).UTC()) {
		t.Fatalf("not moved: %v", ev.Start)
	}
}

func TestIngestInvitesForgedChangesIgnored(t *testing.T) {
	svc := emailSchedule(t)
	ctx := context.Background()
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	if _, err := svc.IngestInvites(ctx, mailDelivery("ada@example.com", true, mailInvite("u1", "ada@example.com", 0, start)), true); err != nil {
		t.Fatal(err)
	}
	forged := mailInvite("u1", "ada@example.com", 5, start.Add(2*time.Hour))
	forged.JoinURL = "https://zoom.us/j/999999999"
	for name, d := range map[string]wire.Delivery{
		"unaligned organizer": mailDelivery("ada@example.com", false, forged),
		"other sender":        mailDelivery("mallory@example.net", true, forged),
	} {
		out, err := svc.IngestInvites(ctx, d, false)
		if err != nil {
			t.Fatal(err)
		}
		if r := mailResults(t, out); r[0].Status != inviteIgnoredNotOrganizer {
			t.Fatalf("%s: %+v", name, r)
		}
	}
	cancel := mailInvite("u1", "ada@example.com", 6, start)
	cancel.Method = "CANCEL"
	out, _ := svc.IngestInvites(ctx, mailDelivery("mallory@example.net", true, cancel), false)
	if r := mailResults(t, out); r[0].Status != inviteIgnoredNotOrganizer {
		t.Fatalf("forged cancel: %+v", r)
	}
	if ev, _ := svc.sched.Get(schedule.EventID("u1", nil)); ev.Status != schedule.StatusScheduled || ev.JoinURL != "https://zoom.us/j/123456789?pwd=abc" {
		t.Fatalf("forged mail changed the meeting: %+v", ev)
	}
	// The organizer's own cancel applies; a cancel for an unknown meeting
	// is ignored; an override cancel finds its series.
	out, _ = svc.IngestInvites(ctx, mailDelivery("ada@example.com", true, cancel), false)
	if r := mailResults(t, out); r[0].Status != string(schedule.StatusCancelled) {
		t.Fatalf("organizer cancel: %+v", r)
	}
	unknown := mailInvite("u-none", "ada@example.com", 1, start)
	unknown.Method = "CANCEL"
	out, _ = svc.IngestInvites(ctx, mailDelivery("ada@example.com", true, unknown), true)
	if r := mailResults(t, out); r[0].Status != inviteIgnoredNoMeeting {
		t.Fatalf("unknown cancel: %+v", r)
	}
	rid := start
	occ := mailInvite("u1", "ada@example.com", 7, start)
	occ.RecurrenceID = &rid
	out, _ = svc.IngestInvites(ctx, mailDelivery("mallory@example.net", true, occ), false)
	if r := mailResults(t, out); r[0].Status != inviteIgnoredNotOrganizer {
		t.Fatalf("override of a series by another sender: %+v", r)
	}
}

func TestIngestInvitesInvalid(t *testing.T) {
	svc := emailSchedule(t)
	bad := mailInvite("u-bad", "ada@example.com", 0, time.Now().Add(time.Hour))
	bad.JoinURL, bad.Platform = "https://evil.example/x", ""
	_, err := svc.IngestInvites(context.Background(), mailDelivery("ada@example.com", true, bad), true)
	if !errors.Is(err, dashboard.ErrMeetingInvalid) {
		t.Fatalf("all invalid: %v", err)
	}
	good := mailInvite("u-good", "ada@example.com", 0, time.Now().Add(time.Hour))
	out, err := svc.IngestInvites(context.Background(), mailDelivery("ada@example.com", true, bad, good), true)
	if err != nil {
		t.Fatal(err)
	}
	if r := mailResults(t, out); !strings.HasPrefix(r[0].Status, "invalid: ") || r[1].Status != string(schedule.StatusScheduled) {
		t.Fatalf("mixed: %+v", r)
	}
	if out, err := svc.IngestInvites(context.Background(), wire.Delivery{}, false); err != nil || len(mailResults(t, out)) != 0 {
		t.Fatalf("empty delivery: %v", err)
	}
}

func TestEventFromInviteWithoutOrganizer(t *testing.T) {
	ev := eventFromInvite(wire.Invite{UID: "x"}, wire.Delivery{Auth: wire.Auth{From: "from@example.com"}}, true)
	if ev.Organizer != nil || ev.InvitedBy != "from@example.com" || !ev.Trusted || ev.Source != schedule.SourceEmail {
		t.Fatalf("event %+v", ev)
	}
}

func TestWireMailAllowlist(t *testing.T) {
	base := t.TempDir()
	b := &boot{logger: slog.New(slog.DiscardHandler)}
	if st := b.wireMailAllowlist(base); st == nil {
		t.Fatal("store not opened")
	}
	if got := mailAllowlistPath(base); got != filepath.Join(base, "connector-state", "email", "allowlist.json") {
		t.Fatalf("path %q", got)
	}
	// A corrupt store is reported and left unwired.
	p := mailAllowlistPath(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := b.wireMailAllowlist(filepath.Dir(filepath.Dir(filepath.Dir(p)))); st != nil {
		t.Fatal("corrupt store opened")
	}
}
