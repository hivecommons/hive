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

// TypeFathom is the connector type for Fathom meeting recordings (the
// Fathom notetaker's transcripts, AI summaries and action items), read
// through the Fathom External API with an API key.
const TypeFathom = "fathom"

// fathomAPIBase is the Fathom External API. A variable so tests point it at
// a fake server.
var fathomAPIBase = "https://api.fathom.ai/external/v1"

const (
	// fathomLabel is the human name used in headings and messages.
	fathomLabel = "Fathom"
	// fathomMaxListedPages bounds one listing so a cursor that never ends
	// cannot loop forever.
	fathomMaxListedPages = 100
	// fathomDownloadCompleted / fathomDownloadProcessing are RecordingDownload
	// statuses.
	fathomDownloadCompleted  = "completed"
	fathomDownloadProcessing = "processing"
)

// fathomIDRE bounds a recording id (an integer).
var fathomIDRE = regexp.MustCompile(`^\d{1,20}$`)

// fathomConnector syncs Fathom meetings into one fact per recording. Auth:
// a Fathom API key through auth.env / auth.file (sent as X-Api-Key). Scope
// keys: since_days, max_recordings, include_summary, include_participants,
// recorded_by, teams, tags.
type fathomConnector struct {
	cfg    ConnectorConfig
	logger *slog.Logger
	http   *HTTPClient
	now    func() time.Time

	mu        sync.Mutex
	auth      AuthState
	truncated bool
	// downloads caches the pending download id per recording, so a second
	// ▶ Play polls the download Fathom is preparing instead of starting a
	// new one.
	downloads map[string]string
}

var (
	_ Authenticator      = (*fathomConnector)(nil)
	_ MeetingReader      = (*fathomConnector)(nil)
	_ MeetingMediaReader = (*fathomConnector)(nil)
	_ Truncator          = (*fathomConnector)(nil)
)

func newFathomConnector(cfg ConnectorConfig, deps Deps) (Connector, error) {
	return &fathomConnector{cfg: cfg, logger: deps.logger(), http: deps.httpClient(), now: time.Now, downloads: map[string]string{}}, nil
}

func (c *fathomConnector) Type() string { return TypeFathom }

// Validate checks the auth block and scope.
func (c *fathomConnector) Validate(cfg ConnectorConfig) error {
	if !cfg.Auth.Configured() {
		return errors.New("fathom: auth.env (variable name) or auth.file (path) must hold a Fathom API key (Fathom → User Settings → API Access)")
	}
	if cfg.Auth.Mode != "" || cfg.Auth.ClientIDEnv != "" || cfg.Auth.ClientSecretEnv != "" || cfg.Auth.AccountIDEnv != "" || cfg.Auth.TenantIDEnv != "" {
		return errors.New("fathom: auth.mode and the *_env client settings are not used; the API key goes in auth.env or auth.file")
	}
	for _, e := range meetingScopeList(cfg, "recorded_by") {
		if !strings.Contains(e, "@") {
			return fmt.Errorf("fathom: scope.recorded_by entry %q is not an email address", e)
		}
	}
	return validateMeetingScope("fathom", cfg)
}

// Truncated implements Truncator.
func (c *fathomConnector) Truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}

// --- REST ---

// token resolves the API key from auth.env / auth.file.
func (c *fathomConnector) token() (string, error) {
	tok, err := c.cfg.Auth.Secret()
	if err != nil {
		return "", fmt.Errorf("fathom: %w — set the API key in the environment variable or file named on the Integrations card", ErrMeetingNotConnected)
	}
	return tok, nil
}

// call performs one request against the External API and decodes the JSON
// object reply. 401 maps to "not connected"; 429 is retried by HTTPClient
// honouring Retry-After.
func (c *fathomConnector) call(ctx context.Context, method, path string, q url.Values) (map[string]any, error) {
	tok, err := c.token()
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("X-Api-Key", tok)
	h.Set("Accept", "application/json")
	u := fathomAPIBase + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var body []byte
	if method == http.MethodPost {
		h.Set("Content-Type", "application/json")
		body = []byte("{}")
	}
	resp, err := c.http.Do(ctx, method, u, h, body)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) {
			switch se.StatusCode {
			case http.StatusUnauthorized:
				return nil, meetingNotConnected("fathom", fathomLabel, err)
			case http.StatusForbidden:
				return nil, fmt.Errorf("fathom: %w — the API key's user cannot read this recording (shared with limited access)", err)
			case http.StatusTooManyRequests:
				return nil, fmt.Errorf("fathom: %w — Fathom rate limit reached (60 calls a minute, 30 for transcripts and summaries); the next sync retries", err)
			}
		}
		return nil, err
	}
	return decodeJSONObject("fathom", resp.Body)
}

// --- mapping ---

// fathomTimestamp parses an HH:MM:SS (or MM:SS) offset.
func fathomTimestamp(s string) time.Duration {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0
	}
	var total time.Duration
	for _, p := range parts {
		n, err := strconv.ParseFloat(p, 64)
		if err != nil || n < 0 {
			return 0
		}
		total = total*60 + time.Duration(n*float64(time.Second))
	}
	return total
}

// parseFathomMeeting maps a Meeting object to a recording.
func (c *fathomConnector) parseFathomMeeting(m map[string]any) MeetingRecording {
	rec := MeetingRecording{
		ID:       jsonString(m, "recording_id"),
		Topic:    jsonString(m, "title", "meeting_title"),
		StartAt:  jsonTime(m, "recording_start_time", "scheduled_start_time", "created_at"),
		URL:      jsonString(m, "share_url", "url"),
		HasMedia: true,
	}
	if end := jsonTime(m, "recording_end_time"); !end.IsZero() && end.After(rec.StartAt) {
		rec.DurationMS = end.Sub(rec.StartAt).Milliseconds()
	}
	if by, ok := m["recorded_by"].(map[string]any); ok {
		rec.Host = jsonString(by, "name", "email")
	}
	if meetingScopeBool(c.cfg, "include_participants", true) {
		for _, inv := range jsonList(m, "calendar_invitees") {
			if n := jsonString(inv, "name", "matched_speaker_display_name", "email"); n != "" {
				rec.Participants = append(rec.Participants, n)
			}
		}
	}
	return rec
}

// fathomTranscript renders transcript items as "[mm:ss] Speaker: text".
func fathomTranscript(items []map[string]any) string {
	segs := make([]transcribe.Segment, 0, len(items))
	for _, it := range items {
		speaker := ""
		if sp, ok := it["speaker"].(map[string]any); ok {
			speaker = jsonString(sp, "display_name")
		}
		segs = append(segs, transcribe.Segment{Start: fathomTimestamp(jsonString(it, "timestamp")), Speaker: speaker, Text: jsonString(it, "text")})
	}
	return transcribe.RenderSegments(segs)
}

// fathomSummary renders the default summary markdown and the action items.
func fathomSummary(summary map[string]any, actions []map[string]any) string {
	var parts []string
	if summary != nil {
		if md := jsonString(summary, "markdown_formatted"); md != "" {
			parts = append(parts, md)
		}
	}
	var lines []string
	for _, a := range actions {
		d := jsonString(a, "description")
		if d == "" {
			continue
		}
		line := "- " + d
		if as, ok := a["assignee"].(map[string]any); ok {
			if n := jsonString(as, "name", "email"); n != "" {
				line += " (" + n + ")"
			}
		}
		if done, _ := a["completed"].(bool); done {
			line += " ✓"
		}
		lines = append(lines, line)
	}
	if len(lines) > 0 {
		parts = append(parts, "### Action items\n\n"+strings.Join(lines, "\n"))
	}
	return strings.Join(parts, "\n\n")
}

// fathomContent renders a Meeting (with its included sections) as content.
func fathomContent(m map[string]any, participants []string) MeetingContent {
	out := MeetingContent{Participants: participants, Transcript: fathomTranscript(jsonList(m, "transcript"))}
	summary, _ := m["default_summary"].(map[string]any)
	out.Summary = fathomSummary(summary, jsonList(m, "action_items"))
	out.Pending = strings.TrimSpace(out.Summary) == "" && strings.TrimSpace(out.Transcript) == ""
	return out
}

// --- listing ---

// listMeetings pages GET /meetings (cursor) from since, newest first,
// capped at limit; with withText the transcript, summary and action items
// come in the same pages.
func (c *fathomConnector) listMeetings(ctx context.Context, since time.Time, limit int, withText bool) ([]map[string]any, error) {
	var out []map[string]any
	cursor := ""
	for page := 0; page < fathomMaxListedPages; page++ {
		q := url.Values{"created_after": {since.UTC().Format(time.RFC3339)}}
		for _, e := range meetingScopeList(c.cfg, "recorded_by") {
			q.Add("recorded_by[]", e)
		}
		for _, t := range meetingScopeList(c.cfg, "teams") {
			q.Add("teams[]", t)
		}
		if withText {
			q.Set("include_transcript", "true")
			if meetingScopeBool(c.cfg, "include_summary", true) {
				q.Set("include_summary", "true")
				q.Set("include_action_items", "true")
			}
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		m, err := c.call(ctx, http.MethodGet, "/meetings", q)
		if err != nil {
			return nil, err
		}
		out = append(out, jsonList(m, "items")...)
		cursor = jsonString(m, "next_cursor")
		if cursor == "" || len(out) > limit {
			break
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && c.parseFathomMeeting(out[j]).StartAt.After(c.parseFathomMeeting(out[j-1]).StartAt); j-- {
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

// --- Sync ---

// Sync lists meetings created since the cursor (minus an overlap, which also
// catches summaries Fathom finishes after the call) with transcript, summary
// and action items in the same pages, and emits one page per recording. The
// listing is incremental, so nothing is tombstoned.
func (c *fathomConnector) Sync(ctx context.Context, cur Cursor, emit func(Page) error) (Cursor, error) {
	since := meetingSince(c.cfg, cur, c.now())
	reportProgress(ctx, func(p *Progress) { p.Phase = ProgressListing })
	list, err := c.listMeetings(ctx, since, meetingScopeInt(c.cfg, "max_recordings", meetingDefaultMaxRecordings), true)
	if err != nil {
		c.setAuthFromError(err)
		return cur, err
	}
	total := len(list)
	reportProgress(ctx, func(p *Progress) { p.Phase, p.ItemsTotal = ProgressFetching, total })
	newest := parsePlaudCursor(cur)
	tags := meetingScopeList(c.cfg, "tags")
	for _, m := range list {
		if err := ctx.Err(); err != nil {
			return cur, err
		}
		rec := c.parseFathomMeeting(m)
		if rec.ID == "" {
			continue
		}
		title := progressTitle(meetingTitle(TypeFathom, fathomLabel, rec))
		reportProgress(ctx, func(p *Progress) { p.Phase, p.Current = ProgressFetching, title })
		rec.Tags = tags
		if err := emit(meetingPage(c.cfg, TypeFathom, fathomLabel, rec, fathomContent(m, rec.Participants), "")); err != nil {
			return cur, err
		}
		reportProgress(ctx, func(p *Progress) { p.ItemsDone++ })
		if rec.StartAt.After(newest) {
			newest = rec.StartAt
		}
	}
	c.refreshAuth(ctx)
	if !newest.IsZero() {
		return Cursor(newest.UTC().Format(time.RFC3339)), nil
	}
	return cur, nil
}

// --- MeetingReader / MeetingMediaReader ---

// ListMeetings lists recordings within since_days, newest first.
func (c *fathomConnector) ListMeetings(ctx context.Context) ([]MeetingRecording, error) {
	since := c.now().Add(-time.Duration(meetingScopeInt(c.cfg, "since_days", meetingDefaultSinceDays)) * 24 * time.Hour)
	list, err := c.listMeetings(ctx, since, meetingScopeInt(c.cfg, "max_recordings", meetingDefaultMaxRecordings), false)
	if err != nil {
		c.setAuthFromError(err)
		return nil, err
	}
	tags := meetingScopeList(c.cfg, "tags")
	out := make([]MeetingRecording, 0, len(list))
	for _, m := range list {
		rec := c.parseFathomMeeting(m)
		if rec.ID == "" {
			continue
		}
		rec.Topic = meetingTitle(TypeFathom, fathomLabel, rec)
		rec.Tags = tags
		out = append(out, rec)
	}
	sortMeetingsNewestFirst(out)
	return out, nil
}

// ReadMeeting reads one recording's transcript and summary
// (GET /recordings/{id}/transcript and /summary). Action items come only
// with a sync, which reads them from /meetings.
func (c *fathomConnector) ReadMeeting(ctx context.Context, id string) (MeetingContent, error) {
	if !fathomIDRE.MatchString(id) {
		return MeetingContent{}, fmt.Errorf("fathom: invalid recording id %q", id)
	}
	tm, err := c.call(ctx, http.MethodGet, "/recordings/"+id+"/transcript", nil)
	if err != nil {
		return MeetingContent{}, err
	}
	out := MeetingContent{Transcript: fathomTranscript(jsonList(tm, "transcript"))}
	if meetingScopeBool(c.cfg, "include_summary", true) {
		sm, err := c.call(ctx, http.MethodGet, "/recordings/"+id+"/summary", nil)
		if err != nil {
			if errors.Is(err, ErrMeetingNotConnected) {
				return MeetingContent{}, err
			}
			c.logger.Debug("fathom summary unavailable", "recording", id, "error", err)
		} else {
			summary, _ := sm["summary"].(map[string]any)
			out.Summary = fathomSummary(summary, nil)
		}
	}
	out.Pending = strings.TrimSpace(out.Summary) == "" && strings.TrimSpace(out.Transcript) == ""
	return out, nil
}

// MediaLink implements MeetingMediaReader through Fathom's recording
// downloads: POST /recordings/{id}/download starts one (audio-only
// recordings complete at once), GET /recordings/{id}/downloads/{download}
// polls it, and a completed download carries a signed, token-free url valid
// until expires_at (about 24 hours). While Fathom is still generating the
// file the call answers "no media yet"; the next ▶ Play polls the same
// download.
func (c *fathomConnector) MediaLink(ctx context.Context, id string) (MediaLink, error) {
	if !fathomIDRE.MatchString(id) {
		return MediaLink{}, fmt.Errorf("fathom: invalid recording id %q", id)
	}
	c.mu.Lock()
	dl := c.downloads[id]
	c.mu.Unlock()
	var m map[string]any
	var err error
	if dl != "" {
		m, err = c.call(ctx, http.MethodGet, "/recordings/"+id+"/downloads/"+url.PathEscape(dl), nil)
		if err == nil {
			if st := jsonString(m, "status"); st != fathomDownloadCompleted && st != fathomDownloadProcessing {
				m = nil // failed or expired: start a new one
			}
		}
	}
	if m == nil || err != nil {
		m, err = c.call(ctx, http.MethodPost, "/recordings/"+id+"/download", nil)
	}
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) && (se.StatusCode == http.StatusUnprocessableEntity || se.StatusCode == http.StatusNotFound) {
			return MediaLink{}, fmt.Errorf("fathom: %w — Fathom has no downloadable media for recording %s", ErrMeetingNoMedia, id)
		}
		return MediaLink{}, err
	}
	switch jsonString(m, "status") {
	case fathomDownloadCompleted:
		c.mu.Lock()
		delete(c.downloads, id)
		c.mu.Unlock()
		for _, key := range []string{"audio", "video"} {
			f, ok := m[key].(map[string]any)
			if !ok {
				continue
			}
			raw := jsonString(f, "url")
			u, perr := url.Parse(raw)
			if perr != nil || u.Scheme != "https" || u.Host == "" {
				return MediaLink{}, fmt.Errorf("fathom: %w — the %s download is not an https link", ErrMeetingNoMedia, key)
			}
			exp := jsonTime(f, "expires_at")
			if exp.IsZero() {
				exp = c.now().Add(meetingMediaLinkTTLFallback)
			}
			return MediaLink{URL: raw, ExpiresAt: exp}, nil
		}
		return MediaLink{}, fmt.Errorf("fathom: %w — the completed download of %s has no file", ErrMeetingNoMedia, id)
	case fathomDownloadProcessing:
		c.mu.Lock()
		c.downloads[id] = jsonString(m, "download_id")
		c.mu.Unlock()
		return MediaLink{}, fmt.Errorf("fathom: %w yet — Fathom is still preparing the download of %s; try again in a minute", ErrMeetingNoMedia, id)
	default:
		return MediaLink{}, fmt.Errorf("fathom: %w — the download of %s %s (%s)", ErrMeetingNoMedia, id, jsonString(m, "status"), jsonString(m, "failure_reason"))
	}
}

// --- Authenticator ---

// AuthStatus implements Authenticator. With refresh it lists one page of
// meetings, so a dead key shows on the card before the first sync.
func (c *fathomConnector) AuthStatus(ctx context.Context, refresh bool) AuthState {
	if refresh {
		c.refreshAuth(ctx)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auth
}

func (c *fathomConnector) refreshAuth(ctx context.Context) {
	m, err := c.call(ctx, http.MethodGet, "/meetings", nil)
	if err != nil {
		c.setAuthFromError(err)
		return
	}
	account := ""
	if items := jsonList(m, "items"); len(items) > 0 {
		if by, ok := items[0]["recorded_by"].(map[string]any); ok {
			account = jsonString(by, "email", "name")
		}
	}
	c.setAuth(AuthState{Connected: true, CheckedAt: c.now().UTC(), Account: account})
}

func (c *fathomConnector) setAuth(st AuthState) {
	c.mu.Lock()
	c.auth = st
	c.mu.Unlock()
}

func (c *fathomConnector) setAuthFromError(err error) {
	c.setAuth(AuthState{CheckedAt: c.now().UTC(), Message: firstLine(err.Error())})
}

// Connect implements Authenticator: an API key has no browser step, so
// Connect probes Fathom and reports the outcome.
func (c *fathomConnector) Connect(ctx context.Context) (AuthState, error) {
	c.refreshAuth(ctx)
	return c.AuthStatus(ctx, false), nil
}

// Disconnect implements Authenticator: forgets the cached state and pending
// downloads. The API key lives in the environment.
func (c *fathomConnector) Disconnect(ctx context.Context) (AuthState, error) {
	c.mu.Lock()
	c.downloads = map[string]string{}
	c.mu.Unlock()
	st := AuthState{CheckedAt: c.now().UTC(), Message: "cached state cleared; the API key in the environment is unchanged (delete it in Fathom → User Settings → API Access to revoke)"}
	c.setAuth(st)
	return st, nil
}
