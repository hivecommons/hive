package schedule

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/meetings"
)

// clock is a settable fake clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// fakeJoiner records joins and serves meetings by id.
type fakeJoiner struct {
	mu       sync.Mutex
	joins    []meetings.JoinRequest
	meetings map[string]meetings.Meeting
	err      error
	state    meetings.State
	n        int
	// during runs inside Join (e.g. to simulate a crash window).
	during func()
}

func (f *fakeJoiner) Join(_ context.Context, req meetings.JoinRequest) (meetings.Meeting, error) {
	if f.during != nil {
		f.during()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.joins = append(f.joins, req)
	f.n++
	if f.meetings == nil {
		f.meetings = map[string]meetings.Meeting{}
	}
	id := "mtg-" + string(rune('a'+f.n-1))
	m := meetings.Meeting{ID: id, Platform: req.Platform, URL: req.URL, Title: req.Title, State: meetings.StateJoining}
	if f.state != "" {
		m.State = f.state
	}
	if f.err != nil {
		m.State, m.StateReason = meetings.StateFailed, f.err.Error()
		f.meetings[id] = m
		return m, f.err
	}
	f.meetings[id] = m
	return m, nil
}

func (f *fakeJoiner) Get(id string) (meetings.Meeting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.meetings[id]
	if !ok {
		return meetings.Meeting{}, meetings.ErrNotFound
	}
	return m, nil
}

func (f *fakeJoiner) set(id string, st meetings.State) meetings.Meeting {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.meetings[id]
	m.State = st
	f.meetings[id] = m
	return m
}

func (f *fakeJoiner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.joins)
}

var t0 = time.Date(2026, 11, 16, 15, 0, 0, 0, time.UTC)

func newTestScheduler(t *testing.T, dir string, c *clock, j *fakeJoiner) *Scheduler {
	t.Helper()
	st, err := NewStore(dir, c.Now)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Store: st, Joiner: j, Now: c.Now, Lead: DefaultLead})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func ownerAdd(t *testing.T, s *Scheduler, req ManualRequest) Event {
	t.Helper()
	req.ByOwner = true
	if req.InvitedBy == "" {
		req.InvitedBy = "octo"
	}
	if req.URL == "" {
		req.URL = "https://meet.google.com/abc-defg-hij"
	}
	ev, err := s.AddManual(req)
	if err != nil {
		t.Fatalf("AddManual: %v", err)
	}
	return ev
}

func TestLeadTimeJoin(t *testing.T) {
	c := &clock{t: t0}
	j := &fakeJoiner{}
	s := newTestScheduler(t, t.TempDir(), c, j)
	start := t0.Add(10 * time.Minute)
	ev := ownerAdd(t, s, ManualRequest{Title: "Weekly sync", Start: start, End: start.Add(30 * time.Minute)})
	if ev.Status != StatusScheduled || ev.Platform != PlatformGoogleMeet || ev.Source != SourceManual {
		t.Fatalf("added = %+v", ev)
	}
	if len(ev.NextOccurrences) != 1 || !ev.NextOccurrences[0].Equal(start) {
		t.Fatalf("next = %v", ev.NextOccurrences)
	}
	if n := s.Tick(context.Background()); n != 0 || j.count() != 0 {
		t.Fatalf("joined too early: %d", n)
	}
	c.Set(start.Add(-61 * time.Second))
	if s.Tick(context.Background()); j.count() != 0 {
		t.Fatal("joined before the lead time")
	}
	c.Set(start.Add(-DefaultLead))
	if n := s.Tick(context.Background()); n != 1 || j.count() != 1 {
		t.Fatalf("lead-time join = %d", n)
	}
	if j.joins[0].URL != "https://meet.google.com/abc-defg-hij" || j.joins[0].Title != "Weekly sync" || j.joins[0].InvitedBy != "octo" {
		t.Fatalf("join request = %+v", j.joins[0])
	}
	got, _ := s.Get(ev.ID)
	if got.Status != StatusLive || got.MeetingID != "mtg-a" || len(got.History) != 1 || got.History[0].MeetingID != "mtg-a" {
		t.Fatalf("after join = %+v", got)
	}
	// Further ticks do not join again.
	c.Set(start.Add(time.Minute))
	s.Tick(context.Background())
	if j.count() != 1 {
		t.Fatalf("double join: %d", j.count())
	}
	// The meeting goes live, then ends.
	s.OnMeeting(j.set("mtg-a", meetings.StateLive))
	if got, _ := s.Get(ev.ID); got.Status != StatusLive {
		t.Fatalf("live = %s", got.Status)
	}
	s.OnMeeting(j.set("mtg-a", meetings.StateEnded))
	got, _ = s.Get(ev.ID)
	if got.Status != StatusEnded || got.History[0].Status != StatusEnded {
		t.Fatalf("ended = %+v", got)
	}
	// Unknown meetings are ignored.
	s.OnMeeting(meetings.Meeting{ID: "other", State: meetings.StateEnded})
	s.OnMeeting(meetings.Meeting{})
}

func TestStoreReloadNoDoubleJoin(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	j := &fakeJoiner{}
	s := newTestScheduler(t, dir, c, j)
	start := t0.Add(30 * time.Second)
	ev := ownerAdd(t, s, ManualRequest{Start: start, End: start.Add(time.Hour)})
	if s.Tick(context.Background()); j.count() != 1 {
		t.Fatalf("joins = %d", j.count())
	}
	// Restart: a fresh store and scheduler over the same directory.
	s2 := newTestScheduler(t, dir, c, j)
	got, err := s2.Get(ev.ID)
	if err != nil || got.Status != StatusLive || got.MeetingID != "mtg-a" {
		t.Fatalf("reloaded = %+v %v", got, err)
	}
	c.Set(start.Add(5 * time.Minute))
	s2.Tick(context.Background())
	if j.count() != 1 {
		t.Fatalf("restart double-joined: %d joins", j.count())
	}
	// The reconcile pass picks up the meeting's state.
	j.set("mtg-a", meetings.StateEnded)
	s2.Tick(context.Background())
	if got, _ := s2.Get(ev.ID); got.Status != StatusEnded {
		t.Fatalf("reconciled = %s", got.Status)
	}
}

func TestRestartMidJoinNeverRejoins(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	j := &fakeJoiner{}
	s := newTestScheduler(t, dir, c, j)
	start := t0.Add(30 * time.Second)
	ev := ownerAdd(t, s, ManualRequest{Start: start})
	// Simulate a crash after the claim was persisted but before the join
	// returned: snapshot the file during Join.
	var snapshot []byte
	j.during = func() {
		snapshot, _ = os.ReadFile(filepath.Join(dir, FileName))
	}
	s.Tick(context.Background())
	if err := os.WriteFile(filepath.Join(dir, FileName), snapshot, 0o600); err != nil {
		t.Fatal(err)
	}
	j.during = nil
	s2 := newTestScheduler(t, dir, c, j)
	if got, _ := s2.Get(ev.ID); got.Status != StatusLive || got.MeetingID != "" {
		t.Fatalf("claimed = %+v", got)
	}
	s2.Tick(context.Background())
	if j.count() != 1 {
		t.Fatalf("rejoined after restart: %d", j.count())
	}
	got, _ := s2.Get(ev.ID)
	if got.Status != StatusFailed || got.StatusReason != ReasonInterrupted || got.History[0].Status != StatusFailed {
		t.Fatalf("interrupted = %+v", got)
	}
}

func TestWeeklyRecurrence(t *testing.T) {
	c := &clock{t: t0}
	j := &fakeJoiner{}
	s := newTestScheduler(t, t.TempDir(), c, j)
	start := t0.Add(time.Hour) // a Monday, 16:00 UTC
	ev := ownerAdd(t, s, ManualRequest{Title: "Standup", Start: start, End: start.Add(15 * time.Minute), RRule: "RRULE:FREQ=WEEKLY;COUNT=3"})
	if !ev.Recurring() || len(ev.NextOccurrences) != 3 || !ev.NextOccurrences[1].Equal(start.AddDate(0, 0, 7)) {
		t.Fatalf("series = %+v", ev)
	}
	for week := 0; week < 3; week++ {
		occ := start.AddDate(0, 0, 7*week)
		c.Set(occ.Add(-30 * time.Second))
		if n := s.Tick(context.Background()); n != 1 {
			t.Fatalf("week %d joins = %d", week, n)
		}
		got, _ := s.Get(ev.ID)
		if got.Status != StatusLive || got.JoinedOccurrence == nil || !got.JoinedOccurrence.Equal(occ) {
			t.Fatalf("week %d = %+v", week, got)
		}
		s.OnMeeting(j.set(got.MeetingID, meetings.StateEnded))
		got, _ = s.Get(ev.ID)
		want := StatusScheduled
		if week == 2 {
			want = StatusEnded
		}
		if got.Status != want {
			t.Fatalf("week %d after end = %s, want %s", week, got.Status, want)
		}
	}
	if j.count() != 3 {
		t.Fatalf("joins = %d", j.count())
	}
	got, _ := s.Get(ev.ID)
	if len(got.History) != 3 || len(got.NextOccurrences) != 0 {
		t.Fatalf("history = %+v next = %v", got.History, got.NextOccurrences)
	}
}

func TestRecurrenceKeepsLocalTimeAcrossDST(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := newTestScheduler(t, t.TempDir(), c, &fakeJoiner{})
	// 09:00 New York on a Tuesday before the November DST change.
	start := time.Date(2026, 10, 27, 13, 0, 0, 0, time.UTC)
	ev := ownerAdd(t, s, ManualRequest{Start: start, RRule: "FREQ=WEEKLY", TZID: "America/New_York"})
	if len(ev.NextOccurrences) != 3 {
		t.Fatalf("next = %v", ev.NextOccurrences)
	}
	after := ev.NextOccurrences[1]
	if after.Hour() != 14 {
		t.Fatalf("after DST the meeting should stay 09:00 local (14:00 UTC), got %v", after)
	}
}

func TestSkipOccurrence(t *testing.T) {
	c := &clock{t: t0}
	j := &fakeJoiner{}
	s := newTestScheduler(t, t.TempDir(), c, j)
	start := t0.Add(time.Hour)
	ev := ownerAdd(t, s, ManualRequest{Start: start, RRule: "FREQ=DAILY"})
	skipped, err := s.Skip(ev.ID, "octo")
	if err != nil {
		t.Fatal(err)
	}
	if skipped.Status != StatusScheduled || !skipped.NextOccurrences[0].Equal(start.AddDate(0, 0, 1)) {
		t.Fatalf("after skip = %+v", skipped)
	}
	c.Set(start.Add(-time.Second))
	if s.Tick(context.Background()); j.count() != 0 {
		t.Fatal("joined a skipped occurrence")
	}
	c.Set(start.AddDate(0, 0, 1).Add(-time.Second))
	if s.Tick(context.Background()); j.count() != 1 {
		t.Fatal("did not join the next occurrence")
	}

	// A single meeting becomes skipped.
	one := ownerAdd(t, s, ManualRequest{Start: start.AddDate(0, 0, 3)})
	got, err := s.Skip(one.ID, "octo")
	if err != nil || got.Status != StatusSkipped {
		t.Fatalf("skip single = %+v %v", got, err)
	}
	if _, err := s.Skip(one.ID, "octo"); !errors.Is(err, ErrConflict) {
		t.Fatalf("skip twice = %v", err)
	}
	if _, err := s.Skip("nope", "octo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("skip unknown = %v", err)
	}
}

func TestApproveDeclineCancel(t *testing.T) {
	c := &clock{t: t0}
	j := &fakeJoiner{}
	s := newTestScheduler(t, t.TempDir(), c, j)
	start := t0.Add(time.Minute)
	// A read-write user's manual add waits for the owner.
	ev, err := s.AddManual(ManualRequest{URL: "https://acme.zoom.us/j/123?pwd=x", Start: start, InvitedBy: "dev"})
	if err != nil || ev.Status != StatusAwaitingApproval || ev.Platform != PlatformZoom {
		t.Fatalf("add = %+v %v", ev, err)
	}
	c.Set(start)
	if s.Tick(context.Background()); j.count() != 0 {
		t.Fatal("joined without approval")
	}
	got, err := s.Approve(ev.ID, "octo")
	if err != nil || got.Status != StatusScheduled || got.ApprovedBy != "octo" {
		t.Fatalf("approve = %+v %v", got, err)
	}
	if _, err := s.Approve(ev.ID, "octo"); !errors.Is(err, ErrConflict) {
		t.Fatalf("approve twice = %v", err)
	}
	if s.Tick(context.Background()); j.count() != 1 || j.joins[0].InvitedBy != "dev" {
		t.Fatalf("approved join = %+v", j.joins)
	}

	other, _ := s.AddManual(ManualRequest{URL: "https://teams.microsoft.com/l/meetup-join/x", Start: start.Add(time.Hour), InvitedBy: "dev"})
	if other.Platform != PlatformTeams {
		t.Fatalf("platform = %s", other.Platform)
	}
	got, err = s.Decline(other.ID, "octo")
	if err != nil || got.Status != StatusDeclined {
		t.Fatalf("decline = %+v %v", got, err)
	}
	if _, err := s.Decline(other.ID, "octo"); !errors.Is(err, ErrConflict) {
		t.Fatalf("decline twice = %v", err)
	}
	if _, err := s.Approve(other.ID, "octo"); !errors.Is(err, ErrConflict) {
		t.Fatalf("approve declined = %v", err)
	}

	third := ownerAdd(t, s, ManualRequest{Start: start.Add(2 * time.Hour)})
	got, err = s.Cancel(third.ID, "octo")
	if err != nil || got.Status != StatusCancelled || !strings.Contains(got.StatusReason, "octo") {
		t.Fatalf("cancel = %+v %v", got, err)
	}
	if _, err := s.Cancel(third.ID, "octo"); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancel twice = %v", err)
	}
	c.Set(start.Add(2 * time.Hour))
	if s.Tick(context.Background()); j.count() != 1 {
		t.Fatal("joined a cancelled meeting")
	}
}

func TestIngestSequenceAndCancel(t *testing.T) {
	c := &clock{t: t0}
	j := &fakeJoiner{}
	s := newTestScheduler(t, t.TempDir(), c, j)
	start := t0.Add(2 * time.Hour)
	inv := Event{
		UID: "abc@google.com", Sequence: 0, Method: MethodRequest, Summary: "Design review",
		Organizer: &Person{Email: "alice@example.com", Name: "Alice"}, Attendees: []Person{{Email: "hive@spoke.example"}},
		Start: start, End: start.Add(time.Hour), JoinURL: "https://meet.google.com/xyz", Source: SourceEmail,
	}
	ev, err := s.Ingest(inv)
	if err != nil || ev.Status != StatusAwaitingApproval || ev.Organizer.Email != "alice@example.com" {
		t.Fatalf("ingest = %+v %v", ev, err)
	}
	if _, err := s.Approve(ev.ID, "octo"); err != nil {
		t.Fatal(err)
	}
	// Equal sequence: ignored.
	same := inv
	same.Summary = "changed"
	if got, err := s.Ingest(same); !errors.Is(err, ErrStaleSequence) || got.Summary != "Design review" {
		t.Fatalf("equal sequence = %+v %v", got, err)
	}
	// Higher sequence from the same organizer replaces and keeps the approval.
	moved := inv
	moved.Sequence, moved.Start, moved.End = 1, start.Add(time.Hour), start.Add(2*time.Hour)
	got, err := s.Ingest(moved)
	if err != nil || !got.Start.Equal(start.Add(time.Hour)) || got.Status != StatusScheduled || got.ApprovedBy != "octo" {
		t.Fatalf("higher sequence = %+v %v", got, err)
	}
	// Lower sequence: ignored.
	if got, err := s.Ingest(inv); !errors.Is(err, ErrStaleSequence) || got.Sequence != 1 {
		t.Fatalf("lower sequence = %+v %v", got, err)
	}
	// CANCEL with a lower sequence is ignored; with the current one cancels.
	if _, err := s.Ingest(Event{UID: inv.UID, Method: MethodCancel, Sequence: 0}); !errors.Is(err, ErrStaleSequence) {
		t.Fatalf("stale cancel = %v", err)
	}
	got, err = s.Ingest(Event{UID: inv.UID, Method: MethodCancel, Sequence: 1})
	if err != nil || got.Status != StatusCancelled || got.StatusReason != ReasonOrganizerCancel {
		t.Fatalf("cancel = %+v %v", got, err)
	}
	if _, err := s.Ingest(Event{UID: "unknown", Method: MethodCancel}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel unknown = %v", err)
	}
	// A declined event stays declined through updates.
	d, _ := s.Ingest(Event{UID: "d@x", Start: start, JoinURL: "https://meet.google.com/d", Source: SourceEmail})
	if _, err := s.Decline(d.ID, "octo"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Ingest(Event{UID: "d@x", Sequence: 2, Start: start, JoinURL: "https://meet.google.com/d", Source: SourceEmail}); got.Status != StatusDeclined {
		t.Fatalf("declined update = %s", got.Status)
	}
}

func TestIngestRecurrenceOverrides(t *testing.T) {
	c := &clock{t: t0}
	j := &fakeJoiner{}
	s := newTestScheduler(t, t.TempDir(), c, j)
	start := t0.Add(time.Hour)
	series, err := s.Ingest(Event{UID: "s@x", Start: start, End: start.Add(time.Hour), RRule: "FREQ=DAILY;COUNT=5",
		ExDates: []time.Time{start.AddDate(0, 0, 1)}, JoinURL: "https://meet.google.com/s", Source: SourceEmail})
	if err != nil {
		t.Fatal(err)
	}
	if len(series.NextOccurrences) != 3 || !series.NextOccurrences[1].Equal(start.AddDate(0, 0, 2)) {
		t.Fatalf("exdate not applied: %v", series.NextOccurrences)
	}
	// An override of day 2 moves it; the series no longer lists day 2.
	rid := start.AddDate(0, 0, 2)
	ov, err := s.Ingest(Event{UID: "s@x", RecurrenceID: &rid, Start: rid.Add(30 * time.Minute), JoinURL: "https://meet.google.com/s", Source: SourceEmail})
	if err != nil || ov.ID == series.ID {
		t.Fatalf("override = %+v %v", ov, err)
	}
	got, _ := s.Get(series.ID)
	for _, n := range got.NextOccurrences {
		if n.Equal(rid) {
			t.Fatalf("overridden occurrence still listed: %v", got.NextOccurrences)
		}
	}
	// Cancelling one occurrence without its own override adds an EXDATE.
	day3 := start.AddDate(0, 0, 3)
	if _, err := s.Ingest(Event{UID: "s@x", RecurrenceID: &day3, Method: MethodCancel}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(series.ID)
	if !containsTime(got.ExDates, day3) {
		t.Fatalf("cancelled occurrence not excluded: %v", got.ExDates)
	}
	// Cancelling the series cancels the override too.
	if _, err := s.Ingest(Event{UID: "s@x", Method: MethodCancel}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ov.ID); got.Status != StatusCancelled {
		t.Fatalf("override after series cancel = %s", got.Status)
	}
}

func TestNormalizeRejects(t *testing.T) {
	cases := map[string]Event{
		"no uid":        {Start: t0, JoinURL: "https://meet.google.com/x"},
		"no start":      {UID: "u", JoinURL: "https://meet.google.com/x"},
		"end<start":     {UID: "u", Start: t0, End: t0.Add(-time.Minute), JoinURL: "https://meet.google.com/x"},
		"bad url":       {UID: "u", Start: t0, JoinURL: "http://meet.google.com/x"},
		"unknown host":  {UID: "u", Start: t0, JoinURL: "https://example.com/x"},
		"no url":        {UID: "u", Start: t0},
		"meet no url":   {UID: "u", Start: t0, Platform: PlatformZoom},
		"bad platform":  {UID: "u", Start: t0, Platform: "webex", JoinURL: "https://meet.google.com/x"},
		"discord bare":  {UID: "u", Start: t0, Platform: PlatformDiscord},
		"bad source":    {UID: "u", Start: t0, JoinURL: "https://meet.google.com/x", Source: "fax"},
		"bad tz":        {UID: "u", Start: t0, JoinURL: "https://meet.google.com/x", TZID: "Mars/Olympus"},
		"bad rrule":     {UID: "u", Start: t0, JoinURL: "https://meet.google.com/x", RRule: "FREQ=SOMETIMES"},
		"hourly rrule":  {UID: "u", Start: t0, JoinURL: "https://meet.google.com/x", RRule: "FREQ=HOURLY"},
		"invalid rrule": {UID: "u", Start: t0, JoinURL: "https://meet.google.com/x", RRule: "FREQ=DAILY;BYHOUR=99"},
		"intraday":      {UID: "u", Start: t0, JoinURL: "https://meet.google.com/x", RRule: "FREQ=DAILY;BYHOUR=9,10"},
	}
	for name, ev := range cases {
		if _, err := normalize(ev); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	long := strings.Repeat("é", MaxSummaryRunes+50)
	ev, err := normalize(Event{UID: " u ", Start: t0, JoinURL: "https://discord.com/channels/1/2", Summary: long,
		Organizer: &Person{Email: "a@b"}, Attendees: make([]Person, maxAttendees+5), Sequence: -3})
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(ev.Summary)) != MaxSummaryRunes || ev.Platform != PlatformDiscord || ev.GuildID != "1" || ev.ChannelID != "2" ||
		ev.Source != SourceManual || len(ev.Attendees) != maxAttendees || ev.Sequence != 0 || ev.UID != "u" {
		t.Fatalf("normalized = %+v", ev)
	}
	req := ev.joinRequest()
	if req.Platform != PlatformDiscord || req.GuildID != "1" || req.URL != "" {
		t.Fatalf("discord join request = %+v", req)
	}
}

func TestDetectPlatform(t *testing.T) {
	cases := map[string]string{
		"https://meet.google.com/abc-defg-hij":        PlatformGoogleMeet,
		"https://teams.microsoft.com/l/meetup-join/x": PlatformTeams,
		"https://teams.live.com/meet/123":             PlatformTeams,
		"https://us02web.zoom.us/j/123?pwd=abc":       PlatformZoom,
		"https://zoom.us/j/1":                         PlatformZoom,
		"https://agency.zoomgov.com/j/1":              PlatformZoom,
		"https://discord.com/channels/1/2":            PlatformDiscord,
		"http://meet.google.com/x":                    "",
		"https://evil.example/meet.google.com":        "",
		"https://meet.google.com.evil.example/abc":    "",
		"::":                     "",
		"https://notzoom.us/j/1": "",
	}
	for link, want := range cases {
		if got := DetectPlatform(link); got != want {
			t.Errorf("DetectPlatform(%q) = %q, want %q", link, got, want)
		}
	}
	if g, c := discordChannel("https://discord.com/channels/9"); g != "" || c != "" {
		t.Fatalf("short discord path = %q %q", g, c)
	}
	if g, _ := discordChannel("::"); g != "" {
		t.Fatal("bad url")
	}
}

func TestJoinFailureAndBlocked(t *testing.T) {
	c := &clock{t: t0}
	j := &fakeJoiner{err: errors.New("quorum down")}
	s := newTestScheduler(t, t.TempDir(), c, j)
	start := t0.Add(30 * time.Second)
	one := ownerAdd(t, s, ManualRequest{Start: start})
	series := ownerAdd(t, s, ManualRequest{Start: start, RRule: "FREQ=DAILY"})
	if n := s.Tick(context.Background()); n != 0 || j.count() != 2 {
		t.Fatalf("failed joins = %d, attempts %d", n, j.count())
	}
	got, _ := s.Get(one.ID)
	if got.Status != StatusFailed || got.StatusReason != "quorum down" || got.History[0].Status != StatusFailed {
		t.Fatalf("failed single = %+v", got)
	}
	got, _ = s.Get(series.ID)
	if got.Status != StatusScheduled || got.History[0].Status != StatusFailed {
		t.Fatalf("failed series = %+v", got)
	}
	// Retries never hit the same occurrence.
	s.Tick(context.Background())
	if j.count() != 2 {
		t.Fatalf("retried a failed occurrence: %d", j.count())
	}
	if statusOf(meetings.StateBlocked) != StatusBlocked || statusOf(meetings.StateHandedOff) != StatusEnded || statusOf(meetings.StateLobby) != StatusLive {
		t.Fatal("statusOf mapping")
	}
}

func TestMissedAndRetired(t *testing.T) {
	c := &clock{t: t0}
	j := &fakeJoiner{}
	s := newTestScheduler(t, t.TempDir(), c, j)
	start := t0.Add(time.Minute)
	ev := ownerAdd(t, s, ManualRequest{Start: start})
	pending, _ := s.AddManual(ManualRequest{URL: "https://meet.google.com/p", Start: start, InvitedBy: "dev"})
	// hive was down through the whole late-join window.
	c.Set(start.Add(DefaultLateJoin + time.Minute))
	if s.Tick(context.Background()); j.count() != 0 {
		t.Fatal("joined a meeting that is over")
	}
	for _, id := range []string{ev.ID, pending.ID} {
		if got, _ := s.Get(id); got.Status != StatusSkipped || got.StatusReason != ReasonMissed {
			t.Fatalf("missed = %+v", got)
		}
	}
	if _, err := s.AddManual(ManualRequest{URL: "https://meet.google.com/p", Start: t0.Add(-time.Hour), ByOwner: true}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("past meeting = %v", err)
	}
	if _, err := s.AddManual(ManualRequest{Start: start}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no url = %v", err)
	}
	// Finished events are pruned after the retention period.
	c.Set(c.Now().Add(DefaultRetention + time.Hour))
	s.Tick(context.Background())
	if len(s.List()) != 0 {
		t.Fatalf("not pruned: %d", len(s.List()))
	}
}

func TestReconcileRemovedMeeting(t *testing.T) {
	c := &clock{t: t0}
	j := &fakeJoiner{}
	s := newTestScheduler(t, t.TempDir(), c, j)
	ev := ownerAdd(t, s, ManualRequest{Start: t0.Add(time.Second)})
	s.Tick(context.Background())
	j.mu.Lock()
	delete(j.meetings, "mtg-a")
	j.mu.Unlock()
	s.Tick(context.Background())
	if got, _ := s.Get(ev.ID); got.Status != StatusEnded {
		t.Fatalf("removed meeting = %s", got.Status)
	}
}

func TestStartCloseAndOptions(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New without store")
	}
	c := &clock{t: t0}
	j := &fakeJoiner{}
	st, err := NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Store: st, Joiner: j, Lead: -1, Tick: time.Millisecond, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	if s.opts.Lead != DefaultLead || s.opts.LateJoin != DefaultLateJoin || s.opts.Retention != DefaultRetention {
		t.Fatalf("defaults = %+v", s.opts)
	}
	s.Start()
	s.Start()
	ownerAdd(t, s, ManualRequest{Start: t0.Add(time.Second)})
	testutil.Eventually(t, 5*time.Second, func() bool { return j.count() > 0 }, "the scheduler loop never joined")
	s.Close()
	s.Close()
	if j.count() != 1 {
		t.Fatalf("loop joins = %d", j.count())
	}
	// A cancelled context stops a tick early.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ownerAdd(t, s, ManualRequest{Start: t0.Add(2 * time.Second)})
	if n := s.Tick(ctx); n != 0 {
		t.Fatalf("cancelled tick joined %d", n)
	}
}

func TestDurationsFromEnv(t *testing.T) {
	env := map[string]string{EnvTickS: "5", EnvLeadS: "0"}
	tick, lead := DurationsFromEnv(func(k string) string { return env[k] })
	if tick != 5*time.Second || lead != 0 {
		t.Fatalf("env = %v %v", tick, lead)
	}
	env = map[string]string{EnvTickS: "x", EnvLeadS: "-1"}
	if tick, lead := DurationsFromEnv(func(k string) string { return env[k] }); tick != DefaultTick || lead != DefaultLead {
		t.Fatalf("defaults = %v %v", tick, lead)
	}
}

func TestStoreFileErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(dir, nil); err == nil {
		t.Fatal("corrupt schedule accepted")
	}
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(filepath.Join(file, "sub"), nil); err == nil {
		t.Fatal("store under a file")
	}
	// A directory where the file should be cannot be read.
	d2 := t.TempDir()
	if err := os.Mkdir(filepath.Join(d2, FileName), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(d2, nil); err == nil {
		t.Fatal("unreadable schedule accepted")
	}
	// Records without a UID are dropped on load.
	d3 := t.TempDir()
	if err := os.WriteFile(filepath.Join(d3, FileName), []byte(`{"version":1,"events":[{"uid":""},{"uid":"a","status":"scheduled"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := NewStore(d3, nil)
	if err != nil || len(st.List()) != 1 || st.List()[0].ID != EventID("a", nil) || st.Path() != filepath.Join(d3, FileName) {
		t.Fatalf("load = %v %v", st.List(), err)
	}
	if _, err := st.Update("missing", func(*Event) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing = %v", err)
	}
	// A write that fails rolls the change back.
	if err := os.Chmod(d3, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(d3, 0o700) }()
	if os.Getuid() != 0 {
		if _, err := st.Update(EventID("a", nil), func(e *Event) error { e.Summary = "x"; return nil }); err == nil {
			t.Fatal("write into a read-only dir succeeded")
		}
		if st.List()[0].Summary != "" {
			t.Fatal("failed update not rolled back")
		}
		if _, err := st.Upsert(Event{UID: "b"}, func(_ *Event, ev Event) Event { return ev }); err == nil {
			t.Fatal("upsert into a read-only dir succeeded")
		}
		if len(st.List()) != 1 {
			t.Fatal("failed upsert not rolled back")
		}
	}
}

func TestStoreFull(t *testing.T) {
	st, err := NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	for i := 0; i < maxEvents; i++ {
		uid := "u" + string(rune(i))
		st.events[Key(uid, nil)] = Event{UID: uid, ID: EventID(uid, nil), Status: StatusScheduled}
	}
	st.mu.Unlock()
	if _, err := st.Upsert(Event{UID: "new"}, func(_ *Event, ev Event) Event { return ev }); !errors.Is(err, ErrConflict) {
		t.Fatalf("full store = %v", err)
	}
	// Finished events make room.
	st.mu.Lock()
	for k, e := range st.events {
		e.Status = StatusEnded
		st.events[k] = e
		break
	}
	st.mu.Unlock()
	if _, err := st.Upsert(Event{UID: "new"}, func(_ *Event, ev Event) Event { return ev }); err != nil {
		t.Fatalf("after pruning = %v", err)
	}
}

func TestRedactedAndTerminal(t *testing.T) {
	e := Event{Passcode: "secret"}
	if e.Redacted().Passcode != "set" || (Event{}).Redacted().Passcode != "" {
		t.Fatal("passcode not redacted")
	}
	for _, st := range []Status{StatusScheduled, StatusAwaitingApproval, StatusLive} {
		if st.Terminal() {
			t.Fatalf("%s terminal", st)
		}
	}
	if !StatusDeclined.Terminal() {
		t.Fatal("declined not terminal")
	}
	if (OwnerApprover{}).NeedsApproval(Event{Source: SourceManual, InvitedByOwner: true}) ||
		!(OwnerApprover{}).NeedsApproval(Event{Source: SourceEmail, InvitedByOwner: true}) {
		t.Fatal("owner approver")
	}
	if !sameInviter(Event{InvitedBy: "a"}, Event{InvitedBy: "a"}) || sameInviter(Event{}, Event{}) {
		t.Fatal("sameInviter")
	}
	if clip("abcdef", 3) != "abc" || len(occurrences(Event{}, t0, time.Hour, 0, nil)) != 0 {
		t.Fatal("helpers")
	}
	if (Event{TZID: "Nowhere/Land"}).location() != time.UTC {
		t.Fatal("bad tz location")
	}
	if utcSlice(nil, 1) != nil || len(utcSlice([]time.Time{t0, {}, t0}, 2)) != 1 {
		t.Fatal("utcSlice")
	}
}
