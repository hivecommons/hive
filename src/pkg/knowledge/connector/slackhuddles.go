package connector

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TypeSlackHuddles is the connector type for Slack huddle notes: the
// AI notes canvas (with its transcript, when the workspace allows one)
// Slack posts in a huddle's thread.
const TypeSlackHuddles = "slack-huddles"

// slackAPIBase is the Slack Web API. A variable so tests point it at a fake
// server.
var slackAPIBase = "https://slack.com/api"

const (
	// slackHuddlesLabel is the human name used in headings and messages.
	slackHuddlesLabel = "Slack huddles"
	// slackHuddlesAttrPrefix prefixes the fact attributes
	// (attr_slack_huddle_*); the connector type has a hyphen.
	slackHuddlesAttrPrefix = "slack_huddle"
	// slackHistoryPageSize is the `limit` requested on history and replies.
	slackHistoryPageSize = 200
	// slackHuddleSubtype marks the message Slack posts when a huddle starts;
	// its `room` object describes the huddle.
	slackHuddleSubtype = "huddle_thread"
	// slackCanvasFiletype / slackCanvasPrettyType identify a canvas file.
	slackCanvasFiletype   = "canvas"
	slackQuipFiletype     = "quip"
	slackCanvasPrettyType = "canvas"
	// slackIDSep joins channel and message ts in an Inbox item id.
	slackIDSep = ":"
)

// slackChannelRE bounds a conversation id (public C…, private G…, DM D…).
var slackChannelRE = regexp.MustCompile(`^[CGD][A-Z0-9]{2,}$`)

// slackTSRE bounds a message timestamp.
var slackTSRE = regexp.MustCompile(`^\d{9,11}\.\d{1,6}$`)

// slackFileIDRE bounds a file (canvas) id.
var slackFileIDRE = regexp.MustCompile(`^F[A-Z0-9]{2,}$`)

// slackTranscriptHeadingRE finds the transcript section of a notes canvas.
var slackTranscriptHeadingRE = regexp.MustCompile(`(?im)^#{1,6}\s*(huddle\s+)?transcript\b.*$`)

// Downloaded canvases are HTML; slackHTMLText keeps headings as "## text"
// lines and block ends as line breaks so the transcript heading survives.
var (
	slackHTMLHeadingRE = regexp.MustCompile(`(?is)<h[1-6][^>]*>(.*?)</h[1-6]>`)
	slackHTMLBlockRE   = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|li|tr|ul|ol|pre|blockquote)>`)
	slackHTMLItemRE    = regexp.MustCompile(`(?i)<li[^>]*>`)
	slackHTMLTagRE     = regexp.MustCompile(`(?s)<[^>]*>`)
	slackBlankLinesRE  = regexp.MustCompile(`\n{3,}`)
)

// slackHTMLText turns canvas HTML into markdown-ish text.
func slackHTMLText(body string) string {
	s := slackHTMLHeadingRE.ReplaceAllString(body, "\n\n## $1\n\n")
	s = slackHTMLItemRE.ReplaceAllString(s, "\n- ")
	s = slackHTMLBlockRE.ReplaceAllString(s, "\n")
	s = html.UnescapeString(slackHTMLTagRE.ReplaceAllString(s, ""))
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return strings.TrimSpace(slackBlankLinesRE.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// slackAuthErrors are the Slack error codes that mean the token is dead.
var slackAuthErrors = map[string]bool{
	"invalid_auth": true, "not_authed": true, "token_revoked": true,
	"token_expired": true, "account_inactive": true,
}

// slackError is a Slack Web API `ok: false` reply.
type slackError struct {
	Method   string
	Code     string
	Needed   string
	Provided string
}

func (e *slackError) Error() string {
	msg := fmt.Sprintf("%s: %s", e.Method, e.Code)
	if e.Needed != "" {
		msg += fmt.Sprintf(" (needs scope %s; the token has %s)", e.Needed, e.Provided)
	}
	switch e.Code {
	case "not_in_channel", "channel_not_found":
		msg += " — add the app to the channel (/invite @app) or check scope.channels"
	}
	return msg
}

// slackHuddle is one ended huddle with the canvases its thread carries.
type slackHuddle struct {
	MeetingRecording
	Channel     string
	TS          string
	ThreadTS    string
	CreatedBy   string
	Members     []string
	AttachedIDs []string
	CanvasID    string
	CanvasTitle string
}

// slackHuddlesConnector syncs Slack huddles whose AI notes canvas exists
// into one fact per huddle. Slack never records huddle audio, so items are
// text only. Auth: a Slack bot or user token through auth.env / auth.file.
// Scope keys: channels (conversation ids, required), since_days,
// max_recordings, include_participants, tags.
type slackHuddlesConnector struct {
	cfg    ConnectorConfig
	logger *slog.Logger
	http   *HTTPClient
	now    func() time.Time

	mu        sync.Mutex
	auth      AuthState
	truncated bool
	teamURL   string
	names     map[string]string
	channels  map[string]string
}

var (
	_ Authenticator      = (*slackHuddlesConnector)(nil)
	_ MeetingReader      = (*slackHuddlesConnector)(nil)
	_ MeetingMediaReader = (*slackHuddlesConnector)(nil)
	_ Truncator          = (*slackHuddlesConnector)(nil)
)

func newSlackHuddlesConnector(cfg ConnectorConfig, deps Deps) (Connector, error) {
	return &slackHuddlesConnector{cfg: cfg, logger: deps.logger(), http: deps.httpClient(), now: time.Now,
		names: map[string]string{}, channels: map[string]string{}}, nil
}

func (c *slackHuddlesConnector) Type() string { return TypeSlackHuddles }

// Validate checks the auth block and scope.
func (c *slackHuddlesConnector) Validate(cfg ConnectorConfig) error {
	if !cfg.Auth.Configured() {
		return errors.New("slack-huddles: auth.env (variable name) or auth.file (path) must hold a Slack bot (xoxb-) or user (xoxp-) token")
	}
	if cfg.Auth.Mode != "" || cfg.Auth.ClientIDEnv != "" || cfg.Auth.ClientSecretEnv != "" || cfg.Auth.AccountIDEnv != "" || cfg.Auth.TenantIDEnv != "" {
		return errors.New("slack-huddles: auth.mode and the *_env client settings are not used; the token goes in auth.env or auth.file")
	}
	chans := meetingScopeList(cfg, "channels")
	if len(chans) == 0 {
		return errors.New("slack-huddles: scope.channels must list the conversation ids whose huddles to read (C…, G… or D…)")
	}
	for _, ch := range chans {
		if !slackChannelRE.MatchString(ch) {
			return fmt.Errorf("slack-huddles: scope.channels entry %q is not a Slack conversation id", ch)
		}
	}
	return validateMeetingScope("slack-huddles", cfg)
}

// Truncated implements Truncator.
func (c *slackHuddlesConnector) Truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}

// --- Web API ---

// token resolves the Slack token from auth.env / auth.file.
func (c *slackHuddlesConnector) token() (string, error) {
	tok, err := c.cfg.Auth.Secret()
	if err != nil {
		return "", fmt.Errorf("slack-huddles: %w — set the token in the environment variable or file named on the Integrations card", ErrMeetingNotConnected)
	}
	return tok, nil
}

// call performs one Web API method (GET with query, or POST form) and
// returns the decoded reply, mapping `ok: false` to *slackError and dead
// tokens to "not connected". 429 is retried by HTTPClient with Retry-After.
func (c *slackHuddlesConnector) call(ctx context.Context, method string, args url.Values, post bool) (map[string]any, error) {
	tok, err := c.token()
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+tok)
	var resp *Response
	if post {
		h.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err = c.http.Do(ctx, http.MethodPost, slackAPIBase+"/"+method, h, []byte(args.Encode()))
	} else {
		resp, err = c.http.Get(ctx, slackAPIBase+"/"+method+"?"+args.Encode(), h)
	}
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.StatusCode == http.StatusUnauthorized {
			return nil, meetingNotConnected("slack-huddles", slackHuddlesLabel, err)
		}
		return nil, err
	}
	m, err := decodeJSONObject("slack-huddles", resp.Body)
	if err != nil {
		return nil, err
	}
	if ok, _ := m["ok"].(bool); !ok {
		se := &slackError{Method: method, Code: jsonString(m, "error"), Needed: jsonString(m, "needed"), Provided: jsonString(m, "provided")}
		if se.Code == "" {
			se.Code = "unknown_error"
		}
		if slackAuthErrors[se.Code] {
			return nil, meetingNotConnected("slack-huddles", slackHuddlesLabel, se)
		}
		return nil, se
	}
	return m, nil
}

// slackTrustedFileURL keeps the token on Slack's file host.
func slackTrustedFileURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("slack-huddles: invalid file link")
	}
	host := strings.ToLower(u.Hostname())
	if host == "files.slack.com" || strings.HasSuffix(host, ".slack.com") {
		return nil
	}
	if api, err := url.Parse(slackAPIBase); err == nil && strings.EqualFold(api.Host, u.Host) {
		return nil
	}
	return fmt.Errorf("slack-huddles: file host %q is not a Slack host", u.Host)
}

// --- names ---

// userName resolves a user id to a display name (users:read), cached; the
// id is kept when the lookup fails.
func (c *slackHuddlesConnector) userName(ctx context.Context, id string) string {
	c.mu.Lock()
	n, ok := c.names[id]
	c.mu.Unlock()
	if ok {
		return n
	}
	n = id
	if m, err := c.call(ctx, "users.info", url.Values{"user": {id}}, false); err == nil {
		if u, ok := m["user"].(map[string]any); ok {
			if p, ok := u["profile"].(map[string]any); ok {
				if v := jsonString(p, "display_name", "real_name"); v != "" {
					n = v
				}
			}
			if n == id {
				if v := jsonString(u, "real_name", "name"); v != "" {
					n = v
				}
			}
		}
	} else {
		c.logger.Debug("slack user name unavailable", "user", id, "error", err)
	}
	c.mu.Lock()
	c.names[id] = n
	c.mu.Unlock()
	return n
}

// channelName resolves a conversation id to "#name" (channels:read /
// groups:read), cached; empty when the lookup fails or for DMs.
func (c *slackHuddlesConnector) channelName(ctx context.Context, id string) string {
	c.mu.Lock()
	n, ok := c.channels[id]
	c.mu.Unlock()
	if ok {
		return n
	}
	if m, err := c.call(ctx, "conversations.info", url.Values{"channel": {id}}, false); err == nil {
		if ch, ok := m["channel"].(map[string]any); ok {
			if v := jsonString(ch, "name"); v != "" {
				n = "#" + v
			}
		}
	} else {
		c.logger.Debug("slack channel name unavailable", "channel", id, "error", err)
	}
	c.mu.Lock()
	c.channels[id] = n
	c.mu.Unlock()
	return n
}

// --- huddles ---

// slackTSTime converts a Slack ts or epoch-seconds value to a time.
func slackTSTime(v any) time.Time {
	switch x := v.(type) {
	case float64:
		if x > 0 {
			return time.Unix(int64(x), 0).UTC()
		}
	case string:
		if f, err := strconv.ParseFloat(x, 64); err == nil && f > 0 {
			return time.Unix(int64(f), 0).UTC()
		}
	}
	return time.Time{}
}

// parseSlackHuddle maps a huddle_thread message; ok is false for other
// messages and for huddles that have not ended.
func parseSlackHuddle(channel string, msg map[string]any) (slackHuddle, bool) {
	if jsonString(msg, "subtype") != slackHuddleSubtype {
		return slackHuddle{}, false
	}
	room, ok := msg["room"].(map[string]any)
	if !ok {
		return slackHuddle{}, false
	}
	start, end := slackTSTime(room["date_start"]), slackTSTime(room["date_end"])
	ended, _ := room["has_ended"].(bool)
	if !ended && end.IsZero() {
		return slackHuddle{}, false
	}
	h := slackHuddle{Channel: channel, TS: jsonString(msg, "ts"), CreatedBy: jsonString(room, "created_by")}
	h.ThreadTS = jsonString(room, "thread_root_ts")
	if h.ThreadTS == "" {
		h.ThreadTS = h.TS
	}
	h.ID = channel + slackIDSep + h.TS
	h.MeetingID = jsonString(room, "id")
	h.StartAt = start
	if h.StartAt.IsZero() {
		h.StartAt = slackTSTime(h.TS)
	}
	if !end.IsZero() && end.After(h.StartAt) {
		h.DurationMS = end.Sub(h.StartAt).Milliseconds()
	}
	for _, key := range []string{"participant_history", "participants"} {
		if arr, ok := room[key].([]any); ok && len(h.Members) == 0 {
			for _, u := range arr {
				if s, ok := u.(string); ok && s != "" {
					h.Members = append(h.Members, s)
				}
			}
		}
	}
	if arr, ok := room["attached_file_ids"].([]any); ok {
		for _, f := range arr {
			if s, ok := f.(string); ok && slackFileIDRE.MatchString(s) {
				h.AttachedIDs = append(h.AttachedIDs, s)
			}
		}
	}
	return h, true
}

// isSlackCanvas reports whether a file object is a canvas.
func isSlackCanvas(f map[string]any) bool {
	ft := strings.ToLower(jsonString(f, "filetype"))
	return ft == slackCanvasFiletype || ft == slackQuipFiletype || strings.ToLower(jsonString(f, "pretty_type")) == slackCanvasPrettyType
}

// findNotes picks the huddle's notes canvas: canvases posted in the huddle
// thread or attached to the room, preferring one titled as huddle notes.
func (c *slackHuddlesConnector) findNotes(ctx context.Context, h *slackHuddle) error {
	type cand struct{ id, title string }
	var cands []cand
	seen := map[string]bool{}
	add := func(id, title string) {
		if id != "" && !seen[id] && slackFileIDRE.MatchString(id) {
			seen[id] = true
			cands = append(cands, cand{id, title})
		}
	}
	cursor := ""
	for {
		args := url.Values{"channel": {h.Channel}, "ts": {h.ThreadTS}, "limit": {strconv.Itoa(slackHistoryPageSize)}}
		if cursor != "" {
			args.Set("cursor", cursor)
		}
		m, err := c.call(ctx, "conversations.replies", args, false)
		if err != nil {
			var se *slackError
			if errors.As(err, &se) && se.Code == "thread_not_found" {
				break
			}
			return err
		}
		for _, msg := range jsonList(m, "messages") {
			for _, f := range jsonList(msg, "files") {
				if isSlackCanvas(f) {
					add(jsonString(f, "id"), jsonString(f, "title", "name"))
				}
			}
		}
		meta, _ := m["response_metadata"].(map[string]any)
		cursor = jsonString(meta, "next_cursor")
		if more, _ := m["has_more"].(bool); !more || cursor == "" {
			break
		}
	}
	for _, id := range h.AttachedIDs {
		if seen[id] {
			continue
		}
		m, err := c.call(ctx, "files.info", url.Values{"file": {id}}, false)
		if err != nil {
			if errors.Is(err, ErrMeetingNotConnected) {
				return err
			}
			c.logger.Debug("slack attached file unavailable", "file", id, "error", err)
			continue
		}
		if f, ok := m["file"].(map[string]any); ok && isSlackCanvas(f) {
			add(id, jsonString(f, "title", "name"))
		}
	}
	for _, cd := range cands {
		if strings.Contains(strings.ToLower(cd.title), "huddle") {
			h.CanvasID, h.CanvasTitle = cd.id, cd.title
			break
		}
	}
	if h.CanvasID == "" && len(cands) > 0 {
		h.CanvasID, h.CanvasTitle = cands[0].id, cands[0].title
	}
	return nil
}

// describe fills the title, host, participants and permalink.
func (c *slackHuddlesConnector) describe(ctx context.Context, h *slackHuddle) {
	if t := strings.TrimSpace(h.CanvasTitle); t != "" {
		h.Topic = t
	} else if n := c.channelName(ctx, h.Channel); n != "" {
		h.Topic = "Huddle in " + n
	}
	if h.CreatedBy != "" {
		h.Host = c.userName(ctx, h.CreatedBy)
	}
	h.Participants = nil
	if meetingScopeBool(c.cfg, "include_participants", true) {
		for _, u := range h.Members {
			h.Participants = append(h.Participants, c.userName(ctx, u))
		}
	}
	c.mu.Lock()
	team := c.teamURL
	c.mu.Unlock()
	if team != "" {
		h.URL = strings.TrimRight(team, "/") + "/archives/" + h.Channel + "/p" + strings.ReplaceAll(h.TS, ".", "")
	}
}

// listChannel pages conversations.history from oldest and returns the
// ended huddles that have a notes canvas.
func (c *slackHuddlesConnector) listChannel(ctx context.Context, channel string, oldest time.Time) ([]slackHuddle, error) {
	var out []slackHuddle
	cursor := ""
	for {
		args := url.Values{"channel": {channel}, "oldest": {strconv.FormatInt(oldest.Unix(), 10)}, "limit": {strconv.Itoa(slackHistoryPageSize)}}
		if cursor != "" {
			args.Set("cursor", cursor)
		}
		m, err := c.call(ctx, "conversations.history", args, false)
		if err != nil {
			return nil, err
		}
		for _, msg := range jsonList(m, "messages") {
			if h, ok := parseSlackHuddle(channel, msg); ok {
				out = append(out, h)
			}
		}
		meta, _ := m["response_metadata"].(map[string]any)
		cursor = jsonString(meta, "next_cursor")
		if more, _ := m["has_more"].(bool); !more || cursor == "" {
			break
		}
	}
	return out, nil
}

// ensureTeam caches the workspace URL from auth.test for permalinks.
func (c *slackHuddlesConnector) ensureTeam(ctx context.Context) (map[string]any, error) {
	m, err := c.call(ctx, "auth.test", url.Values{}, false)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.teamURL = jsonString(m, "url")
	c.mu.Unlock()
	return m, nil
}

// listHuddles lists the ended huddles with notes since oldest across the
// configured channels, newest first, capped at limit.
func (c *slackHuddlesConnector) listHuddles(ctx context.Context, oldest time.Time, limit int) ([]slackHuddle, error) {
	if _, err := c.ensureTeam(ctx); err != nil {
		return nil, err
	}
	var out []slackHuddle
	for _, ch := range meetingScopeList(c.cfg, "channels") {
		hs, err := c.listChannel(ctx, ch, oldest)
		if err != nil {
			return nil, fmt.Errorf("channel %s: %w", ch, err)
		}
		for i := range hs {
			h := hs[i]
			if err := c.findNotes(ctx, &h); err != nil {
				if errors.Is(err, ErrMeetingNotConnected) {
					return nil, err
				}
				c.logger.Debug("slack huddle thread unavailable", "huddle", h.ID, "error", err)
				continue
			}
			if h.CanvasID == "" {
				continue // no AI notes: Slack keeps nothing else to file
			}
			c.describe(ctx, &h)
			out = append(out, h)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].StartAt.After(out[j-1].StartAt); j-- {
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

// splitSlackItemID parses "<channel>:<ts>".
func splitSlackItemID(id string) (channel, ts string, err error) {
	channel, ts, ok := strings.Cut(id, slackIDSep)
	if !ok || !slackChannelRE.MatchString(channel) || !slackTSRE.MatchString(ts) {
		return "", "", fmt.Errorf("slack-huddles: invalid huddle id %q (want <channel>:<ts>)", id)
	}
	return channel, ts, nil
}

// getHuddle re-reads one huddle by item id.
func (c *slackHuddlesConnector) getHuddle(ctx context.Context, id string) (slackHuddle, error) {
	channel, ts, err := splitSlackItemID(id)
	if err != nil {
		return slackHuddle{}, err
	}
	if _, err := c.ensureTeam(ctx); err != nil {
		return slackHuddle{}, err
	}
	m, err := c.call(ctx, "conversations.history", url.Values{"channel": {channel}, "latest": {ts}, "oldest": {ts}, "inclusive": {"true"}, "limit": {"1"}}, false)
	if err != nil {
		return slackHuddle{}, err
	}
	for _, msg := range jsonList(m, "messages") {
		if h, ok := parseSlackHuddle(channel, msg); ok && h.TS == ts {
			if err := c.findNotes(ctx, &h); err != nil {
				return slackHuddle{}, err
			}
			c.describe(ctx, &h)
			return h, nil
		}
	}
	return slackHuddle{}, fmt.Errorf("slack-huddles: huddle %s not found (ended huddle messages only)", id)
}

// --- content ---

// splitSlackNotes separates the canvas into summary (notes) and transcript.
func splitSlackNotes(md string) (summary, transcript string) {
	md = strings.TrimSpace(md)
	loc := slackTranscriptHeadingRE.FindStringIndex(md)
	if loc == nil {
		return md, ""
	}
	return strings.TrimSpace(md[:loc[0]]), strings.TrimSpace(md[loc[1]:])
}

// canvasMarkdown reads a canvas as markdown (canvases.getContent,
// canvases:read), falling back to the file's private download (files:read)
// converted from HTML.
func (c *slackHuddlesConnector) canvasMarkdown(ctx context.Context, id string) (string, error) {
	m, err := c.call(ctx, "canvases.getContent", url.Values{"canvas_id": {id}, "content_type": {"markdown"}}, true)
	if err == nil {
		return jsonString(m, "content"), nil
	}
	if errors.Is(err, ErrMeetingNotConnected) {
		return "", err
	}
	c.logger.Debug("slack canvases.getContent failed; trying the file download", "canvas", id, "error", err)
	fm, ferr := c.call(ctx, "files.info", url.Values{"file": {id}}, false)
	if ferr != nil {
		return "", fmt.Errorf("canvas %s: %v; %w", id, err, ferr)
	}
	f, _ := fm["file"].(map[string]any)
	link := jsonString(f, "url_private_download", "url_private")
	if err := slackTrustedFileURL(link); err != nil {
		return "", err
	}
	tok, terr := c.token()
	if terr != nil {
		return "", terr
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+tok)
	resp, derr := c.http.Get(ctx, link, h)
	if derr != nil {
		return "", fmt.Errorf("canvas %s download: %w", id, derr)
	}
	return slackHTMLText(string(resp.Body)), nil
}

// fetchContent reads the notes canvas of one huddle.
func (c *slackHuddlesConnector) fetchContent(ctx context.Context, h slackHuddle) (MeetingContent, error) {
	out := MeetingContent{Participants: h.Participants}
	if h.CanvasID != "" {
		md, err := c.canvasMarkdown(ctx, h.CanvasID)
		if err != nil {
			return MeetingContent{}, err
		}
		out.Summary, out.Transcript = splitSlackNotes(md)
	}
	out.Pending = strings.TrimSpace(out.Summary) == "" && strings.TrimSpace(out.Transcript) == ""
	return out, nil
}

// --- Sync ---

// Sync lists ended huddles with notes since the cursor (minus an overlap,
// which also catches notes Slack writes after the huddle ends) and emits one
// page per huddle. The listing is incremental, so nothing is tombstoned.
func (c *slackHuddlesConnector) Sync(ctx context.Context, cur Cursor, emit func(Page) error) (Cursor, error) {
	since := meetingSince(c.cfg, cur, c.now())
	reportProgress(ctx, func(p *Progress) { p.Phase = ProgressListing })
	huddles, err := c.listHuddles(ctx, since, meetingScopeInt(c.cfg, "max_recordings", meetingDefaultMaxRecordings))
	if err != nil {
		c.setAuthFromError(err)
		return cur, err
	}
	total := len(huddles)
	reportProgress(ctx, func(p *Progress) { p.Phase, p.ItemsTotal = ProgressFetching, total })
	newest := parsePlaudCursor(cur)
	var skipped []string
	for _, h := range huddles {
		if err := ctx.Err(); err != nil {
			return cur, err
		}
		title := progressTitle(meetingTitle(TypeSlackHuddles, slackHuddlesLabel, h.MeetingRecording))
		reportProgress(ctx, func(p *Progress) { p.Phase, p.Current = ProgressFetching, title })
		content, err := c.fetchContent(ctx, h)
		if err != nil {
			if errors.Is(err, ErrMeetingNotConnected) {
				c.setAuthFromError(err)
				return cur, err
			}
			skipped = append(skipped, fmt.Sprintf("%s: %v", h.ID, err))
			reportProgress(ctx, func(p *Progress) { p.ItemsDone++ })
			continue
		}
		h.Tags = meetingScopeList(c.cfg, "tags")
		if err := emit(meetingPage(c.cfg, slackHuddlesAttrPrefix, slackHuddlesLabel, h.MeetingRecording, content, "")); err != nil {
			return cur, err
		}
		if h.StartAt.After(newest) {
			newest = h.StartAt
		}
	}
	c.refreshAuth(ctx)
	next := cur
	if !newest.IsZero() {
		next = Cursor(newest.UTC().Format(time.RFC3339))
	}
	if len(skipped) > 0 {
		return next, &PartialError{Err: fmt.Errorf("slack-huddles: %d huddle(s) skipped: %s", len(skipped), strings.Join(skipped, "; "))}
	}
	return next, nil
}

// --- MeetingReader / MeetingMediaReader ---

// ListMeetings lists huddles with notes within since_days, newest first.
func (c *slackHuddlesConnector) ListMeetings(ctx context.Context) ([]MeetingRecording, error) {
	since := c.now().Add(-time.Duration(meetingScopeInt(c.cfg, "since_days", meetingDefaultSinceDays)) * 24 * time.Hour)
	huddles, err := c.listHuddles(ctx, since, meetingScopeInt(c.cfg, "max_recordings", meetingDefaultMaxRecordings))
	if err != nil {
		c.setAuthFromError(err)
		return nil, err
	}
	tags := meetingScopeList(c.cfg, "tags")
	out := make([]MeetingRecording, 0, len(huddles))
	for _, h := range huddles {
		rec := h.MeetingRecording
		rec.Topic = meetingTitle(TypeSlackHuddles, slackHuddlesLabel, rec)
		rec.Tags = tags
		out = append(out, rec)
	}
	sortMeetingsNewestFirst(out)
	return out, nil
}

// ReadMeeting reads the notes (and transcript) of one huddle.
func (c *slackHuddlesConnector) ReadMeeting(ctx context.Context, id string) (MeetingContent, error) {
	h, err := c.getHuddle(ctx, id)
	if err != nil {
		return MeetingContent{}, err
	}
	return c.fetchContent(ctx, h)
}

// MediaLink implements MeetingMediaReader: Slack does not record huddle
// audio or video, so there is never a playable link.
func (c *slackHuddlesConnector) MediaLink(ctx context.Context, id string) (MediaLink, error) {
	if _, _, err := splitSlackItemID(id); err != nil {
		return MediaLink{}, err
	}
	return MediaLink{}, fmt.Errorf("slack-huddles: %w — Slack does not record huddle audio; only the AI notes and transcript text exist", ErrMeetingNoMedia)
}

// --- Authenticator ---

// AuthStatus implements Authenticator. With refresh it calls auth.test and
// reads one message of the first channel, so a missing scope or channel
// membership shows on the card before the first sync.
func (c *slackHuddlesConnector) AuthStatus(ctx context.Context, refresh bool) AuthState {
	if refresh {
		c.refreshAuth(ctx)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auth
}

func (c *slackHuddlesConnector) refreshAuth(ctx context.Context) {
	m, err := c.ensureTeam(ctx)
	if err == nil {
		if chans := meetingScopeList(c.cfg, "channels"); len(chans) > 0 {
			_, err = c.call(ctx, "conversations.history", url.Values{"channel": {chans[0]}, "limit": {"1"}}, false)
		}
	}
	if err != nil {
		c.setAuthFromError(err)
		return
	}
	account := jsonString(m, "user")
	if team := jsonString(m, "team"); team != "" {
		account = strings.TrimSpace(account + " @ " + team)
	}
	c.setAuth(AuthState{Connected: true, CheckedAt: c.now().UTC(), Account: account})
}

func (c *slackHuddlesConnector) setAuth(st AuthState) {
	c.mu.Lock()
	c.auth = st
	c.mu.Unlock()
}

func (c *slackHuddlesConnector) setAuthFromError(err error) {
	c.setAuth(AuthState{CheckedAt: c.now().UTC(), Message: firstLine(err.Error())})
}

// Connect implements Authenticator: a token has no browser step, so Connect
// probes Slack and reports the outcome.
func (c *slackHuddlesConnector) Connect(ctx context.Context) (AuthState, error) {
	c.refreshAuth(ctx)
	return c.AuthStatus(ctx, false), nil
}

// Disconnect implements Authenticator: forgets the cached names and state.
// The token itself lives in the environment and the Slack app stays
// installed.
func (c *slackHuddlesConnector) Disconnect(ctx context.Context) (AuthState, error) {
	c.mu.Lock()
	c.names, c.channels, c.teamURL = map[string]string{}, map[string]string{}, ""
	c.mu.Unlock()
	st := AuthState{CheckedAt: c.now().UTC(), Message: "cached state cleared; the token in the environment and the Slack app installation are unchanged"}
	c.setAuth(st)
	return st, nil
}
