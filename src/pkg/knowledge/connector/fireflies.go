package connector

import (
	"context"
	"encoding/json"
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

// TypeFireflies is the connector type for Fireflies.ai meeting transcripts
// (the Fireflies notetaker's recordings, transcripts and AI summaries),
// read through the Fireflies GraphQL API with an API key.
const TypeFireflies = "fireflies"

// firefliesAPIURL is the Fireflies GraphQL endpoint. A variable so tests
// point it at a fake server.
var firefliesAPIURL = "https://api.fireflies.ai/graphql"

const (
	// firefliesLabel is the human name used in headings and messages.
	firefliesLabel = "Fireflies"
	// firefliesPageSize is the `limit` of one transcripts query (the API
	// maximum).
	firefliesPageSize = 50
	// firefliesMediaTTL is how long Fireflies says an audio_url / video_url
	// stays valid.
	firefliesMediaTTL = 24 * time.Hour
	// firefliesMaxListedPages bounds one listing so a misbehaving API that
	// ignores skip cannot loop forever.
	firefliesMaxListedPages = 100
)

// firefliesIDRE bounds a transcript id.
var firefliesIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// firefliesCodeRE / firefliesMessageRE read the first error code and message
// from a (possibly truncated) GraphQL error body.
var (
	firefliesCodeRE    = regexp.MustCompile(`"code"\s*:\s*"([A-Za-z_]+)"`)
	firefliesMessageRE = regexp.MustCompile(`"message"\s*:\s*"([^"]{1,200})"`)
)

// firefliesAuthCodes are the error codes that mean the API key is dead.
var firefliesAuthCodes = map[string]bool{"auth_failed": true, "unauthenticated": true, "invalid_api_key": true, "account_cancelled": true}

// The GraphQL selections. Listing fields come with every query; text fields
// are added for a sync (one request per 50 meetings instead of one per
// meeting) and for a single read.
const (
	firefliesMetaFields    = `id title date dateString duration organizer_email participants transcript_url meeting_attendees { displayName email name }`
	firefliesSentenceField = `sentences { speaker_name text raw_text start_time }`
	firefliesSummaryField  = `summary { overview short_summary action_items keywords topics_discussed }`
)

// firefliesError is one entry of a GraphQL `errors` array.
type firefliesError struct {
	Code    string
	Message string
	Status  int
}

func (e *firefliesError) Error() string {
	msg := "fireflies: " + e.Message
	if e.Message == "" {
		msg = "fireflies: request failed"
	}
	if e.Code != "" {
		msg += " (" + e.Code + ")"
	}
	switch e.Code {
	case "paid_required":
		msg += " — this needs a Fireflies Pro plan or higher"
	case "too_many_requests":
		msg += " — Fireflies rate limit reached (Free plan: 30 requests a minute, 500 a day); the next sync retries"
	}
	return msg
}

// firefliesConnector syncs Fireflies.ai transcripts into one fact per
// meeting. Auth: a Fireflies API key through auth.env / auth.file. Scope
// keys: since_days, max_recordings, mine, include_summary,
// include_participants, tags.
type firefliesConnector struct {
	cfg    ConnectorConfig
	logger *slog.Logger
	http   *HTTPClient
	now    func() time.Time

	mu        sync.Mutex
	auth      AuthState
	truncated bool
}

var (
	_ Authenticator      = (*firefliesConnector)(nil)
	_ MeetingReader      = (*firefliesConnector)(nil)
	_ MeetingMediaReader = (*firefliesConnector)(nil)
	_ Truncator          = (*firefliesConnector)(nil)
)

func newFirefliesConnector(cfg ConnectorConfig, deps Deps) (Connector, error) {
	return &firefliesConnector{cfg: cfg, logger: deps.logger(), http: deps.httpClient(), now: time.Now}, nil
}

func (c *firefliesConnector) Type() string { return TypeFireflies }

// Validate checks the auth block and scope.
func (c *firefliesConnector) Validate(cfg ConnectorConfig) error {
	if !cfg.Auth.Configured() {
		return errors.New("fireflies: auth.env (variable name) or auth.file (path) must hold a Fireflies API key (Fireflies → Integrations → Fireflies API)")
	}
	if cfg.Auth.Mode != "" || cfg.Auth.ClientIDEnv != "" || cfg.Auth.ClientSecretEnv != "" || cfg.Auth.AccountIDEnv != "" || cfg.Auth.TenantIDEnv != "" {
		return errors.New("fireflies: auth.mode and the *_env client settings are not used; the API key goes in auth.env or auth.file")
	}
	if v := cfg.ScopeValue("mine"); v != "" {
		if _, err := strconv.ParseBool(v); err != nil {
			return errors.New("fireflies: scope.mine must be true or false")
		}
	}
	return validateMeetingScope("fireflies", cfg)
}

// Truncated implements Truncator.
func (c *firefliesConnector) Truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}

// --- GraphQL ---

// token resolves the API key from auth.env / auth.file.
func (c *firefliesConnector) token() (string, error) {
	tok, err := c.cfg.Auth.Secret()
	if err != nil {
		return "", fmt.Errorf("fireflies: %w — set the API key in the environment variable or file named on the Integrations card", ErrMeetingNotConnected)
	}
	return tok, nil
}

// parseFirefliesError reads the first GraphQL error of a reply body; nil
// when the body names none.
func parseFirefliesError(body string, status int) *firefliesError {
	var m struct {
		Errors []struct {
			Message    string `json:"message"`
			Code       string `json:"code"`
			Extensions struct {
				Code string `json:"code"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(body), &m); err == nil {
		if len(m.Errors) == 0 {
			return nil
		}
		e := m.Errors[0]
		code := e.Code
		if code == "" {
			code = e.Extensions.Code
		}
		return &firefliesError{Code: strings.ToLower(code), Message: e.Message, Status: status}
	}
	// A StatusError keeps only the first bytes of the body: read what is there.
	cm := firefliesCodeRE.FindStringSubmatch(body)
	mm := firefliesMessageRE.FindStringSubmatch(body)
	if cm == nil && mm == nil {
		return nil
	}
	e := &firefliesError{Status: status}
	if cm != nil {
		e.Code = strings.ToLower(cm[1])
	}
	if mm != nil {
		e.Message = mm[1]
	}
	return e
}

// classify maps a Fireflies error to "not connected" when the key is dead.
func firefliesClassify(e *firefliesError) error {
	if firefliesAuthCodes[e.Code] || e.Status == http.StatusUnauthorized {
		return meetingNotConnected("fireflies", firefliesLabel, e)
	}
	return e
}

// query runs one GraphQL request and returns its `data` object. 429 is
// retried by HTTPClient honouring Retry-After; GraphQL errors are mapped to
// *firefliesError, a dead key to "not connected".
func (c *firefliesConnector) query(ctx context.Context, q string, vars map[string]any) (map[string]any, error) {
	tok, err := c.token()
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{"query": q, "variables": vars})
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+tok)
	h.Set("Content-Type", "application/json")
	resp, err := c.http.Do(ctx, http.MethodPost, firefliesAPIURL, h, body)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) {
			if fe := parseFirefliesError(se.Body, se.StatusCode); fe != nil {
				return nil, firefliesClassify(fe)
			}
			if se.StatusCode == http.StatusUnauthorized || se.StatusCode == http.StatusForbidden {
				return nil, meetingNotConnected("fireflies", firefliesLabel, err)
			}
		}
		return nil, err
	}
	if fe := parseFirefliesError(string(resp.Body), resp.StatusCode); fe != nil {
		return nil, firefliesClassify(fe)
	}
	m, err := decodeJSONObject("fireflies", resp.Body)
	if err != nil {
		return nil, err
	}
	data, ok := m["data"].(map[string]any)
	if !ok {
		return nil, errors.New("fireflies: reply has no data")
	}
	return data, nil
}

// --- mapping ---

// firefliesStrings reads a field that Fireflies returns either as a string
// or as a list of strings.
func firefliesStrings(v any) []string {
	var out []string
	switch x := v.(type) {
	case string:
		if s := strings.TrimSpace(x); s != "" {
			out = append(out, s)
		}
	case []any:
		for _, it := range x {
			if s, ok := it.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	}
	return out
}

// firefliesSeconds reads a sentence offset (seconds, number or string).
func firefliesSeconds(v any) time.Duration {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case string:
		f, _ = strconv.ParseFloat(strings.TrimSpace(x), 64)
	}
	if f <= 0 {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}

// parseFirefliesTranscript maps a Transcript object to a recording.
func (c *firefliesConnector) parseFirefliesTranscript(t map[string]any) MeetingRecording {
	rec := MeetingRecording{
		ID:       jsonString(t, "id"),
		Topic:    jsonString(t, "title"),
		StartAt:  jsonTime(t, "date", "dateString"),
		Host:     jsonString(t, "organizer_email"),
		URL:      jsonString(t, "transcript_url"),
		HasMedia: true, // the notetaker always records audio
	}
	if d, ok := t["duration"].(float64); ok && d > 0 {
		// Fireflies reports duration in minutes.
		rec.DurationMS = int64(d * float64(time.Minute/time.Millisecond))
	}
	if meetingScopeBool(c.cfg, "include_participants", true) {
		rec.Participants = firefliesParticipants(t)
	}
	return rec
}

// firefliesParticipants prefers attendee display names, then participant
// addresses (which Fireflies sometimes returns comma-joined).
func firefliesParticipants(t map[string]any) []string {
	var out []string
	for _, a := range jsonList(t, "meeting_attendees") {
		if n := jsonString(a, "displayName", "name", "email"); n != "" {
			out = append(out, n)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, p := range firefliesStrings(t["participants"]) {
		for _, s := range strings.Split(p, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// firefliesContent renders the sentences and summary of a Transcript.
func firefliesContent(t map[string]any, participants []string) MeetingContent {
	out := MeetingContent{Participants: participants}
	var segs []transcribe.Segment
	for _, s := range jsonList(t, "sentences") {
		text := jsonString(s, "text")
		if text == "" {
			text = jsonString(s, "raw_text")
		}
		segs = append(segs, transcribe.Segment{Start: firefliesSeconds(s["start_time"]), Speaker: jsonString(s, "speaker_name"), Text: text})
	}
	out.Transcript = transcribe.RenderSegments(segs)
	if sm, ok := t["summary"].(map[string]any); ok {
		out.Summary = firefliesSummary(sm)
	}
	out.Pending = strings.TrimSpace(out.Summary) == "" && strings.TrimSpace(out.Transcript) == ""
	return out
}

// firefliesSummary renders the AI summary: overview, action items, topics
// and keywords.
func firefliesSummary(sm map[string]any) string {
	var parts []string
	overview := strings.Join(firefliesStrings(sm["overview"]), "\n")
	if overview == "" {
		overview = strings.Join(firefliesStrings(sm["short_summary"]), "\n")
	}
	if overview != "" {
		parts = append(parts, overview)
	}
	if items := firefliesStrings(sm["action_items"]); len(items) > 0 {
		parts = append(parts, "### Action items\n\n"+strings.Join(items, "\n"))
	}
	if topics := firefliesStrings(sm["topics_discussed"]); len(topics) > 0 {
		parts = append(parts, "**Topics:** "+strings.Join(topics, ", "))
	}
	if kw := firefliesStrings(sm["keywords"]); len(kw) > 0 {
		parts = append(parts, "**Keywords:** "+strings.Join(kw, ", "))
	}
	return strings.Join(parts, "\n\n")
}

// --- listing ---

// fields is the selection of one Transcript, with text when withText.
func (c *firefliesConnector) fields(withText bool) string {
	f := firefliesMetaFields
	if withText {
		f += " " + firefliesSentenceField
		if meetingScopeBool(c.cfg, "include_summary", true) {
			f += " " + firefliesSummaryField
		}
	}
	return f
}

// listTranscripts pages `transcripts` (limit 50, skip) from since, newest
// first, capped at limit; the raw Transcript objects are returned.
func (c *firefliesConnector) listTranscripts(ctx context.Context, since time.Time, limit int, withText bool) ([]map[string]any, error) {
	q := `query HiveTranscripts($limit: Int, $skip: Int, $fromDate: DateTime, $mine: Boolean) { transcripts(limit: $limit, skip: $skip, fromDate: $fromDate, mine: $mine) { ` + c.fields(withText) + ` } }`
	var out []map[string]any
	skip := 0
	for page := 0; page < firefliesMaxListedPages; page++ {
		vars := map[string]any{"limit": firefliesPageSize, "skip": skip, "fromDate": since.UTC().Format("2006-01-02T15:04:05.000Z")}
		if meetingScopeBool(c.cfg, "mine", false) {
			vars["mine"] = true
		}
		data, err := c.query(ctx, q, vars)
		if err != nil {
			return nil, err
		}
		list := jsonList(data, "transcripts")
		out = append(out, list...)
		if len(list) < firefliesPageSize || len(out) > limit {
			break
		}
		skip += len(list)
	}
	recsSortRaw(out)
	truncated := false
	if len(out) > limit {
		out, truncated = out[:limit], true
	}
	c.mu.Lock()
	c.truncated = truncated
	c.mu.Unlock()
	return out, nil
}

// recsSortRaw orders raw transcripts newest first.
func recsSortRaw(list []map[string]any) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && jsonTime(list[j], "date", "dateString").After(jsonTime(list[j-1], "date", "dateString")); j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

// getTranscript reads one Transcript with the given selection.
func (c *firefliesConnector) getTranscript(ctx context.Context, id, fields string) (map[string]any, error) {
	if !firefliesIDRE.MatchString(id) {
		return nil, fmt.Errorf("fireflies: invalid transcript id %q", id)
	}
	data, err := c.query(ctx, `query HiveTranscript($id: String!) { transcript(id: $id) { `+fields+` } }`, map[string]any{"id": id})
	if err != nil {
		return nil, err
	}
	t, ok := data["transcript"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("fireflies: transcript %s not found", id)
	}
	return t, nil
}

// --- Sync ---

// Sync lists transcripts since the cursor (minus an overlap, which also
// catches summaries that finish after the meeting is first listed), with
// sentences and summary in the same paged query, and emits one page per
// meeting. The listing is incremental, so nothing is tombstoned.
func (c *firefliesConnector) Sync(ctx context.Context, cur Cursor, emit func(Page) error) (Cursor, error) {
	since := meetingSince(c.cfg, cur, c.now())
	reportProgress(ctx, func(p *Progress) { p.Phase = ProgressListing })
	list, err := c.listTranscripts(ctx, since, meetingScopeInt(c.cfg, "max_recordings", meetingDefaultMaxRecordings), true)
	if err != nil {
		c.setAuthFromError(err)
		return cur, err
	}
	total := len(list)
	reportProgress(ctx, func(p *Progress) { p.Phase, p.ItemsTotal = ProgressFetching, total })
	newest := parsePlaudCursor(cur)
	tags := meetingScopeList(c.cfg, "tags")
	for _, t := range list {
		if err := ctx.Err(); err != nil {
			return cur, err
		}
		rec := c.parseFirefliesTranscript(t)
		if rec.ID == "" {
			continue
		}
		title := progressTitle(meetingTitle(TypeFireflies, firefliesLabel, rec))
		reportProgress(ctx, func(p *Progress) { p.Phase, p.Current = ProgressFetching, title })
		rec.Tags = tags
		if err := emit(meetingPage(c.cfg, TypeFireflies, firefliesLabel, rec, firefliesContent(t, rec.Participants), "")); err != nil {
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

// ListMeetings lists transcripts within since_days, newest first.
func (c *firefliesConnector) ListMeetings(ctx context.Context) ([]MeetingRecording, error) {
	since := c.now().Add(-time.Duration(meetingScopeInt(c.cfg, "since_days", meetingDefaultSinceDays)) * 24 * time.Hour)
	list, err := c.listTranscripts(ctx, since, meetingScopeInt(c.cfg, "max_recordings", meetingDefaultMaxRecordings), false)
	if err != nil {
		c.setAuthFromError(err)
		return nil, err
	}
	tags := meetingScopeList(c.cfg, "tags")
	out := make([]MeetingRecording, 0, len(list))
	for _, t := range list {
		rec := c.parseFirefliesTranscript(t)
		if rec.ID == "" {
			continue
		}
		rec.Topic = meetingTitle(TypeFireflies, firefliesLabel, rec)
		rec.Tags = tags
		out = append(out, rec)
	}
	sortMeetingsNewestFirst(out)
	return out, nil
}

// ReadMeeting reads the transcript and summary of one meeting.
func (c *firefliesConnector) ReadMeeting(ctx context.Context, id string) (MeetingContent, error) {
	t, err := c.getTranscript(ctx, id, c.fields(true))
	if err != nil {
		return MeetingContent{}, err
	}
	rec := c.parseFirefliesTranscript(t)
	return firefliesContent(t, rec.Participants), nil
}

// MediaLink implements MeetingMediaReader: Fireflies' audio_url (else
// video_url) is a hashed, token-free download link valid for 24 hours, so
// the browser plays it and the transcriber downloads it directly. Fireflies
// serves it on Pro plans and higher.
func (c *firefliesConnector) MediaLink(ctx context.Context, id string) (MediaLink, error) {
	t, err := c.getTranscript(ctx, id, "audio_url video_url")
	if err != nil {
		var fe *firefliesError
		if errors.As(err, &fe) && fe.Code == "paid_required" {
			return MediaLink{}, fmt.Errorf("fireflies: %w — %v", ErrMeetingNoMedia, fe)
		}
		return MediaLink{}, err
	}
	for _, key := range []string{"audio_url", "video_url"} {
		raw := jsonString(t, key)
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return MediaLink{}, fmt.Errorf("fireflies: %w — %s is not an https link", ErrMeetingNoMedia, key)
		}
		return MediaLink{URL: raw, ExpiresAt: c.now().Add(firefliesMediaTTL)}, nil
	}
	return MediaLink{}, fmt.Errorf("fireflies: %w — Fireflies returned no audio or video link for %s", ErrMeetingNoMedia, id)
}

// --- Authenticator ---

// AuthStatus implements Authenticator. With refresh it reads the API key
// owner (`user`), so a dead key shows on the card before the first sync.
func (c *firefliesConnector) AuthStatus(ctx context.Context, refresh bool) AuthState {
	if refresh {
		c.refreshAuth(ctx)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auth
}

func (c *firefliesConnector) refreshAuth(ctx context.Context) {
	data, err := c.query(ctx, `query HiveUser { user { user_id email name } }`, map[string]any{})
	if err != nil {
		c.setAuthFromError(err)
		return
	}
	u, _ := data["user"].(map[string]any)
	c.setAuth(AuthState{Connected: true, CheckedAt: c.now().UTC(), Account: jsonString(u, "email", "name", "user_id")})
}

func (c *firefliesConnector) setAuth(st AuthState) {
	c.mu.Lock()
	c.auth = st
	c.mu.Unlock()
}

func (c *firefliesConnector) setAuthFromError(err error) {
	c.setAuth(AuthState{CheckedAt: c.now().UTC(), Message: firstLine(err.Error())})
}

// Connect implements Authenticator: an API key has no browser step, so
// Connect probes Fireflies and reports the outcome.
func (c *firefliesConnector) Connect(ctx context.Context) (AuthState, error) {
	c.refreshAuth(ctx)
	return c.AuthStatus(ctx, false), nil
}

// Disconnect implements Authenticator: forgets the cached state. The API key
// lives in the environment; revoke it in Fireflies to cut access.
func (c *firefliesConnector) Disconnect(ctx context.Context) (AuthState, error) {
	st := AuthState{CheckedAt: c.now().UTC(), Message: "cached state cleared; the API key in the environment is unchanged (regenerate it in Fireflies → Integrations → Fireflies API to revoke)"}
	c.setAuth(st)
	return st, nil
}
