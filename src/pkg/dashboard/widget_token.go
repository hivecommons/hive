package dashboard

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/hivecommons/hive/pkg/config"
)

// Read-only widget token (#12147).
//
// The owner mints one token per hive from the dashboard's iPhone widget dialog.
// It authenticates exactly one request shape — GET widgetSummaryPath with the
// token in widgetTokenHeader — and nothing else: authenticate admits it only on
// that path, as a read-role identity, so it can never reach a mutation route or
// act as a session. Only the SHA-256 of the token is stored; the plaintext is
// shown once, at mint time.
//
// The token travels in a dedicated header rather than Authorization so a hosted
// hive's hub auth-check (which treats any Authorization bearer as a GitHub
// token and asks github.com about it) never sees or forwards it.
const (
	widgetSummaryPath = "/api/widget/summary"
	widgetTokenHeader = "X-Hive-Widget-Token"
	widgetTokenPrefix = "hwt_"
	widgetTokenBytes  = 32
	widgetTokenActor  = "widget-token"
	widgetScriptName  = "Hive"
	widgetQRSize      = 256
)

// widgetTokenTouchInterval throttles last-used bookkeeping so a widget polling
// every few minutes does not rewrite the record on every request.
const widgetTokenTouchInterval = 10 * time.Minute

// widgetTokenPath is a package var so tests can point it at a temp dir.
var widgetTokenPath = "/data/widget-token.json"

var widgetTokenMu sync.Mutex

type widgetTokenRecord struct {
	Hash       string `json:"hash"`
	CreatedAt  string `json:"createdAt"`
	CreatedBy  string `json:"createdBy,omitempty"`
	LastUsedAt string `json:"lastUsedAt,omitempty"`
}

func widgetTokenFile() string {
	return widgetTokenPath
}

func loadWidgetTokenRecord() (*widgetTokenRecord, error) {
	data, err := os.ReadFile(widgetTokenFile())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var rec widgetTokenRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("parse widget token record: %w", err)
	}
	if rec.Hash == "" {
		return nil, nil
	}
	return &rec, nil
}

func saveWidgetTokenRecord(rec *widgetTokenRecord) error {
	path := widgetTokenFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// mintWidgetToken replaces any existing widget token and returns the new
// plaintext. The previous token stops working immediately.
func mintWidgetToken(createdBy string, now time.Time) (string, *widgetTokenRecord, error) {
	token := widgetTokenPrefix + randomHex(widgetTokenBytes)
	rec := &widgetTokenRecord{
		Hash:      sha256Hex(token),
		CreatedAt: now.UTC().Format(time.RFC3339),
		CreatedBy: createdBy,
	}
	widgetTokenMu.Lock()
	defer widgetTokenMu.Unlock()
	if err := saveWidgetTokenRecord(rec); err != nil {
		return "", nil, err
	}
	return token, rec, nil
}

// revokeWidgetToken deletes the stored token. Idempotent.
func revokeWidgetToken() (bool, error) {
	widgetTokenMu.Lock()
	defer widgetTokenMu.Unlock()
	if err := os.Remove(widgetTokenFile()); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// validWidgetToken reports whether token matches the stored widget token.
// Fails closed on any read/parse error.
func validWidgetToken(token string) bool {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, widgetTokenPrefix) || len(token) <= len(widgetTokenPrefix) {
		return false
	}
	rec, err := loadWidgetTokenRecord()
	if err != nil || rec == nil {
		return false
	}
	hash := sha256Hex(token)
	if !secureCompare(hash, rec.Hash) {
		return false
	}
	touchWidgetToken(hash, time.Now())
	return true
}

// touchWidgetToken records when the token was last used, so a stale token is
// obvious in the dashboard. Best-effort and throttled. It re-reads the record
// under the lock and only writes if the same token is still current, so it can
// never resurrect a token revoked or replaced concurrently.
func touchWidgetToken(hash string, now time.Time) {
	widgetTokenMu.Lock()
	defer widgetTokenMu.Unlock()
	rec, err := loadWidgetTokenRecord()
	if err != nil || rec == nil || !secureCompare(hash, rec.Hash) {
		return
	}
	if last, err := time.Parse(time.RFC3339, rec.LastUsedAt); err == nil && now.Sub(last) < widgetTokenTouchInterval {
		return
	}
	rec.LastUsedAt = now.UTC().Format(time.RFC3339)
	_ = saveWidgetTokenRecord(rec)
}

// widgetTokenAuthenticates is authenticate's hook: true only for a GET/HEAD of
// the widget summary path carrying a valid widget token.
func widgetTokenAuthenticates(r *http.Request) bool {
	if r.URL.Path != widgetSummaryPath {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return validWidgetToken(r.Header.Get(widgetTokenHeader))
}

type widgetSummaryAgent struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// handleWidgetSummary serves the phone widget everything it renders in one
// read-only call: the eval history the sparklines are drawn from and a
// minimal agent list (name + state) for the running/total footer.
func (s *Server) handleWidgetSummary(w http.ResponseWriter, r *http.Request) {
	hiveID := ""
	if s.deps != nil && s.deps.Config != nil {
		hiveID = s.deps.Config.HiveID
	}

	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()

	agents := []widgetSummaryAgent{}
	if status != nil {
		for _, a := range status.Agents {
			agents = append(agents, widgetSummaryAgent{Name: a.Name, State: a.State})
		}
	}

	var history any = []any{}
	if s.deps != nil && s.deps.Governor != nil {
		history = s.historyPayload()
	}

	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, map[string]any{
		"hiveId":  hiveID,
		"history": history,
		"agents":  agents,
	})
}

func (s *Server) handleWidgetTokenStatus(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	rec, err := loadWidgetTokenRecord()
	if err != nil {
		jsonError(w, "could not read widget token state", http.StatusInternalServerError)
		return
	}
	resp := map[string]any{"configured": rec != nil}
	if rec != nil {
		resp["createdAt"] = rec.CreatedAt
		resp["createdBy"] = rec.CreatedBy
		resp["lastUsedAt"] = rec.LastUsedAt
	}
	jsonResponse(w, resp)
}

func (s *Server) handleWidgetTokenMint(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var body struct {
		HiveURL string `json:"hiveUrl"`
	}
	if err := decodeBody(r, &body); err != nil && !errors.Is(err, io.EOF) {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	hiveURL := widgetHiveURL(r, body.HiveURL)

	user := r.Header.Get("X-Hive-User")
	token, rec, err := mintWidgetToken(user, time.Now())
	if err != nil {
		s.logger.Warn("widget token: persist failed", "error", err)
		jsonError(w, "could not store the widget token", http.StatusInternalServerError)
		return
	}

	setupURL := widgetSetupURL(hiveURL, token)
	qrDataURL := ""
	if png, err := qrcode.Encode(setupURL, qrcode.Medium, widgetQRSize); err == nil {
		qrDataURL = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	}

	s.auditFromRequest(r, "widget_token_mint", "", "")
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, map[string]any{
		"ok":        true,
		"token":     token,
		"hiveUrl":   hiveURL,
		"setupUrl":  setupURL,
		"qrDataUrl": qrDataURL,
		"createdAt": rec.CreatedAt,
	})
}

func (s *Server) handleWidgetTokenRevoke(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	removed, err := revokeWidgetToken()
	if err != nil {
		s.logger.Warn("widget token: revoke failed", "error", err)
		jsonError(w, "could not revoke the widget token", http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "widget_token_revoke", "", "")
	jsonResponse(w, map[string]any{"ok": true, "revoked": removed})
}

// widgetHiveURL picks the hive origin the phone should call: the origin the
// dashboard reports for itself when it is a plain http(s) origin, otherwise
// one derived from the request.
func widgetHiveURL(r *http.Request, claimed string) string {
	if u, err := url.Parse(strings.TrimSpace(claimed)); err == nil &&
		(u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil {
		return u.Scheme + "://" + u.Host
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// widgetSetupURL is the Scriptable deep link the QR encodes. Scanning it opens
// the Hive script on the phone, which stores the URL and token in the iOS
// Keychain. The custom scheme means the token is never sent to any server.
func widgetSetupURL(hiveURL, token string) string {
	q := url.Values{}
	q.Set("url", hiveURL)
	q.Set("widgetToken", token)
	return "scriptable:///run/" + url.PathEscape(widgetScriptName) + "?" + q.Encode()
}

// widgetTokenIdentity marks a request authenticated by the widget token with a
// read-only identity that requireOwnerRole and the read-only write gate both
// refuse for anything but reads.
func widgetTokenIdentity(r *http.Request) {
	r.Header.Set("X-Hive-User", widgetTokenActor)
	r.Header.Set("X-Hive-Role", config.RoleRead)
}
