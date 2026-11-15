package connector

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TypeZoom is the connector type for Zoom cloud recordings.
const TypeZoom = "zoom"

// Zoom endpoints. Variables so tests point them at a fake server.
var (
	zoomAPIBase   = "https://api.zoom.us/v2"
	zoomOAuthBase = "https://zoom.us/oauth"
)

const (
	// zoomLabel is the human name used in headings and messages.
	zoomLabel = "Zoom"
	// zoomDefaultRedirectURI is the OAuth redirect the operator registers on
	// the Zoom app when the hive has no public callback: the browser lands on
	// a connection error there and the operator pastes the URL into the card,
	// exactly as for Plaud (localhost:8199).
	zoomDefaultRedirectURI = "http://localhost:8199/zoom/callback"
	// zoomDefaultUser is the recordings owner for user-level OAuth.
	zoomDefaultUser = "me"
	// zoomListPageSize is the recordings page size (API maximum).
	zoomListPageSize = 300
	// zoomListWindow is the widest from/to span one recordings listing may
	// cover (the API rejects more than a month).
	zoomListWindow = 30 * 24 * time.Hour
	// zoomTokenFile is the user-level OAuth token store, relative to the
	// connector's private HOME.
	zoomTokenFile = ".zoom/tokens.json"
	// zoomLoginWindow is how long a started sign-in accepts its pasted
	// redirect URL.
	zoomLoginWindow = 10 * time.Minute
	// zoomMediaTokenTTL is the lifetime requested for a recording download
	// token minted for playback.
	zoomMediaTokenTTL = time.Hour
	// zoomPendingRefetchCap bounds how many text-less recordings are re-read
	// per sync on top of the listing.
	zoomPendingRefetchCap = 25
	// Zoom recording_files file types.
	zoomFileTranscript = "TRANSCRIPT"
	zoomFileAudio      = "M4A"
	zoomFileVideo      = "MP4"
	zoomFileCompleted  = "completed"
)

// zoomFile is one recording_files[] entry.
type zoomFile struct {
	ID            string
	FileType      string
	RecordingType string
	DownloadURL   string
	PlayURL       string
	Status        string
	Start, End    time.Time
}

// zoomMeeting is one meetings[] entry of a recordings listing.
type zoomMeeting struct {
	MeetingRecording
	UUID  string
	Files []zoomFile
}

// zoomConnector syncs Zoom cloud recordings into one fact per recorded
// meeting. Auth: an OAuth app registered by the operator, either user-level
// (authorization code, refresh token kept in the connector token store) or
// Server-to-Server (account_credentials grant). Scope keys: users (comma
// separated user ids/emails; "me" for user-level), since_days,
// max_recordings, include_video_link, include_summary, include_participants,
// redirect_uri, tags.
type zoomConnector struct {
	cfg    ConnectorConfig
	logger *slog.Logger
	http   *HTTPClient
	now    func() time.Time
	store  tokenStore
	// vaultDir is where this connector's facts live; empty disables the
	// pending refetch.
	vaultDir string

	mu sync.Mutex
	// s2s caches the account_credentials token (never persisted).
	s2s oauthTokens
	// pendingState / pendingAt track an in-flight user-level sign-in.
	pendingState string
	pendingAt    time.Time
	auth         AuthState
	truncated    bool
}

var (
	_ Authenticator      = (*zoomConnector)(nil)
	_ AuthCompleter      = (*zoomConnector)(nil)
	_ MeetingReader      = (*zoomConnector)(nil)
	_ MeetingMediaReader = (*zoomConnector)(nil)
	_ Truncator          = (*zoomConnector)(nil)
)

func newZoomConnector(cfg ConnectorConfig, deps Deps) (Connector, error) {
	c := &zoomConnector{cfg: cfg, logger: deps.logger(), http: deps.httpClient(), now: time.Now}
	if deps.StateDir != "" {
		home := filepath.Join(deps.StateDir, "home")
		if _, err := ensureDir(home); err != nil {
			return nil, fmt.Errorf("zoom: %w", err)
		}
		c.store = tokenStore{path: filepath.Join(home, zoomTokenFile)}
	}
	if deps.KnowledgeDir != "" && cfg.Layer != "" {
		c.vaultDir = filepath.Join(deps.KnowledgeDir, "connectors", cfg.Layer)
	}
	return c, nil
}

func (c *zoomConnector) Type() string { return TypeZoom }

// Validate checks the auth block and scope. Credentials are referenced by
// environment variable name only.
func (c *zoomConnector) Validate(cfg ConnectorConfig) error {
	if cfg.Auth.Configured() {
		return errors.New("zoom: auth.env/auth.file are not used; set auth.client_id_env and auth.client_secret_env (and auth.account_id_env for server_to_server)")
	}
	switch cfg.Auth.Mode {
	case "", AuthModeUserOAuth, AuthModeServerToServer:
	default:
		return fmt.Errorf("zoom: auth.mode must be %s or %s", AuthModeUserOAuth, AuthModeServerToServer)
	}
	if cfg.Auth.ClientIDEnv == "" || cfg.Auth.ClientSecretEnv == "" {
		return errors.New("zoom: auth.client_id_env and auth.client_secret_env are required (environment variable names)")
	}
	if cfg.Auth.TenantIDEnv != "" {
		return errors.New("zoom: auth.tenant_id_env is a teams setting")
	}
	if cfg.Auth.Mode == AuthModeServerToServer {
		if cfg.Auth.AccountIDEnv == "" {
			return errors.New("zoom: auth.account_id_env is required for server_to_server")
		}
		users := meetingScopeList(cfg, "users")
		if len(users) == 0 || (len(users) == 1 && users[0] == zoomDefaultUser) {
			return errors.New("zoom: scope.users must list user ids or emails for server_to_server (\"me\" has no meaning for an account token)")
		}
	}
	if r := cfg.ScopeValue("redirect_uri"); r != "" {
		u, err := url.Parse(r)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("zoom: scope.redirect_uri must be an absolute http(s) URL registered on the Zoom app")
		}
	}
	return validateMeetingScope("zoom", cfg)
}

func (c *zoomConnector) mode() string {
	if c.cfg.Auth.Mode == AuthModeServerToServer {
		return AuthModeServerToServer
	}
	return AuthModeUserOAuth
}

func (c *zoomConnector) users() []string {
	if u := meetingScopeList(c.cfg, "users"); len(u) > 0 {
		return u
	}
	return []string{zoomDefaultUser}
}

func (c *zoomConnector) redirectURI() string {
	if r := c.cfg.ScopeValue("redirect_uri"); r != "" {
		return r
	}
	return zoomDefaultRedirectURI
}

// Truncated implements Truncator.
func (c *zoomConnector) Truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}

// --- OAuth ---

// clientCredentials resolves the app registration from the environment.
func (c *zoomConnector) clientCredentials() (id, secret string, err error) {
	if id, err = envValue("zoom", "client_id_env", c.cfg.Auth.ClientIDEnv); err != nil {
		return "", "", err
	}
	if secret, err = envValue("zoom", "client_secret_env", c.cfg.Auth.ClientSecretEnv); err != nil {
		return "", "", err
	}
	return id, secret, nil
}

// tokenRequest posts form to the token endpoint with the client's Basic
// credentials and decodes the reply.
func (c *zoomConnector) tokenRequest(ctx context.Context, form url.Values) (oauthTokens, error) {
	id, secret, err := c.clientCredentials()
	if err != nil {
		return oauthTokens{}, err
	}
	h := http.Header{}
	h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(id+":"+secret)))
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(ctx, http.MethodPost, zoomOAuthBase+"/token", h, []byte(form.Encode()))
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) {
			return oauthTokens{}, fmt.Errorf("zoom: token request rejected (HTTP %d): %s", se.StatusCode, firstLine(se.Body))
		}
		return oauthTokens{}, fmt.Errorf("zoom: token request: %w", err)
	}
	m, err := decodeJSONObject("zoom", resp.Body)
	if err != nil {
		return oauthTokens{}, err
	}
	tok := oauthTokens{
		AccessToken:  jsonString(m, "access_token"),
		RefreshToken: jsonString(m, "refresh_token"),
		Scope:        jsonString(m, "scope"),
	}
	if tok.AccessToken == "" {
		return oauthTokens{}, errors.New("zoom: token reply carried no access_token")
	}
	ttl := time.Hour
	if v, ok := m["expires_in"].(float64); ok && v > 0 {
		ttl = time.Duration(v) * time.Second
	}
	tok.ExpiresAt = c.now().Add(ttl)
	return tok, nil
}

// accessToken returns a usable bearer token: the cached S2S token, or the
// stored user token refreshed when needed (Zoom rotates refresh tokens, so
// the new one is written back at once).
func (c *zoomConnector) accessToken(ctx context.Context) (oauthTokens, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.mode() == AuthModeServerToServer {
		if c.s2s.valid(now) {
			return c.s2s, nil
		}
		account, err := envValue("zoom", "account_id_env", c.cfg.Auth.AccountIDEnv)
		if err != nil {
			return oauthTokens{}, err
		}
		tok, err := c.tokenRequest(ctx, url.Values{"grant_type": {"account_credentials"}, "account_id": {account}})
		if err != nil {
			return oauthTokens{}, err
		}
		c.s2s = tok
		return tok, nil
	}
	tok, err := c.store.load()
	if err != nil {
		if os.IsNotExist(err) {
			return oauthTokens{}, fmt.Errorf("zoom: %w — use Connect in the Knowledge → Integrations → Zoom card", ErrMeetingNotConnected)
		}
		return oauthTokens{}, fmt.Errorf("zoom: %w", err)
	}
	if tok.valid(now) {
		return tok, nil
	}
	if tok.RefreshToken == "" {
		return oauthTokens{}, fmt.Errorf("zoom: %w — the stored sign-in has no refresh token; use Connect in the Knowledge → Integrations → Zoom card", ErrMeetingNotConnected)
	}
	fresh, err := c.tokenRequest(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}})
	if err != nil {
		return oauthTokens{}, fmt.Errorf("zoom: %w — refreshing the sign-in failed, use Connect in the Knowledge → Integrations → Zoom card (%v)", ErrMeetingNotConnected, err)
	}
	fresh.Account = tok.Account
	if fresh.RefreshToken == "" {
		fresh.RefreshToken = tok.RefreshToken
	}
	if err := c.store.save(fresh); err != nil {
		return oauthTokens{}, fmt.Errorf("zoom: saving refreshed token: %w", err)
	}
	return fresh, nil
}

// zoomAuthError rewrites a 401 into the "not connected" hint.
func zoomAuthError(err error) error {
	if err == nil || errors.Is(err, ErrMeetingNotConnected) {
		return err
	}
	if isAuthStatus(err) {
		return meetingNotConnected("zoom", zoomLabel, err)
	}
	return err
}

// api performs an authenticated GET against the Zoom REST API.
func (c *zoomConnector) api(ctx context.Context, token, path string, query url.Values) ([]byte, error) {
	u := zoomAPIBase + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Get(ctx, u, h)
	if err != nil {
		return nil, zoomAuthError(err)
	}
	return resp.Body, nil
}

// zoomTrustedURL accepts download/play links on zoom.us (or the configured
// API host, for tests) so the bearer token is never sent elsewhere.
func zoomTrustedURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("zoom: invalid download URL")
	}
	host := strings.ToLower(u.Hostname())
	if host == "zoom.us" || strings.HasSuffix(host, ".zoom.us") || strings.HasSuffix(host, ".zoomgov.com") {
		return nil
	}
	if api, err := url.Parse(zoomAPIBase); err == nil && strings.EqualFold(api.Host, u.Host) {
		return nil
	}
	return fmt.Errorf("zoom: download URL host %q is not a Zoom host", u.Host)
}

// download reads a recording file (transcript) with the bearer token.
func (c *zoomConnector) download(ctx context.Context, token, raw string) ([]byte, error) {
	if err := zoomTrustedURL(raw); err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Get(ctx, raw, h)
	if err != nil {
		return nil, zoomAuthError(err)
	}
	return resp.Body, nil
}

// zoomPathID encodes a meeting UUID for a path segment: UUIDs that start
// with "/" or contain "//" must be double-encoded (Zoom API rule).
func zoomPathID(uuid string) string {
	if strings.HasPrefix(uuid, "/") || strings.Contains(uuid, "//") {
		return url.PathEscape(url.PathEscape(uuid))
	}
	return url.PathEscape(uuid)
}

// --- listing ---

// parseZoomMeeting maps one meetings[] object.
func parseZoomMeeting(m map[string]any) zoomMeeting {
	zm := zoomMeeting{UUID: jsonString(m, "uuid")}
	zm.ID = zm.UUID
	zm.MeetingID = jsonString(m, "id")
	zm.Topic = jsonString(m, "topic")
	zm.StartAt = jsonTime(m, "start_time")
	zm.URL = jsonString(m, "share_url")
	zm.Host = jsonString(m, "host_email")
	if d, ok := m["duration"].(float64); ok && d > 0 {
		zm.DurationMS = int64(d) * int64(time.Minute/time.Millisecond)
	}
	for _, f := range jsonList(m, "recording_files") {
		zf := zoomFile{
			ID:            jsonString(f, "id"),
			FileType:      strings.ToUpper(jsonString(f, "file_type")),
			RecordingType: jsonString(f, "recording_type"),
			DownloadURL:   jsonString(f, "download_url"),
			PlayURL:       jsonString(f, "play_url"),
			Status:        jsonString(f, "status"),
			Start:         jsonTime(f, "recording_start"),
			End:           jsonTime(f, "recording_end"),
		}
		zm.Files = append(zm.Files, zf)
		if zf.FileType == zoomFileAudio || zf.FileType == zoomFileVideo {
			zm.HasMedia = true
			if !zf.Start.IsZero() && !zf.End.IsZero() && zf.End.After(zf.Start) {
				if ms := zf.End.Sub(zf.Start).Milliseconds(); ms > zm.DurationMS || zm.DurationMS == 0 {
					zm.DurationMS = ms
				}
			}
		}
	}
	return zm
}

// mediaFile picks the audio-only file, else the first video file.
func (zm zoomMeeting) mediaFile() (zoomFile, bool) {
	var video zoomFile
	for _, f := range zm.Files {
		if f.Status != "" && f.Status != zoomFileCompleted {
			continue
		}
		switch f.FileType {
		case zoomFileAudio:
			return f, true
		case zoomFileVideo:
			if video.DownloadURL == "" {
				video = f
			}
		}
	}
	return video, video.DownloadURL != ""
}

// transcriptFile returns the TRANSCRIPT (VTT) file, if the recording has one.
func (zm zoomMeeting) transcriptFile() (zoomFile, bool) {
	for _, f := range zm.Files {
		if f.FileType == zoomFileTranscript && (f.Status == "" || f.Status == zoomFileCompleted) {
			return f, true
		}
	}
	return zoomFile{}, false
}

// listRecordings pages GET /users/{user}/recordings for every configured
// user over month-sized from/to windows between since and now, newest first,
// de-duplicated by meeting UUID and capped at max.
func (c *zoomConnector) listRecordings(ctx context.Context, token string, since time.Time, limit int) ([]zoomMeeting, error) {
	now := c.now().UTC()
	seen := map[string]bool{}
	var out []zoomMeeting
	truncated := false
	for _, user := range c.users() {
		for end := now; end.After(since) && !truncated; end = end.Add(-zoomListWindow) {
			start := end.Add(-zoomListWindow)
			if start.Before(since) {
				start = since
			}
			next := ""
			for {
				q := url.Values{
					"from":      {start.UTC().Format("2006-01-02")},
					"to":        {end.UTC().Format("2006-01-02")},
					"page_size": {strconv.Itoa(zoomListPageSize)},
				}
				if next != "" {
					q.Set("next_page_token", next)
				}
				body, err := c.api(ctx, token, "/users/"+url.PathEscape(user)+"/recordings", q)
				if err != nil {
					return nil, err
				}
				m, err := decodeJSONObject("zoom", body)
				if err != nil {
					return nil, err
				}
				for _, item := range jsonList(m, "meetings") {
					zm := parseZoomMeeting(item)
					if zm.UUID == "" || seen[zm.UUID] {
						continue
					}
					if !zm.StartAt.IsZero() && zm.StartAt.Before(since) {
						continue
					}
					seen[zm.UUID] = true
					out = append(out, zm)
					if len(out) >= limit {
						truncated = true
						break
					}
				}
				next = jsonString(m, "next_page_token")
				if next == "" || truncated {
					break
				}
			}
		}
	}
	c.mu.Lock()
	c.truncated = truncated
	c.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartAt.After(out[j].StartAt) })
	return out, nil
}

// getMeeting reads one recorded meeting by UUID (GET /meetings/{id}/recordings).
func (c *zoomConnector) getMeeting(ctx context.Context, token, uuid string, query url.Values) (zoomMeeting, error) {
	body, err := c.api(ctx, token, "/meetings/"+zoomPathID(uuid)+"/recordings", query)
	if err != nil {
		return zoomMeeting{}, err
	}
	m, err := decodeJSONObject("zoom", body)
	if err != nil {
		return zoomMeeting{}, err
	}
	zm := parseZoomMeeting(m)
	if zm.UUID == "" {
		zm.UUID, zm.ID = uuid, uuid
	}
	return zm, nil
}

// --- content ---

// fetchContent reads the transcript (VTT), the AI meeting summary and the
// participant list for one recording. Summary and participants are optional
// extras: a missing scope or an unsupported plan only drops that section.
func (c *zoomConnector) fetchContent(ctx context.Context, token string, zm zoomMeeting) (MeetingContent, error) {
	var out MeetingContent
	if f, ok := zm.transcriptFile(); ok {
		data, err := c.download(ctx, token, f.DownloadURL)
		if err != nil {
			return MeetingContent{}, fmt.Errorf("transcript: %w", err)
		}
		out.Transcript = renderVTT(string(data))
	}
	if meetingScopeBool(c.cfg, "include_summary", true) {
		if body, err := c.api(ctx, token, "/meetings/"+zoomPathID(zm.UUID)+"/meeting_summary", nil); err == nil {
			out.Summary = renderZoomSummary(body)
		} else if errors.Is(err, ErrMeetingNotConnected) {
			return MeetingContent{}, err
		} else {
			c.logger.Debug("zoom meeting summary unavailable", "meeting", zm.MeetingID, "error", err)
		}
	}
	if meetingScopeBool(c.cfg, "include_participants", true) {
		if body, err := c.api(ctx, token, "/past_meetings/"+zoomPathID(zm.UUID)+"/participants", url.Values{"page_size": {strconv.Itoa(zoomListPageSize)}}); err == nil {
			out.Participants = parseZoomParticipants(body)
		} else if errors.Is(err, ErrMeetingNotConnected) {
			return MeetingContent{}, err
		} else {
			c.logger.Debug("zoom participants unavailable", "meeting", zm.MeetingID, "error", err)
		}
	}
	out.Pending = strings.TrimSpace(out.Summary) == "" && strings.TrimSpace(out.Transcript) == ""
	return out, nil
}

// renderZoomSummary turns a meeting_summary reply into markdown: overview,
// one heading per detail section, and the next steps.
func renderZoomSummary(body []byte) string {
	m, err := decodeJSONObject("zoom", body)
	if err != nil {
		return ""
	}
	// Prefer the host-edited summary when there is one.
	if edited, ok := m["edited_summary"].(map[string]any); ok && len(edited) > 0 {
		for k, v := range edited {
			if _, has := m[k]; !has || k == "summary_details" || k == "next_steps" {
				m[k] = v
			}
		}
	}
	var b strings.Builder
	if o := jsonString(m, "summary_overview"); o != "" {
		b.WriteString(o)
		b.WriteString("\n\n")
	}
	for _, d := range jsonList(m, "summary_details") {
		label, text := jsonString(d, "label"), jsonString(d, "summary")
		if text == "" {
			continue
		}
		if label != "" {
			fmt.Fprintf(&b, "### %s\n\n", label)
		}
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	if steps, ok := m["next_steps"].([]any); ok && len(steps) > 0 {
		b.WriteString("### Next steps\n\n")
		for _, s := range steps {
			if str, ok := s.(string); ok && strings.TrimSpace(str) != "" {
				b.WriteString("- " + strings.TrimSpace(str) + "\n")
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// parseZoomParticipants reads participant names from a past_meetings reply.
func parseZoomParticipants(body []byte) []string {
	m, err := decodeJSONObject("zoom", body)
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range jsonList(m, "participants") {
		if n := jsonString(p, "name", "user_email"); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// mediaLine is the optional fact line linking the recording video page.
func (c *zoomConnector) mediaLine(zm zoomMeeting) string {
	if !meetingScopeBool(c.cfg, "include_video_link", false) {
		return ""
	}
	if f, ok := zm.mediaFile(); ok && f.PlayURL != "" {
		return fmt.Sprintf("- **Recording (%s):** %s\n", zoomLabel, f.PlayURL)
	}
	return ""
}

// --- Sync ---

// Sync lists recordings since the cursor (minus an overlap window) and
// emits one page per recorded meeting. The listing is incremental, so
// unlisted pages are never tombstoned.
func (c *zoomConnector) Sync(ctx context.Context, cur Cursor, emit func(Page) error) (Cursor, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return cur, err
	}
	since := meetingSince(c.cfg, cur, c.now())
	reportProgress(ctx, func(p *Progress) { p.Phase = ProgressListing })
	meetings, err := c.listRecordings(ctx, tok.AccessToken, since, meetingScopeInt(c.cfg, "max_recordings", meetingDefaultMaxRecordings))
	if err != nil {
		return cur, err
	}
	listed := make(map[string]bool, len(meetings))
	for _, zm := range meetings {
		listed[zm.UUID] = true
	}
	for _, id := range meetingPendingIDs(c.vaultDir, c.cfg, listed, zoomPendingRefetchCap) {
		zm, err := c.getMeeting(ctx, tok.AccessToken, id, nil)
		if err != nil {
			c.logger.Debug("zoom pending recording not re-read", "id", id, "error", err)
			continue
		}
		meetings = append(meetings, zm)
	}
	total := len(meetings)
	reportProgress(ctx, func(p *Progress) { p.Phase, p.ItemsTotal = ProgressFetching, total })
	newest := parsePlaudCursor(cur)
	var skipped []string
	for _, zm := range meetings {
		if err := ctx.Err(); err != nil {
			return cur, err
		}
		title := progressTitle(meetingTitle(TypeZoom, zoomLabel, zm.MeetingRecording))
		reportProgress(ctx, func(p *Progress) { p.Phase, p.Current = ProgressFetching, title })
		content, err := c.fetchContent(ctx, tok.AccessToken, zm)
		if err != nil {
			if errors.Is(err, ErrMeetingNotConnected) {
				return cur, err
			}
			skipped = append(skipped, fmt.Sprintf("%s: %v", zm.UUID, err))
			reportProgress(ctx, func(p *Progress) { p.ItemsDone++ })
			continue
		}
		zm.Tags = meetingScopeList(c.cfg, "tags")
		if err := emit(meetingPage(c.cfg, TypeZoom, zoomLabel, zm.MeetingRecording, content, c.mediaLine(zm))); err != nil {
			return cur, err
		}
		if zm.StartAt.After(newest) {
			newest = zm.StartAt
		}
	}
	c.refreshAuth(ctx)
	next := cur
	if !newest.IsZero() {
		next = Cursor(newest.UTC().Format(time.RFC3339))
	}
	if len(skipped) > 0 {
		return next, &PartialError{Err: fmt.Errorf("zoom: %d recording(s) skipped: %s", len(skipped), strings.Join(skipped, "; "))}
	}
	return next, nil
}

// meetingPendingIDs returns the source ids of this connector's facts that
// still have no text and were not in the current listing, newest first,
// capped. Shared by the zoom and teams connectors.
func meetingPendingIDs(vaultDir string, cfg ConnectorConfig, listed map[string]bool, limit int) []string {
	if vaultDir == "" {
		return nil
	}
	entries, err := os.ReadDir(vaultDir)
	if err != nil {
		return nil
	}
	prefix := slugPrefix(cfg.Type, cfg.Name)
	type pending struct {
		id string
		at time.Time
	}
	var out []pending
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(vaultDir, name))
		if err != nil {
			continue
		}
		fm, _ := splitFactFrontMatter(string(data))
		if fm["connector"] != cfg.Name || fm["source"] != cfg.Type || fm["status"] == StatusDeprecated {
			continue
		}
		id := fm["source_id"]
		if id == "" || listed[id] || !meetingFactNeedsRefetch(fm) {
			continue
		}
		at, _ := time.Parse(time.RFC3339, fm["attr_recorded_at"])
		out = append(out, pending{id: id, at: at})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at.After(out[j].at) })
	if len(out) > limit {
		out = out[:limit]
	}
	ids := make([]string, 0, len(out))
	for _, p := range out {
		ids = append(ids, p.id)
	}
	return ids
}

// --- MeetingReader / MeetingMediaReader ---

// ListMeetings lists recordings within the since_days window, newest first.
func (c *zoomConnector) ListMeetings(ctx context.Context) ([]MeetingRecording, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	since := c.now().Add(-time.Duration(meetingScopeInt(c.cfg, "since_days", meetingDefaultSinceDays)) * 24 * time.Hour)
	meetings, err := c.listRecordings(ctx, tok.AccessToken, since, meetingScopeInt(c.cfg, "max_recordings", meetingDefaultMaxRecordings))
	if err != nil {
		return nil, err
	}
	tags := meetingScopeList(c.cfg, "tags")
	out := make([]MeetingRecording, 0, len(meetings))
	for _, zm := range meetings {
		rec := zm.MeetingRecording
		rec.Topic = meetingTitle(TypeZoom, zoomLabel, rec)
		rec.Tags = tags
		out = append(out, rec)
	}
	sortMeetingsNewestFirst(out)
	return out, nil
}

// ReadMeeting reads the transcript and summary of one recording.
func (c *zoomConnector) ReadMeeting(ctx context.Context, id string) (MeetingContent, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return MeetingContent{}, err
	}
	zm, err := c.getMeeting(ctx, tok.AccessToken, id, nil)
	if err != nil {
		return MeetingContent{}, err
	}
	return c.fetchContent(ctx, tok.AccessToken, zm)
}

// MediaLink mints a playable link for one recording: the audio-only file
// (else the video) with a short-lived token appended as the access_token
// query parameter, since an <audio> element cannot send a bearer header.
// Zoom's per-recording download token is used when the API grants one;
// otherwise the OAuth access token (one hour) is. Nothing is persisted.
func (c *zoomConnector) MediaLink(ctx context.Context, id string) (MediaLink, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return MediaLink{}, err
	}
	ttl := strconv.Itoa(int(zoomMediaTokenTTL / time.Second))
	body, err := c.api(ctx, tok.AccessToken, "/meetings/"+zoomPathID(id)+"/recordings", url.Values{"include_fields": {"download_access_token"}, "ttl": {ttl}})
	if err != nil {
		return MediaLink{}, err
	}
	m, err := decodeJSONObject("zoom", body)
	if err != nil {
		return MediaLink{}, err
	}
	zm := parseZoomMeeting(m)
	f, ok := zm.mediaFile()
	if !ok {
		return MediaLink{}, fmt.Errorf("zoom: %w", ErrMeetingNoMedia)
	}
	if err := zoomTrustedURL(f.DownloadURL); err != nil {
		return MediaLink{}, err
	}
	link := MediaLink{URL: f.DownloadURL, ExpiresAt: tok.ExpiresAt}
	bearer := tok.AccessToken
	if dl := jsonString(m, "download_access_token"); dl != "" {
		bearer = dl
		link.ExpiresAt = c.now().Add(zoomMediaTokenTTL)
	}
	sep := "?"
	if strings.Contains(link.URL, "?") {
		sep = "&"
	}
	link.URL += sep + "access_token=" + url.QueryEscape(bearer)
	if link.ExpiresAt.IsZero() {
		link.ExpiresAt = c.now().Add(meetingMediaLinkTTLFallback)
	}
	return link, nil
}

// --- Authenticator / AuthCompleter ---

// AuthStatus implements Authenticator. With refresh it exchanges or
// refreshes a token and reads the signed-in user.
func (c *zoomConnector) AuthStatus(ctx context.Context, refresh bool) AuthState {
	c.mu.Lock()
	st := c.auth
	pending := c.pendingState != "" && c.now().Before(c.pendingAt.Add(zoomLoginWindow))
	c.mu.Unlock()
	if pending {
		st.Pending = true
		return st
	}
	st.Pending = false
	if !refresh {
		if st.CheckedAt.IsZero() && c.mode() == AuthModeUserOAuth {
			// First look: report whether a stored sign-in exists without
			// touching Zoom.
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

// refreshAuth probes the signed-in user and caches the outcome.
func (c *zoomConnector) refreshAuth(ctx context.Context) {
	st := AuthState{CheckedAt: c.now().UTC()}
	tok, err := c.accessToken(ctx)
	if err != nil {
		st.Message = firstLine(err.Error())
		c.setAuth(st)
		return
	}
	user := c.users()[0]
	body, err := c.api(ctx, tok.AccessToken, "/users/"+url.PathEscape(user), nil)
	if err != nil {
		st.Message = firstLine(err.Error())
		c.setAuth(st)
		return
	}
	st.Connected = true
	if m, err := decodeJSONObject("zoom", body); err == nil {
		st.Account = jsonString(m, "email", "display_name", "id")
	}
	if c.mode() == AuthModeUserOAuth && st.Account != "" && tok.Account != st.Account {
		tok.Account = st.Account
		_ = c.store.save(tok)
	}
	c.setAuth(st)
}

func (c *zoomConnector) setAuth(st AuthState) {
	c.mu.Lock()
	c.auth = st
	c.mu.Unlock()
}

// Connect implements Authenticator. For user-level OAuth it returns the
// authorization URL (pending until the redirect URL is pasted); for
// Server-to-Server it fetches an account token and reports the outcome.
func (c *zoomConnector) Connect(ctx context.Context) (AuthState, error) {
	if c.mode() == AuthModeServerToServer {
		c.refreshAuth(ctx)
		return c.AuthStatus(ctx, false), nil
	}
	id, _, err := c.clientCredentials()
	if err != nil {
		return c.AuthStatus(ctx, false), err
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return c.AuthStatus(ctx, false), fmt.Errorf("zoom: generating state: %w", err)
	}
	state := hex.EncodeToString(buf)
	q := url.Values{
		"response_type": {"code"},
		"client_id":     {id},
		"redirect_uri":  {c.redirectURI()},
		"state":         {state},
	}
	c.mu.Lock()
	c.pendingState, c.pendingAt = state, c.now()
	c.auth = AuthState{Pending: true, AuthURL: zoomOAuthBase + "/authorize?" + q.Encode(), CheckedAt: c.now().UTC()}
	st := c.auth
	c.mu.Unlock()
	return st, nil
}

// CompleteAuth implements AuthCompleter: the operator pastes the redirect
// URL the browser landed on; its code is exchanged for tokens and stored.
func (c *zoomConnector) CompleteAuth(ctx context.Context, comp AuthCompletion) (AuthState, error) {
	if strings.TrimSpace(comp.Tokens) != "" {
		return c.AuthStatus(ctx, false), errors.New("zoom: paste the redirect URL the browser landed on; token JSON is not accepted")
	}
	u, err := url.Parse(strings.TrimSpace(comp.RedirectURL))
	if err != nil || u.RawQuery == "" {
		return c.AuthStatus(ctx, false), errors.New("zoom: paste the full URL the browser landed on (it contains ?code=...&state=...)")
	}
	code := u.Query().Get("code")
	if code == "" {
		return c.AuthStatus(ctx, false), errors.New("zoom: the pasted URL has no code parameter")
	}
	c.mu.Lock()
	state, at := c.pendingState, c.pendingAt
	c.mu.Unlock()
	if state == "" || c.now().After(at.Add(zoomLoginWindow)) {
		return c.AuthStatus(ctx, false), errors.New("zoom: no sign-in is pending; press Connect first, then paste the redirect URL within the login window")
	}
	if got := u.Query().Get("state"); got != "" && got != state {
		return c.AuthStatus(ctx, false), errors.New("zoom: the pasted URL belongs to a different sign-in attempt (state mismatch); press Connect again")
	}
	tok, err := c.tokenRequest(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {c.redirectURI()}})
	if err != nil {
		return c.AuthStatus(ctx, false), err
	}
	if err := c.store.save(tok); err != nil {
		return c.AuthStatus(ctx, false), fmt.Errorf("zoom: storing tokens: %w", err)
	}
	c.mu.Lock()
	c.pendingState = ""
	c.mu.Unlock()
	c.refreshAuth(ctx)
	return c.AuthStatus(ctx, false), nil
}

// Disconnect implements Authenticator: revokes the user token upstream (best
// effort) and clears the token store or the cached account token.
func (c *zoomConnector) Disconnect(ctx context.Context) (AuthState, error) {
	c.mu.Lock()
	c.pendingState = ""
	c.s2s = oauthTokens{}
	c.mu.Unlock()
	if c.mode() == AuthModeUserOAuth {
		if tok, err := c.store.load(); err == nil && tok.AccessToken != "" {
			if id, secret, err := c.clientCredentials(); err == nil {
				h := http.Header{}
				h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(id+":"+secret)))
				h.Set("Content-Type", "application/x-www-form-urlencoded")
				if _, err := c.http.Do(ctx, http.MethodPost, zoomOAuthBase+"/revoke", h, []byte(url.Values{"token": {tok.AccessToken}}.Encode())); err != nil {
					c.logger.Debug("zoom token revoke failed", "error", err)
				}
			}
		}
		if err := c.store.clear(); err != nil {
			return c.AuthStatus(ctx, false), fmt.Errorf("zoom: %w", err)
		}
	}
	st := AuthState{CheckedAt: c.now().UTC(), Message: "signed out"}
	c.setAuth(st)
	return st, nil
}
