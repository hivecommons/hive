// Package schedule is hive's meeting calendar (hivecommons/hive#18394, part
// of the Betty plan in #18391): scheduled meetings from manual adds today and
// from calendar invites later (phase C2, email), persisted in
// <meetings data dir>/schedule.json, and the scheduler that sends hive into
// each one through meetings.Manager.Join shortly before it starts.
//
// An Event is keyed by its iCalendar UID plus RECURRENCE-ID, the same key a
// calendar invite carries, so an updated invite (higher SEQUENCE) replaces
// the entry and a CANCEL marks it cancelled.
package schedule

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/meetings"
)

// Source says where an event came from.
type Source string

// Event sources.
const (
	SourceEmail   Source = "email"
	SourceManual  Source = "manual"
	SourceDiscord Source = "discord"
)

// Status is an event's place in its lifecycle. For a recurring event it
// describes the series: after one occurrence ends the series is scheduled
// again for the next one, and History keeps each occurrence's outcome.
type Status string

// Event statuses.
const (
	StatusScheduled        Status = "scheduled"
	StatusAwaitingApproval Status = "awaiting_approval"
	StatusLive             Status = "live"
	StatusEnded            Status = "ended"
	StatusCancelled        Status = "cancelled"
	StatusDeclined         Status = "declined"
	StatusFailed           Status = "failed"
	StatusBlocked          Status = "blocked"
	StatusSkipped          Status = "skipped"
)

// Terminal reports whether the event will never be joined again.
func (s Status) Terminal() bool {
	switch s {
	case StatusEnded, StatusCancelled, StatusDeclined, StatusFailed, StatusBlocked, StatusSkipped:
		return true
	}
	return false
}

// iCalendar METHOD values an invite carries.
const (
	MethodRequest = "REQUEST"
	MethodCancel  = "CANCEL"
	MethodPublish = "PUBLISH"
)

// Platforms, the meetings package's names.
const (
	PlatformGoogleMeet = meetings.PlatformGoogleMeet
	PlatformTeams      = meetings.PlatformTeams
	PlatformZoom       = meetings.PlatformZoom
	PlatformDiscord    = meetings.PlatformDiscord
)

const (
	// MaxSummaryRunes bounds an event title.
	MaxSummaryRunes = 200
	// maxAttendees bounds the attendee list kept per event.
	maxAttendees = 200
	// maxExDates bounds the excluded dates kept per event.
	maxExDates = 500
	// maxHistory bounds the per-occurrence outcomes kept per event.
	maxHistory = 20
	// maxFieldLen bounds free-text identifiers (UID, ids, emails, URLs).
	maxFieldLen = 2048
	// eventIDHexLen is the length of an event id (hex of a SHA-256 prefix).
	eventIDHexLen = 16
	// manualUIDBytes is the random part of a manual event's UID.
	manualUIDBytes = 12
)

// Errors the store and scheduler return (wrapped).
var (
	ErrNotFound = errors.New("schedule: event not found")
	ErrInvalid  = errors.New("schedule: invalid event")
	ErrConflict = errors.New("schedule: event is not in a state that allows this")
	// ErrStaleSequence is returned when an update's SEQUENCE is not higher
	// than the stored one: the update is ignored and the stored event kept.
	ErrStaleSequence = errors.New("schedule: update ignored, sequence is not higher")
)

// Person is an organizer or attendee.
type Person struct {
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
}

// Occurrence is the outcome of one joined (or attempted) occurrence.
type Occurrence struct {
	Start     time.Time `json:"start"`
	MeetingID string    `json:"meeting_id,omitempty"`
	Status    Status    `json:"status"`
	Reason    string    `json:"reason,omitempty"`
}

// Event is one scheduled meeting: a single meeting, a recurring series, or
// one overridden occurrence of a series (RecurrenceID set). The email phase
// fills the iCalendar fields from an invite; a manual add fills the join
// details and times.
type Event struct {
	// ID is the event id used in the API, derived from UID + RecurrenceID.
	ID           string     `json:"id"`
	UID          string     `json:"uid"`
	RecurrenceID *time.Time `json:"recurrence_id,omitempty"`
	Sequence     int        `json:"sequence"`
	Method       string     `json:"method,omitempty"`
	Summary      string     `json:"summary,omitempty"`
	Organizer    *Person    `json:"organizer,omitempty"`
	Attendees    []Person   `json:"attendees,omitempty"`
	// Start and End are UTC. End is zero when the invite has none.
	Start time.Time `json:"start"`
	End   time.Time `json:"end,omitzero"`
	// TZID is the IANA zone a recurrence expands in, so a weekly meeting
	// keeps its local time across daylight saving changes.
	TZID    string      `json:"tzid,omitempty"`
	RRule   string      `json:"rrule,omitempty"`
	ExDates []time.Time `json:"exdates,omitempty"`
	// NextOccurrences are the next start times still to come (computed).
	NextOccurrences []time.Time `json:"next_occurrences,omitempty"`

	JoinURL  string `json:"join_url,omitempty"`
	Platform string `json:"platform,omitempty"`
	// NativeMeetingID and Passcode are the platform's meeting id and
	// passcode where it uses them (Zoom pwd, Teams meeting id and passcode).
	NativeMeetingID string `json:"native_meeting_id,omitempty"`
	Passcode        string `json:"passcode,omitempty"`
	// GuildID and ChannelID are a Discord voice channel (Source discord).
	GuildID   string `json:"guild_id,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`

	Source       Source `json:"source"`
	Status       Status `json:"status"`
	StatusReason string `json:"status_reason,omitempty"`
	// InvitedBy is the dashboard user or invite sender who asked for it;
	// InvitedByOwner is true only for an owner's own manual add.
	InvitedBy      string `json:"invited_by,omitempty"`
	InvitedByOwner bool   `json:"invited_by_owner,omitempty"`
	// Trusted is set by the email invite path (#18395) for an invite whose
	// From address passed DMARC alignment, is the organizer, and is on the
	// hive's email allowlist. It is decided per delivery and never stored.
	Trusted    bool   `json:"-"`
	ApprovedBy string `json:"approved_by,omitempty"`

	// Skipped are occurrence starts the owner chose to skip.
	Skipped []time.Time `json:"skipped,omitempty"`
	// JoinedOccurrence is the start of the latest occurrence hive was sent
	// to, recorded before the join so a restart never joins it twice.
	JoinedOccurrence *time.Time `json:"joined_occurrence,omitempty"`
	// MeetingID is the meeting created for JoinedOccurrence.
	MeetingID string       `json:"meeting_id,omitempty"`
	History   []Occurrence `json:"history,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Recurring reports whether the event is a series.
func (e Event) Recurring() bool { return e.RRule != "" }

// Key is the store key of a UID and RECURRENCE-ID.
func Key(uid string, recurrenceID *time.Time) string {
	if recurrenceID == nil {
		return uid
	}
	return uid + "\x00" + recurrenceID.UTC().Format(time.RFC3339)
}

// EventID is the API id of a UID and RECURRENCE-ID.
func EventID(uid string, recurrenceID *time.Time) string {
	sum := sha256.Sum256([]byte(Key(uid, recurrenceID)))
	return hex.EncodeToString(sum[:])[:eventIDHexLen]
}

// Redacted is the event as the dashboard shows it: the passcode is never
// returned (the join URL already carries what the owner needs).
func (e Event) Redacted() Event {
	if e.Passcode != "" {
		e.Passcode = "set"
	}
	return e
}

// DetectPlatform names the meeting platform of an https join link from its
// host, the same hosts Quorum joins: Google Meet, Microsoft Teams and Zoom.
// It returns "" for a link it does not recognise.
func DetectPlatform(link string) string {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "meet.google.com":
		return PlatformGoogleMeet
	case host == "teams.microsoft.com", host == "teams.live.com", strings.HasSuffix(host, ".teams.microsoft.com"):
		return PlatformTeams
	case host == "zoom.us", strings.HasSuffix(host, ".zoom.us"), host == "zoomgov.com", strings.HasSuffix(host, ".zoomgov.com"):
		return PlatformZoom
	case host == "discord.com", strings.HasSuffix(host, ".discord.com"):
		return PlatformDiscord
	}
	return ""
}

// discordChannel reads the guild and channel of
// https://discord.com/channels/<guild>/<channel>.
func discordChannel(link string) (guild, channel string) {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return "", ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 3 && parts[0] == "channels" && parts[1] != "" && parts[2] != "" {
		return parts[1], parts[2]
	}
	return "", ""
}

func newManualUID() string {
	b := make([]byte, manualUIDBytes)
	_, _ = rand.Read(b)
	return "manual-" + hex.EncodeToString(b)
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// clipRunes trims s to at most n runes.
func clipRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n]))
}

func utcSlice(ts []time.Time, limit int) []time.Time {
	if len(ts) == 0 {
		return nil
	}
	if len(ts) > limit {
		ts = ts[:limit]
	}
	out := make([]time.Time, 0, len(ts))
	for _, t := range ts {
		if !t.IsZero() {
			out = append(out, t.UTC().Truncate(time.Second))
		}
	}
	return out
}

func clipPerson(p Person) Person {
	return Person{Email: clip(p.Email, maxFieldLen), Name: clipRunes(p.Name, MaxSummaryRunes)}
}

// normalize checks and canonicalises an incoming event: UTC times, bounded
// text, a detected platform, a valid recurrence rule. A CANCEL needs only
// its UID.
func normalize(ev Event) (Event, error) {
	ev.UID = clip(ev.UID, maxFieldLen)
	if ev.UID == "" {
		return ev, fmt.Errorf("%w: uid is required", ErrInvalid)
	}
	if ev.RecurrenceID != nil {
		rid := ev.RecurrenceID.UTC().Truncate(time.Second)
		ev.RecurrenceID = &rid
	}
	ev.Method = strings.ToUpper(strings.TrimSpace(ev.Method))
	ev.ID = EventID(ev.UID, ev.RecurrenceID)
	if ev.Method == MethodCancel {
		return ev, nil
	}
	if ev.Sequence < 0 {
		ev.Sequence = 0
	}
	ev.Summary = clipRunes(ev.Summary, MaxSummaryRunes)
	if ev.Organizer != nil {
		p := clipPerson(*ev.Organizer)
		ev.Organizer = &p
	}
	if len(ev.Attendees) > maxAttendees {
		ev.Attendees = ev.Attendees[:maxAttendees]
	}
	for i := range ev.Attendees {
		ev.Attendees[i] = clipPerson(ev.Attendees[i])
	}
	if ev.Start.IsZero() {
		return ev, fmt.Errorf("%w: start is required", ErrInvalid)
	}
	ev.Start = ev.Start.UTC().Truncate(time.Second)
	if !ev.End.IsZero() {
		ev.End = ev.End.UTC().Truncate(time.Second)
		if !ev.End.After(ev.Start) {
			return ev, fmt.Errorf("%w: end must be after start", ErrInvalid)
		}
	}
	ev.TZID = clip(ev.TZID, maxFieldLen)
	if ev.TZID != "" {
		if _, err := time.LoadLocation(ev.TZID); err != nil {
			return ev, fmt.Errorf("%w: unknown time zone %q", ErrInvalid, ev.TZID)
		}
	}
	ev.RRule = strings.TrimPrefix(strings.TrimSpace(ev.RRule), "RRULE:")
	if ev.RRule != "" {
		if _, err := ruleSet(ev); err != nil {
			return ev, err
		}
	}
	ev.ExDates = utcSlice(ev.ExDates, maxExDates)
	ev.NativeMeetingID = clip(ev.NativeMeetingID, maxFieldLen)
	ev.Passcode = clip(ev.Passcode, maxFieldLen)
	ev.JoinURL = clip(ev.JoinURL, maxFieldLen)
	ev.GuildID, ev.ChannelID = clip(ev.GuildID, maxFieldLen), clip(ev.ChannelID, maxFieldLen)
	if ev.JoinURL != "" {
		detected := DetectPlatform(ev.JoinURL)
		if detected == "" {
			return ev, fmt.Errorf("%w: join url must be an https Google Meet, Teams, Zoom or Discord link", ErrInvalid)
		}
		if ev.Platform == "" {
			ev.Platform = detected
		}
		if detected == PlatformDiscord && ev.GuildID == "" {
			ev.GuildID, ev.ChannelID = discordChannel(ev.JoinURL)
		}
	}
	switch ev.Platform {
	case PlatformGoogleMeet, PlatformTeams, PlatformZoom:
		if ev.JoinURL == "" {
			return ev, fmt.Errorf("%w: a join url is required", ErrInvalid)
		}
	case PlatformDiscord:
		if ev.GuildID == "" || ev.ChannelID == "" {
			return ev, fmt.Errorf("%w: discord needs a guild and voice channel", ErrInvalid)
		}
	case "":
		return ev, fmt.Errorf("%w: a join url is required", ErrInvalid)
	default:
		return ev, fmt.Errorf("%w: unknown platform %q", ErrInvalid, ev.Platform)
	}
	switch ev.Source {
	case SourceEmail, SourceManual, SourceDiscord:
	case "":
		ev.Source = SourceManual
	default:
		return ev, fmt.Errorf("%w: unknown source %q", ErrInvalid, ev.Source)
	}
	ev.InvitedBy = clip(ev.InvitedBy, maxFieldLen)
	return ev, nil
}

// joinRequest is the meetings join for this event.
func (e Event) joinRequest() meetings.JoinRequest {
	req := meetings.JoinRequest{Title: e.Summary, InvitedBy: e.InvitedBy}
	if e.ApprovedBy != "" && req.InvitedBy == "" {
		req.InvitedBy = e.ApprovedBy
	}
	if e.Platform == PlatformDiscord {
		req.Platform, req.GuildID, req.ChannelID = PlatformDiscord, e.GuildID, e.ChannelID
		return req
	}
	req.URL, req.Platform = e.JoinURL, e.Platform
	return req
}
