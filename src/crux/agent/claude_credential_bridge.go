package agent

import (
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
)

// Claude credential bridge repair.
//
// Each per-UID claude agent reaches the fleet's one Claude login through a
// symlink, $HOME/.claude/.credentials.json -> /data/home/.claude/.credentials.json
// (perAgentCLIHomes). Claude Code does not write through that link. When it
// refreshes the OAuth token (about every 8 hours) it writes a temp file in
// $HOME/.claude and renames it over .credentials.json, which REPLACES the
// symlink with a regular file owned by the agent's uid, mode 0600. From then
// on that agent runs on a private copy: a later dashboard sign-in rewrites the
// shared file and never reaches it, and when its private token is revoked the
// agent answers every kick with "Please run /login" while a perfectly good
// login sits one directory away.
//
// The Claude Code CLI offers no supported way to keep the link: its documented
// knob, CLAUDE_CONFIG_DIR, moves the WHOLE config directory (settings,
// transcripts, auto-memory) and would undo the per-agent split of
// hivecommons/hive#17031; a bind mount cannot be renamed over (EBUSY) and
// needs mount privileges the pod does not have. So hive cannot prevent the
// replacement and instead DETECTS and REPAIRS it: at every agent launch, on
// every credential-watchdog cycle, and right after a dashboard sign-in.
//
// The repair never deletes a token. It moves the private file aside, keeps
// whichever of the two credentials is newer AND usable in the shared file, and
// restores the symlink:
//
//   - the private copy wins only when it is usable (claude.TokensUsable: a live
//     access token, or a refresh grant that has not expired) and either the
//     shared credential is not usable or the private access token expires
//     LATER than the shared one. A refresh mints a later expiresAt, so this is
//     what promotes one agent's fresh refresh to the whole fleet instead of
//     stranding everyone else on the grant it just rotated;
//   - otherwise the shared file stays as it is (ties keep the shared file, so
//     a private copy identical to it is simply moved aside).
//
// The private file is renamed to .credentials.json.stale-<UTC timestamp> in
// the agent's own .claude directory and keeps its owner and mode; when it is
// promoted, the shared file it replaces is preserved the same way beside the
// shared credential. Both names fall outside sharedCredentialBases, so the
// permissions watcher never opens them up to the group.

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

// Test seams. chownClaudeCredential applies the previous shared file's owner
// to the promoted replacement; it is a seam so tests run without root or real
// agent uids. claudeBridgeNow stamps the moved-aside file names.
var (
	chownClaudeCredential = os.Chown
	claudeBridgeNow       = time.Now
)

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
// agent's credential symlink. Read-only (one Lstat), so it is cheap enough for
// the status snapshot.
func claudeCredentialBridgeBroken(agentName string, uid int, backend string) bool {
	link, _, ok := claudeCredentialBridgePaths(agentName, uid, backend)
	if !ok {
		return false
	}
	info, err := os.Lstat(link)
	return err == nil && info.Mode().IsRegular()
}

// preferAgentClaudeCredential decides which credential the shared file keeps.
// See the file comment for the rule.
func preferAgentClaudeCredential(agent, shared *claude.OAuthTokens, now time.Time) bool {
	if !claude.TokensUsable(agent, now) {
		return false
	}
	if !claude.TokensUsable(shared, now) {
		return true
	}
	return agent.ExpiresAt > shared.ExpiresAt
}

// repairClaudeCredentialBridge re-bridges one agent's credential when a regular
// file has replaced the symlink. It returns nil, nil when there is nothing to
// repair (a symlink, an absent link, or a directory, which is not this
// failure).
func (m *Manager) repairClaudeCredentialBridge(agentName, link, target string) (*claudeBridgeRepair, error) {
	claudeBridgeMu.Lock()
	defer claudeBridgeMu.Unlock()

	info, err := os.Lstat(link)
	if err != nil || !info.Mode().IsRegular() {
		return nil, nil
	}
	now := claudeBridgeNow()

	// Move the private file aside FIRST. After the rename the CLI can no
	// longer change what is being judged (its next write lands at link), so
	// the decision below works on a stable snapshot.
	staleAgent, err := moveAsideClaudeCredential(link, now)
	if err != nil {
		return nil, fmt.Errorf("move private credential aside: %w", err)
	}
	rep := &claudeBridgeRepair{agent: agentName, kept: "shared", staleAgentPath: staleAgent}

	if preferAgentClaudeCredential(claude.LoadTokens(staleAgent), claude.LoadTokens(target), now) {
		staleShared, perr := promoteClaudeCredential(staleAgent, target, now)
		if perr != nil {
			// Put the private file back: the agent's copy was the only usable
			// token, and linking the agent to the older shared one would
			// strand it. The next cycle tries again.
			if rerr := os.Rename(staleAgent, link); rerr != nil {
				m.logger.Error("claude credential bridge: promotion failed and the private credential could not be restored; it is kept at the stale path",
					"agent", agentName, "stale", staleAgent, "error", perr, "restore_error", rerr)
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

// moveAsideClaudeCredential renames path to a unique
// <path>.stale-<UTC timestamp>[-N] beside it and returns the new path. It skips
// any name that already exists.
func moveAsideClaudeCredential(path string, now time.Time) (string, error) {
	base := path + claudeCredentialStaleInfix + now.UTC().Format("20060102T150405Z")
	for i := 0; i < 100; i++ {
		dst := base
		if i > 0 {
			dst = fmt.Sprintf("%s-%d", base, i)
		}
		if _, err := os.Lstat(dst); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		// A rename keeps the file's owner and mode: the private copy stays
		// readable only by the agent that wrote it.
		if err := os.Rename(path, dst); err != nil {
			return "", err
		}
		return dst, nil
	}
	return "", fmt.Errorf("no free stale name for %s", path)
}

// promoteClaudeCredential replaces the shared credential with the bytes of src,
// atomically and at claudeSharedCredentialMode, owned like the file it
// replaces. The replaced file is preserved beside it first; its path is
// returned ("" when there was no shared file).
func promoteClaudeCredential(src, target string, now time.Time) (string, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	uid, gid := -1, -1
	staleShared := ""
	if info, err := os.Stat(target); err == nil {
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(st.Uid), int(st.Gid)
		}
		staleShared, err = preserveClaudeCredential(target, now)
		if err != nil {
			return "", fmt.Errorf("preserve shared credential: %w", err)
		}
	}
	tmp := target + ".tmp-rebridge"
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, data, claudeSharedCredentialMode); err != nil {
		return staleShared, err
	}
	// WriteFile honours the umask; assert the mode explicitly.
	if err := os.Chmod(tmp, claudeSharedCredentialMode); err != nil {
		_ = os.Remove(tmp)
		return staleShared, err
	}
	if uid >= 0 || gid >= 0 {
		// Best-effort: an unprivileged hive cannot chown, and the file it
		// wrote is already group-readable by mode.
		_ = chownClaudeCredential(tmp, uid, gid)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return staleShared, err
	}
	return staleShared, nil
}

// preserveClaudeCredential copies path to a unique stale name beside it,
// leaving path itself in place (agents keep reading it until the rename that
// replaces it). The copy is 0600: it is an archive, not a live credential.
func preserveClaudeCredential(path string, now time.Time) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	base := path + claudeCredentialStaleInfix + now.UTC().Format("20060102T150405Z")
	for i := 0; i < 100; i++ {
		dst := base
		if i > 0 {
			dst = fmt.Sprintf("%s-%d", base, i)
		}
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
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
	return "", fmt.Errorf("no free stale name for %s", path)
}

// rebridgeClaudeCredential runs the repair for one agent and records the
// outcome. It is the single entry point the launch path, the watchdog and the
// sign-in hook share, so all three log and audit identically.
func (m *Manager) rebridgeClaudeCredential(agentName, link, target, trigger string) *claudeBridgeRepair {
	rep, err := m.repairClaudeCredentialBridge(agentName, link, target)
	if err != nil {
		// Logged, not audited: a repair that keeps failing (an unprivileged
		// hive that cannot enter the agent's 0700 home) would otherwise add an
		// audit row every watchdog cycle. The dashboard keeps showing the
		// agent's bridge as broken, which is the standing signal.
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

// RepairClaudeCredentialBridges checks every per-agent Claude credential
// bridge and repairs each one a regular file has replaced. It returns the
// names of the agents it re-bridged, sorted. Agents are snapshotted under m.mu
// and repaired after it is released.
func (m *Manager) RepairClaudeCredentialBridges(trigger string) []string {
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
