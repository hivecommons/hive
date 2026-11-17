package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Desktop companion tokens (hivecommons/hive#18596).
//
// The companion app (hivecommons/quorum-companion) has no browser session, so
// each owner or read-write user mints one companion token for themselves on
// the /companion/token page. The token:
//
//   - travels in companionTokenHeader (never Authorization, so a hosted hive's
//     hub auth-check never treats it as a GitHub token);
//   - is admitted by authenticate only on the companion meeting routes
//     (companionTokenAdmits), never on the token page or any other route;
//   - acts as the user who minted it at their LIVE role (liveAllowlistRole,
//     the rule sessions use), so a revoked or downgraded user stops working at
//     once, and a role below read-write is refused;
//   - is stored only as its SHA-256, one per user; minting again replaces it,
//     revoking deletes it. The plaintext is shown once.
const (
	companionTokenHeader = "X-Hive-Companion-Token"
	companionTokenPrefix = "hct_"
	companionTokenBytes  = 32
	// companionTokenTouchInterval throttles last-used bookkeeping.
	companionTokenTouchInterval = 10 * time.Minute
	companionTokenPagePath      = "/companion/token"
)

// companionTokenPath is a package var so tests can point it at a temp dir.
var companionTokenPath = "/data/companion-tokens.json"

var companionTokenMu sync.Mutex

type companionTokenRecord struct {
	Hash       string `json:"hash"`
	Role       string `json:"role"`
	CreatedAt  string `json:"createdAt"`
	LastUsedAt string `json:"lastUsedAt,omitempty"`
}

type companionTokenFile struct {
	Tokens map[string]companionTokenRecord `json:"tokens"`
}

func loadCompanionTokens() (companionTokenFile, error) {
	out := companionTokenFile{Tokens: map[string]companionTokenRecord{}}
	data, err := os.ReadFile(companionTokenPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("parse companion tokens: %w", err)
	}
	if out.Tokens == nil {
		out.Tokens = map[string]companionTokenRecord{}
	}
	return out, nil
}

func saveCompanionTokens(f companionTokenFile) error {
	if err := os.MkdirAll(filepath.Dir(companionTokenPath), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp := companionTokenPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, companionTokenPath)
}

// mintCompanionToken replaces user's token and returns the new plaintext.
func mintCompanionToken(user, role string, now time.Time) (string, error) {
	token := companionTokenPrefix + randomHex(companionTokenBytes)
	companionTokenMu.Lock()
	defer companionTokenMu.Unlock()
	f, err := loadCompanionTokens()
	if err != nil {
		return "", err
	}
	f.Tokens[user] = companionTokenRecord{Hash: sha256Hex(token), Role: role, CreatedAt: now.UTC().Format(time.RFC3339)}
	if err := saveCompanionTokens(f); err != nil {
		return "", err
	}
	return token, nil
}

// revokeCompanionToken deletes user's token; it reports whether one existed.
func revokeCompanionToken(user string) (bool, error) {
	companionTokenMu.Lock()
	defer companionTokenMu.Unlock()
	f, err := loadCompanionTokens()
	if err != nil {
		return false, err
	}
	if _, ok := f.Tokens[user]; !ok {
		return false, nil
	}
	delete(f.Tokens, user)
	return true, saveCompanionTokens(f)
}

// companionTokenStatus is user's token record, if any.
func companionTokenStatus(user string) (companionTokenRecord, bool) {
	f, err := loadCompanionTokens()
	if err != nil {
		return companionTokenRecord{}, false
	}
	rec, ok := f.Tokens[user]
	return rec, ok
}

// companionTokenOwner returns the user and minted role of a valid token.
// Fails closed on any read or parse error.
func companionTokenOwner(token string, now time.Time) (user, role string, ok bool) {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, companionTokenPrefix) || len(token) <= len(companionTokenPrefix) {
		return "", "", false
	}
	hash := sha256Hex(token)
	companionTokenMu.Lock()
	defer companionTokenMu.Unlock()
	f, err := loadCompanionTokens()
	if err != nil {
		return "", "", false
	}
	for u, rec := range f.Tokens {
		if !secureCompare(hash, rec.Hash) {
			continue
		}
		if last, err := time.Parse(time.RFC3339, rec.LastUsedAt); err != nil || now.Sub(last) >= companionTokenTouchInterval {
			rec.LastUsedAt = now.UTC().Format(time.RFC3339)
			f.Tokens[u] = rec
			_ = saveCompanionTokens(f)
		}
		return u, rec.Role, true
	}
	return "", "", false
}

// companionTokenAdmits lists the only paths a companion token may reach.
func companionTokenAdmits(path string) bool {
	return path == "/api/companion/info" || path == "/api/companion/meetings" ||
		strings.HasPrefix(path, "/api/companion/meetings/")
}

// companionTokenAuthenticates is authenticate's hook: on a companion route
// with a valid token it injects the minting user's identity at their live
// role and reports true. A user whose live role is below read-write (or who
// was revoked) is not authenticated.
func (s *Server) companionTokenAuthenticates(r *http.Request) bool {
	if !companionTokenAdmits(r.URL.Path) {
		return false
	}
	user, minted, ok := companionTokenOwner(r.Header.Get(companionTokenHeader), time.Now())
	if !ok {
		return false
	}
	role, ok := s.liveAllowlistRole(user, minted)
	if !ok || !config.RoleAtLeast(role, config.RoleReadWrite) {
		return false
	}
	r.Header.Set("X-Hive-User", user)
	r.Header.Set("X-Hive-Role", role)
	if isOwnerRole(role) {
		r.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	return true
}

func (s *Server) registerCompanionTokenRoutes() {
	s.mux.HandleFunc("GET "+companionTokenPagePath, s.handleCompanionTokenPage)
	s.mux.HandleFunc("POST "+companionTokenPagePath, s.handleCompanionTokenMint)
	s.mux.HandleFunc("POST "+companionTokenPagePath+"/revoke", s.handleCompanionTokenRevoke)
}

var companionTokenPage = template.Must(template.New("companion-token").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Companion token</title>
<style>
body{font:15px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;max-width:640px;margin:32px auto;padding:0 16px;color:#1f1a10;background:#fffdf7}
@media (prefers-color-scheme: dark){body{color:#f3ecdc;background:#17140e}}
code,pre{font-family:ui-monospace,Menlo,monospace}pre{padding:12px;border:1px solid #c98a00;border-radius:8px;white-space:pre-wrap;word-break:break-all}
button{padding:6px 14px;border-radius:6px;border:1px solid #c98a00;background:#c98a00;color:#fff;cursor:pointer}
form{display:inline-block;margin-right:8px}.muted{opacity:.75;font-size:13px}
</style></head><body>
<h1>Desktop companion token</h1>
<p>The <a href="https://github.com/hivecommons/quorum-companion">Quorum Companion</a> app lets Betty listen to a call through your own meeting app, after you confirm the participants were told. It signs in to this hive with a companion token: it works only for companion meetings, acts as you ({{.User}}), and stops working if your access is removed.</p>
{{if .Token}}<p><strong>Your new token</strong> (shown once; paste it into the companion's Settings with this hive's URL):</p>
<pre>{{.Token}}</pre>
<p class="muted">Hive URL: <code>{{.HiveURL}}</code></p>{{end}}
{{if .Revoked}}<p>Your companion token was revoked. Companions using it stop working now.</p>{{end}}
{{if .Has}}<p class="muted">You have a token, created {{.CreatedAt}}{{if .LastUsedAt}}, last used {{.LastUsedAt}}{{end}}.</p>{{else if not .Token}}<p class="muted">You have no companion token.</p>{{end}}
<form method="post" action="/companion/token"><button type="submit">{{if .Has}}Replace my token{{else}}Create my token{{end}}</button></form>
{{if .Has}}<form method="post" action="/companion/token/revoke"><button type="submit">Revoke my token</button></form>{{end}}
</body></html>`))

type companionTokenView struct {
	User, Token, HiveURL, CreatedAt, LastUsedAt string
	Has, Revoked                                bool
}

func (s *Server) renderCompanionTokenPage(w http.ResponseWriter, r *http.Request, v companionTokenView) {
	v.User = requestUser(r)
	if rec, ok := companionTokenStatus(v.User); ok {
		v.Has, v.CreatedAt, v.LastUsedAt = true, rec.CreatedAt, rec.LastUsedAt
	}
	if v.HiveURL == "" {
		v.HiveURL = widgetHiveURL(r, "")
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := companionTokenPage.Execute(w, v); err != nil {
		s.logger.Warn("companion token page", "error", err)
	}
}

// companionTokenForm admits owner or read-write and refuses cross-site posts.
func companionTokenForm(w http.ResponseWriter, r *http.Request) bool {
	if !requireInboxPlaybackRole(w, r) {
		return false
	}
	if r.Method == http.MethodPost && r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		jsonError(w, "cross-site request refused", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) handleCompanionTokenPage(w http.ResponseWriter, r *http.Request) {
	if !companionTokenForm(w, r) {
		return
	}
	s.renderCompanionTokenPage(w, r, companionTokenView{})
}

func (s *Server) handleCompanionTokenMint(w http.ResponseWriter, r *http.Request) {
	if !companionTokenForm(w, r) {
		return
	}
	token, err := mintCompanionToken(requestUser(r), r.Header.Get("X-Hive-Role"), time.Now())
	if err != nil {
		s.logger.Warn("companion token: persist failed", "error", err)
		jsonError(w, "could not store the companion token", http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "companion_token_mint", "", "")
	s.renderCompanionTokenPage(w, r, companionTokenView{Token: token})
}

func (s *Server) handleCompanionTokenRevoke(w http.ResponseWriter, r *http.Request) {
	if !companionTokenForm(w, r) {
		return
	}
	removed, err := revokeCompanionToken(requestUser(r))
	if err != nil {
		jsonError(w, "could not revoke the companion token", http.StatusInternalServerError)
		return
	}
	if removed {
		s.auditFromRequest(r, "companion_token_revoke", "", "")
	}
	s.renderCompanionTokenPage(w, r, companionTokenView{Revoked: removed})
}
