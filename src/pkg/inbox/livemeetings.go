package inbox

import (
	"context"
	"strings"
	"time"
)

// LiveMeetingType is the inbox source type for meetings hive attended
// itself through Quorum (hivecommons/hive#18293), distinct from recorded
// meetings synced by the zoom/teams/discord connectors.
const LiveMeetingType = "meeting"

// LiveMeeting is one ended meeting as the inbox sees it.
type LiveMeeting struct {
	ID         string
	Title      string
	Platform   string
	StartedAt  time.Time
	EndedAt    time.Time
	Transcript string
	// Notes is the notes Markdown (summary, decisions, actions, ...).
	Notes string
	// Summary is the notes' summary section, for the listing.
	Summary string
}

// LiveMeetingReader is hive's meeting store seen from the inbox; cmd/hive
// implements it over pkg/meetings so this package does not import it.
type LiveMeetingReader interface {
	// EndedMeetings lists meetings that ended and still hold a transcript.
	EndedMeetings(ctx context.Context) ([]LiveMeeting, error)
	ReadMeeting(ctx context.Context, id string) (LiveMeeting, error)
	// HandOff erases the meeting upstream (Quorum meeting.delete) and marks
	// it handed off.
	HandOff(ctx context.Context, id string) error
}

// LiveMeetingSource lists ended meetings with their final transcript and
// notes, routable to spek_rfc and dibs like any other inbox item.
type LiveMeetingSource struct {
	Reader LiveMeetingReader
}

// Type implements Source.
func (s *LiveMeetingSource) Type() string { return LiveMeetingType }

// List implements Source: newest first.
func (s *LiveMeetingSource) List(ctx context.Context) ([]Item, error) {
	list, err := s.Reader.EndedMeetings(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(list))
	for _, m := range list {
		it := Item{
			Source:        LiveMeetingType,
			Connector:     LiveMeetingType,
			ID:            m.ID,
			Title:         m.Title,
			Summary:       truncateRunes(m.Summary, SummaryRunes),
			HasTranscript: strings.TrimSpace(m.Transcript) != "",
			Pending:       strings.TrimSpace(m.Transcript) == "" && strings.TrimSpace(m.Notes) == "",
			RecordedAt:    m.StartedAt.UTC(),
			Tags:          []string{"meeting", m.Platform},
		}
		if !m.StartedAt.IsZero() && m.EndedAt.After(m.StartedAt) {
			it.DurationMS = m.EndedAt.Sub(m.StartedAt).Milliseconds()
		}
		out = append(out, it)
	}
	sortItems(out)
	return out, nil
}

// Fetch implements Source.
func (s *LiveMeetingSource) Fetch(ctx context.Context, it Item) (Content, error) {
	m, err := s.Reader.ReadMeeting(ctx, it.ID)
	if err != nil {
		return Content{}, err
	}
	var md strings.Builder
	md.WriteString("# " + m.Title + "\n")
	if n := strings.TrimSpace(m.Notes); n != "" {
		md.WriteString("\n" + n + "\n")
	}
	if t := strings.TrimSpace(m.Transcript); t != "" {
		md.WriteString("\n## Transcript\n\n" + t + "\n")
	}
	return Content{
		Title:      m.Title,
		Summary:    strings.TrimSpace(m.Notes),
		Transcript: strings.TrimSpace(m.Transcript),
		Markdown:   strings.TrimSpace(md.String()),
		Pending:    it.Pending,
		Provenance: "hive://meetings/" + it.ID,
	}, nil
}

// Builtin implements BuiltinSource: hive's own meetings are always listed.
func (s *LiveMeetingSource) Builtin() bool { return true }

// HandedOff implements HandOffSource.
func (s *LiveMeetingSource) HandedOff(ctx context.Context, it Item, _ string) error {
	return s.Reader.HandOff(ctx, it.ID)
}
