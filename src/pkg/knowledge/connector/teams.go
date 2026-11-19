package connector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TypeTeams is the connector type for Microsoft Teams meeting recordings
// and transcripts read through Microsoft Graph.
const TypeTeams = "teams"

// Microsoft endpoints. Variables so tests point them at a fake server.
var (
	teamsGraphBase = "https://graph.microsoft.com/v1.0"
	teamsLoginBase = "https://login.microsoftonline.com"
)

const (
	// teamsLabel is the human name used in headings and messages.
	teamsLabel = "Teams"
	// teamsGraphScope is the client-credentials scope for Graph.
	teamsGraphScope = "https://graph.microsoft.com/.default"
	// teamsListPageSize is the $top requested on artifact listings.
	teamsListPageSize = 50
	// teamsPermissions names the application permissions the Entra app needs
	// (the admin-consent message quotes it).
	teamsPermissions = "OnlineMeetingRecording.Read.All and OnlineMeetingTranscript.Read.All"
	// teamsVTTFormat is the speaker-attributed transcript format.
	teamsVTTFormat = "text/vtt"
	// teamsPlainTranscriptFormat is the unattributed fallback a tenant may
	// force (SpeakerAttributionNotAllowed).
	teamsPlainTranscriptFormat = "application/vnd.microsoft.graph.transcript+text"
	// teamsTranscriptIDPrefix marks a recording-less item that only has a
	// transcript, so its id never collides with a callRecording id.
	teamsTranscriptIDPrefix = "transcript:"
)

// ErrTeamsConsentRequired is returned when Graph refuses the app: the tenant
// administrator has not granted the application permissions (or the
// application access policy) the connector needs.
var ErrTeamsConsentRequired = errors.New("requires Entra admin consent for " + teamsPermissions)

// teamsGUIDRE matches an Entra object id.
var teamsGUIDRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// teamsArtifact is one callRecording or callTranscript.
type teamsArtifact struct {
	ID         string
	MeetingID  string
	CreatedAt  time.Time
	EndAt      time.Time
	ContentURL string
	Organizer  string
}

// teamsMeeting is one recorded meeting instance: its recordings and
// transcripts grouped by onlineMeeting id.
type teamsMeeting struct {
	MeetingRecording
	UserID      string
	Recordings  []teamsArtifact
	Transcripts []teamsArtifact
	JoinURL     string
}

// teamsConnector syncs Teams meeting recordings and transcripts for the
// configured organizers through Microsoft Graph application permissions
// (client credentials). Auth: tenant_id_env, client_id_env,
// client_secret_env. Scope keys: users (comma separated object ids or UPNs
// of meeting organizers) or group (group object id), since_days,
// max_recordings, include_video_link, tags.
type teamsConnector struct {
	cfg    ConnectorConfig
	logger *slog.Logger
	http   *HTTPClient
	now    func() time.Time
	// vaultDir is where this connector's facts live.
	vaultDir string

	mu sync.Mutex
	// token caches the client-credentials token (never persisted).
	token     oauthTokens
	auth      AuthState
	truncated bool
}

var (
	_ Authenticator      = (*teamsConnector)(nil)
	_ MeetingReader      = (*teamsConnector)(nil)
	_ MeetingMediaReader = (*teamsConnector)(nil)
	_ Truncator          = (*teamsConnector)(nil)
)

func newTeamsConnector(cfg ConnectorConfig, deps Deps) (Connector, error) {
	c := &teamsConnector{cfg: cfg, logger: deps.logger(), http: deps.httpClient(), now: time.Now}
	if deps.KnowledgeDir != "" && cfg.Layer != "" {
		c.vaultDir = filepath.Join(deps.KnowledgeDir, "connectors", cfg.Layer)
	}
	return c, nil
}

func (c *teamsConnector) Type() string { return TypeTeams }

// Validate checks the auth block and scope. Credentials are referenced by
// environment variable name only.
func (c *teamsConnector) Validate(cfg ConnectorConfig) error {
	if cfg.Auth.Configured() {
		return errors.New("teams: auth.env/auth.file are not used; set auth.tenant_id_env, auth.client_id_env and auth.client_secret_env")
	}
	if cfg.Auth.Mode != "" && cfg.Auth.Mode != AuthModeServerToServer {
		return fmt.Errorf("teams: auth.mode must be empty or %s (Graph application permissions)", AuthModeServerToServer)
	}
	if cfg.Auth.TenantIDEnv == "" || cfg.Auth.ClientIDEnv == "" || cfg.Auth.ClientSecretEnv == "" {
		return errors.New("teams: auth.tenant_id_env, auth.client_id_env and auth.client_secret_env are required (environment variable names)")
	}
	if cfg.Auth.AccountIDEnv != "" {
		return errors.New("teams: auth.account_id_env is a zoom setting")
	}
	if len(meetingScopeList(cfg, "users")) == 0 && cfg.ScopeValue("group") == "" {
		return errors.New("teams: scope.users (organizer object ids or UPNs) or scope.group (group object id) is required")
	}
	if g := cfg.ScopeValue("group"); g != "" && !teamsGUIDRE.MatchString(g) {
		return errors.New("teams: scope.group must be an Entra group object id")
	}
	return validateMeetingScope("teams", cfg)
}

// Truncated implements Truncator.
func (c *teamsConnector) Truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}

// --- auth ---

// accessToken returns the cached client-credentials token or fetches one.
func (c *teamsConnector) accessToken(ctx context.Context) (oauthTokens, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token.valid(c.now()) {
		return c.token, nil
	}
	tenant, err := envValue("teams", "tenant_id_env", c.cfg.Auth.TenantIDEnv)
	if err != nil {
		return oauthTokens{}, err
	}
	id, err := envValue("teams", "client_id_env", c.cfg.Auth.ClientIDEnv)
	if err != nil {
		return oauthTokens{}, err
	}
	secret, err := envValue("teams", "client_secret_env", c.cfg.Auth.ClientSecretEnv)
	if err != nil {
		return oauthTokens{}, err
	}
	form := url.Values{
		"client_id":     {id},
		"client_secret": {secret},
		"scope":         {teamsGraphScope},
		"grant_type":    {"client_credentials"},
	}
	h := http.Header{}
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(ctx, http.MethodPost, teamsLoginBase+"/"+url.PathEscape(tenant)+"/oauth2/v2.0/token", h, []byte(form.Encode()))
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) {
			return oauthTokens{}, fmt.Errorf("teams: %w — token request rejected (HTTP %d): %s", ErrMeetingNotConnected, se.StatusCode, teamsErrorText(se.Body))
		}
		return oauthTokens{}, fmt.Errorf("teams: token request: %w", err)
	}
	m, err := decodeJSONObject("teams", resp.Body)
	if err != nil {
		return oauthTokens{}, err
	}
	tok := oauthTokens{AccessToken: jsonString(m, "access_token")}
	if tok.AccessToken == "" {
		return oauthTokens{}, errors.New("teams: token reply carried no access_token")
	}
	ttl := time.Hour
	if v, ok := m["expires_in"].(float64); ok && v > 0 {
		ttl = time.Duration(v) * time.Second
	}
	tok.ExpiresAt = c.now().Add(ttl)
	c.token = tok
	return tok, nil
}

// teamsErrorText pulls the readable part out of an OAuth or Graph error body.
func teamsErrorText(body string) string {
	if m, err := decodeJSONObject("teams", []byte(body)); err == nil {
		if e, ok := m["error"].(map[string]any); ok {
			code, msg := jsonString(e, "code"), jsonString(e, "message")
			if inner, ok := e["innerError"].(map[string]any); ok {
				if ic := jsonString(inner, "code"); ic != "" {
					code = code + "/" + ic
				}
			}
			return strings.TrimSpace(code + ": " + firstLine(msg))
		}
		if code := jsonString(m, "error"); code != "" {
			return strings.TrimSpace(code + ": " + firstLine(jsonString(m, "error_description")))
		}
	}
	return firstLine(body)
}

// teamsAuthError classifies Graph refusals: 401 is "not connected" (bad
// client or secret), 403 is missing admin consent or application access
// policy, which the operator can only fix in the tenant.
func teamsAuthError(err error) error {
	if err == nil || errors.Is(err, ErrMeetingNotConnected) || errors.Is(err, ErrTeamsConsentRequired) {
		return err
	}
	var se *StatusError
	if !errors.As(err, &se) {
		return err
	}
	switch se.StatusCode {
	case http.StatusUnauthorized:
		return meetingNotConnected("teams", teamsLabel, err)
	case http.StatusForbidden:
		text := teamsErrorText(se.Body)
		if strings.Contains(text, "No application access policy") {
			return fmt.Errorf("teams: %w, and an application access policy granting this app the organizer's online meetings (New-CsApplicationAccessPolicy / Grant-CsApplicationAccessPolicy) (%s)", ErrTeamsConsentRequired, text)
		}
		if strings.Contains(text, "GraphAccessToTranscriptsDisabled") {
			return fmt.Errorf("teams: %w; the tenant has also disabled Graph API access to transcripts (%s)", ErrTeamsConsentRequired, text)
		}
		return fmt.Errorf("teams: %w (%s)", ErrTeamsConsentRequired, text)
	}
	return err
}

// sameTeamsGraphHost keeps the bearer token from being sent to a host other
// than the Graph API host (nextLink and content URLs come from the reply).
func sameTeamsGraphHost(link string) error {
	u, err := url.Parse(link)
	if err != nil {
		return fmt.Errorf("teams: invalid link: %w", err)
	}
	base, err := url.Parse(teamsGraphBase)
	if err != nil {
		return err
	}
	if !strings.EqualFold(u.Host, base.Host) {
		return fmt.Errorf("teams: link host %q is not the Graph host", u.Host)
	}
	return nil
}

// get performs an authenticated GET; extra headers are optional.
func (c *teamsConnector) get(ctx context.Context, token, link string, extra http.Header) ([]byte, error) {
	if err := sameTeamsGraphHost(link); err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	for k, vs := range extra {
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	resp, err := c.http.Get(ctx, link, h)
	if err != nil {
		return nil, teamsAuthError(err)
	}
	return resp.Body, nil
}

// getJSON is get plus object decoding.
func (c *teamsConnector) getJSON(ctx context.Context, token, link string) (map[string]any, error) {
	body, err := c.get(ctx, token, link, nil)
	if err != nil {
		return nil, err
	}
	return decodeJSONObject("teams", body)
}

// --- listing ---

// organizers resolves scope.users / scope.group to Entra object ids. A UPN
// is resolved through GET /users/{upn} (User.Read.All); when that is not
// granted the UPN is used as given, which Graph accepts in the path but not
// as meetingOrganizerUserId, so object ids are the recommended form.
func (c *teamsConnector) organizers(ctx context.Context, token string) ([]string, error) {
	var ids []string
	for _, u := range meetingScopeList(c.cfg, "users") {
		if teamsGUIDRE.MatchString(u) {
			ids = append(ids, strings.ToLower(u))
			continue
		}
		m, err := c.getJSON(ctx, token, teamsGraphBase+"/users/"+url.PathEscape(u)+"?$select=id")
		if err != nil {
			if errors.Is(err, ErrMeetingNotConnected) {
				return nil, err
			}
			c.logger.Debug("teams organizer UPN not resolved; using it as given", "user", u, "error", err)
			ids = append(ids, u)
			continue
		}
		if id := jsonString(m, "id"); id != "" {
			ids = append(ids, strings.ToLower(id))
		}
	}
	if g := c.cfg.ScopeValue("group"); g != "" {
		link := teamsGraphBase + "/groups/" + url.PathEscape(g) + "/members?$select=id&$top=" + strconv.Itoa(teamsListPageSize)
		for link != "" {
			m, err := c.getJSON(ctx, token, link)
			if err != nil {
				return nil, fmt.Errorf("group members: %w", err)
			}
			for _, member := range jsonList(m, "value") {
				if id := jsonString(member, "id"); id != "" {
					ids = append(ids, strings.ToLower(id))
				}
			}
			link = jsonString(m, "@odata.nextLink")
		}
	}
	seen := map[string]bool{}
	out := ids[:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

// artifactsURL builds the getAllRecordings / getAllTranscripts function URL.
func teamsArtifactsURL(userID, fn string, since, until time.Time) string {
	return fmt.Sprintf("%s/users/%s/onlineMeetings/%s(meetingOrganizerUserId='%s',startDateTime=%s,endDateTime=%s)?$top=%d",
		teamsGraphBase, url.PathEscape(userID), fn, url.PathEscape(userID),
		since.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339), teamsListPageSize)
}

// listArtifacts follows one artifact listing through its nextLink pages.
func (c *teamsConnector) listArtifacts(ctx context.Context, token, link, contentKey string) ([]teamsArtifact, error) {
	var out []teamsArtifact
	for link != "" {
		m, err := c.getJSON(ctx, token, link)
		if err != nil {
			return nil, err
		}
		for _, v := range jsonList(m, "value") {
			a := teamsArtifact{
				ID:         jsonString(v, "id"),
				MeetingID:  jsonString(v, "meetingId"),
				CreatedAt:  jsonTime(v, "createdDateTime"),
				EndAt:      jsonTime(v, "endDateTime"),
				ContentURL: jsonString(v, contentKey),
			}
			if org, ok := v["meetingOrganizer"].(map[string]any); ok {
				if u, ok := org["user"].(map[string]any); ok {
					a.Organizer = jsonString(u, "displayName", "id")
				}
			}
			if a.ID != "" && a.MeetingID != "" {
				out = append(out, a)
			}
		}
		link = jsonString(m, "@odata.nextLink")
	}
	return out, nil
}

// listMeetings lists recordings and transcripts created since for every
// organizer and groups them per meeting, newest first, capped at max.
func (c *teamsConnector) listMeetings(ctx context.Context, token string, since time.Time, limit int) ([]teamsMeeting, error) {
	organizers, err := c.organizers(ctx, token)
	if err != nil {
		return nil, err
	}
	until := c.now().UTC().Add(time.Minute)
	byMeeting := map[string]*teamsMeeting{}
	var order []string
	for _, user := range organizers {
		recs, err := c.listArtifacts(ctx, token, teamsArtifactsURL(user, "getAllRecordings", since, until), "recordingContentUrl")
		if err != nil {
			return nil, fmt.Errorf("recordings for %s: %w", user, err)
		}
		trs, err := c.listArtifacts(ctx, token, teamsArtifactsURL(user, "getAllTranscripts", since, until), "transcriptContentUrl")
		if err != nil {
			return nil, fmt.Errorf("transcripts for %s: %w", user, err)
		}
		add := func(a teamsArtifact, recording bool) {
			key := user + "/" + a.MeetingID
			tm, ok := byMeeting[key]
			if !ok {
				tm = &teamsMeeting{UserID: user}
				tm.MeetingID = a.MeetingID
				tm.Host = a.Organizer
				byMeeting[key] = tm
				order = append(order, key)
			}
			if recording {
				tm.Recordings = append(tm.Recordings, a)
				tm.HasMedia = true
			} else {
				tm.Transcripts = append(tm.Transcripts, a)
			}
			if tm.StartAt.IsZero() || (!a.CreatedAt.IsZero() && a.CreatedAt.Before(tm.StartAt)) {
				tm.StartAt = a.CreatedAt
			}
		}
		for _, a := range recs {
			add(a, true)
		}
		for _, a := range trs {
			add(a, false)
		}
	}
	out := make([]teamsMeeting, 0, len(order))
	for _, key := range order {
		tm := byMeeting[key]
		switch {
		case len(tm.Recordings) > 0:
			tm.ID = tm.Recordings[0].ID
		case len(tm.Transcripts) > 0:
			tm.ID = teamsTranscriptIDPrefix + tm.Transcripts[0].ID
		default:
			continue
		}
		for _, a := range tm.Recordings {
			if !a.EndAt.IsZero() && !a.CreatedAt.IsZero() && a.EndAt.After(a.CreatedAt) {
				if ms := a.EndAt.Sub(a.CreatedAt).Milliseconds(); ms > tm.DurationMS {
					tm.DurationMS = ms
				}
			}
		}
		out = append(out, *tm)
	}
	sortTeamsNewestFirst(out)
	truncated := false
	if len(out) > limit {
		out, truncated = out[:limit], true
	}
	c.mu.Lock()
	c.truncated = truncated
	c.mu.Unlock()
	return out, nil
}

func sortTeamsNewestFirst(ms []teamsMeeting) {
	for i := 1; i < len(ms); i++ {
		for j := i; j > 0 && ms[j].StartAt.After(ms[j-1].StartAt); j-- {
			ms[j], ms[j-1] = ms[j-1], ms[j]
		}
	}
}

// meetingDetails fills subject, times, participants and join link from the
// onlineMeeting (OnlineMeetings.Read.All). Best effort: without that
// permission the fact keeps the artifact's timestamps and a generic title.
func (c *teamsConnector) meetingDetails(ctx context.Context, token string, tm *teamsMeeting) {
	m, err := c.getJSON(ctx, token, teamsGraphBase+"/users/"+url.PathEscape(tm.UserID)+"/onlineMeetings/"+url.PathEscape(tm.MeetingID))
	if err != nil {
		c.logger.Debug("teams meeting details unavailable", "meeting", tm.MeetingID, "error", err)
		return
	}
	tm.Topic = jsonString(m, "subject")
	tm.JoinURL = jsonString(m, "joinWebUrl")
	tm.URL = tm.JoinURL
	if start := jsonTime(m, "startDateTime"); !start.IsZero() {
		tm.StartAt = start
		if end := jsonTime(m, "endDateTime"); end.After(start) && tm.DurationMS == 0 {
			tm.DurationMS = end.Sub(start).Milliseconds()
		}
	}
	if parts, ok := m["participants"].(map[string]any); ok {
		if org, ok := parts["organizer"].(map[string]any); ok {
			if n := teamsIdentityName(org); n != "" {
				tm.Host = n
			}
		}
		for _, a := range jsonList(parts, "attendees") {
			if n := teamsIdentityName(a); n != "" {
				tm.Participants = append(tm.Participants, n)
			}
		}
	}
}

// teamsIdentityName reads a meetingParticipantInfo: the user display name,
// else the UPN.
func teamsIdentityName(p map[string]any) string {
	if idset, ok := p["identity"].(map[string]any); ok {
		if u, ok := idset["user"].(map[string]any); ok {
			if n := jsonString(u, "displayName"); n != "" {
				return n
			}
		}
	}
	return jsonString(p, "upn")
}

// fetchContent reads the newest transcript as VTT (falling back to the
// unattributed format when the tenant forbids speaker attribution).
func (c *teamsConnector) fetchContent(ctx context.Context, token string, tm teamsMeeting) (MeetingContent, error) {
	var out MeetingContent
	if len(tm.Transcripts) > 0 {
		tr := tm.Transcripts[len(tm.Transcripts)-1]
		link := tr.ContentURL
		if link == "" {
			link = fmt.Sprintf("%s/users/%s/onlineMeetings/%s/transcripts/%s/content", teamsGraphBase, url.PathEscape(tm.UserID), url.PathEscape(tm.MeetingID), url.PathEscape(tr.ID))
		}
		h := http.Header{}
		h.Set("Accept", teamsVTTFormat)
		body, err := c.get(ctx, token, link, h)
		if err != nil && strings.Contains(err.Error(), "SpeakerAttributionNotAllowed") {
			// The tenant forbids speaker attribution: the unattributed
			// format is always allowed.
			h.Set("Accept", teamsPlainTranscriptFormat)
			body, err = c.get(ctx, token, link, h)
		}
		if err != nil {
			return MeetingContent{}, fmt.Errorf("transcript: %w", err)
		}
		out.Transcript = renderVTT(string(body))
	}
	out.Participants = tm.Participants
	out.Pending = strings.TrimSpace(out.Transcript) == ""
	return out, nil
}

// mediaLine is the optional fact line linking the Teams meeting.
func (c *teamsConnector) mediaLine(tm teamsMeeting) string {
	if !meetingScopeBool(c.cfg, "include_video_link", false) || tm.JoinURL == "" {
		return ""
	}
	return fmt.Sprintf("- **Meeting (%s):** %s\n", teamsLabel, tm.JoinURL)
}

// --- Sync ---

// Sync lists recordings and transcripts since the cursor (minus an overlap)
// and emits one page per recorded meeting. The listing is incremental, so
// unlisted pages are never tombstoned.
func (c *teamsConnector) Sync(ctx context.Context, cur Cursor, emit func(Page) error) (Cursor, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return cur, err
	}
	since := meetingSince(c.cfg, cur, c.now())
	reportProgress(ctx, func(p *Progress) { p.Phase = ProgressListing })
	meetings, err := c.listMeetings(ctx, tok.AccessToken, since, meetingScopeInt(c.cfg, "max_recordings", meetingDefaultMaxRecordings))
	if err != nil {
		c.setAuthFromError(err)
		return cur, err
	}
	total := len(meetings)
	reportProgress(ctx, func(p *Progress) { p.Phase, p.ItemsTotal = ProgressFetching, total })
	newest := parsePlaudCursor(cur)
	var skipped []string
	for i := range meetings {
		if err := ctx.Err(); err != nil {
			return cur, err
		}
		tm := meetings[i]
		c.meetingDetails(ctx, tok.AccessToken, &tm)
		title := progressTitle(meetingTitle(TypeTeams, teamsLabel, tm.MeetingRecording))
		reportProgress(ctx, func(p *Progress) { p.Phase, p.Current = ProgressFetching, title })
		content, err := c.fetchContent(ctx, tok.AccessToken, tm)
		if err != nil {
			if errors.Is(err, ErrMeetingNotConnected) || errors.Is(err, ErrTeamsConsentRequired) {
				c.setAuthFromError(err)
				return cur, err
			}
			skipped = append(skipped, fmt.Sprintf("%s: %v", tm.ID, err))
			reportProgress(ctx, func(p *Progress) { p.ItemsDone++ })
			continue
		}
		tm.Tags = meetingScopeList(c.cfg, "tags")
		if err := emit(meetingPage(c.cfg, TypeTeams, teamsLabel, tm.MeetingRecording, content, c.mediaLine(tm))); err != nil {
			return cur, err
		}
		if tm.StartAt.After(newest) {
			newest = tm.StartAt
		}
	}
	c.setAuth(AuthState{Connected: true, CheckedAt: c.now().UTC(), Account: c.accountLabel()})
	next := cur
	if !newest.IsZero() {
		next = Cursor(newest.UTC().Format(time.RFC3339))
	}
	if len(skipped) > 0 {
		return next, &PartialError{Err: fmt.Errorf("teams: %d recording(s) skipped: %s", len(skipped), strings.Join(skipped, "; "))}
	}
	return next, nil
}

// accountLabel describes the signed-in principal without any secret.
func (c *teamsConnector) accountLabel() string {
	users := meetingScopeList(c.cfg, "users")
	switch {
	case len(users) == 1:
		return "organizer " + users[0]
	case len(users) > 1:
		return strconv.Itoa(len(users)) + " organizers"
	case c.cfg.ScopeValue("group") != "":
		return "group " + c.cfg.ScopeValue("group")
	}
	return ""
}

// --- MeetingReader / MeetingMediaReader ---

// ListMeetings lists recordings within the since_days window, newest first.
func (c *teamsConnector) ListMeetings(ctx context.Context) ([]MeetingRecording, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	since := c.now().Add(-time.Duration(meetingScopeInt(c.cfg, "since_days", meetingDefaultSinceDays)) * 24 * time.Hour)
	meetings, err := c.listMeetings(ctx, tok.AccessToken, since, meetingScopeInt(c.cfg, "max_recordings", meetingDefaultMaxRecordings))
	if err != nil {
		c.setAuthFromError(err)
		return nil, err
	}
	tags := meetingScopeList(c.cfg, "tags")
	out := make([]MeetingRecording, 0, len(meetings))
	for i := range meetings {
		tm := meetings[i]
		c.meetingDetails(ctx, tok.AccessToken, &tm)
		rec := tm.MeetingRecording
		rec.Topic = meetingTitle(TypeTeams, teamsLabel, rec)
		rec.Tags = tags
		out = append(out, rec)
	}
	sortMeetingsNewestFirst(out)
	return out, nil
}

// findMeeting re-lists the since_days window to locate one recording by id
// (Graph has no lookup of a callRecording by id alone).
func (c *teamsConnector) findMeeting(ctx context.Context, token, id string) (teamsMeeting, error) {
	since := c.now().Add(-time.Duration(meetingScopeInt(c.cfg, "since_days", meetingDefaultSinceDays)) * 24 * time.Hour)
	meetings, err := c.listMeetings(ctx, token, since, meetingScopeInt(c.cfg, "max_recordings", meetingDefaultMaxRecordings))
	if err != nil {
		return teamsMeeting{}, err
	}
	for _, tm := range meetings {
		if tm.ID == id {
			return tm, nil
		}
	}
	return teamsMeeting{}, fmt.Errorf("teams: recording %s is not in the last %d days of listed meetings", id, meetingScopeInt(c.cfg, "since_days", meetingDefaultSinceDays))
}

// ReadMeeting reads the transcript of one recording.
func (c *teamsConnector) ReadMeeting(ctx context.Context, id string) (MeetingContent, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return MeetingContent{}, err
	}
	tm, err := c.findMeeting(ctx, tok.AccessToken, id)
	if err != nil {
		return MeetingContent{}, err
	}
	c.meetingDetails(ctx, tok.AccessToken, &tm)
	return c.fetchContent(ctx, tok.AccessToken, tm)
}

// MediaLink implements MeetingMediaReader. Graph serves recording content
// only behind a bearer token, which a browser media element cannot send, so
// no playable link exists yet; the error names the meeting page instead.
func (c *teamsConnector) MediaLink(ctx context.Context, id string) (MediaLink, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return MediaLink{}, err
	}
	tm, err := c.findMeeting(ctx, tok.AccessToken, id)
	if err != nil {
		return MediaLink{}, err
	}
	if len(tm.Recordings) == 0 {
		return MediaLink{}, fmt.Errorf("teams: %w (transcript only)", ErrMeetingNoMedia)
	}
	return MediaLink{}, fmt.Errorf("teams: %w — Graph serves Teams recordings behind a bearer token; open the recording from the meeting chat in Teams", ErrMeetingNoMedia)
}

// --- Authenticator ---

// AuthStatus implements Authenticator. With refresh it fetches a token and
// probes the recordings listing for the first organizer, so a missing admin
// consent shows up on the card before the first sync.
func (c *teamsConnector) AuthStatus(ctx context.Context, refresh bool) AuthState {
	if refresh {
		c.refreshAuth(ctx)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auth
}

func (c *teamsConnector) refreshAuth(ctx context.Context) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		c.setAuthFromError(err)
		return
	}
	organizers, err := c.organizers(ctx, tok.AccessToken)
	if err == nil && len(organizers) == 0 {
		err = errors.New("teams: no organizer resolved from scope.users / scope.group")
	}
	if err == nil {
		until := c.now().UTC().Add(time.Minute)
		link := teamsArtifactsURL(organizers[0], "getAllRecordings", until.Add(-24*time.Hour), until)
		_, err = c.getJSON(ctx, tok.AccessToken, link)
	}
	if err != nil {
		c.setAuthFromError(err)
		return
	}
	c.setAuth(AuthState{Connected: true, CheckedAt: c.now().UTC(), Account: c.accountLabel()})
}

func (c *teamsConnector) setAuth(st AuthState) {
	c.mu.Lock()
	c.auth = st
	c.mu.Unlock()
}

// setAuthFromError records a failed probe or sync as the card state, with
// the admin-consent URL when consent is what is missing.
func (c *teamsConnector) setAuthFromError(err error) {
	st := AuthState{CheckedAt: c.now().UTC(), Message: firstLine(err.Error())}
	if errors.Is(err, ErrTeamsConsentRequired) {
		st.AuthURL = c.adminConsentURL()
	}
	c.setAuth(st)
}

// adminConsentURL is the Entra page where a tenant administrator grants the
// app's application permissions; empty when the ids are not resolvable.
func (c *teamsConnector) adminConsentURL() string {
	tenant, err := envValue("teams", "tenant_id_env", c.cfg.Auth.TenantIDEnv)
	if err != nil {
		return ""
	}
	id, err := envValue("teams", "client_id_env", c.cfg.Auth.ClientIDEnv)
	if err != nil {
		return ""
	}
	return teamsLoginBase + "/" + url.PathEscape(tenant) + "/adminconsent?" + url.Values{"client_id": {id}}.Encode()
}

// Connect implements Authenticator. Client credentials have no interactive
// step: Connect probes Graph and, when consent is missing, hands back the
// admin-consent URL for the tenant administrator.
func (c *teamsConnector) Connect(ctx context.Context) (AuthState, error) {
	c.refreshAuth(ctx)
	st := c.AuthStatus(ctx, false)
	if !st.Connected && st.AuthURL == "" {
		st.AuthURL = c.adminConsentURL()
		c.setAuth(st)
	}
	return st, nil
}

// Disconnect implements Authenticator: forgets the cached token. The app
// registration and its consent stay in the tenant.
func (c *teamsConnector) Disconnect(ctx context.Context) (AuthState, error) {
	c.mu.Lock()
	c.token = oauthTokens{}
	c.mu.Unlock()
	st := AuthState{CheckedAt: c.now().UTC(), Message: "cached token cleared; the Entra app registration and its consent are unchanged"}
	c.setAuth(st)
	return st, nil
}
