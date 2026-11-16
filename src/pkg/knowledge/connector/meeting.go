package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/transcribe"
)

// Meeting-recording connectors (Zoom, Microsoft Teams) share one reading
// model with the Plaud voice-note connector: one fact per recorded meeting,
// a live listing for the Inbox, and a short-lived media link minted on demand
// and never persisted. This file holds the parts both platforms use.

const (
	// meetingDefaultSinceDays bounds the first sync of a meeting connector.
	meetingDefaultSinceDays = 30
	// meetingDefaultMaxRecordings caps recordings fetched per sync.
	meetingDefaultMaxRecordings = 200
	// meetingRelistOverlap re-lists this far behind the cursor so transcripts
	// and summaries that finish processing after the recording was first seen
	// are picked up.
	meetingRelistOverlap = 48 * time.Hour
	// meetingContentAttr records whether a fact carries text yet, with the
	// same ready/pending values as the Plaud connector.
	meetingContentAttr = "meeting_content"
	// MeetingPendingText is written into a fact (and shown by the Inbox) when
	// the platform has produced neither a transcript nor a summary yet.
	MeetingPendingText = "The meeting platform has not produced a transcript or summary for this recording yet (cloud transcription may still be processing, or it is disabled for the account). The hive re-checks it on every sync until text arrives."
	// meetingParticipantsMax bounds how many names a fact lists.
	meetingParticipantsMax = 50
	// meetingTokenFileMode is the permission of the stored refresh token.
	meetingTokenFileMode = 0o600
	// meetingTokenRefreshSkew refreshes an access token this long before it
	// expires so a sync never starts with a token that dies mid-listing.
	meetingTokenRefreshSkew = 2 * time.Minute
	// meetingMediaLinkTTLFallback is the ExpiresAt reported for a media link
	// when the platform does not say how long the token behind it lives.
	meetingMediaLinkTTLFallback = time.Hour
)

// MeetingRecording is one recorded meeting as the platform lists it.
type MeetingRecording struct {
	// ID is the platform recording id (Zoom: meeting instance UUID; Teams:
	// callRecording id). It is the fact's source_id and the Inbox item id.
	ID string
	// MeetingID is the platform meeting id (Zoom: numeric meeting id;
	// Teams: onlineMeeting id).
	MeetingID string
	Topic     string
	StartAt   time.Time
	// DurationMS is the recording length when known.
	DurationMS int64
	// Participants are display names or addresses the platform reports.
	Participants []string
	Host         string
	// URL is the platform page for the recording (share link), if any.
	URL string
	// HasMedia reports that a playable audio/video file exists.
	HasMedia bool
	Tags     []string
}

// MeetingContent is the rendered text of one recorded meeting.
type MeetingContent struct {
	Summary    string
	Transcript string
	// Participants are repeated here so a direct read can show them.
	Participants []string
	// Pending is true when the platform has produced neither a transcript
	// nor a summary yet.
	Pending bool
}

// MeetingReader reads recorded meetings straight from the platform without
// going through the facts a sync writes. The zoom and teams connectors
// implement it; the Inbox lists through it while the connector is signed in.
type MeetingReader interface {
	// ListMeetings lists recent recordings, newest first.
	ListMeetings(ctx context.Context) ([]MeetingRecording, error)
	// ReadMeeting returns the summary and transcript of one recording.
	ReadMeeting(ctx context.Context, id string) (MeetingContent, error)
}

// MediaLink is a short-lived link to a recording's audio or video. It is
// minted on demand and never persisted by the hive.
type MediaLink struct {
	URL       string
	ExpiresAt time.Time
}

// MeetingMediaReader mints a playable link for one recording.
type MeetingMediaReader interface {
	MediaLink(ctx context.Context, id string) (MediaLink, error)
}

// ErrMeetingNoMedia is returned when a recording has no playable file, or
// the platform only serves it behind a bearer token the browser cannot send.
var ErrMeetingNoMedia = errors.New("no playable media is available for this recording")

// ErrMeetingNotConnected is returned when the platform rejected the
// credential: the operator has to (re)connect from the Integrations card.
var ErrMeetingNotConnected = errors.New("not connected")

// meetingNotConnected wraps err so the dashboard message points at the card.
func meetingNotConnected(platform, label string, err error) error {
	return fmt.Errorf("%s: %w — use Connect in the Knowledge → Integrations → %s card (%v)", platform, ErrMeetingNotConnected, label, err)
}

// isAuthStatus reports whether err is an HTTP 401 (or a 400 from an OAuth
// token endpoint, which is how both platforms report a dead refresh token).
func isAuthStatus(err error) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return false
	}
	return se.StatusCode == 401 || (se.StatusCode == 400 && strings.Contains(se.URL, "/oauth"))
}

// envValue resolves a credential referenced by environment variable name.
// The name, never the value, appears in errors.
func envValue(platform, field, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("%s: auth.%s is required (an environment variable name)", platform, field)
	}
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return "", fmt.Errorf("%s: auth.%s names %s, which is empty or unset", platform, field, name)
	}
	return v, nil
}

// meetingScopeInt reads a positive integer scope value with a default.
func meetingScopeInt(cfg ConnectorConfig, key string, def int) int {
	n, err := strconv.Atoi(cfg.ScopeValue(key))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// meetingScopeBool reads a boolean scope value with a default.
func meetingScopeBool(cfg ConnectorConfig, key string, def bool) bool {
	v := cfg.ScopeValue(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// meetingScopeList splits a comma-separated scope value.
func meetingScopeList(cfg ConnectorConfig, key string) []string {
	var out []string
	for _, s := range strings.Split(cfg.ScopeValue(key), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// validateMeetingScope checks the scope keys the meeting connectors (zoom,
// teams, discord) share.
func validateMeetingScope(platform string, cfg ConnectorConfig) error {
	for _, key := range []string{"since_days", "max_recordings"} {
		if v := cfg.ScopeValue(key); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return fmt.Errorf("%s: scope.%s must be a positive integer", platform, key)
			}
		}
	}
	for _, key := range []string{"include_video_link", "include_summary", "include_participants", "include_events", "include_chat"} {
		if v := cfg.ScopeValue(key); v != "" {
			if _, err := strconv.ParseBool(v); err != nil {
				return fmt.Errorf("%s: scope.%s must be true or false", platform, key)
			}
		}
	}
	return nil
}

// meetingSince returns the listing start for a sync: the cursor minus the
// overlap, or since_days back when there is no cursor.
func meetingSince(cfg ConnectorConfig, cur Cursor, now time.Time) time.Time {
	since := parsePlaudCursor(cur)
	if since.IsZero() {
		return now.Add(-time.Duration(meetingScopeInt(cfg, "since_days", meetingDefaultSinceDays)) * 24 * time.Hour)
	}
	return since.Add(-meetingRelistOverlap)
}

// meetingPage renders the knowledge page for one recorded meeting. platform
// is the connector type ("zoom", "teams"); label is the heading suffix.
func meetingPage(cfg ConnectorConfig, platform, label string, rec MeetingRecording, content MeetingContent, mediaLine string) Page {
	title := meetingTitle(platform, label, rec)
	var md strings.Builder
	fmt.Fprintf(&md, "# %s\n\n", title)
	if !rec.StartAt.IsZero() {
		fmt.Fprintf(&md, "- **Recorded:** %s\n", rec.StartAt.UTC().Format(time.RFC3339))
	}
	if rec.DurationMS > 0 {
		fmt.Fprintf(&md, "- **Duration:** %s\n", (time.Duration(rec.DurationMS) * time.Millisecond).Round(time.Second))
	}
	if rec.Host != "" {
		fmt.Fprintf(&md, "- **Host:** %s\n", rec.Host)
	}
	participants := meetingParticipants(rec, content)
	if len(participants) > 0 {
		fmt.Fprintf(&md, "- **Participants:** %s\n", strings.Join(participants, ", "))
	}
	if rec.URL != "" {
		fmt.Fprintf(&md, "- **Recording page:** %s\n", rec.URL)
	}
	if mediaLine != "" {
		md.WriteString(mediaLine)
	}
	md.WriteString("\n")
	if content.Pending {
		md.WriteString("_" + MeetingPendingText + "_\n\n")
	}
	if s := strings.TrimSpace(content.Summary); s != "" {
		fmt.Fprintf(&md, "## Summary (%s)\n\n%s\n\n", label, s)
	}
	if t := strings.TrimSpace(content.Transcript); t != "" {
		fmt.Fprintf(&md, "## Transcript\n\n%s\n\n", t)
	}
	attrs := map[string]string{
		platform + "_recording_id": rec.ID,
		meetingContentAttr:         plaudContentReady,
	}
	if content.Pending {
		attrs[meetingContentAttr] = plaudContentPending
	}
	if rec.MeetingID != "" {
		attrs[platform+"_meeting_id"] = rec.MeetingID
	}
	if rec.DurationMS > 0 {
		attrs["duration_ms"] = strconv.FormatInt(rec.DurationMS, 10)
	}
	if !rec.StartAt.IsZero() {
		attrs["recorded_at"] = rec.StartAt.UTC().Format(time.RFC3339)
	}
	if len(participants) > 0 {
		attrs["participants"] = strings.Join(participants, ", ")
	}
	if rec.Host != "" {
		attrs["host"] = rec.Host
	}
	if tags := cfg.ScopeValue("tags"); tags != "" {
		attrs["tags"] = tags
	}
	return Page{
		ID:        rec.ID,
		Title:     title,
		URL:       rec.URL,
		Markdown:  md.String(),
		UpdatedAt: rec.StartAt,
		Path:      []string{label},
		Attrs:     attrs,
	}
}

// meetingTitle is the fact title: the meeting topic, or a placeholder naming
// the platform and the recording time.
func meetingTitle(platform, label string, rec MeetingRecording) string {
	if t := strings.TrimSpace(rec.Topic); t != "" {
		return t
	}
	if !rec.StartAt.IsZero() {
		return label + " meeting " + rec.StartAt.UTC().Format("2006-01-02 15:04")
	}
	return label + " recording " + rec.ID
}

// meetingParticipants merges the listing's and the content's participant
// lists, de-duplicated and capped.
func meetingParticipants(rec MeetingRecording, content MeetingContent) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range [][]string{rec.Participants, content.Participants} {
		for _, p := range list {
			p = strings.TrimSpace(p)
			key := strings.ToLower(p)
			if p == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, p)
			if len(out) >= meetingParticipantsMax {
				return out
			}
		}
	}
	return out
}

// meetingFactNeedsRefetch reports whether a stored meeting fact has no text
// from the platform yet: pending, or holding only the hive's machine
// transcript (so the platform's own text replaces it once it exists).
func meetingFactNeedsRefetch(fm map[string]string) bool {
	switch fm["attr_"+meetingContentAttr] {
	case plaudContentPending, FactContentMachine:
		return true
	}
	return false
}

// --- WebVTT ---

// vttCueTimeRE matches a cue timing line: "00:00:01.000 --> 00:00:04.000".
var vttCueTimeRE = regexp.MustCompile(`^(\d{1,2}:)?\d{2}:\d{2}[.,]\d{3}\s+-->\s+`)

// vttVoiceRE matches the WebVTT voice tag Teams uses: <v Speaker Name>text</v>.
var vttVoiceRE = regexp.MustCompile(`(?s)^<v\s+([^>]*)>(.*?)(?:</v>)?$`)

// vttTagRE strips remaining inline tags (<c>, <i>, <b>, <lang>, timestamps).
var vttTagRE = regexp.MustCompile(`</?[a-zA-Z][^>]*>|<\d{2}:\d{2}[^>]*>`)

// vttSpeakerRE matches Zoom's "Speaker Name: text" cue text.
var vttSpeakerRE = regexp.MustCompile(`^([^:\n]{1,80}?):\s+(.*)$`)

// renderVTT turns a WebVTT transcript into "[mm:ss] Speaker: text" lines,
// one per cue, through transcribe.WriteLine: the same line shape Plaud and
// machine transcripts use. Both Zoom ("Name: text" cue bodies) and Teams ("<v Name>text</v>") speaker
// conventions are recognised; NOTE/STYLE/REGION blocks are skipped and
// consecutive duplicate lines (Teams repeats rolling cues) are collapsed.
// Anything that is not VTT is returned trimmed, as plain text.
func renderVTT(text string) string {
	text = strings.ReplaceAll(strings.TrimSpace(text), "\r\n", "\n")
	if text == "" {
		return ""
	}
	if !strings.HasPrefix(text, "WEBVTT") && !vttCueTimeRE.MatchString(firstLine(text)) {
		// Not VTT: the platform handed back plain text (or an error page the
		// caller already rejected by status).
		return text
	}
	var b strings.Builder
	lines := strings.Split(text, "\n")
	last := ""
	i := 0
	for i < len(lines) {
		line := strings.TrimSpace(lines[i])
		i++
		if line == "" || strings.HasPrefix(line, "WEBVTT") {
			continue
		}
		if strings.HasPrefix(line, "NOTE") || strings.HasPrefix(line, "STYLE") || strings.HasPrefix(line, "REGION") {
			for i < len(lines) && strings.TrimSpace(lines[i]) != "" {
				i++
			}
			continue
		}
		if !vttCueTimeRE.MatchString(line) {
			// A cue identifier: the timing line follows.
			if i < len(lines) && vttCueTimeRE.MatchString(strings.TrimSpace(lines[i])) {
				line = strings.TrimSpace(lines[i])
				i++
			} else {
				continue
			}
		}
		stamp := vttStamp(line)
		var body []string
		for i < len(lines) && strings.TrimSpace(lines[i]) != "" {
			body = append(body, strings.TrimSpace(lines[i]))
			i++
		}
		speaker, content := vttSpeakerAndText(strings.Join(body, " "))
		if content == "" {
			continue
		}
		rendered := speaker + "\x00" + content
		if rendered == last {
			continue
		}
		last = rendered
		transcribe.WriteLine(&b, stamp, speaker, content)
	}
	return strings.TrimSpace(b.String())
}

// vttStamp formats a cue's start time as mm:ss or h:mm:ss.
func vttStamp(timing string) string {
	start := strings.TrimSpace(strings.SplitN(timing, "-->", 2)[0])
	start = strings.ReplaceAll(start, ",", ".")
	parts := strings.Split(start, ":")
	var h, m int
	var s float64
	var err error
	switch len(parts) {
	case 3:
		h, _ = strconv.Atoi(parts[0])
		m, _ = strconv.Atoi(parts[1])
		s, err = strconv.ParseFloat(parts[2], 64)
	case 2:
		m, _ = strconv.Atoi(parts[0])
		s, err = strconv.ParseFloat(parts[1], 64)
	default:
		return ""
	}
	if err != nil {
		return ""
	}
	// Whole seconds, truncated: a cue at 00:04.600 is stamped 00:04.
	return transcribe.FormatOffset(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(int(s))*time.Second)
}

// vttSpeakerAndText splits a cue body into speaker and text.
func vttSpeakerAndText(body string) (speaker, text string) {
	body = strings.TrimSpace(body)
	if m := vttVoiceRE.FindStringSubmatch(body); m != nil {
		speaker = strings.TrimSpace(m[1])
		body = m[2]
	}
	body = strings.TrimSpace(vttTagRE.ReplaceAllString(body, ""))
	if speaker == "" {
		if m := vttSpeakerRE.FindStringSubmatch(body); m != nil && !strings.HasPrefix(m[1], "http") {
			speaker, body = strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
		}
	}
	return speaker, body
}

// --- token store ---

// oauthTokens is what the connector token store keeps for a user-level OAuth
// sign-in. The access token is kept only to avoid a refresh per call; the
// refresh token is what survives a restart.
type oauthTokens struct {
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	Account      string    `json:"account,omitempty"`
	Scope        string    `json:"scope,omitempty"`
}

// valid reports whether the access token can still be used.
func (t oauthTokens) valid(now time.Time) bool {
	return t.AccessToken != "" && now.Add(meetingTokenRefreshSkew).Before(t.ExpiresAt)
}

// tokenStore persists oauthTokens as a 0600 JSON file under the connector's
// private state directory (/data/knowledge/connector-state/<name>/home/...).
type tokenStore struct{ path string }

func (s tokenStore) load() (oauthTokens, error) {
	var t oauthTokens
	if s.path == "" {
		return t, os.ErrNotExist
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return t, err
	}
	if err := json.Unmarshal(data, &t); err != nil {
		return oauthTokens{}, fmt.Errorf("token store %s is corrupt: %w", filepath.Base(s.path), err)
	}
	return t, nil
}

func (s tokenStore) save(t oauthTokens) error {
	if s.path == "" {
		return errors.New("no token store configured (connector state directory missing)")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), meetingTokenFileMode); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s tokenStore) clear() error {
	if s.path == "" {
		return nil
	}
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// sortMeetingsNewestFirst orders recordings for listings.
func sortMeetingsNewestFirst(recs []MeetingRecording) {
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].StartAt.After(recs[j].StartAt) })
}

// jsonString reads a string field, accepting numbers (ids) as well.
func jsonString(m map[string]any, keys ...string) string { return plaudString(m, keys...) }

// jsonTime reads a timestamp field in RFC3339 or epoch form.
func jsonTime(m map[string]any, keys ...string) time.Time { return plaudTime(m, keys...) }

// jsonList reads an array field as maps.
func jsonList(m map[string]any, key string) []map[string]any {
	arr, _ := m[key].([]any)
	out := make([]map[string]any, 0, len(arr))
	for _, it := range arr {
		if mm, ok := it.(map[string]any); ok {
			out = append(out, mm)
		}
	}
	return out
}

// decodeJSONObject decodes a JSON object body.
func decodeJSONObject(platform string, body []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("%s: unexpected reply: %s", platform, firstLine(string(body)))
	}
	return m, nil
}
