package connector

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TypeWebex is the connector type for Webex Meetings recordings,
// transcripts and AI Assistant summaries.
const TypeWebex = "webex"

// webexAPIBase is the Webex REST API (OAuth authorize and token endpoints
// live under it too). A variable so tests point it at a fake server.
var webexAPIBase = "https://webexapis.com/v1"

const (
	// webexLabel is the human name used in headings and messages.
	webexLabel = "Webex"
	// webexScopes are the OAuth scopes requested by default: recordings,
	// transcripts, AI Assistant summaries, participants, and the signed-in
	// person for the card. scope.oauth_scopes overrides them (the request
	// must be a subset of the scopes selected on the integration).
	webexScopes = "spark:people_read meeting:recordings_read meeting:transcripts_read meeting:summaries_read meeting:participants_read"
	// webexDefaultRedirectURI is the OAuth redirect the operator registers on
	// the integration when the hive has no public callback: the browser lands
	// on a connection error and the URL is pasted into the card.
	webexDefaultRedirectURI = "http://localhost:8199/webex/callback"
	// webexPageSize is the `max` requested on listings (the API maximum).
	webexPageSize = 100
	// webexListWindow is the from/to span of one listing request; the API
	// defaults a missing bound to 7 days, so the window is always explicit.
	webexListWindow = 30 * 24 * time.Hour
	// webexTokenFile is the token store, relative to the connector's HOME.
	webexTokenFile = ".webex/tokens.json"
	// webexLoginWindow is how long a started sign-in accepts its pasted
	// redirect URL.
	webexLoginWindow = 10 * time.Minute
	// webexPendingRefetchCap bounds how many text-less meetings are re-read
	// per sync on top of the listing.
	webexPendingRefetchCap = 25
	// webexStatusAvailable is the status of a live recording / transcript /
	// summary (the others are deleted or purged).
	webexStatusAvailable = "available"
)

// webexLinkNextRE pulls the rel="next" target out of a Link header.
var webexLinkNextRE = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="?next"?`)

// webexIDRE bounds an id before it is used in a path or query.
var webexIDRE = regexp.MustCompile(`^[A-Za-z0-9_=-]{1,256}$`)

// webexSiteRE bounds scope.site_url (a Webex site host, e.g. acme.webex.com).
var webexSiteRE = regexp.MustCompile(`^[A-Za-z0-9.-]+\.[A-Za-z]{2,}$`)

// webexMeeting is one ended meeting instance with its recordings and
// transcripts, keyed by the meeting instance id.
type webexMeeting struct {
	MeetingRecording
	RecordingIDs  []string
	TranscriptIDs []string
}

// webexConnector syncs Webex Meetings recordings and transcripts into one
// fact per ended meeting instance. Auth: a Webex integration (OAuth
// authorization code) registered by the operator; the refresh token is kept
// in the connector token store. Scope keys: since_days, max_recordings,
// include_summary, include_participants, include_video_link, site_url,
// redirect_uri, oauth_scopes, tags.
type webexConnector struct {
	cfg      ConnectorConfig
	logger   *slog.Logger
	http     *HTTPClient
	now      func() time.Time
	store    tokenStore
	vaultDir string

	mu           sync.Mutex
	pendingState string
	pendingAt    time.Time
	auth         AuthState
	truncated    bool
}

var (
	_ Authenticator      = (*webexConnector)(nil)
	_ AuthCompleter      = (*webexConnector)(nil)
	_ MeetingReader      = (*webexConnector)(nil)
	_ MeetingMediaReader = (*webexConnector)(nil)
	_ Truncator          = (*webexConnector)(nil)
)

func newWebexConnector(cfg ConnectorConfig, deps Deps) (Connector, error) {
	c := &webexConnector{cfg: cfg, logger: deps.logger(), http: deps.httpClient(), now: time.Now}
	if deps.StateDir != "" {
		home := filepath.Join(deps.StateDir, "home")
		if _, err := ensureDir(home); err != nil {
			return nil, fmt.Errorf("webex: %w", err)
		}
		c.store = tokenStore{path: filepath.Join(home, webexTokenFile)}
	}
	if deps.KnowledgeDir != "" && cfg.Layer != "" {
		c.vaultDir = filepath.Join(deps.KnowledgeDir, "connectors", cfg.Layer)
	}
	return c, nil
}

func (c *webexConnector) Type() string { return TypeWebex }

// Validate checks the auth block and scope. Credentials are referenced by
// environment variable name only.
func (c *webexConnector) Validate(cfg ConnectorConfig) error {
	if cfg.Auth.Configured() {
		return errors.New("webex: auth.env/auth.file are not used; set auth.client_id_env and auth.client_secret_env (the Webex integration)")
	}
	if cfg.Auth.Mode != "" && cfg.Auth.Mode != AuthModeUserOAuth {
		return fmt.Errorf("webex: auth.mode must be empty or %s (a Webex integration signs in as a user)", AuthModeUserOAuth)
	}
	if cfg.Auth.ClientIDEnv == "" || cfg.Auth.ClientSecretEnv == "" {
		return errors.New("webex: auth.client_id_env and auth.client_secret_env are required (environment variable names)")
	}
	if cfg.Auth.AccountIDEnv != "" || cfg.Auth.TenantIDEnv != "" {
		return errors.New("webex: auth.account_id_env / auth.tenant_id_env are zoom / teams settings")
	}
	if r := cfg.ScopeValue("redirect_uri"); r != "" {
		u, err := url.Parse(r)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("webex: scope.redirect_uri must be an absolute http(s) URL registered on the integration")
		}
	}
	if s := cfg.ScopeValue("site_url"); s != "" && !webexSiteRE.MatchString(s) {
		return errors.New("webex: scope.site_url must be a Webex site host such as acme.webex.com")
	}
	return validateMeetingScope("webex", cfg)
}

func (c *webexConnector) redirectURI() string {
	if r := c.cfg.ScopeValue("redirect_uri"); r != "" {
		return r
	}
	return webexDefaultRedirectURI
}

func (c *webexConnector) scopes() string {
	if s := strings.Join(strings.FieldsFunc(c.cfg.ScopeValue("oauth_scopes"), func(r rune) bool { return r == ',' || r == ' ' }), " "); s != "" {
		return s
	}
	return webexScopes
}

// Truncated implements Truncator.
func (c *webexConnector) Truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}

// --- OAuth ---

func (c *webexConnector) clientCredentials() (id, secret string, err error) {
	if id, err = envValue("webex", "client_id_env", c.cfg.Auth.ClientIDEnv); err != nil {
		return "", "", err
	}
	if secret, err = envValue("webex", "client_secret_env", c.cfg.Auth.ClientSecretEnv); err != nil {
		return "", "", err
	}
	return id, secret, nil
}

// tokenRequest posts form (plus the client credentials) to the Webex token
// endpoint and decodes the reply.
func (c *webexConnector) tokenRequest(ctx context.Context, form url.Values) (oauthTokens, error) {
	id, secret, err := c.clientCredentials()
	if err != nil {
		return oauthTokens{}, err
	}
	form.Set("client_id", id)
	form.Set("client_secret", secret)
	h := http.Header{}
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(ctx, http.MethodPost, webexAPIBase+"/access_token", h, []byte(form.Encode()))
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) {
			return oauthTokens{}, fmt.Errorf("webex: token request rejected (HTTP %d): %s", se.StatusCode, webexErrorText(se.Body))
		}
		return oauthTokens{}, fmt.Errorf("webex: token request: %w", err)
	}
	m, err := decodeJSONObject("webex", resp.Body)
	if err != nil {
		return oauthTokens{}, err
	}
	tok := oauthTokens{AccessToken: jsonString(m, "access_token"), RefreshToken: jsonString(m, "refresh_token"), Scope: jsonString(m, "scope")}
	if tok.AccessToken == "" {
		return oauthTokens{}, errors.New("webex: token reply carried no access_token")
	}
	ttl := time.Hour
	if v, ok := m["expires_in"].(float64); ok && v > 0 {
		ttl = time.Duration(v) * time.Second
	}
	tok.ExpiresAt = c.now().Add(ttl)
	return tok, nil
}

// webexErrorText pulls the readable part out of a Webex error body.
func webexErrorText(body string) string {
	if m, err := decodeJSONObject("webex", []byte(body)); err == nil {
		if msg := jsonString(m, "message", "error_description"); msg != "" {
			return firstLine(msg)
		}
		if code := jsonString(m, "error"); code != "" {
			return code
		}
	}
	return firstLine(body)
}

// accessToken returns the stored token, refreshed (and the rotated refresh
// token written back) when it is about to expire.
func (c *webexConnector) accessToken(ctx context.Context) (oauthTokens, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tok, err := c.store.load()
	if err != nil {
		if os.IsNotExist(err) {
			return oauthTokens{}, fmt.Errorf("webex: %w — use Connect in the Knowledge → Integrations → Webex card", ErrMeetingNotConnected)
		}
		return oauthTokens{}, fmt.Errorf("webex: %w", err)
	}
	if tok.valid(c.now()) {
		return tok, nil
	}
	if tok.RefreshToken == "" {
		return oauthTokens{}, fmt.Errorf("webex: %w — the stored sign-in has no refresh token; use Connect in the Knowledge → Integrations → Webex card", ErrMeetingNotConnected)
	}
	fresh, err := c.tokenRequest(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}})
	if err != nil {
		return oauthTokens{}, fmt.Errorf("webex: %w — refreshing the sign-in failed, use Connect in the Knowledge → Integrations → Webex card (%v)", ErrMeetingNotConnected, err)
	}
	fresh.Account = tok.Account
	if fresh.RefreshToken == "" {
		fresh.RefreshToken = tok.RefreshToken
	}
	if err := c.store.save(fresh); err != nil {
		return oauthTokens{}, fmt.Errorf("webex: saving refreshed token: %w", err)
	}
	return fresh, nil
}

// webexAuthError: 401 is "not connected"; 403 is a scope the integration
// was not granted (or a feature the site lacks), kept with Webex's reason.
func webexAuthError(err error) error {
	if err == nil || errors.Is(err, ErrMeetingNotConnected) {
		return err
	}
	var se *StatusError
	if !errors.As(err, &se) {
		return err
	}
	switch se.StatusCode {
	case http.StatusUnauthorized:
		return meetingNotConnected("webex", webexLabel, err)
	case http.StatusForbidden:
		return fmt.Errorf("webex: forbidden — the integration lacks a scope for this call or the site does not offer the feature (%s)", webexErrorText(se.Body))
	}
	return err
}

// webexTrustedURL keeps the bearer token on the Webex API host (Link
// headers come from the reply).
func webexTrustedURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("webex: invalid link")
	}
	base, err := url.Parse(webexAPIBase)
	if err != nil {
		return err
	}
	if !strings.EqualFold(u.Host, base.Host) {
		return fmt.Errorf("webex: link host %q is not the Webex API host", u.Host)
	}
	return nil
}

// get performs an authenticated GET.
func (c *webexConnector) get(ctx context.Context, token, link string) (*Response, error) {
	if err := webexTrustedURL(link); err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("timezone", "UTC")
	resp, err := c.http.Get(ctx, link, h)
	if err != nil {
		return nil, webexAuthError(err)
	}
	return resp, nil
}

// webexNextLink returns the rel="next" URL of a Link header, if any.
func webexNextLink(h http.Header) string {
	for _, v := range h.Values("Link") {
		if m := webexLinkNextRE.FindStringSubmatch(v); m != nil {
			return m[1]
		}
	}
	return ""
}

// paged follows Link rel="next" through a listing, calling each for every
// object under items.
func (c *webexConnector) paged(ctx context.Context, token, link string, each func(map[string]any)) error {
	for link != "" {
		resp, err := c.get(ctx, token, link)
		if err != nil {
			return err
		}
		m, err := decodeJSONObject("webex", resp.Body)
		if err != nil {
			return err
		}
		for _, item := range jsonList(m, "items") {
			each(item)
		}
		link = webexNextLink(resp.Header)
	}
	return nil
}

// listURL builds a listing URL with the configured site.
func (c *webexConnector) listURL(path string, q url.Values) string {
	if s := c.cfg.ScopeValue("site_url"); s != "" {
		q.Set("siteUrl", s)
	}
	return webexAPIBase + path + "?" + q.Encode()
}

// --- listing ---

// webexMeetings groups recordings and transcripts by meeting instance id.
type webexMeetings struct {
	byID  map[string]*webexMeeting
	order []string
}

func (g *webexMeetings) get(id string) *webexMeeting {
	if g.byID == nil {
		g.byID = map[string]*webexMeeting{}
	}
	wm, ok := g.byID[id]
	if !ok {
		wm = &webexMeeting{}
		wm.ID, wm.MeetingID = id, id
		g.byID[id] = wm
		g.order = append(g.order, id)
	}
	return wm
}

// addRecording merges one recordings[] item.
func (g *webexMeetings) addRecording(r map[string]any) {
	id := jsonString(r, "meetingId")
	if id == "" || (jsonString(r, "status") != "" && jsonString(r, "status") != webexStatusAvailable) {
		return
	}
	wm := g.get(id)
	wm.RecordingIDs = append(wm.RecordingIDs, jsonString(r, "id"))
	wm.HasMedia = true
	if wm.Topic == "" {
		wm.Topic = jsonString(r, "topic")
	}
	if start := jsonTime(r, "timeRecorded", "createTime"); !start.IsZero() && (wm.StartAt.IsZero() || start.Before(wm.StartAt)) {
		wm.StartAt = start
	}
	if d, ok := r["durationSeconds"].(float64); ok && int64(d*1000) > wm.DurationMS {
		wm.DurationMS = int64(d * 1000)
	}
	if wm.URL == "" {
		wm.URL = jsonString(r, "playbackUrl")
	}
	if wm.Host == "" {
		wm.Host = jsonString(r, "hostEmail")
	}
}

// addTranscript merges one meetingTranscripts[] item.
func (g *webexMeetings) addTranscript(t map[string]any) {
	id := jsonString(t, "meetingId")
	if id == "" || (jsonString(t, "status") != "" && jsonString(t, "status") != webexStatusAvailable) {
		return
	}
	wm := g.get(id)
	wm.TranscriptIDs = append(wm.TranscriptIDs, jsonString(t, "id"))
	if wm.Topic == "" {
		wm.Topic = jsonString(t, "meetingTopic")
	}
	if start := jsonTime(t, "startTime"); !start.IsZero() && (wm.StartAt.IsZero() || start.Before(wm.StartAt)) {
		wm.StartAt = start
	}
}

// list lists the recordings and transcripts in [since, now] (in windows of
// webexListWindow), optionally for one meeting, grouped per meeting.
func (c *webexConnector) list(ctx context.Context, token string, since time.Time, meetingID string) (*webexMeetings, error) {
	g := &webexMeetings{}
	now := c.now().UTC().Add(time.Minute)
	for end := now; end.After(since); end = end.Add(-webexListWindow) {
		start := end.Add(-webexListWindow)
		if start.Before(since) {
			start = since
		}
		q := func() url.Values {
			v := url.Values{"from": {start.Format(time.RFC3339)}, "to": {end.Format(time.RFC3339)}, "max": {strconv.Itoa(webexPageSize)}}
			if meetingID != "" {
				v.Set("meetingId", meetingID)
			}
			return v
		}
		if err := c.paged(ctx, token, c.listURL("/recordings", q()), g.addRecording); err != nil {
			return nil, fmt.Errorf("recordings: %w", err)
		}
		if err := c.paged(ctx, token, c.listURL("/meetingTranscripts", q()), g.addTranscript); err != nil {
			return nil, fmt.Errorf("transcripts: %w", err)
		}
	}
	return g, nil
}

// listMeetings lists meetings since, newest first, capped at limit.
func (c *webexConnector) listMeetings(ctx context.Context, token string, since time.Time, limit int) ([]webexMeeting, error) {
	g, err := c.list(ctx, token, since, "")
	if err != nil {
		return nil, err
	}
	out := make([]webexMeeting, 0, len(g.order))
	for _, id := range g.order {
		out = append(out, *g.byID[id])
	}
	sortWebexNewestFirst(out)
	truncated := false
	if len(out) > limit {
		out, truncated = out[:limit], true
	}
	c.mu.Lock()
	c.truncated = truncated
	c.mu.Unlock()
	return out, nil
}

func sortWebexNewestFirst(ms []webexMeeting) {
	for i := 1; i < len(ms); i++ {
		for j := i; j > 0 && ms[j].StartAt.After(ms[j-1].StartAt); j-- {
			ms[j], ms[j-1] = ms[j-1], ms[j]
		}
	}
}

// lookbackStart is the start of the since_days window used to find one
// meeting by id (the listing APIs default a missing range to 7 days).
func (c *webexConnector) lookbackStart() time.Time {
	return c.now().UTC().Add(-time.Duration(meetingScopeInt(c.cfg, "since_days", meetingDefaultSinceDays)) * 24 * time.Hour)
}

// getMeeting reads one meeting instance by id within the since_days window.
func (c *webexConnector) getMeeting(ctx context.Context, token, id string) (webexMeeting, error) {
	if !webexIDRE.MatchString(id) {
		return webexMeeting{}, fmt.Errorf("webex: invalid meeting id %q", id)
	}
	g, err := c.list(ctx, token, c.lookbackStart(), id)
	if err != nil {
		return webexMeeting{}, err
	}
	wm, ok := g.byID[id]
	if !ok {
		return webexMeeting{}, fmt.Errorf("webex: meeting %s has no recording or transcript in the last %d days", id, meetingScopeInt(c.cfg, "since_days", meetingDefaultSinceDays))
	}
	return *wm, nil
}

// --- content ---

// renderWebexSummary turns a meetingSummaries item into markdown: the
// AI Assistant notes (HTML) and the action items.
func renderWebexSummary(item map[string]any) string {
	var b strings.Builder
	if notes, ok := item["notes"].(map[string]any); ok {
		if html := jsonString(notes, "content"); html != "" {
			_, md := HTMLToMarkdown([]byte(html))
			b.WriteString(strings.TrimSpace(md))
			b.WriteString("\n\n")
		}
	}
	var actions []string
	for _, a := range jsonList(item, "actionItems") {
		if t := strings.TrimSpace(jsonString(a, "content")); t != "" {
			actions = append(actions, "- "+t)
		}
	}
	if len(actions) > 0 {
		b.WriteString("### Action items\n\n" + strings.Join(actions, "\n"))
	}
	return strings.TrimSpace(b.String())
}

// fetchContent reads the newest transcript (VTT), the AI Assistant summary
// and the participants of one meeting. Summary and participants are
// optional: a missing scope or feature only drops that section.
func (c *webexConnector) fetchContent(ctx context.Context, token string, wm webexMeeting) (MeetingContent, error) {
	var out MeetingContent
	if n := len(wm.TranscriptIDs); n > 0 {
		id := wm.TranscriptIDs[0]
		if !webexIDRE.MatchString(id) {
			return MeetingContent{}, fmt.Errorf("webex: invalid transcript id %q", id)
		}
		resp, err := c.get(ctx, token, webexAPIBase+"/meetingTranscripts/"+url.PathEscape(id)+"/download?format=vtt")
		if err != nil {
			return MeetingContent{}, fmt.Errorf("transcript: %w", err)
		}
		out.Transcript = renderVTT(string(resp.Body))
	}
	if meetingScopeBool(c.cfg, "include_summary", true) {
		var summary string
		err := c.paged(ctx, token, webexAPIBase+"/meetingSummaries?"+url.Values{"meetingId": {wm.MeetingID}}.Encode(), func(item map[string]any) {
			if summary == "" && (jsonString(item, "status") == "" || jsonString(item, "status") == webexStatusAvailable) {
				summary = renderWebexSummary(item)
			}
		})
		switch {
		case err == nil:
			out.Summary = summary
		case errors.Is(err, ErrMeetingNotConnected):
			return MeetingContent{}, err
		default:
			c.logger.Debug("webex meeting summary unavailable", "meeting", wm.MeetingID, "error", err)
		}
	}
	if meetingScopeBool(c.cfg, "include_participants", true) {
		var names []string
		err := c.paged(ctx, token, webexAPIBase+"/meetingParticipants?"+url.Values{"meetingId": {wm.MeetingID}, "max": {strconv.Itoa(webexPageSize)}}.Encode(), func(p map[string]any) {
			if n := jsonString(p, "displayName", "email"); n != "" {
				names = append(names, n)
			}
		})
		switch {
		case err == nil:
			out.Participants = names
		case errors.Is(err, ErrMeetingNotConnected):
			return MeetingContent{}, err
		default:
			c.logger.Debug("webex participants unavailable", "meeting", wm.MeetingID, "error", err)
		}
	}
	out.Pending = strings.TrimSpace(out.Summary) == "" && strings.TrimSpace(out.Transcript) == ""
	return out, nil
}

// mediaLine is the optional fact line linking the Webex playback page.
func (c *webexConnector) mediaLine(wm webexMeeting) string {
	if !meetingScopeBool(c.cfg, "include_video_link", false) || wm.URL == "" {
		return ""
	}
	return fmt.Sprintf("- **Recording (%s):** %s\n", webexLabel, wm.URL)
}

// --- Sync ---

// Sync lists recordings and transcripts since the cursor (minus an overlap
// window) and emits one page per meeting. The listing is incremental, so
// unlisted pages are never tombstoned.
func (c *webexConnector) Sync(ctx context.Context, cur Cursor, emit func(Page) error) (Cursor, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		c.setAuthFromError(err)
		return cur, err
	}
	since := meetingSince(c.cfg, cur, c.now())
	reportProgress(ctx, func(p *Progress) { p.Phase = ProgressListing })
	meetings, err := c.listMeetings(ctx, tok.AccessToken, since, meetingScopeInt(c.cfg, "max_recordings", meetingDefaultMaxRecordings))
	if err != nil {
		c.setAuthFromError(err)
		return cur, err
	}
	listed := make(map[string]bool, len(meetings))
	for _, wm := range meetings {
		listed[wm.ID] = true
	}
	for _, id := range meetingPendingIDs(c.vaultDir, c.cfg, listed, webexPendingRefetchCap) {
		wm, err := c.getMeeting(ctx, tok.AccessToken, id)
		if err != nil {
			c.logger.Debug("webex pending meeting not re-read", "id", id, "error", err)
			continue
		}
		meetings = append(meetings, wm)
	}
	total := len(meetings)
	reportProgress(ctx, func(p *Progress) { p.Phase, p.ItemsTotal = ProgressFetching, total })
	newest := parsePlaudCursor(cur)
	var skipped []string
	for _, wm := range meetings {
		if err := ctx.Err(); err != nil {
			return cur, err
		}
		title := progressTitle(meetingTitle(TypeWebex, webexLabel, wm.MeetingRecording))
		reportProgress(ctx, func(p *Progress) { p.Phase, p.Current = ProgressFetching, title })
		content, err := c.fetchContent(ctx, tok.AccessToken, wm)
		if err != nil {
			if errors.Is(err, ErrMeetingNotConnected) {
				c.setAuthFromError(err)
				return cur, err
			}
			skipped = append(skipped, fmt.Sprintf("%s: %v", wm.ID, err))
			reportProgress(ctx, func(p *Progress) { p.ItemsDone++ })
			continue
		}
		wm.Tags = meetingScopeList(c.cfg, "tags")
		if err := emit(meetingPage(c.cfg, TypeWebex, webexLabel, wm.MeetingRecording, content, c.mediaLine(wm))); err != nil {
			return cur, err
		}
		if wm.StartAt.After(newest) {
			newest = wm.StartAt
		}
	}
	c.refreshAuth(ctx)
	next := cur
	if !newest.IsZero() {
		next = Cursor(newest.UTC().Format(time.RFC3339))
	}
	if len(skipped) > 0 {
		return next, &PartialError{Err: fmt.Errorf("webex: %d meeting(s) skipped: %s", len(skipped), strings.Join(skipped, "; "))}
	}
	return next, nil
}

// --- MeetingReader / MeetingMediaReader ---

// ListMeetings lists meetings within the since_days window, newest first.
func (c *webexConnector) ListMeetings(ctx context.Context) ([]MeetingRecording, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	meetings, err := c.listMeetings(ctx, tok.AccessToken, c.lookbackStart(), meetingScopeInt(c.cfg, "max_recordings", meetingDefaultMaxRecordings))
	if err != nil {
		c.setAuthFromError(err)
		return nil, err
	}
	tags := meetingScopeList(c.cfg, "tags")
	out := make([]MeetingRecording, 0, len(meetings))
	for _, wm := range meetings {
		rec := wm.MeetingRecording
		rec.Topic = meetingTitle(TypeWebex, webexLabel, rec)
		rec.Tags = tags
		out = append(out, rec)
	}
	sortMeetingsNewestFirst(out)
	return out, nil
}

// ReadMeeting reads the transcript, summary and participants of a meeting.
func (c *webexConnector) ReadMeeting(ctx context.Context, id string) (MeetingContent, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return MeetingContent{}, err
	}
	wm, err := c.getMeeting(ctx, tok.AccessToken, id)
	if err != nil {
		return MeetingContent{}, err
	}
	return c.fetchContent(ctx, tok.AccessToken, wm)
}

// MediaLink implements MeetingMediaReader: the recording details carry
// temporary direct download links (audio, else the MP4) that need no bearer
// token and expire three hours after the request. Nothing is persisted.
func (c *webexConnector) MediaLink(ctx context.Context, id string) (MediaLink, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return MediaLink{}, err
	}
	wm, err := c.getMeeting(ctx, tok.AccessToken, id)
	if err != nil {
		return MediaLink{}, err
	}
	if len(wm.RecordingIDs) == 0 {
		return MediaLink{}, fmt.Errorf("webex: %w (transcript only)", ErrMeetingNoMedia)
	}
	rid := wm.RecordingIDs[0]
	if !webexIDRE.MatchString(rid) {
		return MediaLink{}, fmt.Errorf("webex: invalid recording id %q", rid)
	}
	resp, err := c.get(ctx, tok.AccessToken, webexAPIBase+"/recordings/"+url.PathEscape(rid))
	if err != nil {
		return MediaLink{}, err
	}
	m, err := decodeJSONObject("webex", resp.Body)
	if err != nil {
		return MediaLink{}, err
	}
	links, _ := m["temporaryDirectDownloadLinks"].(map[string]any)
	link := jsonString(links, "audioDownloadLink", "recordingDownloadLink")
	if link == "" {
		return MediaLink{}, fmt.Errorf("webex: %w — the recording has no direct download link (Prevent downloading is on, or the format is ARF); play it on Webex: %s", ErrMeetingNoMedia, jsonString(m, "playbackUrl"))
	}
	if u, err := url.Parse(link); err != nil || u.Scheme != "https" {
		return MediaLink{}, errors.New("webex: the direct download link is not https")
	}
	expires := jsonTime(links, "expiration")
	if expires.IsZero() {
		expires = c.now().Add(meetingMediaLinkTTLFallback)
	}
	return MediaLink{URL: link, ExpiresAt: expires}, nil
}

// --- Authenticator / AuthCompleter ---

// AuthStatus implements Authenticator. With refresh it refreshes the token
// and reads the signed-in person.
func (c *webexConnector) AuthStatus(ctx context.Context, refresh bool) AuthState {
	c.mu.Lock()
	st := c.auth
	pending := c.pendingState != "" && c.now().Before(c.pendingAt.Add(webexLoginWindow))
	c.mu.Unlock()
	if pending {
		st.Pending = true
		return st
	}
	st.Pending = false
	if !refresh {
		if st.CheckedAt.IsZero() {
			if tok, err := c.store.load(); err == nil && (tok.RefreshToken != "" || tok.AccessToken != "") {
				st.Connected, st.Account = true, tok.Account
			}
		}
		return st
	}
	c.refreshAuth(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auth
}

// refreshAuth probes the signed-in person (spark:people_read), falling back
// to a one-item recordings listing when that scope was not requested.
func (c *webexConnector) refreshAuth(ctx context.Context) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		c.setAuthFromError(err)
		return
	}
	account := tok.Account
	resp, err := c.get(ctx, tok.AccessToken, webexAPIBase+"/people/me")
	if err == nil {
		if m, derr := decodeJSONObject("webex", resp.Body); derr == nil {
			if emails, ok := m["emails"].([]any); ok && len(emails) > 0 {
				account, _ = emails[0].(string)
			}
			if account == "" {
				account = jsonString(m, "displayName")
			}
		}
	} else if !errors.Is(err, ErrMeetingNotConnected) {
		_, err = c.get(ctx, tok.AccessToken, c.listURL("/recordings", url.Values{"max": {"1"}}))
	}
	if err != nil {
		c.setAuthFromError(err)
		return
	}
	if account != "" && account != tok.Account {
		tok.Account = account
		c.mu.Lock()
		_ = c.store.save(tok)
		c.mu.Unlock()
	}
	c.setAuth(AuthState{Connected: true, CheckedAt: c.now().UTC(), Account: account})
}

func (c *webexConnector) setAuth(st AuthState) {
	c.mu.Lock()
	c.auth = st
	c.mu.Unlock()
}

func (c *webexConnector) setAuthFromError(err error) {
	c.setAuth(AuthState{CheckedAt: c.now().UTC(), Message: firstLine(err.Error())})
}

// Connect implements Authenticator: returns the integration's authorization
// URL, pending until the redirect URL is pasted.
func (c *webexConnector) Connect(ctx context.Context) (AuthState, error) {
	id, _, err := c.clientCredentials()
	if err != nil {
		return c.AuthStatus(ctx, false), err
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return c.AuthStatus(ctx, false), fmt.Errorf("webex: generating state: %w", err)
	}
	state := hex.EncodeToString(buf)
	q := url.Values{
		"response_type": {"code"},
		"client_id":     {id},
		"redirect_uri":  {c.redirectURI()},
		"scope":         {c.scopes()},
		"state":         {state},
	}
	c.mu.Lock()
	c.pendingState, c.pendingAt = state, c.now()
	c.auth = AuthState{Pending: true, AuthURL: webexAPIBase + "/authorize?" + q.Encode(), CheckedAt: c.now().UTC()}
	st := c.auth
	c.mu.Unlock()
	return st, nil
}

// CompleteAuth implements AuthCompleter: the operator pastes the redirect
// URL the browser landed on; its code is exchanged and the tokens stored.
func (c *webexConnector) CompleteAuth(ctx context.Context, comp AuthCompletion) (AuthState, error) {
	if strings.TrimSpace(comp.Tokens) != "" {
		return c.AuthStatus(ctx, false), errors.New("webex: paste the redirect URL the browser landed on; token JSON is not accepted")
	}
	u, err := url.Parse(strings.TrimSpace(comp.RedirectURL))
	if err != nil || u.RawQuery == "" {
		return c.AuthStatus(ctx, false), errors.New("webex: paste the full URL the browser landed on (it contains ?code=...&state=...)")
	}
	if e := u.Query().Get("error"); e != "" {
		return c.AuthStatus(ctx, false), fmt.Errorf("webex: Webex refused the sign-in: %s", e)
	}
	code := u.Query().Get("code")
	if code == "" {
		return c.AuthStatus(ctx, false), errors.New("webex: the pasted URL has no code parameter")
	}
	c.mu.Lock()
	state, at := c.pendingState, c.pendingAt
	c.mu.Unlock()
	if state == "" || c.now().After(at.Add(webexLoginWindow)) {
		return c.AuthStatus(ctx, false), errors.New("webex: no sign-in is pending; press Connect first, then paste the redirect URL within the login window")
	}
	if got := u.Query().Get("state"); got != "" && got != state {
		return c.AuthStatus(ctx, false), errors.New("webex: the pasted URL belongs to a different sign-in attempt (state mismatch); press Connect again")
	}
	tok, err := c.tokenRequest(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {c.redirectURI()}})
	if err != nil {
		return c.AuthStatus(ctx, false), err
	}
	if err := c.store.save(tok); err != nil {
		return c.AuthStatus(ctx, false), fmt.Errorf("webex: storing tokens: %w", err)
	}
	c.mu.Lock()
	c.pendingState = ""
	c.mu.Unlock()
	c.refreshAuth(ctx)
	return c.AuthStatus(ctx, false), nil
}

// Disconnect implements Authenticator: clears the token store. Webex has no
// token revoke endpoint for integrations; the grant can be removed by the
// user under Webex settings → Authorized apps.
func (c *webexConnector) Disconnect(ctx context.Context) (AuthState, error) {
	c.mu.Lock()
	c.pendingState = ""
	c.mu.Unlock()
	if err := c.store.clear(); err != nil {
		return c.AuthStatus(ctx, false), fmt.Errorf("webex: %w", err)
	}
	st := AuthState{CheckedAt: c.now().UTC(), Message: "signed out; remove the grant under Webex settings → Authorized apps to revoke it upstream"}
	c.setAuth(st)
	return st, nil
}
