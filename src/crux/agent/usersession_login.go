package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/claude"
	"github.com/hivecommons/hive/crux/sharedfile"
)

// One refreshable Claude login per hive: what a user session is given, and how
// a login that still ends up in a session HOME gets back to the shared file.
//
// The failure this closes. A session used to receive a full COPY of the shared
// ~/.claude/.credentials.json, refresh token included. The CLI in the session
// refreshed that copy when its access token was due, the refresh ROTATED the
// grant, and the session's private copy became the only valid login: the
// shared file every agent reads held a dead refresh token, the agents failed
// their own refreshes and fell to the login prompt, and the fleet stayed
// locked out until a person signed in again. Deleting the session deleted the
// only valid login with it.
//
// Three rules, in order of weight:
//
//  1. A session is never a refresh holder. Its copy is the access token block
//     WITHOUT the refresh token (sessionClaudeLogin), so the CLI in a session
//     has nothing to rotate. The refresh stays with the agents' credential
//     bridge (claude_credential_bridge.go) and the dashboard sign-in, which
//     are the only writers of the shared file. Nothing else from the shared
//     file travels either: only the claudeAiOauth block is copied.
//  2. The session's copy follows the shared login. When the shared file holds
//     a different, later credential (an agent refreshed, an operator signed
//     in), the copy in every session HOME is replaced, atomically, as the
//     session's own uid.
//  3. A login that is in a session HOME anyway is never lost and never left
//     to diverge. If the session's file is complete (it has a refresh token),
//     usable, and strictly newer than the shared login, hive ADOPTS it into
//     the shared file and puts the session back on a refresh-less copy. That
//     covers copies handed out before rule 1 existed (a hive already locked
//     out recovers without a sign-in while such a copy survives), and a
//     person signing in from inside a session. It runs at boot, on every
//     credential-bridge poll, after a dashboard sign-in, when a session
//     starts or resumes, and BEFORE a session's sandbox is removed.
//
// "Strictly newer" is decided from two facts only, the recorded access-token
// expiry and the files' modification times; token values are compared only
// for equality, in memory, and are never logged or audited.
//
// Every read-compare-write of the shared login here runs under
// claudeBridgeMu, the same mutex the agents' bridge repair holds, so an
// adoption and a re-bridge can never interleave and two newer credentials
// arriving together settle on the later one.

// userSessionClaudeLoginRel is the Claude entry of userSessionLoginFiles.
const userSessionClaudeLoginRel = ".claude/.credentials.json"

// Triggers of a session login check, recorded in the log and the audit detail.
const (
	sessionLoginTriggerBoot     = "boot"
	sessionLoginTriggerWatchdog = claudeBridgeTriggerWatchdog
	sessionLoginTriggerSignIn   = claudeBridgeTriggerSignIn
	sessionLoginTriggerStart    = "session-start"
	sessionLoginTriggerRemove   = "session-remove"
)

// Outcomes of one check.
const (
	sessionLoginAdopted   = "adopted"
	sessionLoginRefreshed = "refreshed"
	sessionLoginInstalled = "installed"
)

// sessionLoginRecheckInterval is how long an unchanged pair of files (the
// session's copy and the shared login) is trusted before it is judged again;
// usability also depends on the clock, not only on the files.
const sessionLoginRecheckInterval = 10 * time.Minute

// claudeOAuthBlockKey is the credentials-file key of the token block.
const claudeOAuthBlockKey = "claudeAiOauth"

// sessionLoginMode selects what one check may do besides adopting.
type sessionLoginMode int

const (
	// sessionLoginModeWatch adopts a newer session login and brings the
	// session's copy up to date with the shared one.
	sessionLoginModeWatch sessionLoginMode = iota
	// sessionLoginModeStart is sessionLoginModeWatch plus installing the copy
	// when the session has none yet.
	sessionLoginModeStart
	// sessionLoginModeRemove only adopts: the session's files are about to
	// be deleted, so nothing is written into them.
	sessionLoginModeRemove
)

// sessionLoginResult describes what one check did. Never token material.
type sessionLoginResult struct {
	// action is "" (nothing to do), sessionLoginAdopted, sessionLoginRefreshed
	// or sessionLoginInstalled.
	action string
	// staleShared is where the replaced shared login was preserved after an
	// adoption; empty when there was no shared file.
	staleShared string
}

// sessionLoginOps carries the seams one check needs.
type sessionLoginOps struct {
	priv    privRunFunc
	statUID func(string) (int, error)
	now     func() time.Time
}

// Seams of the manager-side sweep, which has no UserSessionRuntime: tests
// substitute them because a unit test cannot create another uid's files.
var (
	sessionLoginPriv    privRunFunc               = execUserSessionPriv
	sessionLoginStatUID func(string) (int, error) = statPathUID
)

// sessionClaudeLogin returns the credential a session is given for the shared
// credentials file: its token block without the refresh token and the refresh
// token's expiry, and nothing else from the file. ok is false when shared is
// not a credentials file with an access token; such a file is not handed out.
func sessionClaudeLogin(shared []byte) ([]byte, bool) {
	if len(shared) > claudeBridgeMaxCredentialBytes {
		return nil, false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(shared, &top); err != nil {
		return nil, false
	}
	var block map[string]json.RawMessage
	if err := json.Unmarshal(top[claudeOAuthBlockKey], &block); err != nil || block == nil {
		return nil, false
	}
	var access string
	if err := json.Unmarshal(block["accessToken"], &access); err != nil || access == "" {
		return nil, false
	}
	delete(block, "refreshToken")
	delete(block, "refreshTokenExpiresAt")
	out, err := json.Marshal(map[string]any{claudeOAuthBlockKey: block})
	if err != nil {
		return nil, false
	}
	return out, true
}

// sessionClaudeLoginNewer reports whether the session's credential must be
// adopted into the shared login: it is complete and usable
// (claudeBridgePromotable) and either the shared login is not usable, or the
// session's access token expires later AND its file was written after the
// shared one. A refresh mints a later expiry and writes a new file, so both
// hold for a rotation; a copy that merely sits beside a newer sign-in fails
// the second test and the sign-in stays.
func sessionClaudeLoginNewer(sess *claude.OAuthTokens, sessMod time.Time, shared *claude.OAuthTokens, sharedMod time.Time, now time.Time) bool {
	if !claudeBridgePromotable(sess, now) {
		return false
	}
	if !claude.TokensUsable(shared, now) {
		return true
	}
	return sess.ExpiresAt > shared.ExpiresAt && sessMod.After(sharedMod)
}

// sessionClaudeLoginIsHiveCopy reports whether the session's credential is one
// hive put there: a refresh-less copy, or (from releases that copied the whole
// file) one carrying the very refresh token the shared file holds. Anything
// else is a credential the session obtained itself and is left alone unless
// it is adopted.
func sessionClaudeLoginIsHiveCopy(sess, shared *claude.OAuthTokens) bool {
	if sess == nil {
		return false
	}
	return sess.RefreshToken == "" || (shared != nil && sess.RefreshToken == shared.RefreshToken)
}

// sessionUID is the owner of the session home, refused outside the reserved
// session range (the same rule as UserSessionRuntime.sessionUID).
func (o sessionLoginOps) sessionUID(home string) (int, error) {
	uid, err := o.statUID(home)
	if err != nil {
		return 0, fmt.Errorf("session sandbox unavailable: %w", err)
	}
	if !userSessionUIDInRange(uid) {
		return 0, fmt.Errorf("session home is owned by uid %d, outside the reserved session range", uid)
	}
	return uid, nil
}

// readSessionLogin reads the session's credential: directly when this process
// may, otherwise as the session's own uid. The helper is `head -c`, bounded,
// so a path the session swapped for something endless cannot fill memory, and
// it runs with the minimal environment of every session helper. The error
// never carries what was read.
func (o sessionLoginOps) readSessionLogin(home, path string) ([]byte, error) {
	data, err := sharedfile.ReadNoFollow(path, claudeBridgeMaxCredentialBytes)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return data, err
	}
	uid, uerr := o.sessionUID(home)
	if uerr != nil {
		return nil, errors.Join(err, uerr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), userSessionCmdTimeout)
	defer cancel()
	owned, perr := o.priv(ctx, userSessionSpec(uid), nil, "head", "-c", strconv.Itoa(claudeBridgeMaxCredentialBytes+1), "--", path)
	if perr != nil {
		// The helper's output is deliberately dropped: on a partial read it
		// is credential bytes.
		return nil, fmt.Errorf("reading the session's login as its owner: %w", errors.Join(err, perr))
	}
	if len(owned) > claudeBridgeMaxCredentialBytes {
		return nil, fmt.Errorf("%w: the session's login is over %d bytes", sharedfile.ErrTooLarge, claudeBridgeMaxCredentialBytes)
	}
	return owned, nil
}

// writeSessionLogin replaces the session's copy with the refresh-less form of
// the shared login, as the session's uid. written is false, with no error,
// when the shared file is not a credential that can be handed out.
func (o sessionLoginOps) writeSessionLogin(home, path, target string) (bool, error) {
	shared, ok := readLoginSource(target)
	if !ok {
		return false, nil
	}
	copyBytes, ok := sessionClaudeLogin([]byte(shared))
	if !ok {
		return false, nil
	}
	if err := o.writeSessionCopy(home, path, copyBytes); err != nil {
		return false, err
	}
	return true, nil
}

// writeSessionCopy replaces the session's login file at path with data,
// atomically, read-only (0400) and as the session's own uid.
func (o sessionLoginOps) writeSessionCopy(home, path string, data []byte) error {
	uid, err := o.sessionUID(home)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), userSessionCmdTimeout)
	defer cancel()
	// The script writes nothing to its output, so the output is safe to keep
	// in the error.
	if out, err := o.priv(ctx, userSessionSpec(uid), bytes.NewReader(data), "sh", "-c", userSessionCopyLoginScript, "sh", path); err != nil {
		return outputErr("writing the session's login copy", err, out)
	}
	return nil
}

// fileModTime is the modification time of path, zero when it cannot be read.
func fileModTime(path string) time.Time {
	info, err := os.Lstat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// reconcileClaudeLogin applies the three rules of the file comment to one
// session. The caller holds claudeBridgeMu.
func (o sessionLoginOps) reconcileClaudeLogin(id string, mode sessionLoginMode) (sessionLoginResult, error) {
	var none sessionLoginResult
	home := userSessionHome(id)
	path := filepath.Join(home, userSessionClaudeLoginRel)
	target := sharedClaudeBridgeTarget()

	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if mode != sessionLoginModeStart {
			return none, nil
		}
		written, werr := o.writeSessionLogin(home, path, target)
		if werr != nil || !written {
			return none, werr
		}
		return sessionLoginResult{action: sessionLoginInstalled}, nil
	}
	if err != nil {
		return none, fmt.Errorf("inspecting the session's login: %w", err)
	}
	if !info.Mode().IsRegular() {
		// A directory or a link is not a copy hive made and holds no login
		// hive could judge.
		return none, nil
	}
	data, err := o.readSessionLogin(home, path)
	if errors.Is(err, fs.ErrNotExist) {
		return none, nil
	}
	if err != nil {
		return none, err
	}
	now := o.now()
	sess := claude.ParseTokens(data)
	shared := claude.LoadTokens(target)

	if sessionClaudeLoginNewer(sess, info.ModTime(), shared, fileModTime(target), now) {
		staleShared, perr := promoteClaudeCredential(data, target, now)
		if perr != nil {
			return none, fmt.Errorf("%w; adopting it failed: %w", crux.ErrUserSessionHoldsLogin, perr)
		}
		res := sessionLoginResult{action: sessionLoginAdopted, staleShared: staleShared}
		if mode == sessionLoginModeRemove {
			return res, nil
		}
		// The session stops being a second refresh holder. A failure here
		// loses nothing: the login is in the shared file, and the next check
		// replaces the copy.
		if _, werr := o.writeSessionLogin(home, path, target); werr != nil {
			return res, fmt.Errorf("the login was adopted, but the session's copy could not be replaced: %w", werr)
		}
		return res, nil
	}
	if mode == sessionLoginModeRemove || !sessionClaudeLoginIsHiveCopy(sess, shared) {
		return none, nil
	}
	if shared == nil || shared.AccessToken == "" {
		return none, nil
	}
	if sess.RefreshToken == "" {
		changed := sess.AccessToken != shared.AccessToken
		later := shared.ExpiresAt > sess.ExpiresAt || fileModTime(target).After(info.ModTime())
		if !changed || !later {
			return none, nil
		}
	}
	written, werr := o.writeSessionLogin(home, path, target)
	if werr != nil || !written {
		return none, werr
	}
	return sessionLoginResult{action: sessionLoginRefreshed}, nil
}

// checkSessionClaudeLogin runs one check under claudeBridgeMu.
func (o sessionLoginOps) checkSessionClaudeLogin(id string, mode sessionLoginMode) (sessionLoginResult, error) {
	claudeBridgeMu.Lock()
	defer claudeBridgeMu.Unlock()
	return o.reconcileClaudeLogin(id, mode)
}

// reportSessionLogin logs and audits one check. An adoption is the only
// outcome that changes the shared login, and the only one audited. Neither
// the log nor the audit ever receives token material: the fields are the
// session id, the trigger and file paths.
func reportSessionLogin(logger *slog.Logger, audit func(action string, fields map[string]any), id, trigger string, res sessionLoginResult, err error) {
	if res.action == sessionLoginAdopted {
		logger.Warn("user session login: the session held a newer Claude login than the shared file; adopted it into the shared login",
			"session", id, "trigger", trigger, "stale_shared_file", res.staleShared)
		if audit != nil {
			audit(AuditUserSessionLoginAdopted, auditFields(
				"outcome", "success",
				"session", id,
				"trigger", trigger,
				"stale_shared_file", res.staleShared,
			))
		}
	} else if res.action != "" {
		logger.Debug("user session login: session copy brought up to date with the shared login",
			"session", id, "trigger", trigger, "action", res.action)
	}
	if err != nil {
		logger.Warn("user session login: check failed", "session", id, "trigger", trigger, "error", err)
	}
}

// ---- session runtime side

// SetAuditSink installs the audit sink an adoption made by the session
// runtime (at a session's start or removal) is recorded in. Nil is allowed.
func (r *UserSessionRuntime) SetAuditSink(sink crux.AuditSink) {
	r.auditMu.Lock()
	defer r.auditMu.Unlock()
	r.auditSink = sink
}

func (r *UserSessionRuntime) loginOps() sessionLoginOps {
	return sessionLoginOps{priv: r.priv, statUID: r.statUID, now: r.now}
}

// syncClaudeLogin checks one session's Claude login and reports the outcome.
func (r *UserSessionRuntime) syncClaudeLogin(id, trigger string, mode sessionLoginMode) error {
	res, err := r.loginOps().checkSessionClaudeLogin(id, mode)
	reportSessionLogin(r.logger, r.loginAudit(), id, trigger, res, err)
	return err
}

// loginAudit is the audit function of a session login check made by the
// runtime, nil when no sink is installed.
func (r *UserSessionRuntime) loginAudit() func(string, map[string]any) {
	r.auditMu.Lock()
	sink := r.auditSink
	r.auditMu.Unlock()
	if sink == nil {
		return nil
	}
	return func(action string, fields map[string]any) { sink.Record(auditActorSystem, action, "", fields) }
}

// ---- manager side

// sessionLoginStamp identifies the pair of files one check judged.
type sessionLoginStamp struct {
	sessMod, sharedMod   int64
	sessSize, sharedSize int64
}

// sessionLoginCache remembers, per session, the files last judged, so the
// poll costs two Lstats for a session whose login has not changed.
type sessionLoginCache struct {
	mu   sync.Mutex
	seen map[string]sessionLoginSeen
}

type sessionLoginSeen struct {
	stamp sessionLoginStamp
	at    time.Time
}

func sessionLoginStampOf(path, target string) (sessionLoginStamp, bool) {
	var st sessionLoginStamp
	info, err := os.Lstat(path)
	if err != nil {
		return st, false
	}
	st.sessMod, st.sessSize = info.ModTime().UnixNano(), info.Size()
	if tinfo, terr := os.Lstat(target); terr == nil {
		st.sharedMod, st.sharedSize = tinfo.ModTime().UnixNano(), tinfo.Size()
	}
	return st, true
}

// due reports whether id must be judged (again).
func (w *sessionLoginCache) due(id string, stamp sessionLoginStamp, now time.Time, force bool) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	prev, ok := w.seen[id]
	return force || !ok || prev.stamp != stamp || now.Sub(prev.at) >= sessionLoginRecheckInterval
}

func (w *sessionLoginCache) record(id string, stamp sessionLoginStamp, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen == nil {
		w.seen = map[string]sessionLoginSeen{}
	}
	w.seen[id] = sessionLoginSeen{stamp: stamp, at: now}
}

// forget drops every session not in keep (its sandbox is gone).
func (w *sessionLoginCache) forget(keep map[string]bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for id := range w.seen {
		if !keep[id] {
			delete(w.seen, id)
		}
	}
}

// syncUserSessionClaudeLogins checks the Claude login of every session
// sandbox on this hive: it adopts a strictly newer login a session holds into
// the shared file (with an audit entry) and brings every session's copy up to
// date with the shared one. It returns the ids it adopted a Claude login
// from. The same sweep adopts a newer Codex or OpenCode login
// (syncUserSessionRotatingLogins).
//
// It runs at boot, on the credential-bridge poll and after a dashboard
// sign-in, whether or not any agent uses Claude: sessions use the shared
// login either way. The sandboxes are found on disk rather than through the
// dashboard's session store, so an ended session whose record is gone but
// whose files remain is still covered.
func (m *Manager) syncUserSessionClaudeLogins(trigger string) []string {
	root := userSessionSandboxRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	target := sharedClaudeBridgeTarget()
	ops := sessionLoginOps{priv: sessionLoginPriv, statUID: sessionLoginStatUID, now: claudeBridgeNow}
	force := trigger != sessionLoginTriggerWatchdog
	present := map[string]bool{}
	var adopted []string
	for _, e := range entries {
		id := e.Name()
		if !e.IsDir() || !userSessionIDPattern.MatchString(id) {
			continue
		}
		// Codex and OpenCode logins (usersession_login_rotating.go).
		m.syncUserSessionRotatingLogins(id, trigger, ops, force, present)
		path := filepath.Join(userSessionHome(id), userSessionClaudeLoginRel)
		stamp, ok := sessionLoginStampOf(path, target)
		if !ok {
			continue
		}
		present[id] = true
		now := ops.now()
		if !m.sessionLogins.due(id, stamp, now, force) {
			continue
		}
		res, cerr := ops.checkSessionClaudeLogin(id, sessionLoginModeWatch)
		reportSessionLogin(m.logger, func(action string, fields map[string]any) { m.audit(action, "", fields) }, id, trigger, res, cerr)
		if res.action == sessionLoginAdopted {
			adopted = append(adopted, id)
		}
		// Stamp what is on disk after the check, so its own writes do not
		// look like a change next time. A failed check is stamped too: it is
		// retried when a file changes or the recheck interval passes, not
		// logged on every poll.
		if after, aok := sessionLoginStampOf(path, target); aok {
			m.sessionLogins.record(id, after, now)
		}
	}
	m.sessionLogins.forget(present)
	return adopted
}
