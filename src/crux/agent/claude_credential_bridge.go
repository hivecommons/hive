package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/hivecommons/hive/crux/claude"
	"github.com/hivecommons/hive/crux/sharedfile"
)

// Claude credential bridge: share the LOGIN across per-agent .claude homes
// without sharing anything else.
//
// Since hivecommons/hive#17039 each per-UID claude agent has a REAL ~/.claude
// (its own transcripts, todos, history and auto-memory) with one symlink in
// it, ~/.claude/.credentials.json -> /data/home/.claude/.credentials.json, so
// one operator sign-in serves the fleet. Claude Code does not write through
// that link. When it refreshes the OAuth token (about every 8 hours) it writes
// a temp file in ~/.claude and renames it over .credentials.json, which
// REPLACES the symlink with a regular file owned by the agent's uid, mode
// 0600. The refresh also rotates the grant: the shared file now holds a dead
// refresh token, the fresh one is private to that agent, and every other
// agent fails its own refresh within the access token's lifetime. On per-UID
// hives the whole fleet logged out about every 8 hours.
//
// Claude Code offers no supported way to keep the link: CLAUDE_CONFIG_DIR
// moves the WHOLE config directory (settings, transcripts, auto-memory) and
// would undo the per-agent memory split; a bind mount cannot be renamed over
// (EBUSY) and needs mount privileges the pod does not have. So hive cannot
// prevent the replacement and instead DETECTS and PUBLISHES it: at every agent
// launch, on a short poll from the credential watchdog, and right after a
// dashboard sign-in.
//
// The repair never deletes a token. It moves the private file aside, keeps
// whichever of the two credentials is newer AND usable in the shared file, and
// restores the symlink:
//
//   - the private copy wins only when it is complete and usable
//     (claudeBridgePromotable: access and refresh tokens present, an expiresAt
//     recorded, and claude.TokensUsable) and either the shared credential is
//     not usable or the private access token expires LATER than the shared
//     one. A refresh mints a later expiresAt, so this is what promotes one
//     agent's fresh refresh to the whole fleet instead of stranding everyone
//     else on the grant it just rotated. A failed refresh leaves an
//     empty-token, expiresAt-0 file; it never qualifies;
//   - otherwise the shared file stays as it is (ties keep the shared file, so
//     a private copy identical to it is simply moved aside).
//
// Who reads the private file. The hive server runs as its own uid (dev) and
// cannot open another uid's 0600 file directly. It CAN run a helper as that
// uid: the same su-exec seam seedClaudeSessionForAgent uses to adopt an
// agent-owned .claude.json (runAsAgentUser, gated to the hive-launch group).
// So the judgement happens in-process, as the file's owner, with no root
// helper and no widening of the private file's mode. When neither read works
// (no su-exec on this deployment) the repair fails SAFE: the private file is
// left exactly where it is, the agent keeps running on the token it already
// had, and one ERROR per agent and file version says so.
//
// The private file is renamed to .credentials.json.stale-<UTC timestamp> in
// the agent's own .claude directory and keeps its owner and mode; when it is
// promoted, the shared file it replaces is preserved the same way beside the
// shared credential.
//
// The repair never acts on a link path that resolves to the shared credential
// itself (an agent whose whole ~/.claude is still the legacy directory symlink
// to /data/home/.claude): that "private file" is the fleet's login. Such an
// agent is skipped with one warning; its next launch retires the directory
// link (setupPerAgentCLIHome) before the repair runs.

// Repair triggers, recorded in the audit detail and the log.
const (
	claudeBridgeTriggerLaunch   = "launch"
	claudeBridgeTriggerWatchdog = "watchdog"
	claudeBridgeTriggerSignIn   = "sign-in"
)

// claudeCredentialStaleInfix names a credential moved aside by the repair.
const claudeCredentialStaleInfix = ".stale-"

// claudeSharedCredentialMode is the mode of a shared credential the repair
// writes: owner and group read/write, the same as claude.WriteCredentials (the
// dashboard sign-in) and what the entrypoint guard asserts, so every agent uid
// reaches it through the node group.
const claudeSharedCredentialMode = 0o660

// claudeBridgeStaleNameAttempts bounds the search for a free
// .stale-<timestamp>[-N] name beside a credential.
const claudeBridgeStaleNameAttempts = 100

// claudeBridgeMaxCredentialBytes caps a credential file the repair will read.
// A real credentials file is well under 4 KiB.
const claudeBridgeMaxCredentialBytes = 64 << 10

// claudeBridgeNow stamps the moved-aside file names; a seam for tests.
var claudeBridgeNow = time.Now

// claudeBridgeMu serializes repairs: the launch path, the watchdog and the
// sign-in hook can all reach the same agent's link. It is never held while
// taking m.mu, so it cannot invert against the launch path, which already
// holds m.mu when it provisions the home.
var claudeBridgeMu sync.Mutex

// claudeBridgeRepair describes one repaired bridge, for the audit and log.
type claudeBridgeRepair struct {
	agent string
	// kept is "shared" when the shared credential was kept as it was, or
	// "agent" when the agent's private copy was newer and usable and was
	// promoted into the shared file.
	kept string
	// staleAgentPath is where the private file was moved.
	staleAgentPath string
	// staleSharedPath is where the replaced shared file was preserved; empty
	// unless kept == "agent" and a shared file existed.
	staleSharedPath string
}

// claudeCredentialBridgePaths returns the agent's bridged credential path and
// the shared target it must link to. ok is false when the agent has no
// per-agent home (no uid, an inference backend, or HIVE_SHARED_AGENT_HOME=1),
// i.e. exactly when setupInteractiveHome provisions no bridge.
func claudeCredentialBridgePaths(agentName string, uid int, backend string) (link, target string, ok bool) {
	if uid <= 0 || IsInferenceBackend(backend) || sharedAgentHomeForced() {
		return "", "", false
	}
	link = filepath.Join(interactiveHomePath(agentName), claudeHomeDirName, claudeCredentialsFileName)
	return link, sharedClaudeBridgeTarget(), true
}

// sharedClaudeBridgeTarget is the shared credential every bridge links to.
// Derived from sharedAgentHome (not the claude.CredentialsPath constant) so it
// follows the same test seam as the links themselves.
func sharedClaudeBridgeTarget() string {
	return filepath.Join(sharedAgentHome, claudeHomeDirName, claudeCredentialsFileName)
}

// claudeCredentialBridgeBroken reports whether a regular file has replaced the
// agent's credential symlink. Read-only (one Lstat, plus two directory
// resolutions only when that Lstat finds a regular file), so it is cheap
// enough for the status snapshot. It follows the same rule as the repair: the
// legacy layout, where the link path resolves to the shared credential itself,
// is not a broken bridge and is never reported as one.
func claudeCredentialBridgeBroken(agentName string, uid int, backend string) bool {
	link, target, ok := claudeCredentialBridgePaths(agentName, uid, backend)
	if !ok {
		return false
	}
	info, err := os.Lstat(link)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	same, err := claudeBridgeLinkIsTarget(link, target)
	return err == nil && !same
}

// errClaudeBridgeLinkIsTarget: the agent's credential path IS the shared
// credential, so there is no private copy to move aside and no link to
// restore (a legacy whole-directory ~/.claude symlink). Moving that file aside
// would strand the fleet.
var errClaudeBridgeLinkIsTarget = errors.New("credential link path resolves to the shared credential it would link to")

// errClaudeBridgeAgentUnreadable: the agent's private credential exists but
// neither this process nor a helper running as the file's owner could read
// it. Its token cannot be judged, so the repair leaves it exactly where it is.
var errClaudeBridgeAgentUnreadable = errors.New("agent's private credential cannot be read")

// errClaudeBridgeSourceChanged: the file renamed aside is not the regular
// file that was inspected a moment earlier. It is put back and not read.
var errClaudeBridgeSourceChanged = errors.New("private credential changed under the repair")

// errClaudeBridgeNotPromotable: promoteClaudeCredential re-checks the bytes it
// is about to write and refuses an incomplete or unusable credential.
var errClaudeBridgeNotPromotable = errors.New("refusing to promote an incomplete or unusable credential over the shared login")

// claudeBridgeLegacyWarned holds the credential links the legacy-layout skip
// has already been logged for, so the watchdog, which walks every managed
// agent each cycle, logs it once per agent instead of every cycle. Keyed by
// the link path, which names the agent.
var claudeBridgeLegacyWarned sync.Map

// claudeBridgeUnreadableWarned holds, per credential link, the modification
// time of the unreadable private file last logged, so the poll logs once per
// agent and file version instead of every cycle. A new refresh (a new mtime)
// is logged again.
var claudeBridgeUnreadableWarned sync.Map

// claudeBridgeUnsafeWarned holds the shared credential paths whose location
// sharedfile.Replace refused, so the refusal is logged once, not per cycle.
var claudeBridgeUnsafeWarned sync.Map

// claudeBridgeLinkIsTarget reports whether link names the shared credential
// itself: the same path, or a file of the same name in a directory that
// resolves to the target's directory (a legacy whole-directory ~/.claude
// symlink). An error means a directory could not be resolved (absent, or not
// reachable by this process); callers treat that as nothing to repair.
func claudeBridgeLinkIsTarget(link, target string) (bool, error) {
	if filepath.Clean(link) == filepath.Clean(target) {
		return true, nil
	}
	if filepath.Base(link) != filepath.Base(target) {
		return false, nil
	}
	linkDir, err := filepath.EvalSymlinks(filepath.Dir(link))
	if err != nil {
		return false, err
	}
	targetDir, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return false, err
	}
	if linkDir == targetDir {
		return true, nil
	}
	// Different resolved paths can still be one directory (a bind mount).
	li, err := os.Stat(linkDir)
	if err != nil {
		return false, err
	}
	ti, err := os.Stat(targetDir)
	if err != nil {
		return false, err
	}
	return os.SameFile(li, ti), nil
}

// claudeBridgePromotable reports whether t may replace the shared login: a
// complete credential (access token, refresh token and a recorded expiresAt)
// that is usable now. A failed refresh leaves an empty-token, expiresAt-0
// file, which never qualifies; neither does a token with no recorded expiry,
// which cannot be judged newer than anything.
func claudeBridgePromotable(t *claude.OAuthTokens, now time.Time) bool {
	return t != nil && t.AccessToken != "" && t.RefreshToken != "" && t.ExpiresAt > 0 &&
		claude.TokensUsable(t, now)
}

// preferAgentClaudeCredential decides which credential the shared file keeps.
// See the file comment for the rule.
func preferAgentClaudeCredential(agent, shared *claude.OAuthTokens, now time.Time) bool {
	if !claudeBridgePromotable(agent, now) {
		return false
	}
	if !claude.TokensUsable(shared, now) {
		return true
	}
	return agent.ExpiresAt > shared.ExpiresAt
}

// readClaudeCredentialAsOwner reads an agent's private credential: directly
// when this process may, otherwise through su-exec as the file's owner (the
// per-UID case: an agent-owned 0600 file and a server running as dev).
// claudeSessionOwnerSpec fails closed — a symlink, a root-owned or an
// oversized file never gets a helper — so a planted link at the credential
// path cannot make the helper read something else.
func readClaudeCredentialAsOwner(path string) ([]byte, error) {
	data, err := sharedfile.ReadNoFollow(path, claudeBridgeMaxCredentialBytes)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return data, err
	}
	spec, serr := claudeSessionOwnerSpec(path)
	if serr != nil {
		return nil, errors.Join(err, serr)
	}
	owned, oerr := runAsAgentUser(spec, nil, "cat", path)
	if oerr != nil {
		return nil, errors.Join(err, oerr)
	}
	if len(owned) > claudeBridgeMaxCredentialBytes {
		return nil, fmt.Errorf("%w: %s returned %d bytes as owner", sharedfile.ErrTooLarge, path, len(owned))
	}
	return owned, nil
}

// canonicalClaudeCredential decodes data as a credentials file and re-encodes
// only its typed token block. What reaches the shared file is this encoding,
// never the bytes read from an agent's directory: a file that is not a
// credential in the shape claude.Credentials describes is not promotable.
func canonicalClaudeCredential(data []byte, now time.Time) ([]byte, *claude.OAuthTokens, error) {
	if len(data) > claudeBridgeMaxCredentialBytes {
		return nil, nil, errClaudeBridgeNotPromotable
	}
	tok := claude.ParseTokens(data)
	if !claudeBridgePromotable(tok, now) {
		return nil, nil, errClaudeBridgeNotPromotable
	}
	out, err := json.Marshal(claude.Credentials{ClaudeAIOAuth: tok})
	if err != nil {
		return nil, nil, errClaudeBridgeNotPromotable
	}
	return out, tok, nil
}

// repairClaudeCredentialBridge re-bridges one agent's credential when a regular
// file has replaced the symlink. It returns nil, nil when there is nothing to
// repair (a symlink, an absent link, a directory, which is not this failure,
// or a link or target directory that cannot be resolved). It returns an error
// wrapping errClaudeBridgeLinkIsTarget, and touches nothing, when the link
// path resolves to the shared credential itself, and, unless trigger is
// claudeBridgeTriggerSignIn, one wrapping errClaudeBridgeAgentUnreadable,
// again touching nothing, when the private file cannot be read.
//
// The sign-in trigger is the exception: a dashboard sign-in has just written
// a new shared login, newer than any private copy by construction, so on that
// trigger an unreadable private file is still moved aside (unread) and the
// agent relinked to the shared login.
func (m *Manager) repairClaudeCredentialBridge(agentName, link, target, trigger string) (*claudeBridgeRepair, error) {
	claudeBridgeMu.Lock()
	defer claudeBridgeMu.Unlock()

	info, err := os.Lstat(link)
	if err != nil || !info.Mode().IsRegular() {
		return nil, nil
	}

	// Never operate on the shared credential itself. Checked BEFORE anything
	// is moved: when the link path resolves to the target, the "private file"
	// below IS the shared login, moving it aside strands the fleet, and the
	// symlink restored afterwards would point at its own path.
	same, err := claudeBridgeLinkIsTarget(link, target)
	if err != nil {
		m.logger.Debug("claude credential bridge: cannot resolve the link or shared directory; not repairing",
			"agent", agentName, "link", link, "target", target, "error", err)
		return nil, nil
	}
	if same {
		return nil, fmt.Errorf("%w: %s", errClaudeBridgeLinkIsTarget, link)
	}

	// Never move aside a file whose token cannot be judged: it may be the
	// only fresh token in the fleet.
	if _, rerr := readClaudeCredentialAsOwner(link); rerr != nil {
		if errors.Is(rerr, fs.ErrNotExist) {
			// Gone since the Lstat: the CLI is mid-rename. Next cycle.
			return nil, nil
		}
		if trigger == claudeBridgeTriggerSignIn && claude.HasUsableToken(target) {
			return m.relinkUnreadableAfterSignIn(agentName, link, target)
		}
		return nil, fmt.Errorf("%w: %w", errClaudeBridgeAgentUnreadable, rerr)
	}
	now := claudeBridgeNow()

	// Move the private file aside FIRST, then judge the moved file: after the
	// rename the CLI can no longer change what is being judged (its next
	// write lands at link), so the decision works on a stable snapshot.
	staleAgent, err := moveAsideClaudeCredential(link, now)
	if err != nil {
		return nil, fmt.Errorf("move private credential aside: %w", err)
	}
	agentData, rerr := readClaudeCredentialAsOwner(staleAgent)
	if rerr != nil {
		// The file renamed is not the one read above (the CLI replaced it in
		// between) and this one cannot be read either. Put it back unless the
		// CLI has already written yet another file there.
		if _, lerr := os.Lstat(link); errors.Is(lerr, fs.ErrNotExist) {
			if berr := os.Rename(staleAgent, link); berr != nil {
				m.logger.Error("claude credential bridge: an unreadable private credential could not be put back; it is kept at the stale path",
					"agent", agentName, "stale", staleAgent, "error", berr)
			}
		}
		return nil, fmt.Errorf("%w: %w", errClaudeBridgeAgentUnreadable, rerr)
	}
	rep := &claudeBridgeRepair{agent: agentName, kept: "shared", staleAgentPath: staleAgent}

	if preferAgentClaudeCredential(claude.ParseTokens(agentData), claude.LoadTokens(target), now) {
		staleShared, perr := promoteClaudeCredential(agentData, target, now)
		if perr != nil {
			// Put the private file back: the agent's copy was the only usable
			// token, and linking the agent to the older shared one would
			// strand it. The next cycle tries again.
			if berr := os.Rename(staleAgent, link); berr != nil {
				m.logger.Error("claude credential bridge: promotion failed and the private credential could not be restored; it is kept at the stale path",
					"agent", agentName, "stale", staleAgent, "error", perr, "restore_error", berr)
			}
			return nil, fmt.Errorf("promote private credential to shared: %w", perr)
		}
		rep.kept = "agent"
		rep.staleSharedPath = staleShared
	}

	if err := os.Symlink(target, link); err != nil {
		// EEXIST: the CLI wrote a fresh file between the rename and now. The
		// next cycle repairs that one; nothing is lost either way.
		return rep, fmt.Errorf("restore credential symlink: %w", err)
	}
	return rep, nil
}

// claudeBridgeStaleName returns a free <path>.stale-<UTC timestamp>[-N] name
// beside path, or "" when none is free. exists reports whether a candidate is
// taken; callers that create the file atomically pass a function that tries
// the create and reports EEXIST.
func claudeBridgeStaleName(path string, now time.Time, exists func(string) (bool, error)) (string, error) {
	base := path + claudeCredentialStaleInfix + now.UTC().Format("20060102T150405Z")
	for i := 0; i < claudeBridgeStaleNameAttempts; i++ {
		dst := base
		if i > 0 {
			dst = fmt.Sprintf("%s-%d", base, i)
		}
		taken, err := exists(dst)
		if err != nil {
			return "", err
		}
		if !taken {
			return dst, nil
		}
	}
	return "", fmt.Errorf("no free stale name for %s", path)
}

// moveAsideClaudeCredential renames path to a unique stale name beside it and
// returns the new path. A rename keeps the file's owner and mode: the private
// copy stays readable only by the agent that wrote it. The file is identified
// before the rename and checked after it (same device and inode, still a
// regular file); anything else is put back and reported as
// errClaudeBridgeSourceChanged, unread.
func moveAsideClaudeCredential(path string, now time.Time) (string, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %s is %s", errClaudeBridgeSourceChanged, path, before.Mode())
	}
	dst, err := claudeBridgeStaleName(path, now, func(dst string) (bool, error) {
		_, lerr := os.Lstat(dst)
		if lerr == nil {
			return true, nil
		}
		if errors.Is(lerr, fs.ErrNotExist) {
			return false, nil
		}
		return false, lerr
	})
	if err != nil {
		return "", err
	}
	if err := os.Rename(path, dst); err != nil {
		return "", err
	}
	after, err := os.Lstat(dst)
	if err != nil {
		return "", err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		if _, lerr := os.Lstat(path); errors.Is(lerr, fs.ErrNotExist) {
			_ = os.Rename(dst, path)
		}
		return "", fmt.Errorf("%w: %s", errClaudeBridgeSourceChanged, path)
	}
	return dst, nil
}

// relinkUnreadableAfterSignIn is the sign-in exception described on
// repairClaudeCredentialBridge: the private file is moved aside without being
// read and the agent is relinked to the shared login just written.
func (m *Manager) relinkUnreadableAfterSignIn(agentName, link, target string) (*claudeBridgeRepair, error) {
	staleAgent, err := moveAsideClaudeCredential(link, claudeBridgeNow())
	if err != nil {
		return nil, fmt.Errorf("move private credential aside: %w", err)
	}
	rep := &claudeBridgeRepair{agent: agentName, kept: "shared", staleAgentPath: staleAgent}
	if err := os.Symlink(target, link); err != nil {
		return rep, fmt.Errorf("restore credential symlink: %w", err)
	}
	return rep, nil
}

// promoteClaudeCredential replaces the shared credential with the token block
// decoded from data, atomically and at claudeSharedCredentialMode, owned like
// the file it replaces. The replaced file is preserved beside it first; its
// path is returned ("" when there was no shared file). The bytes are decoded
// and re-checked here (canonicalClaudeCredential), so a credential that
// changed after it was judged can never put an incomplete token over the
// shared login, and nothing but a typed token block is ever written. The
// write itself is sharedfile.Replace: a random O_EXCL|O_NOFOLLOW temporary,
// metadata by descriptor, and a refusal (sharedfile.ErrUnsafeLocation) when
// the shared directory or file is not what it should be.
func promoteClaudeCredential(data []byte, target string, now time.Time) (string, error) {
	canonical, _, err := canonicalClaudeCredential(data, now)
	if err != nil {
		return "", err
	}
	// Refuse before the backup copy, not only at the write: the copy lands in
	// the same directory, and the directory is part of what is being checked.
	info, err := sharedfile.CheckLocation(target)
	if err != nil {
		return "", err
	}
	staleShared := ""
	if info != nil {
		staleShared, err = preserveClaudeCredential(target, now)
		if err != nil {
			return "", fmt.Errorf("preserve shared credential: %w", err)
		}
	}
	if err := sharedfile.Replace(target, canonical, claudeSharedCredentialMode, sharedfile.Options{PreserveOwner: true}); err != nil {
		return staleShared, err
	}
	return staleShared, nil
}

// preserveClaudeCredential copies path to a unique stale name beside it,
// leaving path itself in place (agents keep reading it until the rename that
// replaces it). The copy is 0600: it is an archive, not a live credential.
// Neither the read nor the create follows a symlink.
func preserveClaudeCredential(path string, now time.Time) (string, error) {
	data, err := sharedfile.ReadNoFollow(path, claudeBridgeMaxCredentialBytes)
	if err != nil {
		return "", err
	}
	var f *os.File
	dst, err := claudeBridgeStaleName(path, now, func(dst string) (bool, error) {
		var oerr error
		f, oerr = os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if errors.Is(oerr, fs.ErrExist) {
			return true, nil
		}
		return false, oerr
	})
	if err != nil {
		return "", err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(dst)
		return "", errors.Join(werr, cerr)
	}
	return dst, nil
}

// rebridgeClaudeCredential runs the repair for one agent and records the
// outcome. It is the single entry point the launch path, the watchdog poll and
// the sign-in hook share, so all three log and audit identically.
func (m *Manager) rebridgeClaudeCredential(agentName, link, target, trigger string) *claudeBridgeRepair {
	// Operator-token mode: agents do not authenticate with the shared
	// credentials file, so a private copy left by a refresh is nothing to
	// repair (claude_operator_token_mode.go). Covers launch, watchdog and
	// sign-in alike.
	if m.claudeOperatorTokenMode() {
		m.logClaudeOperatorTokenModeOnce()
		return nil
	}
	rep, err := m.repairClaudeCredentialBridge(agentName, link, target, trigger)
	if errors.Is(err, errClaudeBridgeLinkIsTarget) {
		// The legacy layout: skipped, and said once per agent rather than on
		// every cycle. Not audited (nothing changed).
		if _, seen := claudeBridgeLegacyWarned.LoadOrStore(link, true); !seen {
			m.logger.Warn("claude credential bridge: this agent's .claude directory resolves to the shared login directory (a legacy whole-directory symlink), so its credential path IS the shared login; not re-bridging. The agent's next launch replaces the directory link with a per-agent directory",
				"agent", agentName, "trigger", trigger, "dir", filepath.Dir(link), "shared", target)
		}
		return nil
	}
	// Any other outcome clears the mark, so a legacy layout that shows up
	// again later is logged again.
	claudeBridgeLegacyWarned.Delete(link)
	if errors.Is(err, errClaudeBridgeAgentUnreadable) {
		m.logClaudeBridgeUnreadable(agentName, link, trigger, err)
		return nil
	}
	claudeBridgeUnreadableWarned.Delete(link)
	if errors.Is(err, sharedfile.ErrUnsafeLocation) {
		// The shared location is not what the repair expects; nothing was
		// written and the agent keeps its private copy. Said once per target.
		if _, seen := claudeBridgeUnsafeWarned.LoadOrStore(target, true); !seen {
			m.logger.Error("claude credential bridge: the shared credential location was refused and nothing was written; the agent keeps its private copy",
				"agent", agentName, "trigger", trigger, "shared", target, "error", err)
		}
		return nil
	}
	claudeBridgeUnsafeWarned.Delete(target)
	if err != nil {
		// Logged, not audited: a repair that keeps failing would otherwise add
		// an audit row every cycle.
		m.logger.Warn("claude credential bridge: repair failed",
			"agent", agentName, "link", link, "trigger", trigger, "error", err)
		return nil
	}
	if rep == nil {
		return nil
	}
	m.logger.Warn("claude credential bridge: a private credential file had replaced the shared-login symlink; re-bridged",
		"agent", agentName,
		"trigger", trigger,
		"kept", rep.kept,
		"stale_agent_file", rep.staleAgentPath,
		"stale_shared_file", rep.staleSharedPath,
		"cause", "Claude Code token refresh writes a temp file and renames it over the link")
	m.audit(AuditClaudeCredentialRebridged, agentName, auditFields(
		"outcome", "success",
		"trigger", trigger,
		"kept", rep.kept,
		"stale_agent_file", rep.staleAgentPath,
		"stale_shared_file", rep.staleSharedPath,
	))
	return rep
}

// logClaudeBridgeUnreadable says, once per agent and private-file version,
// that the repair left an agent on a private credential it could not read.
// Not audited: nothing changed.
func (m *Manager) logClaudeBridgeUnreadable(agentName, link, trigger string, err error) {
	var stamp int64
	if info, lerr := os.Lstat(link); lerr == nil {
		stamp = info.ModTime().UnixNano()
	}
	if prev, loaded := claudeBridgeUnreadableWarned.Swap(link, stamp); loaded && prev == stamp {
		return
	}
	m.logger.Error("claude credential bridge: this agent's private credential could not be read, directly or as its owner through su-exec, so its token cannot be judged; leaving the agent on its private copy (not moved aside, not re-linked). Other agents will not see this agent's token refresh until it is readable",
		"agent", agentName, "path", link, "trigger", trigger, "error", err)
}

// RepairClaudeCredentialBridges checks every per-agent Claude credential
// bridge and repairs each one a regular file has replaced. It returns the
// names of the agents it re-bridged, sorted. Agents are snapshotted under m.mu
// and repaired after it is released. In operator-token mode it does nothing
// and returns nil (claude_operator_token_mode.go).
func (m *Manager) RepairClaudeCredentialBridges(trigger string) []string {
	if m.claudeOperatorTokenMode() {
		m.logClaudeOperatorTokenModeOnce()
		return nil
	}
	type candidate struct {
		name    string
		uid     int
		backend string
	}
	m.mu.RLock()
	cands := make([]candidate, 0, len(m.agents))
	for name, a := range m.agents {
		if a == nil {
			continue
		}
		cands = append(cands, candidate{name: name, uid: a.UID, backend: effectiveBackend(a)})
	}
	m.mu.RUnlock()
	sort.Slice(cands, func(i, j int) bool { return cands[i].name < cands[j].name })

	var repaired []string
	for _, c := range cands {
		link, target, ok := claudeCredentialBridgePaths(c.name, c.uid, c.backend)
		if !ok {
			continue
		}
		if rep := m.rebridgeClaudeCredential(c.name, link, target, trigger); rep != nil {
			repaired = append(repaired, c.name)
		}
	}
	return repaired
}

// rebridgeClaudeCredentialsAfterSignIn runs from ReloadClaudeToken: a sign-in
// wrote the shared file, and agents whose link a refresh replaced would never
// see it. Relink them now rather than on the next poll. A logout leaves
// nothing usable, so nothing is moved.
func (m *Manager) rebridgeClaudeCredentialsAfterSignIn() {
	if claude.HasUsableToken(sharedClaudeBridgeTarget()) {
		m.RepairClaudeCredentialBridges(claudeBridgeTriggerSignIn)
	}
}
