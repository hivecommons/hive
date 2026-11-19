package agent

import (
	"bytes"
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

// Codex and OpenCode logins in a session HOME: what a session is given, and
// how a login that still ends up there gets back to the shared login (#18740,
// a follow-up to #18724).
//
// Both files can hold a ROTATING refresh token: Codex's ChatGPT login, and an
// OpenCode provider entry of type "oauth". The agents link to the shared file
// (Codex: each agent's CODEX_HOME/auth.json is a symlink to it,
// setupCodexHome; OpenCode: each agent's $XDG_DATA_HOME/opencode is a symlink
// to the shared directory, xdgDataSharedBridges), so the shared file is the
// single owner of each login, and the CLIs of the agents write their own
// refreshes into it. A session used to get a full copy: when the CLI in the
// session refreshed it, the refresh rotated the token, the session's copy
// became a second, diverging holder of the grant, and a session refresh and
// an agent refresh in the same window left two logins of which at most one
// still worked.
//
// The same three rules as the Claude login (usersession_login.go):
//
//  1. A session is never a refresh holder. Its copy is the shared file with
//     every refresh token EMPTIED (sessionCodexLogin, sessionOpenCodeLogin).
//     The key stays: both CLIs require it in their login file, but accept an
//     empty value; the CLI then keeps using the access token and a refresh it
//     attempts fails without rotating anything. Nothing else changes, so the
//     session sees the fleet's account, mode and API-key entries as before.
//  2. The session's copy follows the shared login. When the shared file was
//     written after the copy (an agent refreshed, an operator signed in), the
//     copy is replaced, atomically and as the session's uid. A session
//     therefore depends on an agent or a sign-in refreshing the shared login
//     before its access token expires; it cannot refresh it itself.
//  3. A login that is in a session HOME anyway (a full copy from an earlier
//     release, refreshed there, or a person signing in inside the session) is
//     never lost and never left to diverge: when it is strictly newer than the
//     shared file it is ADOPTED into it atomically, the replaced shared file
//     is kept beside it as "<name>.stale-<UTC time>", and
//     user_session_login_adopted is audited. The session is then put back on
//     a refresh-less copy. A full copy carrying the shared file's own refresh
//     token is put on a refresh-less copy too. A refresh token the shared
//     file does not hold and that is not adoptable is the session's own login
//     and is left alone.
//
// It runs at the same points as the Claude check: at boot, on every
// credential-bridge poll, at a session's start or resume, and before a
// session's sandbox is removed (a login that cannot be adopted keeps the
// sandbox).
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

// rotatingSessionLogin is one vendor login whose refresh token rotates. A
// session holds it only as a refresh-less copy.
type rotatingSessionLogin struct {
	rel    string // relative to HOME, in the session and in the shared home
	vendor string // for logs and the audit detail
	// adopt returns the new content of the shared file when sess holds a
	// strictly newer login than shared, nil when there is nothing to adopt.
	// It never logs and never returns token material in an error.
	adopt func(sess, shared []byte, sessMod, sharedMod time.Time) []byte
	// sessionCopy returns what a session is given for the shared file: the
	// same file with every refresh token emptied. ok is false when shared is
	// not a login file of this vendor; such a file is not handed out.
	sessionCopy func(shared []byte) ([]byte, bool)
	// refreshTokens returns every non-empty refresh token data holds.
	refreshTokens func(data []byte) []string
}

var rotatingSessionLogins = []rotatingSessionLogin{
	{rel: userSessionCodexLoginRel, vendor: "codex", adopt: adoptCodexLogin, sessionCopy: sessionCodexLogin, refreshTokens: codexRefreshTokens},
	{rel: userSessionOpenCodeLoginRel, vendor: "opencode", adopt: adoptOpenCodeLogin, sessionCopy: sessionOpenCodeLogin, refreshTokens: openCodeRefreshTokens},
}

// emptyJSONString is the value a session copy holds in place of a refresh
// token: the key both CLIs require, with nothing to redeem.
var emptyJSONString = json.RawMessage(`""`)

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
// login made by the hive (the manager's poll, a session's start and its
// removal), so two sessions holding newer logins settle on the later one and
// a copy is never written from a shared file another check is replacing.
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

// sessionCodexLogin implements rotatingSessionLogin.sessionCopy for Codex:
// the shared file with tokens.refresh_token emptied. A file without a tokens
// block (an API-key login) is handed out unchanged in content.
func sessionCodexLogin(shared []byte) ([]byte, bool) {
	if len(shared) > claudeBridgeMaxCredentialBytes {
		return nil, false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(shared, &top); err != nil || top == nil {
		return nil, false
	}
	if raw, ok := top["tokens"]; ok {
		var tokens map[string]json.RawMessage
		if err := json.Unmarshal(raw, &tokens); err != nil {
			return nil, false
		}
		if _, has := tokens["refresh_token"]; has {
			tokens["refresh_token"] = emptyJSONString
			b, err := json.Marshal(tokens)
			if err != nil {
				return nil, false
			}
			top["tokens"] = b
		}
	}
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return nil, false
	}
	return append(out, '\n'), true
}

// codexRefreshTokens implements rotatingSessionLogin.refreshTokens for Codex.
func codexRefreshTokens(data []byte) []string {
	var f codexLoginFile
	if json.Unmarshal(data, &f) != nil || f.Tokens == nil || f.Tokens.RefreshToken == "" {
		return nil
	}
	return []string{f.Tokens.RefreshToken}
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

// sessionOpenCodeLogin implements rotatingSessionLogin.sessionCopy for
// OpenCode: the shared file with the refresh token of every "oauth" entry
// emptied. Every other entry (API keys, well-known logins) is kept as it is.
func sessionOpenCodeLogin(shared []byte) ([]byte, bool) {
	if len(shared) > claudeBridgeMaxCredentialBytes {
		return nil, false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(shared, &top); err != nil || top == nil {
		return nil, false
	}
	for provider, raw := range top {
		var entry map[string]json.RawMessage
		if json.Unmarshal(raw, &entry) != nil || entry == nil {
			continue
		}
		var typ string
		if json.Unmarshal(entry["type"], &typ) != nil || typ != "oauth" {
			continue
		}
		if _, has := entry["refresh"]; !has {
			continue
		}
		entry["refresh"] = emptyJSONString
		b, err := json.Marshal(entry)
		if err != nil {
			return nil, false
		}
		top[provider] = b
	}
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return nil, false
	}
	return out, true
}

// openCodeRefreshTokens implements rotatingSessionLogin.refreshTokens for
// OpenCode: the refresh token of every "oauth" entry that has one.
func openCodeRefreshTokens(data []byte) []string {
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil {
		return nil
	}
	var out []string
	for _, raw := range top {
		if e, ok := openCodeOAuthEntry(raw); ok {
			out = append(out, e.Refresh)
		}
	}
	return out
}

// refreshTokenOverlap reports whether sess holds a refresh token shared does
// not (the session's own login) and whether it holds one shared does too (a
// full copy of the shared login). Only equality is used.
func refreshTokenOverlap(sess, shared []string) (own, copied bool) {
	for _, s := range sess {
		found := false
		for _, h := range shared {
			if s == h {
				found = true
				break
			}
		}
		if found {
			copied = true
		} else {
			own = true
		}
	}
	return own, copied
}

// ---- the check

// rotatingLoginKey is the sessionLoginCache key of one session's rotating
// login; the Claude check keys the same cache by the bare session id.
func rotatingLoginKey(id, rel string) string { return id + "\x00" + rel }

// reconcileRotatingLogin applies the three rules of the file comment to one
// vendor login of one session. The caller holds sessionRotatingLoginMu.
func (o sessionLoginOps) reconcileRotatingLogin(id string, l rotatingSessionLogin, mode sessionLoginMode) (sessionLoginResult, error) {
	var none sessionLoginResult
	home := userSessionHome(id)
	path := filepath.Join(home, l.rel)
	target := filepath.Join(sharedAgentHome, l.rel)

	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if mode != sessionLoginModeStart {
			return none, nil
		}
		shared, ok := readLoginSource(target)
		if !ok {
			return none, nil
		}
		written, werr := o.writeRotatingCopy(home, path, l, []byte(shared))
		if werr != nil || !written {
			return none, werr
		}
		return sessionLoginResult{action: sessionLoginInstalled}, nil
	}
	if err != nil {
		return none, fmt.Errorf("inspecting the session's %s login: %w", l.vendor, err)
	}
	if !info.Mode().IsRegular() {
		return none, nil
	}
	sharedInfo, err := os.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		// No shared login to split from or to follow.
		return none, nil
	}
	if err != nil {
		return none, fmt.Errorf("inspecting the shared %s login: %w", l.vendor, err)
	}
	sessNewer := info.ModTime().After(sharedInfo.ModTime())
	if !sharedInfo.Mode().IsRegular() {
		if !sessNewer {
			return none, nil
		}
		// The session's file may hold the newest login, and the shared one
		// is nothing hive could compare or replace: report it, so a removal
		// keeps the session.
		return none, fmt.Errorf("%w: the shared %s login %s is not a regular file", sharedfile.ErrUnsafeLocation, l.vendor, target)
	}
	if !sessNewer && mode == sessionLoginModeRemove {
		// Nothing to adopt, and the copy is about to be deleted.
		return none, nil
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
	if sessNewer {
		if merged := l.adopt(data, shared, info.ModTime(), sharedInfo.ModTime()); merged != nil {
			stale, err := replaceSharedLogin(target, merged, sharedInfo, o.now())
			if err != nil {
				return none, fmt.Errorf("%w (%s); adopting it failed: %w", crux.ErrUserSessionHoldsLogin, l.vendor, err)
			}
			res := sessionLoginResult{action: sessionLoginAdopted, staleShared: stale}
			if mode == sessionLoginModeRemove {
				return res, nil
			}
			// The session stops being a second refresh holder. A failure here
			// loses nothing: the login is in the shared file, and the next
			// check replaces the copy.
			if _, werr := o.writeRotatingCopy(home, path, l, merged); werr != nil {
				return res, fmt.Errorf("the %s login was adopted, but the session's copy could not be replaced: %w", l.vendor, werr)
			}
			return res, nil
		}
	}
	if mode == sessionLoginModeRemove {
		return none, nil
	}
	own, copied := refreshTokenOverlap(l.refreshTokens(data), l.refreshTokens(shared))
	if own {
		// A refresh token the shared login does not hold and that was not
		// adoptable: the session's own login, left alone.
		return none, nil
	}
	if sessNewer && !copied {
		// A refresh-less copy the shared login has not moved past.
		return none, nil
	}
	want, ok := l.sessionCopy(shared)
	if !ok || bytes.Equal(want, data) {
		return none, nil
	}
	written, werr := o.writeRotatingCopy(home, path, l, shared)
	if werr != nil || !written {
		return none, werr
	}
	return sessionLoginResult{action: sessionLoginRefreshed}, nil
}

// writeRotatingCopy replaces the session's copy with the refresh-less form of
// shared, as the session's uid. written is false, with no error, when shared
// is not a login that can be handed out.
func (o sessionLoginOps) writeRotatingCopy(home, path string, l rotatingSessionLogin, shared []byte) (bool, error) {
	copyBytes, ok := l.sessionCopy(shared)
	if !ok {
		return false, nil
	}
	if err := o.writeSessionCopy(home, path, copyBytes); err != nil {
		return false, err
	}
	return true, nil
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
func (o sessionLoginOps) checkRotatingLogin(id string, l rotatingSessionLogin, mode sessionLoginMode) (sessionLoginResult, error) {
	sessionRotatingLoginMu.Lock()
	defer sessionRotatingLoginMu.Unlock()
	return o.reconcileRotatingLogin(id, l, mode)
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
	} else if res.action != "" {
		logger.Debug("user session login: session copy brought up to date with the shared login",
			"session", id, "trigger", trigger, "vendor", l.vendor, "action", res.action)
	}
	if err != nil {
		logger.Warn("user session login: check failed", "session", id, "trigger", trigger, "vendor", l.vendor, "error", err)
	}
}

// syncRotatingLogin checks one session's login of one vendor and reports it.
func (r *UserSessionRuntime) syncRotatingLogin(id, trigger string, l rotatingSessionLogin, mode sessionLoginMode) error {
	res, err := r.loginOps().checkRotatingLogin(id, l, mode)
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
		res, err := ops.checkRotatingLogin(id, l, sessionLoginModeWatch)
		reportRotatingLogin(m.logger, func(action string, fields map[string]any) { m.audit(action, "", fields) }, id, trigger, l, res, err)
		if after, aok := sessionLoginStampOf(path, target); aok {
			m.sessionLogins.record(key, after, now)
		}
	}
}
