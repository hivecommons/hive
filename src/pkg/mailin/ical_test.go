package mailin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func parseOne(t *testing.T, name string) wire.Invite {
	t.Helper()
	invs, err := ParseCalendar(fixture(t, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if len(invs) != 1 {
		t.Fatalf("%s: %d invites, want 1", name, len(invs))
	}
	return invs[0]
}

func mustUTC(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v.UTC()
}

func TestParseGoogleInvite(t *testing.T) {
	inv := parseOne(t, "google.ics")
	if inv.UID != "3k1n9q7f0v2h5m8c4r6t1p0s2a@google.com" || inv.Method != "REQUEST" || inv.Sequence != 0 {
		t.Fatalf("ids: %+v", inv)
	}
	if inv.Summary != "Roadmap sync" {
		t.Fatalf("summary %q", inv.Summary)
	}
	if inv.Organizer == nil || inv.Organizer.Email != "ada@example.com" || inv.Organizer.Name != "Ada Lovelace" {
		t.Fatalf("organizer %+v", inv.Organizer)
	}
	if len(inv.Attendees) != 2 || inv.Attendees[0].Email != "hive@acme.hive.hivecommons.dev" {
		t.Fatalf("attendees %+v", inv.Attendees)
	}
	// 10:00 EST is 15:00 UTC.
	if !inv.Start.Equal(mustUTC(t, "2026-12-01T15:00:00Z")) || !inv.End.Equal(mustUTC(t, "2026-12-01T15:30:00Z")) {
		t.Fatalf("times %v %v", inv.Start, inv.End)
	}
	if inv.TZID != "America/New_York" {
		t.Fatalf("tzid %q", inv.TZID)
	}
	if inv.JoinURL != "https://meet.google.com/abc-defg-hij" || inv.Platform != PlatformGoogleMeet || inv.NativeMeetingID != "abc-defg-hij" || inv.Passcode != "" {
		t.Fatalf("join %+v", inv)
	}
	if inv.RRule != "" || len(inv.ExDates) != 0 {
		t.Fatalf("unexpected recurrence %+v", inv)
	}
}

func TestParseGoogleSequenceBump(t *testing.T) {
	orig := parseOne(t, "google.ics")
	bump := parseOne(t, "google_sequence_bump.ics")
	if bump.UID != orig.UID || bump.Sequence != 2 || bump.Sequence <= orig.Sequence {
		t.Fatalf("sequence %d -> %d", orig.Sequence, bump.Sequence)
	}
	if !bump.Start.Equal(mustUTC(t, "2026-12-01T16:00:00Z")) || bump.Summary != "Roadmap sync (moved)" {
		t.Fatalf("bumped event %+v", bump)
	}
}

func TestParseOutlookTeamsInvite(t *testing.T) {
	inv := parseOne(t, "outlook_teams.ics")
	if !strings.HasPrefix(inv.UID, "040000008200E00074C5B7101A82E008") {
		t.Fatalf("uid %q", inv.UID)
	}
	// "Eastern Standard Time" is a Windows zone: 14:00 EST is 19:00 UTC.
	if inv.TZID != "America/New_York" || !inv.Start.Equal(mustUTC(t, "2026-12-03T19:00:00Z")) || !inv.End.Equal(mustUTC(t, "2026-12-03T20:00:00Z")) {
		t.Fatalf("times %v %v %q", inv.Start, inv.End, inv.TZID)
	}
	if inv.Platform != PlatformTeams || !strings.HasPrefix(inv.JoinURL, "https://teams.microsoft.com/l/meetup-join/19%3ameeting_") {
		t.Fatalf("join %q %q", inv.Platform, inv.JoinURL)
	}
	if inv.NativeMeetingID != "254545384891" || inv.Passcode != "x7Yz9Q" {
		t.Fatalf("teams id %q passcode %q", inv.NativeMeetingID, inv.Passcode)
	}
	if inv.Organizer == nil || inv.Organizer.Email != "grace@contoso.com" {
		t.Fatalf("organizer %+v", inv.Organizer)
	}
}

func TestParseZoomInvite(t *testing.T) {
	inv := parseOne(t, "zoom.ics")
	if inv.Platform != PlatformZoom || inv.JoinURL != "https://us06web.zoom.us/j/81234567890?pwd=bXlQYXNzd29yZEhhc2gx.1" {
		t.Fatalf("join %q %q", inv.Platform, inv.JoinURL)
	}
	if inv.NativeMeetingID != "81234567890" || inv.Passcode != "bXlQYXNzd29yZEhhc2gx.1" {
		t.Fatalf("zoom id %q pwd %q", inv.NativeMeetingID, inv.Passcode)
	}
	// 09:00 PST is 17:00 UTC.
	if !inv.Start.Equal(mustUTC(t, "2026-12-04T17:00:00Z")) || inv.TZID != "America/Los_Angeles" {
		t.Fatalf("start %v %q", inv.Start, inv.TZID)
	}
}

func TestParseCancel(t *testing.T) {
	inv := parseOne(t, "cancel.ics")
	if inv.Method != "CANCEL" || inv.Sequence != 3 || inv.UID != "3k1n9q7f0v2h5m8c4r6t1p0s2a@google.com" {
		t.Fatalf("cancel %+v", inv)
	}
	if inv.Start.IsZero() || inv.Summary != "Roadmap sync" {
		t.Fatalf("cancel keeps start and summary: %+v", inv)
	}
}

func TestParseWeeklyWithExdate(t *testing.T) {
	inv := parseOne(t, "weekly_exdate.ics")
	if inv.RRule != "FREQ=WEEKLY;UNTIL=20270302T090000Z;INTERVAL=1;BYDAY=TU;WKST=MO" {
		t.Fatalf("rrule %q", inv.RRule)
	}
	if inv.TZID != "Europe/Berlin" || !inv.Start.Equal(mustUTC(t, "2026-12-01T09:00:00Z")) {
		t.Fatalf("start %v %q", inv.Start, inv.TZID)
	}
	want := []time.Time{mustUTC(t, "2026-12-22T09:00:00Z"), mustUTC(t, "2026-12-29T09:00:00Z")}
	if len(inv.ExDates) != len(want) || !inv.ExDates[0].Equal(want[0]) || !inv.ExDates[1].Equal(want[1]) {
		t.Fatalf("exdates %v", inv.ExDates)
	}
	if inv.Platform != PlatformTeams || inv.NativeMeetingID != "31415926535" || inv.Passcode != "Qw3rTy" {
		t.Fatalf("teams /meet link %+v", inv)
	}
	if inv.Sequence != 1 {
		t.Fatalf("sequence %d", inv.Sequence)
	}
}

const icsHead = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:test\r\n"

func icsEvent(method string, lines ...string) []byte {
	var b strings.Builder
	b.WriteString(icsHead)
	if method != "" {
		b.WriteString("METHOD:" + method + "\r\n")
	}
	b.WriteString("BEGIN:VEVENT\r\n")
	for _, l := range lines {
		b.WriteString(l + "\r\n")
	}
	b.WriteString("END:VEVENT\r\nEND:VCALENDAR\r\n")
	return []byte(b.String())
}

func TestParseCalendarEdgeCases(t *testing.T) {
	// No METHOD defaults to REQUEST; DURATION instead of DTEND; a UTC start
	// has no zone; DESCRIPTION links to other hosts are not kept.
	invs, err := ParseCalendar(icsEvent("", "UID:a", "DTSTART:20261201T100000Z", "DURATION:PT45M",
		"SUMMARY:"+strings.Repeat("x", 300),
		"DESCRIPTION:see https://evil.example/phish and https://zoom.us/j/123456789",
		"SEQUENCE:-4", "RRULE:NOT-A-RULE", "ATTENDEE:mailto:not an address", "ORGANIZER:mailto:Boss@Example.COM"))
	if err != nil {
		t.Fatal(err)
	}
	inv := invs[0]
	if inv.Method != "REQUEST" || inv.TZID != "" || inv.Sequence != 0 || inv.RRule != "" || len(inv.Attendees) != 0 {
		t.Fatalf("defaults %+v", inv)
	}
	if !inv.End.Equal(inv.Start.Add(45 * time.Minute)) {
		t.Fatalf("duration end %v", inv.End)
	}
	if len([]rune(inv.Summary)) != wire.MaxTitleRunes {
		t.Fatalf("summary not clipped: %d", len([]rune(inv.Summary)))
	}
	if inv.JoinURL != "https://zoom.us/j/123456789" || inv.NativeMeetingID != "123456789" {
		t.Fatalf("join %+v", inv)
	}
	if inv.Organizer == nil || inv.Organizer.Email != "boss@example.com" {
		t.Fatalf("organizer %+v", inv.Organizer)
	}

	// STATUS:CANCELLED inside a REQUEST is a cancel.
	invs, err = ParseCalendar(icsEvent("REQUEST", "UID:b", "DTSTART:20261201T100000Z", "STATUS:CANCELLED"))
	if err != nil || invs[0].Method != "CANCEL" {
		t.Fatalf("status cancelled: %v %+v", err, invs)
	}

	// Replies and broken calendars carry nothing to schedule.
	for name, data := range map[string][]byte{
		"reply":       icsEvent("REPLY", "UID:c", "DTSTART:20261201T100000Z"),
		"no uid":      icsEvent("REQUEST", "DTSTART:20261201T100000Z"),
		"no start":    icsEvent("REQUEST", "UID:d"),
		"bad start":   icsEvent("REQUEST", "UID:e", "DTSTART:tomorrow"),
		"bad zone":    icsEvent("REQUEST", "UID:f", "DTSTART;TZID=Mars/Olympus:20261201T100000"),
		"bad rid":     icsEvent("REQUEST", "UID:g", "DTSTART:20261201T100000Z", "RECURRENCE-ID:soon"),
		"not ics":     []byte("hello"),
		"too nested":  []byte(icsHead + strings.Repeat("BEGIN:X\r\n", maxCalendarComponents+1)),
		"unbalanced":  []byte(icsHead + "BEGIN:VEVENT\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"),
		"not toplvl":  []byte("BEGIN:VEVENT\r\nUID:x\r\nEND:VEVENT\r\n"),
		"end of data": []byte(icsHead + "BEGIN:VEVENT\r\nUID:x\r\n"),
	} {
		if _, err := ParseCalendar(data); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestParseCalendarRecurrenceOverride(t *testing.T) {
	invs, err := ParseCalendar(icsEvent("REQUEST", "UID:s", "RECURRENCE-ID;TZID=Europe/Paris:20261208T100000",
		"DTSTART;TZID=Europe/Paris:20261208T110000", "DTEND;TZID=Europe/Paris:20261208T100000"))
	if err != nil {
		t.Fatal(err)
	}
	inv := invs[0]
	if inv.RecurrenceID == nil || !inv.RecurrenceID.Equal(mustUTC(t, "2026-12-08T09:00:00Z")) {
		t.Fatalf("recurrence id %v", inv.RecurrenceID)
	}
	if !inv.End.IsZero() {
		t.Fatalf("an end before start is dropped, got %v", inv.End)
	}
}

func TestParseCalendarCustomZones(t *testing.T) {
	cal := icsHead + "METHOD:REQUEST\r\n" +
		"BEGIN:VTIMEZONE\r\nTZID:Customized Time Zone\r\n" +
		"BEGIN:DAYLIGHT\r\nTZOFFSETTO:+1100\r\nEND:DAYLIGHT\r\n" +
		"BEGIN:STANDARD\r\nTZOFFSETTO:+1000\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n" +
		"BEGIN:VTIMEZONE\r\nTZID:Broken\r\nBEGIN:STANDARD\r\nTZOFFSETTO:junk\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n" +
		"BEGIN:VTIMEZONE\r\nBEGIN:STANDARD\r\nTZOFFSETTO:+0100\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n" +
		"BEGIN:VEVENT\r\nUID:z1\r\nDTSTART;TZID=Customized Time Zone:20261201T100000\r\n" +
		"EXDATE;TZID=Nowhere:20261208T100000\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:z2\r\nDTSTART;TZID=\"(UTC-05:00) Eastern Time (US & Canada)\":20261201T100000\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:z3\r\nDTSTART;TZID=/mozilla.org/20050126_1/Asia/Tokyo:20261201T100000\r\nEND:VEVENT\r\n" +
		"BEGIN:VTODO\r\nUID:todo\r\nEND:VTODO\r\n" +
		"END:VCALENDAR\r\n"
	invs, err := ParseCalendar([]byte(cal))
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 3 {
		t.Fatalf("%d invites", len(invs))
	}
	if !invs[0].Start.Equal(mustUTC(t, "2026-12-01T00:00:00Z")) || invs[0].TZID != "" || len(invs[0].ExDates) != 0 {
		t.Fatalf("custom zone %+v", invs[0])
	}
	if !invs[1].Start.Equal(mustUTC(t, "2026-12-01T15:00:00Z")) || invs[1].TZID != "" {
		t.Fatalf("offset display name %+v", invs[1])
	}
	if !invs[2].Start.Equal(mustUTC(t, "2026-12-01T01:00:00Z")) || invs[2].TZID != "Asia/Tokyo" {
		t.Fatalf("mozilla zone %+v", invs[2])
	}
}

func TestParseCalendarCaps(t *testing.T) {
	var b strings.Builder
	b.WriteString(icsHead)
	for i := 0; i < maxEventsPerCalendar+5; i++ {
		b.WriteString("BEGIN:VEVENT\r\nUID:u" + strings.Repeat("x", i) + "\r\nDTSTART:20261201T100000Z\r\n")
		if i == 0 {
			for j := 0; j < maxAttendees+3; j++ {
				b.WriteString("ATTENDEE:mailto:a@example.com\r\n")
			}
			b.WriteString("EXDATE:" + strings.TrimSuffix(strings.Repeat("20261208T100000Z,", maxExDates+3), ",") + "\r\n")
		}
		b.WriteString("END:VEVENT\r\n")
	}
	b.WriteString("END:VCALENDAR\r\n")
	invs, err := ParseCalendar([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != maxEventsPerCalendar || len(invs[0].Attendees) != maxAttendees || len(invs[0].ExDates) != maxExDates {
		t.Fatalf("caps: %d events, %d attendees, %d exdates", len(invs), len(invs[0].Attendees), len(invs[0].ExDates))
	}
}

func TestParseUTCOffset(t *testing.T) {
	for in, want := range map[string]int{"+0100": 3600, "-0530": -19800, "+053015": 19815} {
		got, ok := parseUTCOffset(in)
		if !ok || got != want {
			t.Errorf("%s: %d %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "0100", "+1", "+2500", "+0160", "+010060", "+ab00"} {
		if _, ok := parseUTCOffset(in); ok {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestResolveTZID(t *testing.T) {
	cases := map[string]string{
		"":                           "",
		"America/Chicago":            "America/Chicago",
		"pacific standard time":      "America/Los_Angeles",
		"Tokyo Standard Time":        "Asia/Tokyo",
		"UTC":                        "UTC",
		"(UTC) Coordinated Time":     "",
		"(UTC+05:30) Chennai":        "",
		"\"India Standard Time\"":    "Asia/Calcutta",
		"/mozilla.org/x/Europe/Oslo": "Europe/Oslo",
	}
	for in, want := range cases {
		loc, iana, ok := resolveTZID(in)
		if !ok || loc == nil || iana != want {
			t.Errorf("%q: %v %q %v, want %q", in, loc, iana, ok, want)
		}
	}
	if _, _, ok := resolveTZID("(UTC+15:00) Nowhere"); ok {
		t.Error("out-of-range offset accepted")
	}
	if _, _, ok := resolveTZID("Nowhere Standard Time"); ok {
		t.Error("unknown zone accepted")
	}
	for win, iana := range windowsZones {
		if _, err := time.LoadLocation(iana); err != nil {
			t.Errorf("%s maps to %s: %v", win, iana, err)
		}
	}
}
