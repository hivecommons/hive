package connector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/transcribe"
)

// TypeGranola is the connector type for Granola meeting notes: the AI
// notes, the owner's own notes and the transcript of each meeting, read
// through the Granola public API with an API key.
const TypeGranola = "granola"

// granolaAPIBase is the Granola public API. A variable so tests point it at
// a fake server.
var granolaAPIBase = "https://public-api.granola.ai"

const (
	// granolaLabel is the human name used in headings and messages.
	granolaLabel = "Granola"
	// granolaPageSize is the page_size of one notes listing (the API
	// maximum).
	granolaPageSize = 30
	// granolaTranscriptPageSize is the page_size of one transcript page (the
	// API maximum), used when a transcript is too large to inline.
	granolaTranscriptPageSize = 100
	// granolaMaxListedPages bounds a listing so a cursor that never ends
	// cannot loop forever.
	granolaMaxListedPages = 200
)

// granolaIDRE bounds a note id (not_ + 14 alphanumerics, case-insensitive).
var granolaIDRE = regexp.MustCompile(`^[Nn][Oo][Tt]_[A-Za-z0-9]{14}$`)

// granolaConnector syncs Granola notes into one fact per meeting. Auth: a
// Granola API key (grn_…) through auth.env / auth.file. Scope keys:
// since_days, max_recordings, folder, space, include_private_notes,
// include_participants, tags.
type granolaConnector struct {
	cfg    ConnectorConfig
	logger *slog.Logger
	http   *HTTPClient
	now    func() time.Time

	mu        sync.Mutex
	auth      AuthState
	truncated bool
}

var (
	_ Authenticator      = (*granolaConnector)(nil)
	_ MeetingReader      = (*granolaConnector)(nil)
	_ MeetingMediaReader = (*granolaConnector)(nil)
	_ Truncator          = (*granolaConnector)(nil)
)

func newGranolaConnector(cfg ConnectorConfig, deps Deps) (Connector, error) {
	return &granolaConnector{cfg: cfg, logger: deps.logger(), http: deps.httpClient(), now: time.Now}, nil
}

func (c *granolaConnector) Type() string { return TypeGranola }

// Validate checks the auth block and scope.
func (c *granolaConnector) Validate(cfg ConnectorConfig) error {
	if !cfg.Auth.Configured() {
		return errors.New("granola: auth.env (variable name) or auth.file (path) must hold a Granola API key (grn_…, Granola → Settings → API)")
	}
	if cfg.Auth.Mode != "" || cfg.Auth.ClientIDEnv != "" || cfg.Auth.ClientSecretEnv != "" || cfg.Auth.AccountIDEnv != "" || cfg.Auth.TenantIDEnv != "" {
		return errors.New("granola: auth.mode and the *_env client settings are not used; the API key goes in auth.env or auth.file")
	}
	if v := cfg.ScopeValue("include_private_notes"); v != "" {
		if _, err := strconv.ParseBool(v); err != nil {
			return errors.New("granola: scope.include_private_notes must be true or false")
		}
	}
	return validateMeetingScope("granola", cfg)
}

// Truncated implements Truncator.
func (c *granolaConnector) Truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}

// --- REST ---

// errGranolaNotFound marks a note the key cannot read, or one Granola has
// not generated notes for yet (NOTE_NOT_FOUND).
var errGranolaNotFound = errors.New("granola: note not found (not readable with this key, or no notes generated yet)")

// errGranolaTooLarge is the 413 Granola answers when a transcript is too
// large to inline in the note.
var errGranolaTooLarge = errors.New("granola: transcript too large to inline")

// token resolves the API key from auth.env / auth.file.
func (c *granolaConnector) token() (string, error) {
	tok, err := c.cfg.Auth.Secret()
	if err != nil {
		return "", fmt.Errorf("granola: %w — set the API key in the environment variable or file named on the Integrations card", ErrMeetingNotConnected)
	}
	return tok, nil
}

// get performs one GET and decodes the JSON object reply. 401/403 map to
// "not connected", 404 to errGranolaNotFound, 413 to errGranolaTooLarge;
// 429 is retried by HTTPClient honouring Retry-After.
func (c *granolaConnector) get(ctx context.Context, path string, q url.Values) (map[string]any, error) {
	tok, err := c.token()
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+tok)
	h.Set("Accept", "application/json")
	u := granolaAPIBase + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	resp, err := c.http.Get(ctx, u, h)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) {
			switch se.StatusCode {
			case http.StatusUnauthorized, http.StatusForbidden:
				return nil, meetingNotConnected("granola", granolaLabel, err)
			case http.StatusNotFound:
				return nil, errGranolaNotFound
			case http.StatusRequestEntityTooLarge:
				return nil, errGranolaTooLarge
			case http.StatusTooManyRequests:
				return nil, fmt.Errorf("granola: %w — Granola rate limit reached; the next sync retries", err)
			}
		}
		return nil, err
	}
	return decodeJSONObject("granola", resp.Body)
}

// --- mapping ---

// granolaUser renders a User object as its name, else email.
func granolaUser(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	return jsonString(m, "name", "email")
}

// parseGranolaNote maps a Note (or NoteSummary) to a recording.
func (c *granolaConnector) parseGranolaNote(n map[string]any) MeetingRecording {
	rec := MeetingRecording{
		ID:    jsonString(n, "id"),
		Topic: jsonString(n, "title"),
		URL:   jsonString(n, "web_url"),
		Host:  granolaUser(n["owner"]),
	}
	ev, _ := n["calendar_event"].(map[string]any)
	if rec.Topic == "" && ev != nil {
		rec.Topic = jsonString(ev, "event_title")
	}
	if ev != nil {
		rec.StartAt = jsonTime(ev, "scheduled_start_time")
		if end := jsonTime(ev, "scheduled_end_time"); !end.IsZero() && end.After(rec.StartAt) && !rec.StartAt.IsZero() {
			rec.DurationMS = end.Sub(rec.StartAt).Milliseconds()
		}
	}
	if rec.StartAt.IsZero() {
		rec.StartAt = jsonTime(n, "created_at")
	}
	if tr := jsonList(n, "transcript"); len(tr) > 0 {
		first, last := jsonTime(tr[0], "start_time"), jsonTime(tr[len(tr)-1], "end_time", "start_time")
		if !first.IsZero() && last.After(first) {
			rec.DurationMS = last.Sub(first).Milliseconds()
		}
	}
	if meetingScopeBool(c.cfg, "include_participants", true) {
		for _, a := range jsonList(n, "attendees") {
			if s := jsonString(a, "name", "email"); s != "" {
				rec.Participants = append(rec.Participants, s)
			}
		}
		if len(rec.Participants) == 0 && ev != nil {
			for _, inv := range jsonList(ev, "invitees") {
				if s := jsonString(inv, "email"); s != "" {
					rec.Participants = append(rec.Participants, s)
				}
			}
		}
	}
	return rec
}

// granolaTranscript renders transcript items as "[mm:ss] Speaker: text",
// offsets relative to the first item. Granola labels the device owner's
// microphone "me" and the call audio "them" when it has no name.
func granolaTranscript(items []map[string]any) string {
	var origin time.Time
	segs := make([]transcribe.Segment, 0, len(items))
	for _, it := range items {
		t := jsonTime(it, "start_time")
		if origin.IsZero() {
			origin = t
		}
		var off time.Duration
		if !t.IsZero() && t.After(origin) {
			off = t.Sub(origin)
		}
		speaker := ""
		if sp, ok := it["speaker"].(map[string]any); ok {
			speaker = jsonString(sp, "name", "diarization_label")
			if speaker == "" {
				switch jsonString(sp, "attribution") {
				case "me":
					speaker = "Me"
				case "them":
					speaker = "Them"
				}
			}
		}
		segs = append(segs, transcribe.Segment{Start: off, Speaker: speaker, Text: jsonString(it, "text")})
	}
	return transcribe.RenderSegments(segs)
}

// granolaContent renders the AI notes (and, when enabled, the owner's own
// notes) as the summary and the transcript as the transcript.
func (c *granolaConnector) granolaContent(n map[string]any, transcript []map[string]any, participants []string) MeetingContent {
	out := MeetingContent{Participants: participants, Transcript: granolaTranscript(transcript)}
	var parts []string
	if s := jsonString(n, "summary_markdown", "summary_text"); s != "" {
		parts = append(parts, s)
	}
	if meetingScopeBool(c.cfg, "include_private_notes", true) {
		if p := jsonString(n, "private_notes_markdown", "private_notes_text"); p != "" {
			parts = append(parts, "### My notes\n\n"+p)
		}
	}
	out.Summary = strings.Join(parts, "\n\n")
	out.Pending = strings.TrimSpace(out.Summary) == "" && strings.TrimSpace(out.Transcript) == ""
	return out
}

// --- listing ---

// listNotes pages GET /v1/notes (updated_after, cursor, page_size 30) and
// returns the NoteSummary objects, newest first, capped at limit.
func (c *granolaConnector) listNotes(ctx context.Context, since time.Time, limit int) ([]map[string]any, error) {
	var out []map[string]any
	cursor := ""
	for page := 0; page < granolaMaxListedPages; page++ {
		q := url.Values{"updated_after": {since.UTC().Format(time.RFC3339)}, "page_size": {strconv.Itoa(granolaPageSize)}}
		if f := c.cfg.ScopeValue("folder"); f != "" {
			q.Set("folder_id", f)
		}
		if s := c.cfg.ScopeValue("space"); s != "" {
			q.Set("space_id", s)
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		m, err := c.get(ctx, "/v1/notes", q)
		if err != nil {
			return nil, err
		}
		for _, n := range jsonList(m, "notes") {
			if jsonString(n, "deleted_at") == "" {
				out = append(out, n)
			}
		}
		more, _ := m["hasMore"].(bool)
		cursor = jsonString(m, "cursor")
		if !more || cursor == "" || len(out) > limit {
			break
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && jsonTime(out[j], "created_at").After(jsonTime(out[j-1], "created_at")); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	truncated := false
	if len(out) > limit {
		out, truncated = out[:limit], true
	}
	c.mu.Lock()
	c.truncated = truncated
	c.mu.Unlock()
	return out, nil
}

// getNote reads one note with its transcript inlined, falling back to the
// paged transcript endpoint when Granola answers 413.
func (c *granolaConnector) getNote(ctx context.Context, id string) (map[string]any, []map[string]any, error) {
	if !granolaIDRE.MatchString(id) {
		return nil, nil, fmt.Errorf("granola: invalid note id %q", id)
	}
	n, err := c.get(ctx, "/v1/notes/"+id, url.Values{"include": {"transcript"}})
	if err == nil {
		return n, jsonList(n, "transcript"), nil
	}
	if !errors.Is(err, errGranolaTooLarge) {
		return nil, nil, err
	}
	n, err = c.get(ctx, "/v1/notes/"+id, nil)
	if err != nil {
		return nil, nil, err
	}
	var items []map[string]any
	cursor := ""
	for page := 0; page < granolaMaxListedPages; page++ {
		q := url.Values{"page_size": {strconv.Itoa(granolaTranscriptPageSize)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		m, err := c.get(ctx, "/v1/notes/"+id+"/transcript", q)
		if err != nil {
			return nil, nil, err
		}
		items = append(items, jsonList(m, "transcript")...)
		more, _ := m["hasMore"].(bool)
		cursor = jsonString(m, "cursor")
		if !more || cursor == "" {
			break
		}
	}
	return n, items, nil
}

// --- Sync ---

// Sync lists notes updated since the cursor (minus an overlap), reads each
// with its transcript and emits one page per meeting. A note Granola has not
// generated notes for yet answers NOTE_NOT_FOUND and is picked up by a later
// sync. The listing is incremental, so nothing is tombstoned.
func (c *granolaConnector) Sync(ctx context.Context, cur Cursor, emit func(Page) error) (Cursor, error) {
	since := meetingSince(c.cfg, cur, c.now())
	reportProgress(ctx, func(p *Progress) { p.Phase = ProgressListing })
	list, err := c.listNotes(ctx, since, meetingScopeInt(c.cfg, "max_recordings", meetingDefaultMaxRecordings))
	if err != nil {
		c.setAuthFromError(err)
		return cur, err
	}
	total := len(list)
	reportProgress(ctx, func(p *Progress) { p.Phase, p.ItemsTotal = ProgressFetching, total })
	newest := parsePlaudCursor(cur)
	tags := meetingScopeList(c.cfg, "tags")
	var skipped []string
	for _, s := range list {
		if err := ctx.Err(); err != nil {
			return cur, err
		}
		id := jsonString(s, "id")
		title := progressTitle(jsonString(s, "title"))
		reportProgress(ctx, func(p *Progress) { p.Phase, p.Current = ProgressFetching, title })
		n, tr, err := c.getNote(ctx, id)
		if err != nil {
			if errors.Is(err, ErrMeetingNotConnected) {
				c.setAuthFromError(err)
				return cur, err
			}
			if !errors.Is(err, errGranolaNotFound) {
				skipped = append(skipped, fmt.Sprintf("%s: %v", id, err))
			}
			reportProgress(ctx, func(p *Progress) { p.ItemsDone++ })
			continue
		}
		n["transcript"] = anyList(tr)
		rec := c.parseGranolaNote(n)
		rec.Tags = tags
		if err := emit(meetingPage(c.cfg, TypeGranola, granolaLabel, rec, c.granolaContent(n, tr, rec.Participants), "")); err != nil {
			return cur, err
		}
		reportProgress(ctx, func(p *Progress) { p.ItemsDone++ })
		if u := jsonTime(s, "updated_at"); u.After(newest) {
			newest = u
		}
	}
	c.refreshAuth(ctx)
	next := cur
	if !newest.IsZero() {
		next = Cursor(newest.UTC().Format(time.RFC3339))
	}
	if len(skipped) > 0 {
		return next, &PartialError{Err: fmt.Errorf("granola: %d note(s) skipped: %s", len(skipped), strings.Join(skipped, "; "))}
	}
	return next, nil
}

// anyList turns decoded maps back into a JSON-shaped list.
func anyList(list []map[string]any) []any {
	out := make([]any, len(list))
	for i, m := range list {
		out[i] = m
	}
	return out
}

// --- MeetingReader / MeetingMediaReader ---

// ListMeetings lists notes updated within since_days, newest first.
func (c *granolaConnector) ListMeetings(ctx context.Context) ([]MeetingRecording, error) {
	since := c.now().Add(-time.Duration(meetingScopeInt(c.cfg, "since_days", meetingDefaultSinceDays)) * 24 * time.Hour)
	list, err := c.listNotes(ctx, since, meetingScopeInt(c.cfg, "max_recordings", meetingDefaultMaxRecordings))
	if err != nil {
		c.setAuthFromError(err)
		return nil, err
	}
	tags := meetingScopeList(c.cfg, "tags")
	out := make([]MeetingRecording, 0, len(list))
	for _, n := range list {
		rec := c.parseGranolaNote(n)
		if rec.ID == "" {
			continue
		}
		rec.Topic = meetingTitle(TypeGranola, granolaLabel, rec)
		rec.Tags = tags
		out = append(out, rec)
	}
	sortMeetingsNewestFirst(out)
	return out, nil
}

// ReadMeeting reads one note's AI notes and transcript.
func (c *granolaConnector) ReadMeeting(ctx context.Context, id string) (MeetingContent, error) {
	n, tr, err := c.getNote(ctx, id)
	if err != nil {
		return MeetingContent{}, err
	}
	rec := c.parseGranolaNote(n)
	return c.granolaContent(n, tr, rec.Participants), nil
}

// MediaLink implements MeetingMediaReader: Granola transcribes on the device
// and keeps no audio or video, so there is never a playable link.
func (c *granolaConnector) MediaLink(ctx context.Context, id string) (MediaLink, error) {
	if !granolaIDRE.MatchString(id) {
		return MediaLink{}, fmt.Errorf("granola: invalid note id %q", id)
	}
	return MediaLink{}, fmt.Errorf("granola: %w — Granola keeps no meeting audio; only the notes and transcript text exist", ErrMeetingNoMedia)
}

// --- Authenticator ---

// AuthStatus implements Authenticator. With refresh it lists one note, so a
// dead key shows on the card before the first sync.
func (c *granolaConnector) AuthStatus(ctx context.Context, refresh bool) AuthState {
	if refresh {
		c.refreshAuth(ctx)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auth
}

func (c *granolaConnector) refreshAuth(ctx context.Context) {
	m, err := c.get(ctx, "/v1/notes", url.Values{"page_size": {"1"}})
	if err != nil {
		c.setAuthFromError(err)
		return
	}
	account := ""
	if notes := jsonList(m, "notes"); len(notes) > 0 {
		if o, ok := notes[0]["owner"].(map[string]any); ok {
			account = jsonString(o, "email", "name")
		}
	}
	c.setAuth(AuthState{Connected: true, CheckedAt: c.now().UTC(), Account: account})
}

func (c *granolaConnector) setAuth(st AuthState) {
	c.mu.Lock()
	c.auth = st
	c.mu.Unlock()
}

func (c *granolaConnector) setAuthFromError(err error) {
	c.setAuth(AuthState{CheckedAt: c.now().UTC(), Message: firstLine(err.Error())})
}

// Connect implements Authenticator: an API key has no browser step, so
// Connect probes Granola and reports the outcome.
func (c *granolaConnector) Connect(ctx context.Context) (AuthState, error) {
	c.refreshAuth(ctx)
	return c.AuthStatus(ctx, false), nil
}

// Disconnect implements Authenticator: forgets the cached state. The API key
// lives in the environment.
func (c *granolaConnector) Disconnect(ctx context.Context) (AuthState, error) {
	st := AuthState{CheckedAt: c.now().UTC(), Message: "cached state cleared; the API key in the environment is unchanged (revoke it in Granola's API settings)"}
	c.setAuth(st)
	return st, nil
}
