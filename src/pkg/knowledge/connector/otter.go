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

// TypeOtter is the connector type for Otter.ai conversations (recordings,
// transcripts, AI summaries, outlines and action items), read through the
// Otter.ai Public API with an API key (Enterprise workspaces).
const TypeOtter = "otter"

// otterAPIBase is the Otter.ai Public API. A variable so tests point it at a
// fake server.
var otterAPIBase = "https://api.otter.ai/v1"

const (
	// otterLabel is the human name used in headings and messages.
	otterLabel = "Otter"
	// otterPageSize is the `limit` of one conversations page (the API
	// maximum).
	otterPageSize = 20
	// otterMaxListedPages bounds a listing so a cursor that never ends cannot
	// loop forever.
	otterMaxListedPages = 200
	// otterInclude is the relationship set read per conversation.
	otterInclude = "action_items,outline,transcript"
)

// otterIDRE bounds a conversation id.
var otterIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// otterSpeakerLineRE matches the head of a transcript block in Otter's txt
// format: "Jane Doe  00:07" (speaker, two or more spaces, offset).
var otterSpeakerLineRE = regexp.MustCompile(`^(.*?)\s{2,}(\d{1,2}(?::\d{2}){1,2})$`)

// otterConnector syncs Otter.ai conversations into one fact per recording.
// Auth: an Otter API key through auth.env / auth.file. Scope keys:
// since_days, max_recordings, include_shared, channel, include_summary,
// include_participants, tags.
type otterConnector struct {
	cfg    ConnectorConfig
	logger *slog.Logger
	http   *HTTPClient
	now    func() time.Time

	mu        sync.Mutex
	auth      AuthState
	truncated bool
}

var (
	_ Authenticator      = (*otterConnector)(nil)
	_ MeetingReader      = (*otterConnector)(nil)
	_ MeetingMediaReader = (*otterConnector)(nil)
	_ Truncator          = (*otterConnector)(nil)
)

func newOtterConnector(cfg ConnectorConfig, deps Deps) (Connector, error) {
	return &otterConnector{cfg: cfg, logger: deps.logger(), http: deps.httpClient(), now: time.Now}, nil
}

func (c *otterConnector) Type() string { return TypeOtter }

// Validate checks the auth block and scope.
func (c *otterConnector) Validate(cfg ConnectorConfig) error {
	if !cfg.Auth.Configured() {
		return errors.New("otter: auth.env (variable name) or auth.file (path) must hold an Otter.ai API key (Otter → Integrations → Developer; Enterprise workspaces)")
	}
	if cfg.Auth.Mode != "" || cfg.Auth.ClientIDEnv != "" || cfg.Auth.ClientSecretEnv != "" || cfg.Auth.AccountIDEnv != "" || cfg.Auth.TenantIDEnv != "" {
		return errors.New("otter: auth.mode and the *_env client settings are not used; the API key goes in auth.env or auth.file")
	}
	if v := cfg.ScopeValue("include_shared"); v != "" {
		if _, err := strconv.ParseBool(v); err != nil {
			return errors.New("otter: scope.include_shared must be true or false")
		}
	}
	if ch := cfg.ScopeValue("channel"); ch != "" && !otterIDRE.MatchString(ch) {
		return fmt.Errorf("otter: scope.channel %q is not an Otter channel id", ch)
	}
	return validateMeetingScope("otter", cfg)
}

// Truncated implements Truncator.
func (c *otterConnector) Truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}

// --- REST ---

// token resolves the API key from auth.env / auth.file.
func (c *otterConnector) token() (string, error) {
	tok, err := c.cfg.Auth.Secret()
	if err != nil {
		return "", fmt.Errorf("otter: %w — set the API key in the environment variable or file named on the Integrations card", ErrMeetingNotConnected)
	}
	return tok, nil
}

// get performs one GET and decodes the JSON object reply. 401 maps to "not
// connected" (a wrong or deleted key, or a workspace without the Public
// API); 429 is retried by HTTPClient honouring Retry-After.
func (c *otterConnector) get(ctx context.Context, path string, q url.Values) (map[string]any, error) {
	tok, err := c.token()
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+tok)
	h.Set("Accept", "application/json")
	u := otterAPIBase + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	resp, err := c.http.Get(ctx, u, h)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) {
			switch se.StatusCode {
			case http.StatusUnauthorized:
				return nil, meetingNotConnected("otter", otterLabel, fmt.Errorf("%w (a wrong or deleted key, or a workspace without the Public API)", err))
			case http.StatusForbidden:
				return nil, fmt.Errorf("otter: %w — the key was accepted but Otter refused this request", err)
			case http.StatusNotFound:
				return nil, fmt.Errorf("otter: %w — conversation or channel not found", err)
			case http.StatusTooManyRequests:
				return nil, fmt.Errorf("otter: %w — Otter rate limit reached (10 requests a second); the next sync retries", err)
			}
		}
		return nil, err
	}
	return decodeJSONObject("otter", resp.Body)
}

// --- mapping ---

// otterUser renders a User object as its name, else email.
func otterUser(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	return jsonString(m, "name", "email")
}

// parseOtterConversation maps a Conversation object to a recording.
func (c *otterConnector) parseOtterConversation(m map[string]any) MeetingRecording {
	rec := MeetingRecording{
		ID:       jsonString(m, "id"),
		Topic:    jsonString(m, "title"),
		StartAt:  jsonTime(m, "created_at"),
		URL:      jsonString(m, "url"),
		Host:     otterUser(m["owner"]),
		HasMedia: true,
	}
	if meetingScopeBool(c.cfg, "include_participants", true) {
		for _, g := range jsonList(m, "calendar_guests") {
			if s := jsonString(g, "name", "email"); s != "" {
				rec.Participants = append(rec.Participants, s)
			}
		}
	}
	return rec
}

// otterOffset parses "mm:ss" or "h:mm:ss".
func otterOffset(s string) time.Duration {
	var total time.Duration
	for _, p := range strings.Split(s, ":") {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0
		}
		total = total*60 + time.Duration(n)*time.Second
	}
	return total
}

// otterTranscript renders Otter's txt transcript ("Speaker  00:07" heads,
// text lines, blank-line separated blocks) as "[mm:ss] Speaker: text"
// lines, and returns the last offset seen. Text in any other shape is
// returned trimmed.
func otterTranscript(txt string) (string, time.Duration) {
	txt = strings.TrimSpace(strings.ReplaceAll(txt, "\r\n", "\n"))
	if txt == "" {
		return "", 0
	}
	var b strings.Builder
	var last time.Duration
	matched := false
	for _, block := range strings.Split(txt, "\n\n") {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		if len(lines) == 0 {
			continue
		}
		m := otterSpeakerLineRE.FindStringSubmatch(strings.TrimSpace(lines[0]))
		if m == nil {
			if t := strings.TrimSpace(strings.Join(lines, " ")); t != "" {
				transcribe.WriteLine(&b, "", "", t)
			}
			continue
		}
		matched = true
		off := otterOffset(m[2])
		if off > last {
			last = off
		}
		if text := strings.TrimSpace(strings.Join(lines[1:], " ")); text != "" {
			transcribe.WriteLine(&b, transcribe.FormatOffset(off), strings.TrimSpace(m[1]), text)
		}
	}
	if !matched {
		return txt, 0
	}
	return strings.TrimSpace(b.String()), last
}

// otterSummary renders the abstract summary, the outline and the action
// items.
func otterSummary(conv map[string]any, rel map[string]any) string {
	var parts []string
	if s := jsonString(conv, "abstract_summary"); s != "" {
		parts = append(parts, s)
	}
	var outline []string
	for _, sec := range jsonList(rel, "outline") {
		if name := jsonString(sec, "section"); name != "" {
			outline = append(outline, "- **"+name+"**")
		}
		for _, t := range firefliesStrings(sec["text"]) {
			outline = append(outline, "  - "+t)
		}
	}
	if len(outline) > 0 {
		parts = append(parts, "### Outline\n\n"+strings.Join(outline, "\n"))
	}
	var items []string
	for _, a := range jsonList(rel, "action_items") {
		t := jsonString(a, "text")
		if t == "" {
			continue
		}
		line := "- " + t
		if who := otterUser(a["assignee"]); who != "" {
			line += " (" + who + ")"
		}
		if st, ok := a["status"].(map[string]any); ok {
			if done, _ := st["completed"].(bool); done {
				line += " ✓"
			}
		}
		items = append(items, line)
	}
	if len(items) > 0 {
		parts = append(parts, "### Action items\n\n"+strings.Join(items, "\n"))
	}
	return strings.Join(parts, "\n\n")
}

// otterContent renders one conversation read with its relationships.
func (c *otterConnector) otterContent(conv map[string]any, participants []string) (MeetingContent, time.Duration) {
	rel, _ := conv["relationships"].(map[string]any)
	out := MeetingContent{Participants: participants}
	var last time.Duration
	if tr, ok := rel["transcript"].(map[string]any); ok {
		out.Transcript, last = otterTranscript(jsonString(tr, "content"))
	}
	if meetingScopeBool(c.cfg, "include_summary", true) {
		out.Summary = otterSummary(conv, rel)
	}
	out.Pending = strings.TrimSpace(out.Summary) == "" && strings.TrimSpace(out.Transcript) == ""
	return out, last
}

// --- listing ---

// listConversations pages GET /conversations (newest first, cursor, limit
// 20) until a conversation older than since, capped at limit.
func (c *otterConnector) listConversations(ctx context.Context, since time.Time, limit int) ([]map[string]any, error) {
	var out []map[string]any
	cursor := ""
	done := false
	for page := 0; page < otterMaxListedPages && !done; page++ {
		q := url.Values{"limit": {strconv.Itoa(otterPageSize)}}
		if ch := c.cfg.ScopeValue("channel"); ch != "" {
			q.Set("channel_id", ch)
		} else if meetingScopeBool(c.cfg, "include_shared", false) {
			q.Set("include_shared", "true")
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		m, err := c.get(ctx, "/conversations", q)
		if err != nil {
			return nil, err
		}
		for _, conv := range jsonList(m, "data") {
			if t := jsonTime(conv, "created_at"); !t.IsZero() && t.Before(since) {
				done = true
				break
			}
			out = append(out, conv)
		}
		meta, _ := m["meta"].(map[string]any)
		more, _ := meta["has_more"].(bool)
		cursor = jsonString(meta, "next_cursor")
		if !more || cursor == "" || len(out) > limit {
			break
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

// getConversation reads one conversation with its transcript, outline and
// action items.
func (c *otterConnector) getConversation(ctx context.Context, id string) (map[string]any, error) {
	if !otterIDRE.MatchString(id) {
		return nil, fmt.Errorf("otter: invalid conversation id %q", id)
	}
	m, err := c.get(ctx, "/conversations/"+id, url.Values{"include": {otterInclude}})
	if err != nil {
		return nil, err
	}
	conv, ok := m["data"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("otter: conversation %s not found", id)
	}
	return conv, nil
}

// --- Sync ---

// Sync lists conversations created since the cursor (minus an overlap, which
// also catches summaries Otter finishes later), reads each with its
// relationships and emits one page per conversation. The listing is
// incremental, so nothing is tombstoned.
func (c *otterConnector) Sync(ctx context.Context, cur Cursor, emit func(Page) error) (Cursor, error) {
	since := meetingSince(c.cfg, cur, c.now())
	reportProgress(ctx, func(p *Progress) { p.Phase = ProgressListing })
	list, err := c.listConversations(ctx, since, meetingScopeInt(c.cfg, "max_recordings", meetingDefaultMaxRecordings))
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
		conv, err := c.getConversation(ctx, id)
		if err != nil {
			if errors.Is(err, ErrMeetingNotConnected) {
				c.setAuthFromError(err)
				return cur, err
			}
			skipped = append(skipped, fmt.Sprintf("%s: %v", id, err))
			reportProgress(ctx, func(p *Progress) { p.ItemsDone++ })
			continue
		}
		rec := c.parseOtterConversation(conv)
		content, last := c.otterContent(conv, rec.Participants)
		if last > 0 {
			rec.DurationMS = last.Milliseconds()
		}
		rec.Tags = tags
		if err := emit(meetingPage(c.cfg, TypeOtter, otterLabel, rec, content, "")); err != nil {
			return cur, err
		}
		reportProgress(ctx, func(p *Progress) { p.ItemsDone++ })
		if rec.StartAt.After(newest) {
			newest = rec.StartAt
		}
	}
	c.refreshAuth(ctx)
	next := cur
	if !newest.IsZero() {
		next = Cursor(newest.UTC().Format(time.RFC3339))
	}
	if len(skipped) > 0 {
		return next, &PartialError{Err: fmt.Errorf("otter: %d conversation(s) skipped: %s", len(skipped), strings.Join(skipped, "; "))}
	}
	return next, nil
}

// --- MeetingReader / MeetingMediaReader ---

// ListMeetings lists conversations within since_days, newest first.
func (c *otterConnector) ListMeetings(ctx context.Context) ([]MeetingRecording, error) {
	since := c.now().Add(-time.Duration(meetingScopeInt(c.cfg, "since_days", meetingDefaultSinceDays)) * 24 * time.Hour)
	list, err := c.listConversations(ctx, since, meetingScopeInt(c.cfg, "max_recordings", meetingDefaultMaxRecordings))
	if err != nil {
		c.setAuthFromError(err)
		return nil, err
	}
	tags := meetingScopeList(c.cfg, "tags")
	out := make([]MeetingRecording, 0, len(list))
	for _, conv := range list {
		rec := c.parseOtterConversation(conv)
		if rec.ID == "" {
			continue
		}
		rec.Topic = meetingTitle(TypeOtter, otterLabel, rec)
		rec.Tags = tags
		out = append(out, rec)
	}
	sortMeetingsNewestFirst(out)
	return out, nil
}

// ReadMeeting reads one conversation's summary, outline, action items and
// transcript.
func (c *otterConnector) ReadMeeting(ctx context.Context, id string) (MeetingContent, error) {
	conv, err := c.getConversation(ctx, id)
	if err != nil {
		return MeetingContent{}, err
	}
	rec := c.parseOtterConversation(conv)
	content, _ := c.otterContent(conv, rec.Participants)
	return content, nil
}

// MediaLink implements MeetingMediaReader with GET /conversations/{id}/audio,
// the conversation's MP3 download link. Otter does not say how long the link
// lives, so it is reported with the default one-hour expiry and minted again
// per click.
func (c *otterConnector) MediaLink(ctx context.Context, id string) (MediaLink, error) {
	if !otterIDRE.MatchString(id) {
		return MediaLink{}, fmt.Errorf("otter: invalid conversation id %q", id)
	}
	m, err := c.get(ctx, "/conversations/"+id+"/audio", nil)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.StatusCode == http.StatusNotFound {
			return MediaLink{}, fmt.Errorf("otter: %w — Otter has no audio for conversation %s", ErrMeetingNoMedia, id)
		}
		return MediaLink{}, err
	}
	data, _ := m["data"].(map[string]any)
	raw := jsonString(data, "url")
	u, perr := url.Parse(raw)
	if raw == "" || perr != nil || u.Scheme != "https" || u.Host == "" {
		return MediaLink{}, fmt.Errorf("otter: %w — Otter returned no https audio link for %s", ErrMeetingNoMedia, id)
	}
	return MediaLink{URL: raw, ExpiresAt: c.now().Add(meetingMediaLinkTTLFallback)}, nil
}

// --- Authenticator ---

// AuthStatus implements Authenticator. With refresh it reads the workspace
// (GET /workspace), so a dead key or a workspace without the Public API
// shows on the card before the first sync.
func (c *otterConnector) AuthStatus(ctx context.Context, refresh bool) AuthState {
	if refresh {
		c.refreshAuth(ctx)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auth
}

func (c *otterConnector) refreshAuth(ctx context.Context) {
	m, err := c.get(ctx, "/workspace", nil)
	if err != nil {
		c.setAuthFromError(err)
		return
	}
	ws, _ := m["data"].(map[string]any)
	account := jsonString(ws, "name", "handle")
	if owner := otterUser(ws["owner"]); owner != "" && account == "" {
		account = owner
	}
	c.setAuth(AuthState{Connected: true, CheckedAt: c.now().UTC(), Account: account})
}

func (c *otterConnector) setAuth(st AuthState) {
	c.mu.Lock()
	c.auth = st
	c.mu.Unlock()
}

func (c *otterConnector) setAuthFromError(err error) {
	c.setAuth(AuthState{CheckedAt: c.now().UTC(), Message: firstLine(err.Error())})
}

// Connect implements Authenticator: an API key has no browser step, so
// Connect probes Otter and reports the outcome.
func (c *otterConnector) Connect(ctx context.Context) (AuthState, error) {
	c.refreshAuth(ctx)
	return c.AuthStatus(ctx, false), nil
}

// Disconnect implements Authenticator: forgets the cached state. The API key
// lives in the environment.
func (c *otterConnector) Disconnect(ctx context.Context) (AuthState, error) {
	st := AuthState{CheckedAt: c.now().UTC(), Message: "cached state cleared; the API key in the environment is unchanged (delete it in Otter → Integrations → Developer to revoke)"}
	c.setAuth(st)
	return st, nil
}
