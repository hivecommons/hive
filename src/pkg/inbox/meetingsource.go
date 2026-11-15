package inbox

import (
	"context"
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// Meeting-recording source types (Zoom, Microsoft Teams, Discord, Google Meet, Webex, Slack huddles, Fireflies, Fathom, Limitless, Granola, Otter).
const (
	ZoomType         = connector.TypeZoom
	TeamsType        = connector.TypeTeams
	DiscordType      = connector.TypeDiscord
	GoogleMeetType   = connector.TypeGoogleMeet
	WebexType        = connector.TypeWebex
	SlackHuddlesType = connector.TypeSlackHuddles
	OtterType        = connector.TypeOtter
	GranolaType      = connector.TypeGranola
	LimitlessType    = connector.TypeLimitless
	FathomType       = connector.TypeFathom
	FirefliesType    = connector.TypeFireflies
)

// MeetingSource reads recorded meetings straight from a meeting connector
// (zoom, teams, discord, google-meet, webex, slack-huddles, fireflies, fathom, limitless, granola, otter), the sibling of PlaudSource. Item.ID is the platform
// recording id, the same value the sync writes to `attr_<type>_recording_id`,
// so route state carries over between this source and FactSource.
type MeetingSource struct {
	// SourceType is the connector type (zoom, teams, discord, google-meet, webex,
	// slack-huddles, fireflies, fathom, limitless, granola, otter).
	SourceType string
	// Name is the knowledge.connectors[] entry name.
	Name   string
	Reader connector.MeetingReader
	// Ready reports whether the connector is signed in; nil means always.
	Ready func(ctx context.Context) bool
}

// Type implements Source.
func (m *MeetingSource) Type() string { return m.SourceType }

// Available reports whether the connector can be read right now.
func (m *MeetingSource) Available(ctx context.Context) bool {
	return m.Reader != nil && (m.Ready == nil || m.Ready(ctx))
}

// List implements Source: newest first.
func (m *MeetingSource) List(ctx context.Context) ([]Item, error) {
	recs, err := m.Reader.ListMeetings(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(recs))
	for _, r := range recs {
		out = append(out, Item{
			Source:     m.SourceType,
			Connector:  m.Name,
			ID:         r.ID,
			Title:      r.Topic,
			RecordedAt: r.StartAt.UTC(),
			DurationMS: r.DurationMS,
			Tags:       r.Tags,
		})
	}
	sortItems(out)
	return out, nil
}

// Fetch implements Source.
func (m *MeetingSource) Fetch(ctx context.Context, it Item) (Content, error) {
	c, err := m.Reader.ReadMeeting(ctx, it.ID)
	if err != nil {
		return Content{}, err
	}
	var md strings.Builder
	md.WriteString("# " + it.Title + "\n")
	if len(c.Participants) > 0 {
		md.WriteString("\n- **Participants:** " + strings.Join(c.Participants, ", ") + "\n")
	}
	if c.Pending {
		md.WriteString("\n_" + connector.MeetingPendingText + "_\n")
	}
	if c.Summary != "" {
		md.WriteString("\n## Summary\n\n" + c.Summary + "\n")
	}
	if c.Transcript != "" {
		md.WriteString("\n## Transcript\n\n" + c.Transcript + "\n")
	}
	return Content{
		Title:      it.Title,
		Summary:    c.Summary,
		Transcript: c.Transcript,
		Pending:    c.Pending,
		Markdown:   strings.TrimSpace(md.String()),
		Provenance: m.SourceType + "://recordings/" + it.ID,
	}, nil
}

// AudioLink implements AudioSource: a short-lived media link minted by the
// platform connector. The link is returned to the caller only; nothing is
// written.
func (m *MeetingSource) AudioLink(ctx context.Context, it Item) (AudioLink, error) {
	media, ok := m.Reader.(connector.MeetingMediaReader)
	if !ok {
		return AudioLink{}, ErrNoAudio
	}
	link, err := media.MediaLink(ctx, it.ID)
	if err != nil {
		return AudioLink{}, fmt.Errorf("%w: %v", ErrNoAudio, err)
	}
	return AudioLink{URL: link.URL, ExpiresAt: link.ExpiresAt}, nil
}
