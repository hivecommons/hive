package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/sharedfile"
)

// Codex and OpenCode logins in a session HOME, adopted back into the shared
// login when a session refreshed them (#18740, a follow-up to #18724).
//
// A session gets a plain copy of these two files (userSessionLoginFiles).
// Both can hold a ROTATING refresh token: Codex's ChatGPT login, and an
// OpenCode provider entry of type "oauth". When the CLI in a session refreshes
// its copy, the refresh rotates the token, the session's copy becomes the
// only valid login, and the shared file every agent uses is left stale. The
// agents link to that shared file (Codex: each agent's CODEX_HOME/auth.json
// is a symlink to it, setupCodexHome; OpenCode: each agent's
// $XDG_DATA_HOME/opencode is a symlink to the shared directory,
// xdgDataSharedBridges), so the shared file is the single owner of each
// login, and the CLIs of the agents write their own refreshes into it.
//
// The fix here is ADOPT-BACK, the same safety net usersession_login.go has for
// Claude: when a session's file holds a strictly newer login than the shared
// file, that login is written into the shared file atomically, the replaced
// shared file is kept beside it as "<name>.stale-<UTC time>", and
// user_session_login_adopted is audited. It runs at the same points as the
// Claude check: at boot, on every credential-bridge poll, at a session's
// start or resume, and before a session's sandbox is removed (a login that
// cannot be adopted keeps the sandbox).
//
// What a session is NOT given here, unlike Claude, is a copy that cannot
// refresh. Whether each CLI accepts a login file without its refresh token
// (Codex may reject one) has not been verified against the CLIs, so the
// session keeps a full copy and stays a refresh holder. Adopt-back makes the
// fleet converge on the newest login within one poll; it does not stop a
// session and an agent refreshing the same token in the same few seconds.
//
// "Strictly newer" is decided from the files' own freshness fields and
// modification times only:
//
//   - Codex: the session's file was written after the shared one, holds a
//     complete ChatGPT login (access and refresh token), records a
//     last_refresh later than the shared file's, and its refresh token
//     differs from the shared one. The shared file must itself hold a ChatGPT
//     login: an API-key login does not rotate and is never replaced.
//   - OpenCode, per provider entry: the session's file was written after the
//     shared one, both entries are "oauth" with a refresh token, the
//     session's records a later "expires", and its refresh token differs.
//     Only those entries are replaced; every other entry of the shared file
//     is kept as it is, and an entry the shared file does not have is not
//     added.
//
// Token values are compared only for equality, in memory, and never logged,
// audited or put in an error.

// The rotating entries of userSessionLoginFiles.
const (
	userSessionCodexLoginRel    = ".codex/auth.json"
	userSessionOpenCodeLoginRel = ".local/share/opencode/auth.json"
)

// rotatingSessionLogin is one vendor login a session holds as a full copy
// whose refresh token rotates.
type rotatingSessionLogin struct {
	rel    string // relative to HOME, in the session and in the shared home
	vendor string // for logs and the audit detail
	// adopt returns the new content of the shared file when sess holds a
	// strictly newer login than shared, nil when there is nothing to adopt.
	// It never logs and never returns token material in an error.
	adopt func(sess, shared []byte, sessMod, sharedMod time.Time) []byte
}

var rotatingSessionLogins = []rotatingSessionLogin{
	{rel: userSessionCodexLoginRel, vendor: "codex", adopt: adoptCodexLogin},
	{rel: userSessionOpenCodeLoginRel, vendor: "opencode", adopt: adoptOpenCodeLogin},
}

// rotatingSessionLoginFor returns the rotating login stored at rel.
func rotatingSessionLoginFor(rel string) (rotatingSessionLogin, bool) {
	for _, l := range rotatingSessionLogins {
		if l.rel == rel {
			return l, true
		}
	}
	return rotatingSessionLogin{}, false
}

// sessionRotatingLoginMu serialises every read-compare-write of a rotating
// shared login made by the hive (the manager's poll, a session's start and
// its removal), so two sessions holding newer logins settle on the later one.
// The agents' CLIs write the shared file without it; replaceSharedLogin
// refuses to replace a shared file that changed after it was judged.
var sessionRotatingLoginMu sync.Mutex

// errSharedLoginChanged: the shared file changed between the judgement and
// the write. Nothing was written; the next check judges again.
var errSharedLoginChanged = errors.New("the shared login changed while it was being compared; nothing was replaced")

// ---- Codex

// codexLoginFile is the part of Codex's auth.json the judgement reads.
type codexLoginFile struct {
	Tokens *struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	} `json:"tokens"`
	LastRefresh string `json:"last_refresh"`
}

// parseCodexLogin returns the refresh token and the last refresh time of a
// Codex login file. ok is false when data is not a login file with a ChatGPT
// refresh token; last is zero when the file records no (valid) refresh time.
func parseCodexLogin(data []byte) (access, refresh string, last time.Time, ok bool) {
	var f codexLoginFile
	if err := json.Unmarshal(data, &f); err != nil || f.Tokens == nil || f.Tokens.RefreshToken == "" {
		return "", "", time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, f.LastRefresh); err == nil {
		last = t
	}
	return f.Tokens.AccessToken, f.Tokens.RefreshToken, last, true
}

// codexLoginKeys are the top-level keys of a Codex login file that make up
// the ChatGPT login; an adoption takes exactly these from the session's file.
var codexLoginKeys = []string{"tokens", "last_refresh", "auth_mode"}

// adoptCodexLogin implements rotatingSessionLogin.adopt for Codex: the
// shared file with the session's ChatGPT login in place of its own, or nil.
func adoptCodexLogin(sess, shared []byte, sessMod, sharedMod time.Time) []byte {
	if !sessMod.After(sharedMod) {
		return nil
	}
	sAccess, sRefresh, sLast, ok := parseCodexLogin(sess)
	if !ok || sAccess == "" || sLast.IsZero() {
		return nil
	}
	_, hRefresh, hLast, ok := parseCodexLogin(shared)
	if !ok || sRefresh == hRefresh || !sLast.After(hLast) {
		return nil
	}
	var sessTop, sharedTop map[string]json.RawMessage
	if json.Unmarshal(sess, &sessTop) != nil || json.Unmarshal(shared, &sharedTop) != nil || sharedTop == nil {
		return nil
	}
	for _, k := range codexLoginKeys {
		if v, ok := sessTop[k]; ok {
			sharedTop[k] = v
		}
	}
	out, err := json.MarshalIndent(sharedTop, "", "  ")
	if err != nil {
		return nil
	}
	return append(out, '\n')
}

// ---- OpenCode

// openCodeAuthEntry is the part of one OpenCode auth.json provider entry the
// judgement reads.
type openCodeAuthEntry struct {
	Type    string `json:"type"`
	Access  string `json:"access"`
	Refresh string `json:"refresh"`
	Expires int64  `json:"expires"`
}

// openCodeOAuthEntry parses one provider entry; ok is false unless it is an
// "oauth" entry with a refresh token.
func openCodeOAuthEntry(raw json.RawMessage) (openCodeAuthEntry, bool) {
	var e openCodeAuthEntry
	if err := json.Unmarshal(raw, &e); err != nil || e.Type != "oauth" || e.Refresh == "" {
		return openCodeAuthEntry{}, false
	}
	return e, true
}

// adoptOpenCodeLogin implements rotatingSessionLogin.adopt for OpenCode: the
// shared file with every provider entry the session holds strictly newer
// replaced by the session's, or nil when there is none.
func adoptOpenCodeLogin(sess, shared []byte, sessMod, sharedMod time.Time) []byte {
	if !sessMod.After(sharedMod) {
		return nil
	}
	var sessTop, sharedTop map[string]json.RawMessage
	if json.Unmarshal(sess, &sessTop) != nil || json.Unmarshal(shared, &sharedTop) != nil || sharedTop == nil {
		return nil
	}
	changed := false
	for provider, sharedRaw := range sharedTop {
		h, ok := openCodeOAuthEntry(sharedRaw)
		if !ok {
			continue
		}
		s, ok := openCodeOAuthEntry(sessTop[provider])
		if !ok || s.Access == "" || s.Refresh == h.Refresh || s.Expires <= h.Expires {
			continue
		}
		sharedTop[provider] = sessTop[provider]
		changed = true
	}
	if !changed {
		return nil
	}
	out, err := json.MarshalIndent(sharedTop, "", "  ")
	if err != nil {
		return nil
	}
	return out
}

// ---- the check

// rotatingLoginKey is the sessionLoginCache key of one session's rotating
// login; the Claude check keys the same cache by the bare session id.
func rotatingLoginKey(id, rel string) string { return id + "\x00" + rel }

// reconcileRotatingLogin adopts a strictly newer login of one vendor from a
// session HOME into the shared file. The session's copy is never written:
// after an adoption it holds the same login as the shared file. The caller
// holds sessionRotatingLoginMu.
func (o sessionLoginOps) reconcileRotatingLogin(id string, l rotatingSessionLogin) (sessionLoginResult, error) {
	var none sessionLoginResult
	home := userSessionHome(id)
	path := filepath.Join(home, l.rel)
	target := filepath.Join(sharedAgentHome, l.rel)

	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return none, nil
	}
	if err != nil {
		return none, fmt.Errorf("inspecting the session's %s login: %w", l.vendor, err)
	}
	if !info.Mode().IsRegular() {
		return none, nil
	}
	sharedInfo, err := os.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		// No shared login to split from.
		return none, nil
	}
	if err != nil {
		return none, fmt.Errorf("inspecting the shared %s login: %w", l.vendor, err)
	}
	if !info.ModTime().After(sharedInfo.ModTime()) {
		// Not written after the shared file: an unchanged copy, or one an
		// adoption already settled. Nothing is read.
		return none, nil
	}
	if !sharedInfo.Mode().IsRegular() {
		// The session's file may hold the newest login, and the shared one
		// is nothing hive could compare or replace: report it, so a removal
		// keeps the session.
		return none, fmt.Errorf("%w: the shared %s login %s is not a regular file", sharedfile.ErrUnsafeLocation, l.vendor, target)
	}
	data, err := o.readSessionLogin(home, path)
	if errors.Is(err, fs.ErrNotExist) {
		return none, nil
	}
	if err != nil {
		return none, err
	}
	shared, err := sharedfile.ReadNoFollow(target, claudeBridgeMaxCredentialBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return none, nil
	}
	if err != nil {
		return none, fmt.Errorf("reading the shared %s login: %w", l.vendor, err)
	}
	merged := l.adopt(data, shared, info.ModTime(), sharedInfo.ModTime())
	if merged == nil {
		return none, nil
	}
	stale, err := replaceSharedLogin(target, merged, sharedInfo, o.now())
	if err != nil {
		return none, fmt.Errorf("%w (%s); adopting it failed: %w", crux.ErrUserSessionHoldsLogin, l.vendor, err)
	}
	return sessionLoginResult{action: sessionLoginAdopted, staleShared: stale}, nil
}

// replaceSharedLogin writes data over the shared login target, keeping the
// previous file beside it, provided target is still the file that was judged
// (same modification time and size). The new file keeps the old one's mode
// and, where the hive may, its owner, so every agent reaches it as before.
func replaceSharedLogin(target string, data []byte, judged os.FileInfo, now time.Time) (string, error) {
	info, err := sharedfile.CheckLocation(target)
	if err != nil {
		return "", err
	}
	if info == nil || !info.ModTime().Equal(judged.ModTime()) || info.Size() != judged.Size() {
		return "", errSharedLoginChanged
	}
	stale, err := preserveClaudeCredential(target, now)
	if err != nil {
		return "", fmt.Errorf("preserve shared login: %w", err)
	}
	if err := sharedfile.Replace(target, data, info.Mode().Perm(), sharedfile.Options{PreserveOwner: true}); err != nil {
		return stale, err
	}
	return stale, nil
}

// checkRotatingLogin runs one check under sessionRotatingLoginMu.
func (o sessionLoginOps) checkRotatingLogin(id string, l rotatingSessionLogin) (sessionLoginResult, error) {
	sessionRotatingLoginMu.Lock()
	defer sessionRotatingLoginMu.Unlock()
	return o.reconcileRotatingLogin(id, l)
}

// reportRotatingLogin logs and audits one check. Only an adoption is
// audited. The fields are the session id, the trigger, the vendor and file
// paths; never token material.
func reportRotatingLogin(logger *slog.Logger, audit func(action string, fields map[string]any), id, trigger string, l rotatingSessionLogin, res sessionLoginResult, err error) {
	if res.action == sessionLoginAdopted {
		logger.Warn("user session login: the session held a newer login than the shared file; adopted it into the shared login",
			"session", id, "trigger", trigger, "vendor", l.vendor, "login", l.rel, "stale_shared_file", res.staleShared)
		if audit != nil {
			audit(AuditUserSessionLoginAdopted, auditFields(
				"outcome", "success",
				"session", id,
				"trigger", trigger,
				"vendor", l.vendor,
				"login", l.rel,
				"stale_shared_file", res.staleShared,
			))
		}
	}
	if err != nil {
		logger.Warn("user session login: check failed", "session", id, "trigger", trigger, "vendor", l.vendor, "error", err)
	}
}

// syncRotatingLogin checks one session's login of one vendor and reports it.
func (r *UserSessionRuntime) syncRotatingLogin(id, trigger string, l rotatingSessionLogin) error {
	res, err := r.loginOps().checkRotatingLogin(id, l)
	reportRotatingLogin(r.logger, r.loginAudit(), id, trigger, l, res, err)
	return err
}

// syncUserSessionRotatingLogins checks every rotating login of one session
// sandbox from the manager's sweep (syncUserSessionClaudeLogins), marking the
// cache keys it saw in present.
func (m *Manager) syncUserSessionRotatingLogins(id, trigger string, ops sessionLoginOps, force bool, present map[string]bool) {
	for _, l := range rotatingSessionLogins {
		path := filepath.Join(userSessionHome(id), l.rel)
		target := filepath.Join(sharedAgentHome, l.rel)
		stamp, ok := sessionLoginStampOf(path, target)
		if !ok {
			continue
		}
		key := rotatingLoginKey(id, l.rel)
		present[key] = true
		now := ops.now()
		if !m.sessionLogins.due(key, stamp, now, force) {
			continue
		}
		res, err := ops.checkRotatingLogin(id, l)
		reportRotatingLogin(m.logger, func(action string, fields map[string]any) { m.audit(action, "", fields) }, id, trigger, l, res, err)
		if after, aok := sessionLoginStampOf(path, target); aok {
			m.sessionLogins.record(key, after, now)
		}
	}
}
