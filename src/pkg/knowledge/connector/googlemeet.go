package connector

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
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

// TypeGoogleMeet is the connector type for Google Meet conference records
// (recordings, transcripts and Gemini notes) read through the Meet REST API.
const TypeGoogleMeet = "google-meet"

// Google endpoints. Variables so tests point them at a fake server.
var (
	googleMeetAPIBase = "https://meet.googleapis.com/v2"
	googleDriveAPI    = "https://www.googleapis.com/drive/v3"
	googleAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL    = "https://oauth2.googleapis.com/token"
	googleRevokeURL   = "https://oauth2.googleapis.com/revoke"
)

const (
	// googleMeetLabel is the human name used in headings and messages.
	googleMeetLabel = "Google Meet"
	// googleMeetAttrPrefix prefixes the fact attributes (attr_google_meet_*);
	// the connector type has a hyphen, which attribute keys avoid.
	googleMeetAttrPrefix = "google_meet"
	// googleMeetScopes are the OAuth scopes requested: conference records,
	// recordings, transcripts and smart notes (meetings.space.readonly), the
	// Meet-created Drive files behind them (drive.meet.readonly), and the
	// signed-in address for the card (openid email).
	googleMeetScopes = "https://www.googleapis.com/auth/meetings.space.readonly https://www.googleapis.com/auth/drive.meet.readonly openid email"
	// googleMeetDefaultRedirectURI is the OAuth redirect the operator registers
	// on the Google OAuth client when the hive has no public callback: the
	// browser lands on a connection error and the URL is pasted into the card.
	googleMeetDefaultRedirectURI = "http://localhost:8199/google-meet/callback"
	// googleMeetDefaultUser names the signed-in user for user-level OAuth.
	googleMeetDefaultUser = "me"
	// googleMeetPageSize is the pageSize requested on every Meet listing (the
	// API maximum for conference records, transcript entries and smart notes).
	googleMeetPageSize = 100
	// googleMeetTokenFile is the user-level token store, relative to the
	// connector's private HOME.
	googleMeetTokenFile = ".google-meet/tokens.json"
	// googleMeetLoginWindow is how long a started sign-in accepts its pasted
	// redirect URL.
	googleMeetLoginWindow = 10 * time.Minute
	// googleMeetPendingRefetchCap bounds how many text-less records are
	// re-read per sync on top of the listing.
	googleMeetPendingRefetchCap = 25
	// googleMeetJWTLifetime is the lifetime of a service-account assertion
	// (Google's maximum).
	googleMeetJWTLifetime = time.Hour
	// googleMeetJWTBearerGrant is the grant type of a service-account token
	// request.
	googleMeetJWTBearerGrant = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	// googleMeetRecordPrefix is the resource name prefix of a conference record.
	googleMeetRecordPrefix = "conferenceRecords/"
	// Artifact states of recordings, transcripts and smart notes.
	googleMeetStateEnded         = "ENDED"
	googleMeetStateFileGenerated = "FILE_GENERATED"
	// googleMeetExportMIME is the Drive export format of a Docs transcript or
	// Gemini notes document.
	googleMeetExportMIME = "text/plain"
)

// googleMeetIDRE bounds a conference record id before it is used in a path.
var googleMeetIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// googleMeetTitleSuffixRE strips the artifact suffix Meet appends to Drive
// file names ("<event> - Transcript", "<event> - Notes by Gemini").
var googleMeetTitleSuffixRE = regexp.MustCompile(`(?i)\s+-\s+(transcript|notes by gemini|recording)\s*$`)

// googleMeetTitleDateRE strips the trailing date Meet puts in Drive file
// names ("<event> (2026-11-12 10:00 GMT-5)", "<event> - 2026/11/12 10:00 EST").
var googleMeetTitleDateRE = regexp.MustCompile(`\s*(\(|-\s+)\d{4}[/-]\d{2}[/-]\d{2}[^()]*\)?\s*$`)

// meetArtifact is one recording, transcript or smart-notes resource.
type meetArtifact struct {
	Name      string
	State     string
	FileID    string
	ExportURI string
}

// generated reports whether the artifact's Drive file exists.
func (a meetArtifact) generated() bool {
	return a.State == googleMeetStateFileGenerated
}

// meetRecord is one conference record with the artifacts the listing found.
type meetRecord struct {
	MeetingRecording
	// User is the account whose token reads this record ("me" for user-level
	// OAuth; the impersonated address for a service account).
	User        string
	Space       string
	Recordings  []meetArtifact
	Transcripts []meetArtifact
	Notes       []meetArtifact
}

// hasArtifacts reports whether the record produced anything worth a fact.
func (r meetRecord) hasArtifacts() bool {
	return len(r.Recordings)+len(r.Transcripts)+len(r.Notes) > 0
}

// googleServiceAccount is the part of a service-account JSON key the
// connector reads.
type googleServiceAccount struct {
	Type         string `json:"type"`
	ClientEmail  string `json:"client_email"`
	PrivateKey   string `json:"private_key"`
	PrivateKeyID string `json:"private_key_id"`
}

// googleMeetConnector syncs Google Meet conference records that produced a
// recording, a transcript or Gemini notes into one fact per meeting. Auth:
// an OAuth client registered by the operator, either user-level
// (authorization code, refresh token kept in the connector token store) or a
// service account with domain-wide delegation (server_to_server; the key
// JSON through auth.env or auth.file, scope.users lists the addresses to
// impersonate). Scope keys: users, since_days, max_recordings,
// include_summary, include_participants, include_video_link, redirect_uri,
// tags.
type googleMeetConnector struct {
	cfg      ConnectorConfig
	logger   *slog.Logger
	http     *HTTPClient
	now      func() time.Time
	store    tokenStore
	vaultDir string

	mu sync.Mutex
	// saTokens caches service-account tokens per impersonated user (never
	// persisted).
	saTokens     map[string]oauthTokens
	pendingState string
	pendingAt    time.Time
	auth         AuthState
	truncated    bool
	// titles caches Drive file names per conference record id.
	titles map[string]string
	// codes caches meeting codes per space.
	codes map[string]string
}

var (
	_ Authenticator      = (*googleMeetConnector)(nil)
	_ AuthCompleter      = (*googleMeetConnector)(nil)
	_ MeetingReader      = (*googleMeetConnector)(nil)
	_ MeetingMediaReader = (*googleMeetConnector)(nil)
	_ Truncator          = (*googleMeetConnector)(nil)
)

func newGoogleMeetConnector(cfg ConnectorConfig, deps Deps) (Connector, error) {
	c := &googleMeetConnector{cfg: cfg, logger: deps.logger(), http: deps.httpClient(), now: time.Now,
		saTokens: map[string]oauthTokens{}, titles: map[string]string{}, codes: map[string]string{}}
	if deps.StateDir != "" {
		home := filepath.Join(deps.StateDir, "home")
		if _, err := ensureDir(home); err != nil {
			return nil, fmt.Errorf("google-meet: %w", err)
		}
		c.store = tokenStore{path: filepath.Join(home, googleMeetTokenFile)}
	}
	if deps.KnowledgeDir != "" && cfg.Layer != "" {
		c.vaultDir = filepath.Join(deps.KnowledgeDir, "connectors", cfg.Layer)
	}
	return c, nil
}

func (c *googleMeetConnector) Type() string { return TypeGoogleMeet }

// Validate checks the auth block and scope. Credentials are referenced by
// environment variable name or file path only.
func (c *googleMeetConnector) Validate(cfg ConnectorConfig) error {
	if cfg.Auth.AccountIDEnv != "" || cfg.Auth.TenantIDEnv != "" {
		return errors.New("google-meet: auth.account_id_env / auth.tenant_id_env are zoom / teams settings")
	}
	switch cfg.Auth.Mode {
	case "", AuthModeUserOAuth:
		if cfg.Auth.Configured() {
			return errors.New("google-meet: auth.env/auth.file hold a service-account key and need auth.mode server_to_server; user_oauth uses auth.client_id_env and auth.client_secret_env")
		}
		if cfg.Auth.ClientIDEnv == "" || cfg.Auth.ClientSecretEnv == "" {
			return errors.New("google-meet: auth.client_id_env and auth.client_secret_env are required (environment variable names)")
		}
	case AuthModeServerToServer:
		if !cfg.Auth.Configured() {
			return errors.New("google-meet: server_to_server needs the service-account key JSON through auth.env (variable name) or auth.file (path)")
		}
		if cfg.Auth.ClientIDEnv != "" || cfg.Auth.ClientSecretEnv != "" {
			return errors.New("google-meet: auth.client_id_env/auth.client_secret_env are user_oauth settings; a service account signs with its key")
		}
		users := meetingScopeList(cfg, "users")
		if len(users) == 0 {
			return errors.New("google-meet: scope.users must list the Workspace addresses to impersonate for server_to_server (domain-wide delegation)")
		}
		for _, u := range users {
			if !strings.Contains(u, "@") {
				return fmt.Errorf("google-meet: scope.users entry %q is not an email address", u)
			}
		}
	default:
		return fmt.Errorf("google-meet: auth.mode must be %s or %s", AuthModeUserOAuth, AuthModeServerToServer)
	}
	if r := cfg.ScopeValue("redirect_uri"); r != "" {
		u, err := url.Parse(r)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("google-meet: scope.redirect_uri must be an absolute http(s) URL registered on the OAuth client")
		}
	}
	return validateMeetingScope("google-meet", cfg)
}

func (c *googleMeetConnector) mode() string {
	if c.cfg.Auth.Mode == AuthModeServerToServer {
		return AuthModeServerToServer
	}
	return AuthModeUserOAuth
}

// users lists the accounts read: the impersonated addresses for a service
// account, the signed-in user otherwise.
func (c *googleMeetConnector) users() []string {
	if c.mode() == AuthModeServerToServer {
		return meetingScopeList(c.cfg, "users")
	}
	return []string{googleMeetDefaultUser}
}

func (c *googleMeetConnector) redirectURI() string {
	if r := c.cfg.ScopeValue("redirect_uri"); r != "" {
		return r
	}
	return googleMeetDefaultRedirectURI
}

// Truncated implements Truncator.
func (c *googleMeetConnector) Truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}

// --- OAuth ---

// clientCredentials resolves the OAuth client registration.
func (c *googleMeetConnector) clientCredentials() (id, secret string, err error) {
	if id, err = envValue("google-meet", "client_id_env", c.cfg.Auth.ClientIDEnv); err != nil {
		return "", "", err
	}
	if secret, err = envValue("google-meet", "client_secret_env", c.cfg.Auth.ClientSecretEnv); err != nil {
		return "", "", err
	}
	return id, secret, nil
}

// tokenRequest posts form to the Google token endpoint and decodes the reply.
func (c *googleMeetConnector) tokenRequest(ctx context.Context, form url.Values) (oauthTokens, error) {
	h := http.Header{}
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(ctx, http.MethodPost, googleTokenURL, h, []byte(form.Encode()))
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) {
			return oauthTokens{}, fmt.Errorf("google-meet: token request rejected (HTTP %d): %s", se.StatusCode, googleErrorText(se.Body))
		}
		return oauthTokens{}, fmt.Errorf("google-meet: token request: %w", err)
	}
	m, err := decodeJSONObject("google-meet", resp.Body)
	if err != nil {
		return oauthTokens{}, err
	}
	tok := oauthTokens{
		AccessToken:  jsonString(m, "access_token"),
		RefreshToken: jsonString(m, "refresh_token"),
		Scope:        jsonString(m, "scope"),
		Account:      googleIDTokenEmail(jsonString(m, "id_token")),
	}
	if tok.AccessToken == "" {
		return oauthTokens{}, errors.New("google-meet: token reply carried no access_token")
	}
	ttl := time.Hour
	if v, ok := m["expires_in"].(float64); ok && v > 0 {
		ttl = time.Duration(v) * time.Second
	}
	tok.ExpiresAt = c.now().Add(ttl)
	return tok, nil
}

// googleIDTokenEmail reads the email claim of an id_token the token endpoint
// just returned over TLS (used only as the card's account label).
func googleIDTokenEmail(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Email
}

// googleErrorText pulls the readable part out of a Google error body.
func googleErrorText(body string) string {
	if m, err := decodeJSONObject("google-meet", []byte(body)); err == nil {
		if e, ok := m["error"].(map[string]any); ok {
			status, msg := jsonString(e, "status"), jsonString(e, "message")
			return strings.TrimSpace(strings.Trim(status+": "+firstLine(msg), ": "))
		}
		if code := jsonString(m, "error"); code != "" {
			return strings.TrimSpace(strings.Trim(code+": "+firstLine(jsonString(m, "error_description")), ": "))
		}
	}
	return firstLine(body)
}

// serviceAccount loads the service-account key named by auth.env/auth.file.
func (c *googleMeetConnector) serviceAccount() (googleServiceAccount, error) {
	raw, err := c.cfg.Auth.Secret()
	if err != nil {
		return googleServiceAccount{}, fmt.Errorf("google-meet: service-account key: %w", err)
	}
	var sa googleServiceAccount
	if err := json.Unmarshal([]byte(raw), &sa); err != nil {
		return googleServiceAccount{}, errors.New("google-meet: the service-account key is not valid JSON (download it from Google Cloud → IAM → Service accounts → Keys)")
	}
	if sa.Type != "service_account" || sa.ClientEmail == "" || sa.PrivateKey == "" {
		return googleServiceAccount{}, errors.New("google-meet: the key is not a service-account key (type, client_email and private_key are required)")
	}
	return sa, nil
}

// googleServiceAccountJWT signs the RS256 assertion a service account trades
// for an access token on behalf of sub (domain-wide delegation).
func googleServiceAccountJWT(sa googleServiceAccount, sub, scope, aud string, now time.Time) (string, error) {
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return "", errors.New("google-meet: the service-account private_key is not PEM")
	}
	var key *rsa.PrivateKey
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return "", errors.New("google-meet: the service-account private_key is not an RSA key")
		}
		key = rk
	} else if rk, err2 := x509.ParsePKCS1PrivateKey(block.Bytes); err2 == nil {
		key = rk
	} else {
		return "", fmt.Errorf("google-meet: parsing the service-account private_key: %w", err)
	}
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if sa.PrivateKeyID != "" {
		header["kid"] = sa.PrivateKeyID
	}
	claims := map[string]any{
		"iss":   sa.ClientEmail,
		"sub":   sub,
		"scope": scope,
		"aud":   aud,
		"iat":   now.Unix(),
		"exp":   now.Add(googleMeetJWTLifetime).Unix(),
	}
	hj, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	cj, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := base64.RawURLEncoding.EncodeToString(hj) + "." + base64.RawURLEncoding.EncodeToString(cj)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("google-meet: signing the assertion: %w", err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// accessToken returns a usable bearer token for user: a cached
// service-account token, or the stored user token refreshed when needed.
func (c *googleMeetConnector) accessToken(ctx context.Context, user string) (oauthTokens, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.mode() == AuthModeServerToServer {
		if tok, ok := c.saTokens[user]; ok && tok.valid(now) {
			return tok, nil
		}
		sa, err := c.serviceAccount()
		if err != nil {
			return oauthTokens{}, err
		}
		assertion, err := googleServiceAccountJWT(sa, user, googleMeetScopes, googleTokenURL, now)
		if err != nil {
			return oauthTokens{}, err
		}
		tok, err := c.tokenRequest(ctx, url.Values{"grant_type": {googleMeetJWTBearerGrant}, "assertion": {assertion}})
		if err != nil {
			return oauthTokens{}, fmt.Errorf("google-meet: %w — the service account could not act as %s; check domain-wide delegation for its client id with the scopes meetings.space.readonly and drive.meet.readonly (%v)", ErrMeetingNotConnected, user, err)
		}
		tok.Account = user
		c.saTokens[user] = tok
		return tok, nil
	}
	tok, err := c.store.load()
	if err != nil {
		if os.IsNotExist(err) {
			return oauthTokens{}, fmt.Errorf("google-meet: %w — use Connect in the Knowledge → Integrations → Google Meet card", ErrMeetingNotConnected)
		}
		return oauthTokens{}, fmt.Errorf("google-meet: %w", err)
	}
	if tok.valid(now) {
		return tok, nil
	}
	if tok.RefreshToken == "" {
		return oauthTokens{}, fmt.Errorf("google-meet: %w — the stored sign-in has no refresh token; use Connect in the Knowledge → Integrations → Google Meet card", ErrMeetingNotConnected)
	}
	id, secret, err := c.clientCredentials()
	if err != nil {
		return oauthTokens{}, err
	}
	fresh, err := c.tokenRequest(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}, "client_id": {id}, "client_secret": {secret}})
	if err != nil {
		return oauthTokens{}, fmt.Errorf("google-meet: %w — refreshing the sign-in failed, use Connect in the Knowledge → Integrations → Google Meet card (%v)", ErrMeetingNotConnected, err)
	}
	if fresh.Account == "" {
		fresh.Account = tok.Account
	}
	if fresh.RefreshToken == "" {
		// Google keeps the refresh token across refreshes.
		fresh.RefreshToken = tok.RefreshToken
	}
	if err := c.store.save(fresh); err != nil {
		return oauthTokens{}, fmt.Errorf("google-meet: saving refreshed token: %w", err)
	}
	return fresh, nil
}

// googleMeetAuthError classifies refusals: 401 is "not connected"; 403 keeps
// Google's reason (API disabled, scope not granted) in the message.
func googleMeetAuthError(err error) error {
	if err == nil || errors.Is(err, ErrMeetingNotConnected) {
		return err
	}
	var se *StatusError
	if !errors.As(err, &se) {
		return err
	}
	switch se.StatusCode {
	case http.StatusUnauthorized:
		return meetingNotConnected("google-meet", googleMeetLabel, err)
	case http.StatusForbidden:
		return fmt.Errorf("google-meet: permission denied — enable the Google Meet REST API and Drive API on the OAuth client's project and grant meetings.space.readonly and drive.meet.readonly (%s)", googleErrorText(se.Body))
	}
	return err
}

// googleTrustedURL keeps the bearer token on the Meet and Drive API hosts.
func googleTrustedURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("google-meet: invalid link")
	}
	for _, base := range []string{googleMeetAPIBase, googleDriveAPI} {
		if b, err := url.Parse(base); err == nil && strings.EqualFold(b.Host, u.Host) {
			return nil
		}
	}
	return fmt.Errorf("google-meet: link host %q is not a Google API host", u.Host)
}

// get performs an authenticated GET and returns the body.
func (c *googleMeetConnector) get(ctx context.Context, token, link string) ([]byte, error) {
	if err := googleTrustedURL(link); err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Get(ctx, link, h)
	if err != nil {
		return nil, googleMeetAuthError(err)
	}
	return resp.Body, nil
}

// getJSON is get plus object decoding.
func (c *googleMeetConnector) getJSON(ctx context.Context, token, link string) (map[string]any, error) {
	body, err := c.get(ctx, token, link)
	if err != nil {
		return nil, err
	}
	return decodeJSONObject("google-meet", body)
}

// listPaged follows pageToken through a Meet listing, calling each for every
// object under key.
func (c *googleMeetConnector) listPaged(ctx context.Context, token, base string, query url.Values, key string, each func(map[string]any) bool) error {
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("pageSize", strconv.Itoa(googleMeetPageSize))
	for {
		m, err := c.getJSON(ctx, token, base+"?"+q.Encode())
		if err != nil {
			return err
		}
		for _, item := range jsonList(m, key) {
			if !each(item) {
				return nil
			}
		}
		next := jsonString(m, "nextPageToken")
		if next == "" {
			return nil
		}
		q.Set("pageToken", next)
	}
}

// --- listing ---

// meetRecordID turns "conferenceRecords/abc" into "abc".
func meetRecordID(name string) string {
	return strings.TrimPrefix(name, googleMeetRecordPrefix)
}

// parseMeetRecord maps one ConferenceRecord.
func parseMeetRecord(m map[string]any, user string) meetRecord {
	r := meetRecord{User: user, Space: jsonString(m, "space")}
	r.ID = meetRecordID(jsonString(m, "name"))
	r.StartAt = jsonTime(m, "startTime")
	if end := jsonTime(m, "endTime"); !r.StartAt.IsZero() && end.After(r.StartAt) {
		r.DurationMS = end.Sub(r.StartAt).Milliseconds()
	}
	if user != googleMeetDefaultUser {
		r.Host = user
	}
	return r
}

// parseMeetArtifact maps a recording, transcript or smart note.
func parseMeetArtifact(m map[string]any) meetArtifact {
	a := meetArtifact{Name: jsonString(m, "name"), State: jsonString(m, "state")}
	for _, key := range []string{"driveDestination", "docsDestination"} {
		if d, ok := m[key].(map[string]any); ok {
			a.FileID = jsonString(d, "file", "document")
			a.ExportURI = jsonString(d, "exportUri")
		}
	}
	return a
}

// artifacts lists the recordings, transcripts and (when summaries are on)
// smart notes of one record. A smart-notes failure other than "not
// connected" only drops the notes: the resource needs Gemini in Meet.
func (c *googleMeetConnector) artifacts(ctx context.Context, token string, r *meetRecord) error {
	base := googleMeetAPIBase + "/" + googleMeetRecordPrefix + url.PathEscape(r.ID)
	collect := func(kind, key string, into *[]meetArtifact) error {
		*into = nil
		return c.listPaged(ctx, token, base+"/"+kind, nil, key, func(m map[string]any) bool {
			*into = append(*into, parseMeetArtifact(m))
			return true
		})
	}
	if err := collect("recordings", "recordings", &r.Recordings); err != nil {
		return fmt.Errorf("recordings: %w", err)
	}
	if err := collect("transcripts", "transcripts", &r.Transcripts); err != nil {
		return fmt.Errorf("transcripts: %w", err)
	}
	if meetingScopeBool(c.cfg, "include_summary", true) {
		if err := collect("smartNotes", "smartNotes", &r.Notes); err != nil {
			if errors.Is(err, ErrMeetingNotConnected) {
				return err
			}
			c.logger.Debug("google meet smart notes unavailable", "record", r.ID, "error", err)
			r.Notes = nil
		}
	}
	for _, a := range r.Recordings {
		if a.generated() {
			r.HasMedia = true
			if r.URL == "" {
				r.URL = a.ExportURI
			}
		}
	}
	return nil
}

// cleanMeetTitle turns a Meet-created Drive file name into the event title.
func cleanMeetTitle(name string) string {
	name = strings.TrimSpace(googleMeetTitleSuffixRE.ReplaceAllString(strings.TrimSpace(name), ""))
	return strings.TrimSpace(googleMeetTitleDateRE.ReplaceAllString(name, ""))
}

// describe fills the title (from the Meet-created Drive file name, which
// carries the calendar event title) and the meeting code. Both are best
// effort and cached per record / space.
func (c *googleMeetConnector) describe(ctx context.Context, token string, r *meetRecord) {
	c.mu.Lock()
	title, haveTitle := c.titles[r.ID]
	code, haveCode := c.codes[r.Space]
	c.mu.Unlock()
	if !haveTitle {
		for _, list := range [][]meetArtifact{r.Notes, r.Transcripts, r.Recordings} {
			for _, a := range list {
				if a.FileID == "" || title != "" {
					continue
				}
				m, err := c.getJSON(ctx, token, googleDriveAPI+"/files/"+url.PathEscape(a.FileID)+"?fields=name&supportsAllDrives=true")
				if err != nil {
					c.logger.Debug("google meet file name unavailable", "file", a.FileID, "error", err)
					continue
				}
				title = cleanMeetTitle(jsonString(m, "name"))
			}
		}
		c.mu.Lock()
		c.titles[r.ID] = title
		c.mu.Unlock()
	}
	if !haveCode && strings.HasPrefix(r.Space, "spaces/") {
		if m, err := c.getJSON(ctx, token, googleMeetAPIBase+"/spaces/"+url.PathEscape(strings.TrimPrefix(r.Space, "spaces/"))); err == nil {
			code = jsonString(m, "meetingCode")
			if r.URL == "" {
				r.URL = jsonString(m, "meetingUri")
			}
		}
		c.mu.Lock()
		c.codes[r.Space] = code
		c.mu.Unlock()
	}
	r.MeetingID = code
	switch {
	case title != "":
		r.Topic = title
	case code != "":
		r.Topic = googleMeetLabel + " " + code
	}
}

// listRecords lists conference records that started since, per user, keeps
// those that ended and produced an artifact, newest first, capped at limit.
func (c *googleMeetConnector) listRecords(ctx context.Context, since time.Time, limit int) ([]meetRecord, error) {
	var out []meetRecord
	seen := map[string]bool{}
	truncated := false
	for _, user := range c.users() {
		if truncated {
			break
		}
		tok, err := c.accessToken(ctx, user)
		if err != nil {
			return nil, err
		}
		var page []meetRecord
		q := url.Values{"filter": {`start_time>="` + since.UTC().Format(time.RFC3339) + `"`}}
		err = c.listPaged(ctx, tok.AccessToken, googleMeetAPIBase+"/conferenceRecords", q, "conferenceRecords", func(m map[string]any) bool {
			if jsonString(m, "endTime") == "" {
				return true // still in progress: artifacts are not ready
			}
			page = append(page, parseMeetRecord(m, user))
			return true
		})
		if err != nil {
			return nil, err
		}
		for i := range page {
			r := page[i]
			if r.ID == "" || !googleMeetIDRE.MatchString(r.ID) || seen[r.ID] {
				continue
			}
			// A meeting several impersonated users can see is read once.
			seen[r.ID] = true
			if err := c.artifacts(ctx, tok.AccessToken, &r); err != nil {
				if errors.Is(err, ErrMeetingNotConnected) {
					return nil, err
				}
				c.logger.Debug("google meet artifacts unavailable", "record", r.ID, "error", err)
				continue
			}
			if !r.hasArtifacts() {
				continue
			}
			c.describe(ctx, tok.AccessToken, &r)
			out = append(out, r)
			if len(out) >= limit {
				truncated = true
				break
			}
		}
	}
	c.mu.Lock()
	c.truncated = truncated
	c.mu.Unlock()
	sortMeetRecordsNewestFirst(out)
	return out, nil
}

func sortMeetRecordsNewestFirst(rs []meetRecord) {
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j].StartAt.After(rs[j-1].StartAt); j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}

// getRecord reads one conference record by id with its artifacts, trying
// each configured user until one can see it.
func (c *googleMeetConnector) getRecord(ctx context.Context, id string) (meetRecord, oauthTokens, error) {
	if !googleMeetIDRE.MatchString(id) {
		return meetRecord{}, oauthTokens{}, fmt.Errorf("google-meet: invalid conference record id %q", id)
	}
	var lastErr error
	for _, user := range c.users() {
		tok, err := c.accessToken(ctx, user)
		if err != nil {
			return meetRecord{}, oauthTokens{}, err
		}
		m, err := c.getJSON(ctx, tok.AccessToken, googleMeetAPIBase+"/"+googleMeetRecordPrefix+url.PathEscape(id))
		if err != nil {
			if errors.Is(err, ErrMeetingNotConnected) {
				return meetRecord{}, oauthTokens{}, err
			}
			lastErr = err
			continue
		}
		r := parseMeetRecord(m, user)
		if r.ID == "" {
			r.ID = id
		}
		if err := c.artifacts(ctx, tok.AccessToken, &r); err != nil {
			return meetRecord{}, oauthTokens{}, err
		}
		c.describe(ctx, tok.AccessToken, &r)
		return r, tok, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no user configured")
	}
	return meetRecord{}, oauthTokens{}, fmt.Errorf("google-meet: conference record %s: %w", id, lastErr)
}

// --- content ---

// meetParticipantName reads a Participant's display name.
func meetParticipantName(p map[string]any) string {
	for _, key := range []string{"signedinUser", "anonymousUser", "phoneUser"} {
		if u, ok := p[key].(map[string]any); ok {
			if n := jsonString(u, "displayName"); n != "" {
				return n
			}
		}
	}
	return ""
}

// participants lists the record's participants as name-by-resource-name and
// the names in join order.
func (c *googleMeetConnector) participants(ctx context.Context, token string, r meetRecord) (map[string]string, []string, error) {
	byName := map[string]string{}
	var names []string
	err := c.listPaged(ctx, token, googleMeetAPIBase+"/"+googleMeetRecordPrefix+url.PathEscape(r.ID)+"/participants", nil, "participants", func(p map[string]any) bool {
		if n := meetParticipantName(p); n != "" {
			byName[jsonString(p, "name")] = n
			names = append(names, n)
		}
		return true
	})
	return byName, names, err
}

// meetStamp formats an offset from the meeting start as mm:ss or h:mm:ss.
func meetStamp(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	sec := int(d / time.Second)
	h, m, s := sec/3600, (sec%3600)/60, sec%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// transcriptEntries renders a transcript's structured entries as
// "[mm:ss] Speaker: text" lines.
func (c *googleMeetConnector) transcriptEntries(ctx context.Context, token string, r meetRecord, tr meetArtifact, names map[string]string) (string, error) {
	var b strings.Builder
	err := c.listPaged(ctx, token, googleMeetAPIBase+"/"+tr.Name+"/entries", nil, "transcriptEntries", func(e map[string]any) bool {
		text := strings.TrimSpace(jsonString(e, "text"))
		if text == "" {
			return true
		}
		line := text
		if who := names[jsonString(e, "participant")]; who != "" {
			line = who + ": " + text
		}
		if start := jsonTime(e, "startTime"); !start.IsZero() && !r.StartAt.IsZero() {
			line = "[" + meetStamp(start.Sub(r.StartAt)) + "] " + line
		}
		b.WriteString(line)
		b.WriteByte('\n')
		return true
	})
	return strings.TrimSpace(b.String()), err
}

// exportDoc reads a Meet-created Google Doc as plain text through Drive.
func (c *googleMeetConnector) exportDoc(ctx context.Context, token, fileID string) (string, error) {
	body, err := c.get(ctx, token, googleDriveAPI+"/files/"+url.PathEscape(fileID)+"/export?"+url.Values{"mimeType": {googleMeetExportMIME}}.Encode())
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.TrimPrefix(string(body), "\ufeff")), nil
}

// fetchContent reads the transcript (structured entries, else the Docs
// export), the Gemini notes as summary and the participants of one record.
// Notes and participants are optional extras: a failure only drops them.
func (c *googleMeetConnector) fetchContent(ctx context.Context, token string, r meetRecord) (MeetingContent, error) {
	var out MeetingContent
	names := map[string]string{}
	if len(r.Transcripts) > 0 || meetingScopeBool(c.cfg, "include_participants", true) {
		byName, list, err := c.participants(ctx, token, r)
		switch {
		case err == nil:
			names = byName
			if meetingScopeBool(c.cfg, "include_participants", true) {
				out.Participants = list
			}
		case errors.Is(err, ErrMeetingNotConnected):
			return MeetingContent{}, err
		default:
			c.logger.Debug("google meet participants unavailable", "record", r.ID, "error", err)
		}
	}
	for i := len(r.Transcripts) - 1; i >= 0 && out.Transcript == ""; i-- {
		tr := r.Transcripts[i]
		if tr.State != googleMeetStateEnded && !tr.generated() {
			continue
		}
		text, err := c.transcriptEntries(ctx, token, r, tr, names)
		if err != nil {
			if errors.Is(err, ErrMeetingNotConnected) {
				return MeetingContent{}, err
			}
			c.logger.Debug("google meet transcript entries unavailable", "transcript", tr.Name, "error", err)
		}
		if text == "" && tr.generated() && tr.FileID != "" {
			if doc, err := c.exportDoc(ctx, token, tr.FileID); err == nil {
				text = doc
			} else if errors.Is(err, ErrMeetingNotConnected) {
				return MeetingContent{}, err
			} else {
				return MeetingContent{}, fmt.Errorf("transcript: %w", err)
			}
		}
		out.Transcript = text
	}
	if meetingScopeBool(c.cfg, "include_summary", true) {
		for i := len(r.Notes) - 1; i >= 0 && out.Summary == ""; i-- {
			n := r.Notes[i]
			if !n.generated() || n.FileID == "" {
				continue
			}
			doc, err := c.exportDoc(ctx, token, n.FileID)
			if err != nil {
				if errors.Is(err, ErrMeetingNotConnected) {
					return MeetingContent{}, err
				}
				c.logger.Debug("google meet notes unavailable", "notes", n.Name, "error", err)
				continue
			}
			out.Summary = doc
		}
	}
	out.Pending = strings.TrimSpace(out.Summary) == "" && strings.TrimSpace(out.Transcript) == ""
	return out, nil
}

// mediaLine is the optional fact line linking the recording in Drive.
func (c *googleMeetConnector) mediaLine(r meetRecord) string {
	if !meetingScopeBool(c.cfg, "include_video_link", false) {
		return ""
	}
	for _, a := range r.Recordings {
		if a.generated() && a.ExportURI != "" {
			return fmt.Sprintf("- **Recording (%s):** %s\n", googleMeetLabel, a.ExportURI)
		}
	}
	return ""
}

// --- Sync ---

// Sync lists conference records since the cursor (minus an overlap window)
// and emits one page per meeting with a recording, transcript or notes. The
// listing is incremental, so unlisted pages are never tombstoned.
func (c *googleMeetConnector) Sync(ctx context.Context, cur Cursor, emit func(Page) error) (Cursor, error) {
	since := meetingSince(c.cfg, cur, c.now())
	reportProgress(ctx, func(p *Progress) { p.Phase = ProgressListing })
	records, err := c.listRecords(ctx, since, meetingScopeInt(c.cfg, "max_recordings", meetingDefaultMaxRecordings))
	if err != nil {
		c.setAuthFromError(err)
		return cur, err
	}
	listed := make(map[string]bool, len(records))
	for _, r := range records {
		listed[r.ID] = true
	}
	for _, id := range meetingPendingIDs(c.vaultDir, c.cfg, listed, googleMeetPendingRefetchCap) {
		r, _, err := c.getRecord(ctx, id)
		if err != nil {
			c.logger.Debug("google meet pending record not re-read", "id", id, "error", err)
			continue
		}
		records = append(records, r)
	}
	total := len(records)
	reportProgress(ctx, func(p *Progress) { p.Phase, p.ItemsTotal = ProgressFetching, total })
	newest := parsePlaudCursor(cur)
	var skipped []string
	for _, r := range records {
		if err := ctx.Err(); err != nil {
			return cur, err
		}
		title := progressTitle(meetingTitle(TypeGoogleMeet, googleMeetLabel, r.MeetingRecording))
		reportProgress(ctx, func(p *Progress) { p.Phase, p.Current = ProgressFetching, title })
		tok, err := c.accessToken(ctx, r.User)
		if err != nil {
			c.setAuthFromError(err)
			return cur, err
		}
		content, err := c.fetchContent(ctx, tok.AccessToken, r)
		if err != nil {
			if errors.Is(err, ErrMeetingNotConnected) {
				c.setAuthFromError(err)
				return cur, err
			}
			skipped = append(skipped, fmt.Sprintf("%s: %v", r.ID, err))
			reportProgress(ctx, func(p *Progress) { p.ItemsDone++ })
			continue
		}
		r.Tags = meetingScopeList(c.cfg, "tags")
		if err := emit(meetingPage(c.cfg, googleMeetAttrPrefix, googleMeetLabel, r.MeetingRecording, content, c.mediaLine(r))); err != nil {
			return cur, err
		}
		if r.StartAt.After(newest) {
			newest = r.StartAt
		}
	}
	c.refreshAuth(ctx)
	next := cur
	if !newest.IsZero() {
		next = Cursor(newest.UTC().Format(time.RFC3339))
	}
	if len(skipped) > 0 {
		return next, &PartialError{Err: fmt.Errorf("google-meet: %d meeting(s) skipped: %s", len(skipped), strings.Join(skipped, "; "))}
	}
	return next, nil
}

// --- MeetingReader / MeetingMediaReader ---

// ListMeetings lists meetings within the since_days window, newest first.
func (c *googleMeetConnector) ListMeetings(ctx context.Context) ([]MeetingRecording, error) {
	since := c.now().Add(-time.Duration(meetingScopeInt(c.cfg, "since_days", meetingDefaultSinceDays)) * 24 * time.Hour)
	records, err := c.listRecords(ctx, since, meetingScopeInt(c.cfg, "max_recordings", meetingDefaultMaxRecordings))
	if err != nil {
		c.setAuthFromError(err)
		return nil, err
	}
	tags := meetingScopeList(c.cfg, "tags")
	out := make([]MeetingRecording, 0, len(records))
	for _, r := range records {
		rec := r.MeetingRecording
		rec.Topic = meetingTitle(TypeGoogleMeet, googleMeetLabel, rec)
		rec.Tags = tags
		out = append(out, rec)
	}
	sortMeetingsNewestFirst(out)
	return out, nil
}

// ReadMeeting reads the transcript, notes and participants of one meeting.
func (c *googleMeetConnector) ReadMeeting(ctx context.Context, id string) (MeetingContent, error) {
	r, tok, err := c.getRecord(ctx, id)
	if err != nil {
		return MeetingContent{}, err
	}
	return c.fetchContent(ctx, tok.AccessToken, r)
}

// MediaLink implements MeetingMediaReader. Meet stores recordings as Drive
// files that Drive serves only behind a bearer token, which a browser media
// element cannot send, so no playable link exists; the error names the
// Drive page instead.
func (c *googleMeetConnector) MediaLink(ctx context.Context, id string) (MediaLink, error) {
	r, _, err := c.getRecord(ctx, id)
	if err != nil {
		return MediaLink{}, err
	}
	for _, a := range r.Recordings {
		if a.generated() {
			where := "open it from the meeting's Drive folder"
			if a.ExportURI != "" {
				where = "open it in Google Drive: " + a.ExportURI
			}
			return MediaLink{}, fmt.Errorf("google-meet: %w — Drive serves Meet recordings behind a bearer token; %s", ErrMeetingNoMedia, where)
		}
	}
	return MediaLink{}, fmt.Errorf("google-meet: %w (transcript or notes only)", ErrMeetingNoMedia)
}

// --- Authenticator / AuthCompleter ---

// AuthStatus implements Authenticator. With refresh it obtains a token and
// probes the conference-records listing.
func (c *googleMeetConnector) AuthStatus(ctx context.Context, refresh bool) AuthState {
	c.mu.Lock()
	st := c.auth
	pending := c.pendingState != "" && c.now().Before(c.pendingAt.Add(googleMeetLoginWindow))
	c.mu.Unlock()
	if pending {
		st.Pending = true
		return st
	}
	st.Pending = false
	if !refresh {
		if st.CheckedAt.IsZero() && c.mode() == AuthModeUserOAuth {
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

// accountLabel describes the account read without any secret.
func (c *googleMeetConnector) accountLabel(tok oauthTokens) string {
	if c.mode() == AuthModeServerToServer {
		users := c.users()
		if len(users) == 1 {
			return users[0]
		}
		return strconv.Itoa(len(users)) + " users (domain-wide delegation)"
	}
	return tok.Account
}

// refreshAuth probes the first user's conference records and caches the
// outcome.
func (c *googleMeetConnector) refreshAuth(ctx context.Context) {
	users := c.users()
	if len(users) == 0 {
		c.setAuthFromError(errors.New("google-meet: scope.users is empty"))
		return
	}
	tok, err := c.accessToken(ctx, users[0])
	if err == nil {
		_, err = c.getJSON(ctx, tok.AccessToken, googleMeetAPIBase+"/conferenceRecords?pageSize=1")
	}
	if err != nil {
		c.setAuthFromError(err)
		return
	}
	c.setAuth(AuthState{Connected: true, CheckedAt: c.now().UTC(), Account: c.accountLabel(tok)})
}

func (c *googleMeetConnector) setAuth(st AuthState) {
	c.mu.Lock()
	c.auth = st
	c.mu.Unlock()
}

func (c *googleMeetConnector) setAuthFromError(err error) {
	c.setAuth(AuthState{CheckedAt: c.now().UTC(), Message: firstLine(err.Error())})
}

// Connect implements Authenticator. For user-level OAuth it returns the
// Google authorization URL (pending until the redirect URL is pasted); for a
// service account it signs an assertion and reports the outcome.
func (c *googleMeetConnector) Connect(ctx context.Context) (AuthState, error) {
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
		return c.AuthStatus(ctx, false), fmt.Errorf("google-meet: generating state: %w", err)
	}
	state := hex.EncodeToString(buf)
	q := url.Values{
		"response_type":          {"code"},
		"client_id":              {id},
		"redirect_uri":           {c.redirectURI()},
		"scope":                  {googleMeetScopes},
		"access_type":            {"offline"},
		"prompt":                 {"consent"},
		"include_granted_scopes": {"true"},
		"state":                  {state},
	}
	c.mu.Lock()
	c.pendingState, c.pendingAt = state, c.now()
	c.auth = AuthState{Pending: true, AuthURL: googleAuthURL + "?" + q.Encode(), CheckedAt: c.now().UTC()}
	st := c.auth
	c.mu.Unlock()
	return st, nil
}

// CompleteAuth implements AuthCompleter: the operator pastes the redirect
// URL the browser landed on; its code is exchanged and the tokens stored.
func (c *googleMeetConnector) CompleteAuth(ctx context.Context, comp AuthCompletion) (AuthState, error) {
	if strings.TrimSpace(comp.Tokens) != "" {
		return c.AuthStatus(ctx, false), errors.New("google-meet: paste the redirect URL the browser landed on; token JSON is not accepted")
	}
	if c.mode() == AuthModeServerToServer {
		return c.AuthStatus(ctx, false), errors.New("google-meet: a service account has no browser sign-in; use Connect")
	}
	u, err := url.Parse(strings.TrimSpace(comp.RedirectURL))
	if err != nil || u.RawQuery == "" {
		return c.AuthStatus(ctx, false), errors.New("google-meet: paste the full URL the browser landed on (it contains ?state=...&code=...)")
	}
	if e := u.Query().Get("error"); e != "" {
		return c.AuthStatus(ctx, false), fmt.Errorf("google-meet: Google refused the sign-in: %s", e)
	}
	code := u.Query().Get("code")
	if code == "" {
		return c.AuthStatus(ctx, false), errors.New("google-meet: the pasted URL has no code parameter")
	}
	c.mu.Lock()
	state, at := c.pendingState, c.pendingAt
	c.mu.Unlock()
	if state == "" || c.now().After(at.Add(googleMeetLoginWindow)) {
		return c.AuthStatus(ctx, false), errors.New("google-meet: no sign-in is pending; press Connect first, then paste the redirect URL within the login window")
	}
	if got := u.Query().Get("state"); got != "" && got != state {
		return c.AuthStatus(ctx, false), errors.New("google-meet: the pasted URL belongs to a different sign-in attempt (state mismatch); press Connect again")
	}
	id, secret, err := c.clientCredentials()
	if err != nil {
		return c.AuthStatus(ctx, false), err
	}
	tok, err := c.tokenRequest(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {c.redirectURI()}, "client_id": {id}, "client_secret": {secret}})
	if err != nil {
		return c.AuthStatus(ctx, false), err
	}
	if tok.RefreshToken == "" {
		return c.AuthStatus(ctx, false), errors.New("google-meet: Google returned no refresh token; revoke the app at myaccount.google.com/permissions and Connect again")
	}
	if err := c.store.save(tok); err != nil {
		return c.AuthStatus(ctx, false), fmt.Errorf("google-meet: storing tokens: %w", err)
	}
	c.mu.Lock()
	c.pendingState = ""
	c.mu.Unlock()
	c.refreshAuth(ctx)
	return c.AuthStatus(ctx, false), nil
}

// Disconnect implements Authenticator: revokes the user grant upstream (best
// effort) and clears the token store or the cached service-account tokens.
func (c *googleMeetConnector) Disconnect(ctx context.Context) (AuthState, error) {
	c.mu.Lock()
	c.pendingState = ""
	c.saTokens = map[string]oauthTokens{}
	c.mu.Unlock()
	if c.mode() == AuthModeUserOAuth {
		if tok, err := c.store.load(); err == nil && (tok.RefreshToken != "" || tok.AccessToken != "") {
			revoke := tok.RefreshToken
			if revoke == "" {
				revoke = tok.AccessToken
			}
			h := http.Header{}
			h.Set("Content-Type", "application/x-www-form-urlencoded")
			if _, err := c.http.Do(ctx, http.MethodPost, googleRevokeURL, h, []byte(url.Values{"token": {revoke}}.Encode())); err != nil {
				c.logger.Debug("google meet token revoke failed", "error", err)
			}
		}
		if err := c.store.clear(); err != nil {
			return c.AuthStatus(ctx, false), fmt.Errorf("google-meet: %w", err)
		}
	}
	st := AuthState{CheckedAt: c.now().UTC(), Message: "signed out"}
	c.setAuth(st)
	return st, nil
}
