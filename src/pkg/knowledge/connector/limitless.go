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

// TypeLimitless is the connector type for Limitless Pendant lifelogs: the
// transcribed conversations the Pendant captured, read through the
// Limitless Developer API with an API key.
const TypeLimitless = "limitless"

// limitlessAPIBase is the Limitless Developer API. A variable so tests point
// it at a fake server.
var limitlessAPIBase = "https://api.limitless.ai"

const (
	// limitlessLabel is the human name used in headings and messages.
	limitlessLabel = "Limitless"
	// limitlessPageSize is the `limit` of one lifelogs request (the API
	// maximum).
	limitlessPageSize = 10
	// limitlessMaxListedPages bounds one listing so a cursor that never ends
	// cannot loop forever.
	limitlessMaxListedPages = 500
	// limitlessStartLayout is the "modified ISO-8601" form `start` accepts;
	// the zone comes from the timezone parameter.
	limitlessStartLayout = "2006-01-02 15:04:05"
	// Segment types in a lifelog's contents.
	limitlessHeading1   = "heading1"
	limitlessHeading2   = "heading2"
	limitlessBlockquote = "blockquote"
)

// limitlessIDRE bounds a lifelog id.
var limitlessIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// limitlessRetryAfterRE reads retryAfter from a 429 body.
var limitlessRetryAfterRE = regexp.MustCompile(`"retryAfter"\s*:\s*"?(\d+)`)

// limitlessConnector syncs Limitless lifelogs into one fact per
// conversation. Auth: a Limitless API key through auth.env / auth.file
// (sent as X-API-Key). Scope keys: since_days, max_recordings,
// starred_only, include_participants, tags.
type limitlessConnector struct {
	cfg    ConnectorConfig
	logger *slog.Logger
	http   *HTTPClient
	now    func() time.Time

	mu        sync.Mutex
	auth      AuthState
	truncated bool
}

var (
	_ Authenticator      = (*limitlessConnector)(nil)
	_ MeetingReader      = (*limitlessConnector)(nil)
	_ MeetingMediaReader = (*limitlessConnector)(nil)
	_ Truncator          = (*limitlessConnector)(nil)
)

func newLimitlessConnector(cfg ConnectorConfig, deps Deps) (Connector, error) {
	return &limitlessConnector{cfg: cfg, logger: deps.logger(), http: deps.httpClient(), now: time.Now}, nil
}

func (c *limitlessConnector) Type() string { return TypeLimitless }

// Validate checks the auth block and scope.
func (c *limitlessConnector) Validate(cfg ConnectorConfig) error {
	if !cfg.Auth.Configured() {
		return errors.New("limitless: auth.env (variable name) or auth.file (path) must hold a Limitless API key (Limitless app → Settings → Developer)")
	}
	if cfg.Auth.Mode != "" || cfg.Auth.ClientIDEnv != "" || cfg.Auth.ClientSecretEnv != "" || cfg.Auth.AccountIDEnv != "" || cfg.Auth.TenantIDEnv != "" {
		return errors.New("limitless: auth.mode and the *_env client settings are not used; the API key goes in auth.env or auth.file")
	}
	if v := cfg.ScopeValue("starred_only"); v != "" {
		if _, err := strconv.ParseBool(v); err != nil {
			return errors.New("limitless: scope.starred_only must be true or false")
		}
	}
	return validateMeetingScope("limitless", cfg)
}

// Truncated implements Truncator.
func (c *limitlessConnector) Truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}

// --- REST ---

// token resolves the API key from auth.env / auth.file.
func (c *limitlessConnector) token() (string, error) {
	tok, err := c.cfg.Auth.Secret()
	if err != nil {
		return "", fmt.Errorf("limitless: %w — set the API key in the environment variable or file named on the Integrations card", ErrMeetingNotConnected)
	}
	return tok, nil
}

// get performs one GET and decodes the `data` object of the reply. 401/403
// map to "not connected"; 429 is retried by HTTPClient (Retry-After when
// the header is sent, else backoff) and a final 429 names the retryAfter
// Limitless reported.
func (c *limitlessConnector) get(ctx context.Context, path string, q url.Values) (map[string]any, map[string]any, error) {
	tok, err := c.token()
	if err != nil {
		return nil, nil, err
	}
	h := http.Header{}
	h.Set("X-API-Key", tok)
	h.Set("Accept", "application/json")
	u := limitlessAPIBase + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	resp, err := c.http.Get(ctx, u, h)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) {
			switch se.StatusCode {
			case http.StatusUnauthorized, http.StatusForbidden:
				return nil, nil, meetingNotConnected("limitless", limitlessLabel, err)
			case http.StatusTooManyRequests:
				wait := "a minute"
				if m := limitlessRetryAfterRE.FindStringSubmatch(se.Body); m != nil {
					wait = m[1] + "s"
				}
				return nil, nil, fmt.Errorf("limitless: %w — Limitless rate limit reached (180 requests a minute per key); retry after %s", err, wait)
			case http.StatusNotFound:
				return nil, nil, fmt.Errorf("limitless: %w — not found (deleted, or not Pendant data)", err)
			}
		}
		return nil, nil, err
	}
	m, err := decodeJSONObject("limitless", resp.Body)
	if err != nil {
		return nil, nil, err
	}
	data, ok := m["data"].(map[string]any)
	if !ok {
		return nil, nil, errors.New("limitless: reply has no data")
	}
	meta, _ := m["meta"].(map[string]any)
	return data, meta, nil
}

// --- mapping ---

// parseLimitlessLifelog maps a lifelog to a recording.
func (c *limitlessConnector) parseLimitlessLifelog(l map[string]any) MeetingRecording {
	rec := MeetingRecording{
		ID:      jsonString(l, "id"),
		Topic:   jsonString(l, "title"),
		StartAt: jsonTime(l, "startTime"),
	}
	if end := jsonTime(l, "endTime"); !end.IsZero() && end.After(rec.StartAt) {
		rec.DurationMS = end.Sub(rec.StartAt).Milliseconds()
	}
	if meetingScopeBool(c.cfg, "include_participants", true) {
		rec.Participants = limitlessSpeakers(limitlessSegments(l))
	}
	return rec
}

// limitlessSegments flattens a lifelog's contents tree in order.
func limitlessSegments(l map[string]any) []map[string]any {
	var out []map[string]any
	var walk func([]map[string]any)
	walk = func(list []map[string]any) {
		for _, s := range list {
			out = append(out, s)
			walk(jsonList(s, "children"))
		}
	}
	walk(jsonList(l, "contents"))
	return out
}

// limitlessSpeakers lists the distinct speaker names, the wearer
// ("user" identifier) included under the name Limitless gives them.
func limitlessSpeakers(segs []map[string]any) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range segs {
		n := jsonString(s, "speakerName")
		if n == "" || seen[strings.ToLower(n)] {
			continue
		}
		seen[strings.ToLower(n)] = true
		out = append(out, n)
	}
	return out
}

// limitlessContent renders the topics (headings) as the summary and the
// spoken lines (blockquotes) as the transcript. Without contents it falls
// back to the lifelog markdown as the transcript.
func limitlessContent(l map[string]any, participants []string) MeetingContent {
	out := MeetingContent{Participants: participants}
	segs := limitlessSegments(l)
	var topics []string
	var lines []transcribe.Segment
	for _, s := range segs {
		text := jsonString(s, "content")
		switch jsonString(s, "type") {
		case limitlessHeading1, limitlessHeading2:
			if text != "" && !strings.EqualFold(text, jsonString(l, "title")) {
				topics = append(topics, "- "+text)
			}
		case limitlessBlockquote:
			var off time.Duration
			if ms, ok := s["startOffsetMs"].(float64); ok && ms > 0 {
				off = time.Duration(ms) * time.Millisecond
			}
			lines = append(lines, transcribe.Segment{Start: off, Speaker: jsonString(s, "speakerName"), Text: text})
		}
	}
	if len(topics) > 0 {
		out.Summary = "### Topics\n\n" + strings.Join(topics, "\n")
	}
	out.Transcript = transcribe.RenderSegments(lines)
	if out.Transcript == "" && len(segs) == 0 {
		out.Transcript = jsonString(l, "markdown")
	}
	out.Pending = strings.TrimSpace(out.Summary) == "" && strings.TrimSpace(out.Transcript) == ""
	return out
}

// --- listing ---

// listLifelogs pages GET /v1/lifelogs (cursor, limit 10, newest first) from
// since, capped at limit; with withText the contents come in the same pages.
func (c *limitlessConnector) listLifelogs(ctx context.Context, since time.Time, limit int, withText bool) ([]map[string]any, error) {
	var out []map[string]any
	cursor := ""
	for page := 0; page < limitlessMaxListedPages; page++ {
		q := url.Values{
			"start":     {since.UTC().Format(limitlessStartLayout)},
			"timezone":  {"UTC"},
			"direction": {"desc"},
			"limit":     {strconv.Itoa(limitlessPageSize)},
		}
		q.Set("includeMarkdown", strconv.FormatBool(withText))
		q.Set("includeHeadings", "true")
		q.Set("includeContents", strconv.FormatBool(withText))
		if meetingScopeBool(c.cfg, "starred_only", false) {
			q.Set("isStarred", "true")
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		data, meta, err := c.get(ctx, "/v1/lifelogs", q)
		if err != nil {
			return nil, err
		}
		out = append(out, jsonList(data, "lifelogs")...)
		lm, _ := meta["lifelogs"].(map[string]any)
		cursor = jsonString(lm, "nextCursor")
		if cursor == "" || len(out) > limit {
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

// --- Sync ---

// Sync lists lifelogs since the cursor (minus an overlap, which also catches
// conversations Limitless re-processes after they end) with their contents,
// and emits one page per lifelog. The listing is incremental, so nothing is
// tombstoned.
func (c *limitlessConnector) Sync(ctx context.Context, cur Cursor, emit func(Page) error) (Cursor, error) {
	since := meetingSince(c.cfg, cur, c.now())
	reportProgress(ctx, func(p *Progress) { p.Phase = ProgressListing })
	list, err := c.listLifelogs(ctx, since, meetingScopeInt(c.cfg, "max_recordings", meetingDefaultMaxRecordings), true)
	if err != nil {
		c.setAuthFromError(err)
		return cur, err
	}
	total := len(list)
	reportProgress(ctx, func(p *Progress) { p.Phase, p.ItemsTotal = ProgressFetching, total })
	newest := parsePlaudCursor(cur)
	tags := meetingScopeList(c.cfg, "tags")
	for _, l := range list {
		if err := ctx.Err(); err != nil {
			return cur, err
		}
		rec := c.parseLimitlessLifelog(l)
		if rec.ID == "" {
			continue
		}
		title := progressTitle(meetingTitle(TypeLimitless, limitlessLabel, rec))
		reportProgress(ctx, func(p *Progress) { p.Phase, p.Current = ProgressFetching, title })
		rec.Tags = tags
		if err := emit(meetingPage(c.cfg, TypeLimitless, limitlessLabel, rec, limitlessContent(l, rec.Participants), "")); err != nil {
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

// ListMeetings lists lifelogs within since_days, newest first.
func (c *limitlessConnector) ListMeetings(ctx context.Context) ([]MeetingRecording, error) {
	since := c.now().Add(-time.Duration(meetingScopeInt(c.cfg, "since_days", meetingDefaultSinceDays)) * 24 * time.Hour)
	list, err := c.listLifelogs(ctx, since, meetingScopeInt(c.cfg, "max_recordings", meetingDefaultMaxRecordings), false)
	if err != nil {
		c.setAuthFromError(err)
		return nil, err
	}
	tags := meetingScopeList(c.cfg, "tags")
	out := make([]MeetingRecording, 0, len(list))
	for _, l := range list {
		rec := c.parseLimitlessLifelog(l)
		if rec.ID == "" {
			continue
		}
		rec.Topic = meetingTitle(TypeLimitless, limitlessLabel, rec)
		rec.Tags = tags
		out = append(out, rec)
	}
	sortMeetingsNewestFirst(out)
	return out, nil
}

// ReadMeeting reads one lifelog with its contents (GET /v1/lifelogs/{id}).
func (c *limitlessConnector) ReadMeeting(ctx context.Context, id string) (MeetingContent, error) {
	if !limitlessIDRE.MatchString(id) {
		return MeetingContent{}, fmt.Errorf("limitless: invalid lifelog id %q", id)
	}
	data, _, err := c.get(ctx, "/v1/lifelogs/"+id, url.Values{"includeMarkdown": {"true"}, "includeHeadings": {"true"}, "includeContents": {"true"}})
	if err != nil {
		return MeetingContent{}, err
	}
	l, ok := data["lifelog"].(map[string]any)
	if !ok {
		return MeetingContent{}, fmt.Errorf("limitless: lifelog %s not found", id)
	}
	rec := c.parseLimitlessLifelog(l)
	return limitlessContent(l, rec.Participants), nil
}

// MediaLink implements MeetingMediaReader: Limitless serves Pendant audio
// only from /v1/download-audio behind the X-API-Key header, so there is no
// link a browser (or the transcriber) can fetch without the key, and the
// hive never hands the key out. Limitless transcribes every lifelog itself.
func (c *limitlessConnector) MediaLink(ctx context.Context, id string) (MediaLink, error) {
	if !limitlessIDRE.MatchString(id) {
		return MediaLink{}, fmt.Errorf("limitless: invalid lifelog id %q", id)
	}
	return MediaLink{}, fmt.Errorf("limitless: %w — Limitless serves Pendant audio only behind the API key (/v1/download-audio), never as a shareable link", ErrMeetingNoMedia)
}

// --- Authenticator ---

// AuthStatus implements Authenticator. With refresh it lists one lifelog,
// so a dead key shows on the card before the first sync.
func (c *limitlessConnector) AuthStatus(ctx context.Context, refresh bool) AuthState {
	if refresh {
		c.refreshAuth(ctx)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auth
}

func (c *limitlessConnector) refreshAuth(ctx context.Context) {
	if _, _, err := c.get(ctx, "/v1/lifelogs", url.Values{"limit": {"1"}, "includeMarkdown": {"false"}, "includeHeadings": {"false"}}); err != nil {
		c.setAuthFromError(err)
		return
	}
	c.setAuth(AuthState{Connected: true, CheckedAt: c.now().UTC(), Account: "Limitless API key"})
}

func (c *limitlessConnector) setAuth(st AuthState) {
	c.mu.Lock()
	c.auth = st
	c.mu.Unlock()
}

func (c *limitlessConnector) setAuthFromError(err error) {
	c.setAuth(AuthState{CheckedAt: c.now().UTC(), Message: firstLine(err.Error())})
}

// Connect implements Authenticator: an API key has no browser step, so
// Connect probes Limitless and reports the outcome.
func (c *limitlessConnector) Connect(ctx context.Context) (AuthState, error) {
	c.refreshAuth(ctx)
	return c.AuthStatus(ctx, false), nil
}

// Disconnect implements Authenticator: forgets the cached state. The API key
// lives in the environment.
func (c *limitlessConnector) Disconnect(ctx context.Context) (AuthState, error) {
	st := AuthState{CheckedAt: c.now().UTC(), Message: "cached state cleared; the API key in the environment is unchanged (revoke it in the Limitless app's Developer settings)"}
	c.setAuth(st)
	return st, nil
}
