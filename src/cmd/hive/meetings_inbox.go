package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/inbox"
	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/live"
)

// meetingsInbox presents ended meetings to the inbox (hivecommons/hive#18293):
// the final transcript plus the notes Spek took, routable to spek_rfc and
// dibs. Hand-off erases the meeting upstream through the manager.
type meetingsInbox struct {
	mgr   *meetings.Manager
	store *meetings.Store
	// name is the attendee in the notes header: the persona's name, or
	// "hive" with the persona off.
	name string
}

// meetingsInboxStates are the states whose meetings the inbox lists.
var meetingsInboxStates = map[meetings.State]bool{
	meetings.StateEnded: true, meetings.StateFailed: true, meetings.StateBlocked: true, meetings.StateHandedOff: true,
}

func (mi *meetingsInbox) build(m meetings.Meeting) (inbox.LiveMeeting, error) {
	tr, err := mi.store.Transcript(m.ID)
	if err != nil {
		return inbox.LiveMeeting{}, err
	}
	// The attendee's own lines are in the transcript, in the order they
	// were said, labelled as the assistant's ("Betty (assistant, spoken;
	// source: ...)") so routing and summarising never read them as a
	// participant speaking.
	transcript := live.RenderTranscript(tr)
	var st live.State
	if err := mi.store.LoadDoc(m.ID, live.StateDoc, &st); err != nil && !errors.Is(err, meetings.ErrNotFound) {
		return inbox.LiveMeeting{}, err
	}
	out := inbox.LiveMeeting{
		ID: m.ID, Title: live.NotesTitle(m), Platform: m.Platform,
		StartedAt: m.CreatedAt, Transcript: transcript,
	}
	if m.LiveAt != nil {
		out.StartedAt = *m.LiveAt
	}
	if m.EndedAt != nil {
		out.EndedAt = *m.EndedAt
	}
	if len(st.Notes) > 0 {
		out.Notes = live.RenderNotesWithReplies(mi.attendee(), m, st.Notes, tr)
		out.Summary = strings.Join(st.Notes[live.SectionSummary], "\n")
	}
	// Questions nobody answered because the budget ran out stay in the
	// record, so nothing asked is lost.
	if skipped := live.RenderSkippedNote(st); skipped != "" {
		out.Notes = strings.TrimRight(out.Notes, "\n") + "\n\n" + skipped
		out.Notes = strings.TrimLeft(out.Notes, "\n")
	}
	return out, nil
}

// attendee names the note taker in the notes header.
func (mi *meetingsInbox) attendee() string {
	if n := strings.TrimSpace(mi.name); n != "" {
		return n
	}
	return defaultNotesAttendee
}

// defaultNotesAttendee names the note taker with the persona off.
const defaultNotesAttendee = "hive"

// meetingsAttendeeName is the persona's name when it is on.
func meetingsAttendeeName(p config.ResolvedMeetingsPersona) string {
	if p.Enabled && strings.TrimSpace(p.Name) != "" {
		return p.Name
	}
	return defaultNotesAttendee
}

// EndedMeetings implements inbox.LiveMeetingReader.
func (mi *meetingsInbox) EndedMeetings(context.Context) ([]inbox.LiveMeeting, error) {
	list, err := mi.mgr.List()
	if err != nil {
		return nil, err
	}
	var out []inbox.LiveMeeting
	for _, m := range list {
		if !meetingsInboxStates[m.State] {
			continue
		}
		lm, err := mi.build(m)
		if err != nil {
			continue
		}
		if strings.TrimSpace(lm.Transcript) == "" && lm.Notes == "" {
			continue
		}
		out = append(out, lm)
	}
	return out, nil
}

// ReadMeeting implements inbox.LiveMeetingReader.
func (mi *meetingsInbox) ReadMeeting(_ context.Context, id string) (inbox.LiveMeeting, error) {
	m, err := mi.mgr.Get(id)
	if err != nil {
		return inbox.LiveMeeting{}, err
	}
	return mi.build(m)
}

// HandOff implements inbox.LiveMeetingReader.
func (mi *meetingsInbox) HandOff(ctx context.Context, id string) error {
	return mi.mgr.HandOff(ctx, id)
}

// hoursPerDay converts meetings.retention_days to a duration.
const hoursPerDay = 24

// wireMeetingsInbox adds ended meetings to the inbox and starts the
// retention sweeper.
func (b *boot) wireMeetingsInbox(mgr *meetings.Manager, store *meetings.Store) {
	b.meetingsInboxSrc = &inbox.LiveMeetingSource{Reader: &meetingsInbox{mgr: mgr, store: store, name: meetingsAttendeeName(b.cfg.Meetings.Persona.Resolve())}}
	b.refreshInboxSources(b.cfg.Knowledge.Connectors, b.knowledgeConnectors)
	retention := time.Duration(b.cfg.Meetings.RetentionDaysOrDefault()) * hoursPerDay * time.Hour
	mgr.StartRetention(retention, 0, live.StateDoc)
}
