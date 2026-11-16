package mailin

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// Limits on what one calendar part may carry.
const (
	maxEventsPerCalendar = 50
	// maxCalendarComponents bounds BEGIN lines (events, alarms, zones).
	maxCalendarComponents = 1000
	maxAttendees          = 200
	maxExDates            = 500
	maxFieldLen           = 2048
)

// iCalendar date-time layouts (RFC 5545 3.3.4, 3.3.5).
const (
	icsDate       = "20060102"
	icsDateTime   = "20060102T150405"
	icsDateTimeUT = "20060102T150405Z"
)

// Conference properties the big three put the join link in.
const (
	propGoogleConference = "X-GOOGLE-CONFERENCE"
	propTeamsURL         = "X-MICROSOFT-SKYPETEAMSMEETINGURL"
	propTeamsConfLink    = "X-MICROSOFT-ONLINEMEETINGEXTERNALLINK"
	propURL              = "URL"
)

// ErrNoEvents is returned for a calendar part with no usable VEVENT.
var ErrNoEvents = errors.New("mailin: calendar has no usable event")

// ParseCalendar reads one text/calendar part into invites, one per VEVENT.
// Only scheduling fields are kept; from DESCRIPTION only a meeting link and
// the platform's meeting id and passcode lines.
func ParseCalendar(data []byte) (invites []wire.Invite, err error) {
	// The decoder recurses once per BEGIN; bound the nesting a hostile
	// part can ask for before decoding.
	if bytes.Count(bytes.ToUpper(data), []byte("BEGIN:")) > maxCalendarComponents {
		return nil, fmt.Errorf("%w: too many components", ErrNoEvents)
	}
	defer func() {
		if r := recover(); r != nil {
			invites, err = nil, fmt.Errorf("mailin: parse calendar: %v", r)
		}
	}()
	cal, err := ical.NewDecoder(bytes.NewReader(data)).Decode()
	if err != nil {
		return nil, fmt.Errorf("mailin: parse calendar: %w", err)
	}
	method := strings.ToUpper(strings.TrimSpace(propValue(cal.Props, ical.PropMethod)))
	switch method {
	case "", "REQUEST", "PUBLISH", "CANCEL":
	default:
		// REPLY, COUNTER, REFRESH, ADD, DECLINECOUNTER: nothing to schedule.
		return nil, fmt.Errorf("%w: method %s", ErrNoEvents, method)
	}
	if method == "" {
		method = "REQUEST"
	}
	zones := vtimezones(cal.Children)
	var out []wire.Invite
	for _, child := range cal.Children {
		if child.Name != ical.CompEvent {
			continue
		}
		if len(out) == maxEventsPerCalendar {
			break
		}
		inv, err := parseEvent(child, method, zones)
		if err != nil {
			continue
		}
		out = append(out, inv)
	}
	if len(out) == 0 {
		return nil, ErrNoEvents
	}
	return out, nil
}

func propValue(props ical.Props, name string) string {
	p := props.Get(name)
	if p == nil {
		return ""
	}
	return p.Value
}

func propText(props ical.Props, name string) string {
	p := props.Get(name)
	if p == nil {
		return ""
	}
	t, err := p.Text()
	if err != nil {
		return p.Value
	}
	return t
}

// vtimezones maps a custom TZID ("Customized Time Zone") to the fixed
// standard offset its VTIMEZONE declares, the fallback when the name is
// neither IANA nor a Windows zone.
func vtimezones(children []*ical.Component) map[string]*time.Location {
	out := map[string]*time.Location{}
	for _, c := range children {
		if c.Name != ical.CompTimezone {
			continue
		}
		id := strings.TrimSpace(propValue(c.Props, ical.PropTimezoneID))
		if id == "" {
			continue
		}
		var offset string
		for _, sub := range c.Children {
			v := propValue(sub.Props, ical.PropTimezoneOffsetTo)
			if v == "" {
				continue
			}
			if sub.Name == ical.CompTimezoneStandard || offset == "" {
				offset = v
			}
		}
		if secs, ok := parseUTCOffset(offset); ok {
			out[strings.ToLower(id)] = time.FixedZone(id, secs)
		}
	}
	return out
}

// parseUTCOffset reads an RFC 5545 UTC-OFFSET such as -0500 or +053000.
func parseUTCOffset(v string) (int, bool) {
	if len(v) != len("+hhmm") && len(v) != len("+hhmmss") {
		return 0, false
	}
	sign := 1
	switch v[0] {
	case '-':
		sign = -1
	case '+':
	default:
		return 0, false
	}
	h, err1 := strconv.Atoi(v[1:3])
	m, err2 := strconv.Atoi(v[3:5])
	s := 0
	var err3 error
	if len(v) == len("+hhmmss") {
		s, err3 = strconv.Atoi(v[5:7])
	}
	if err1 != nil || err2 != nil || err3 != nil || h > maxOffsetHours || m >= minutesPerHour || s >= secondsPerMin {
		return 0, false
	}
	return sign * ((h*minutesPerHour+m)*secondsPerMin + s), true
}

// zoneFor resolves a property's TZID, falling back to the calendar's own
// VTIMEZONE offsets.
func zoneFor(tzid string, zones map[string]*time.Location) (*time.Location, string, error) {
	if loc, iana, ok := resolveTZID(tzid); ok {
		return loc, iana, nil
	}
	if loc, ok := zones[strings.ToLower(strings.Trim(strings.TrimSpace(tzid), `"`))]; ok {
		return loc, "", nil
	}
	return nil, "", fmt.Errorf("mailin: unknown time zone %q", tzid)
}

// parseTime reads one DATE or DATE-TIME value in loc.
func parseTime(value string, loc *time.Location) (time.Time, error) {
	value = strings.TrimSpace(value)
	switch len(value) {
	case len(icsDateTimeUT):
		return time.ParseInLocation(icsDateTimeUT, value, time.UTC)
	case len(icsDateTime):
		return time.ParseInLocation(icsDateTime, value, loc)
	case len(icsDate):
		return time.ParseInLocation(icsDate, value, loc)
	}
	return time.Time{}, fmt.Errorf("mailin: bad date-time %q", value)
}

// propTime reads a DTSTART-like property; iana is its IANA zone name.
func propTime(p *ical.Prop, zones map[string]*time.Location) (t time.Time, iana string, err error) {
	loc, iana, err := zoneFor(p.Params.Get(ical.PropTimezoneID), zones)
	if err != nil {
		return time.Time{}, "", err
	}
	t, err = parseTime(p.Value, loc)
	if strings.HasSuffix(strings.TrimSpace(p.Value), "Z") {
		iana = ""
	}
	return t, iana, err
}

func parsePerson(p *ical.Prop) (wire.Person, bool) {
	v := strings.TrimSpace(p.Value)
	if len(v) > len("mailto:") && strings.EqualFold(v[:len("mailto:")], "mailto:") {
		v = v[len("mailto:"):]
	}
	if !strings.Contains(v, "@") || strings.ContainsAny(v, " \t<>") {
		return wire.Person{}, false
	}
	return wire.Person{
		Email: clipBytes(strings.ToLower(v), maxFieldLen),
		Name:  clipRunes(p.Params.Get(ical.ParamCommonName), wire.MaxTitleRunes),
	}, true
}

// clipRunes trims s to at most n runes, dropping invalid UTF-8.
func clipRunes(s string, n int) string {
	s = strings.TrimSpace(strings.ToValidUTF8(s, ""))
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strings.TrimSpace(string([]rune(s)[:n]))
}

func parseEvent(c *ical.Component, method string, zones map[string]*time.Location) (wire.Invite, error) {
	inv := wire.Invite{Method: method}
	inv.UID = clipBytes(strings.TrimSpace(propValue(c.Props, ical.PropUID)), maxFieldLen)
	if inv.UID == "" {
		return inv, errors.New("mailin: event has no UID")
	}
	if v := strings.TrimSpace(propValue(c.Props, ical.PropSequence)); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 {
			inv.Sequence = n
		}
	}
	if p := c.Props.Get(ical.PropRecurrenceID); p != nil {
		rid, _, err := propTime(p, zones)
		if err != nil {
			return inv, err
		}
		rid = rid.UTC()
		inv.RecurrenceID = &rid
	}
	if p := c.Props.Get(ical.PropOrganizer); p != nil {
		if who, ok := parsePerson(p); ok {
			inv.Organizer = &who
		}
	}
	if method == "CANCEL" {
		// A cancel needs only its UID (and RECURRENCE-ID); keep the rest
		// that is cheap and present for the dashboard.
		inv.Summary = clipRunes(propText(c.Props, ical.PropSummary), wire.MaxTitleRunes)
		if p := c.Props.Get(ical.PropDateTimeStart); p != nil {
			if t, _, err := propTime(p, zones); err == nil {
				inv.Start = t.UTC()
			}
		}
		return inv, nil
	}
	if strings.EqualFold(strings.TrimSpace(propValue(c.Props, ical.PropStatus)), "CANCELLED") {
		inv.Method = "CANCEL"
	}
	inv.Summary = clipRunes(propText(c.Props, ical.PropSummary), wire.MaxTitleRunes)
	for _, p := range c.Props.Values(ical.PropAttendee) {
		if len(inv.Attendees) == maxAttendees {
			break
		}
		if who, ok := parsePerson(&p); ok {
			inv.Attendees = append(inv.Attendees, who)
		}
	}
	start := c.Props.Get(ical.PropDateTimeStart)
	if start == nil {
		return inv, errors.New("mailin: event has no DTSTART")
	}
	st, iana, err := propTime(start, zones)
	if err != nil {
		return inv, err
	}
	inv.Start, inv.TZID = st.UTC(), iana
	if end := c.Props.Get(ical.PropDateTimeEnd); end != nil {
		if et, _, err := propTime(end, zones); err == nil && et.After(st) {
			inv.End = et.UTC()
		}
	} else if dur := c.Props.Get(ical.PropDuration); dur != nil {
		if d, err := dur.Duration(); err == nil && d > 0 {
			inv.End = st.Add(d).UTC()
		}
	}
	if rr := strings.TrimSpace(propValue(c.Props, ical.PropRecurrenceRule)); rr != "" {
		if _, err := rrule.StrToROption(rr); err == nil {
			inv.RRule = clipBytes(rr, maxFieldLen)
		}
	}
	for _, p := range c.Props.Values(ical.PropExceptionDates) {
		loc, _, err := zoneFor(p.Params.Get(ical.PropTimezoneID), zones)
		if err != nil {
			continue
		}
		for _, v := range strings.Split(p.Value, ",") {
			if len(inv.ExDates) == maxExDates {
				break
			}
			if t, err := parseTime(v, loc); err == nil {
				inv.ExDates = append(inv.ExDates, t.UTC())
			}
		}
	}
	fields := []string{
		propValue(c.Props, propGoogleConference),
		propValue(c.Props, propTeamsURL),
		propValue(c.Props, propTeamsConfLink),
		propValue(c.Props, propURL),
	}
	j := findJoin(fields, propText(c.Props, ical.PropLocation), propText(c.Props, ical.PropDescription))
	inv.JoinURL, inv.Platform, inv.NativeMeetingID, inv.Passcode = j.URL, j.Platform, j.NativeMeetingID, j.Passcode
	return inv, nil
}
