package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux/sharedfile"
)

// Copilot config bridge: keep each per-agent ~/.copilot/config.json linked to
// the fleet-shared login (hivecommons/hive#18443).
//
// Each per-UID agent has a REAL ~/.copilot (its own session-state) with one
// symlink in it, ~/.copilot/config.json -> /data/home/.copilot/config.json
// (perAgentCLIHomes). The Copilot CLI saves its config by writing a temp file
// and renaming it over config.json, which REPLACES the symlink with a private
// regular file. From then on the agent never sees a new login written to the
// shared file (dashboard device flow, token sync), and bridgeHomeEntry never
// repairs it because it refuses to clobber a regular file. On the Bluefin
// hosted hive one agent sat at the login prompt with a private copy that held
// no login while every other agent ran on private copies of an older token.
//
// The repair mirrors rebridgeClaudeCredential, simplified for Copilot: the
// private file is moved aside to config.json.bak-<UTC timestamp> (only the
// newest backup is kept) and the symlink is restored. It runs at every agent
// launch and restart (setupPerAgentCLIHome) and when the pane poller sees a
// Copilot agent stuck at a login prompt with a diverged private copy. It never
// reads token material into a log line: only paths and outcomes are logged.
//
// One guard: when the private copy holds a token and the shared file holds
// none, the private copy may be the only login the fleet has, so it is left in
// place (logged once). A later dashboard login or token sync fills the shared
// file and the next launch relinks.

// Repair triggers, recorded in the log.
const (
	copilotBridgeTriggerLaunch      = "launch"
	copilotBridgeTriggerLoginPrompt = "login-prompt"
)

// copilotConfigBackupInfix names a private config moved aside by the repair.
const copilotConfigBackupInfix = ".bak-"

// copilotConfigBackupNameAttempts bounds the search for a free backup name.
const copilotConfigBackupNameAttempts = 100

// copilotConfigMaxBytes caps a config file the divergence probe reads. A real
// Copilot config.json is a few KiB.
const copilotConfigMaxBytes = 256 << 10

// copilotConfigTmpInfix is the suffix the Copilot CLI's atomic save appends to
// its temp file (config.json.tmp.<random>). A CLI killed between the write and
// the rename leaves it behind.
const copilotConfigTmpInfix = ".tmp."

// copilotConfigTmpMaxAgeEnv overrides how old a leftover config.json.tmp.*
// must be before the launch-time sweep removes it. Go duration syntax.
const copilotConfigTmpMaxAgeEnv = "HIVE_COPILOT_CONFIG_TMP_MAX_AGE"

// defaultCopilotConfigTmpMaxAge is old enough that no live save (milliseconds)
// can still own the file.
const defaultCopilotConfigTmpMaxAge = time.Hour

// copilotConfigTmpSweepCap bounds one sweep, like claudeOrphanTmpSweepCap.
const copilotConfigTmpSweepCap = 200

// copilotBridgeNow stamps backup names and ages tmp files; a seam for tests.
var copilotBridgeNow = time.Now

// copilotBridgeMu serializes repairs: the launch path and the poller can reach
// the same agent's link. Never held while taking m.mu (the launch path already
// holds m.mu when it provisions the home).
var copilotBridgeMu sync.Mutex

// copilotBridgeWarned holds, per link, the last failure message logged, so a
// repair that keeps failing on every poll logs once per distinct cause.
var copilotBridgeWarned sync.Map

// errCopilotBridgeLinkIsTarget: the agent's config path IS the shared file (a
// legacy whole-directory ~/.copilot symlink). Moving it aside would log the
// whole fleet out.
var errCopilotBridgeLinkIsTarget = errors.New("copilot config link path resolves to the shared config it would link to")

// errCopilotBridgeSharedEmpty: the private copy holds a token and the shared
// file holds none, so relinking would log the agent out.
var errCopilotBridgeSharedEmpty = errors.New("private copilot config holds a login and the shared config holds none; leaving the private copy in place")

// copilotConfigBridgePaths returns the agent's bridged config path and the
// shared target it must link to. ok is false when the agent has no per-agent
// home, i.e. exactly when setupInteractiveHome provisions no bridge.
func copilotConfigBridgePaths(agentName string, uid int, backend string) (link, target string, ok bool) {
	if uid <= 0 || IsInferenceBackend(backend) || sharedAgentHomeForced() {
		return "", "", false
	}
	link = filepath.Join(interactiveHomePath(agentName), copilotHomeDirName, copilotConfigFileName)
	return link, sharedCopilotBridgeTarget(), true
}

// sharedCopilotBridgeTarget is the shared config every bridge links to,
// derived from sharedAgentHome like the links themselves.
func sharedCopilotBridgeTarget() string {
	return filepath.Join(sharedAgentHome, copilotHomeDirName, copilotConfigFileName)
}

// copilotConfigDiverged reports whether the file the agent's Copilot CLI
// actually reads at link is a private regular file that no longer follows the
// shared login: unreadable, holding no usable token, or different from the
// shared file. A symlink, an absent file, or a path that IS the shared file is
// not diverged.
func copilotConfigDiverged(link, target string) bool {
	info, err := os.Lstat(link)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if same, serr := claudeBridgeLinkIsTarget(link, target); serr != nil || same {
		return false
	}
	private, err := sharedfile.ReadNoFollow(link, copilotConfigMaxBytes)
	if err != nil {
		return true
	}
	if !copilotCredentialFileHasTokens(link) {
		return true
	}
	shared, err := sharedfile.ReadNoFollow(target, copilotConfigMaxBytes)
	if err != nil {
		// No shared file to compare against: the private copy with a token is
		// the only login this agent has, and it is the one it reads.
		return false
	}
	return string(private) != string(shared)
}

// repairCopilotConfigBridge moves a private regular-file config.json aside and
// restores the symlink to the shared file. It returns the backup path, or ""
// with a nil error when there is nothing to repair (a symlink, an absent path,
// a directory).
func repairCopilotConfigBridge(link, target string) (string, error) {
	copilotBridgeMu.Lock()
	defer copilotBridgeMu.Unlock()

	info, err := os.Lstat(link)
	if err != nil || !info.Mode().IsRegular() {
		return "", nil
	}
	same, err := claudeBridgeLinkIsTarget(link, target)
	if err != nil {
		return "", nil
	}
	if same {
		return "", fmt.Errorf("%w: %s", errCopilotBridgeLinkIsTarget, link)
	}
	if copilotCredentialFileHasTokens(link) && !copilotCredentialFileHasTokens(target) {
		return "", errCopilotBridgeSharedEmpty
	}
	backup, err := moveAsideCopilotConfig(link, copilotBridgeNow())
	if err != nil {
		return "", fmt.Errorf("move private copilot config aside: %w", err)
	}
	pruneCopilotConfigBackups(link, backup)
	if err := os.Symlink(target, link); err != nil {
		// EEXIST: the CLI saved again between the rename and now. The next
		// launch repairs that one.
		return backup, fmt.Errorf("restore copilot config symlink: %w", err)
	}
	return backup, nil
}

// moveAsideCopilotConfig renames path to a free <path>.bak-<UTC timestamp>[-N]
// beside it. A rename keeps the file's owner and mode.
func moveAsideCopilotConfig(path string, now time.Time) (string, error) {
	base := path + copilotConfigBackupInfix + now.UTC().Format("20060102T150405Z")
	for i := 0; i < copilotConfigBackupNameAttempts; i++ {
		dst := base
		if i > 0 {
			dst = fmt.Sprintf("%s-%d", base, i)
		}
		if _, err := os.Lstat(dst); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if err := os.Rename(path, dst); err != nil {
			return "", err
		}
		return dst, nil
	}
	return "", fmt.Errorf("no free backup name for %s", path)
}

// pruneCopilotConfigBackups removes every earlier config.json.bak-* regular
// file beside link except keep, so repeated repairs never accumulate copies of
// stale logins.
func pruneCopilotConfigBackups(link, keep string) {
	matches, err := filepath.Glob(link + copilotConfigBackupInfix + "*")
	if err != nil {
		return
	}
	for _, p := range matches {
		if p == keep {
			continue
		}
		if info, lerr := os.Lstat(p); lerr == nil && info.Mode().IsRegular() {
			_ = os.Remove(p)
		}
	}
}

// rebridgeCopilotConfig runs the repair for one agent and logs the outcome. It
// reports whether the link was restored.
func (m *Manager) rebridgeCopilotConfig(agentName, link, target, trigger string) bool {
	backup, err := repairCopilotConfigBridge(link, target)
	if err != nil {
		if prev, loaded := copilotBridgeWarned.Swap(link, err.Error()); loaded && prev == err.Error() {
			return false
		}
		m.logger.Warn("copilot config bridge: repair not applied",
			"agent", agentName, "link", link, "shared", target, "trigger", trigger, "error", err)
		return false
	}
	copilotBridgeWarned.Delete(link)
	if backup == "" {
		return false
	}
	// A completed repair is routine self-healing, not a fault: WARN made the
	// fleet self-report it as an unclassified repeated error (#18497).
	m.logger.Info("copilot config bridge: a private config.json had replaced the shared-login symlink; re-linked",
		"agent", agentName,
		"trigger", trigger,
		"backup", backup,
		"cause", "Copilot CLI saves its config with a temp file renamed over the link",
		"see", "hivecommons/hive#18443")
	return true
}

// repairCopilotConfigOnLoginPrompt is the poller's hook: a Copilot agent stuck
// at a login prompt whose config.json diverged from the shared login is
// relinked, so the credential probe then evaluates the shared login and the
// token restart that follows starts a CLI that reads it.
func (m *Manager) repairCopilotConfigOnLoginPrompt(agent *AgentProcess) bool {
	m.mu.RLock()
	name, uid, backend := agent.Name, agent.UID, effectiveBackend(agent)
	m.mu.RUnlock()
	if backend != "copilot" {
		return false
	}
	link, target, ok := copilotConfigBridgePaths(name, uid, backend)
	if !ok || !copilotConfigDiverged(link, target) {
		return false
	}
	return m.rebridgeCopilotConfig(name, link, target, copilotBridgeTriggerLoginPrompt)
}

// copilotConfigTmpMaxAge returns the sweep threshold: the env override when it
// parses to a positive duration, the default otherwise.
func copilotConfigTmpMaxAge() time.Duration {
	if v := strings.TrimSpace(os.Getenv(copilotConfigTmpMaxAgeEnv)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultCopilotConfigTmpMaxAge
}

// sweepStaleCopilotConfigTmp removes leftover config.json.tmp.* files older
// than copilotConfigTmpMaxAge from the shared .copilot directory and the
// agent's own. They are the debris of a Copilot CLI save that died between
// writing the temp file and renaming it; a younger one may belong to a save in
// progress and is kept. Bounded and best-effort; only regular files are
// removed, and contents are never read.
func (m *Manager) sweepStaleCopilotConfigTmp(agentName, home string) {
	maxAge := copilotConfigTmpMaxAge()
	cutoff := copilotBridgeNow().Add(-maxAge)
	prefix := copilotConfigFileName + copilotConfigTmpInfix
	removed := 0
	for _, dir := range []string{filepath.Join(sharedAgentHome, copilotHomeDirName), filepath.Join(home, copilotHomeDirName)} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if removed >= copilotConfigTmpSweepCap {
				break
			}
			if !strings.HasPrefix(e.Name(), prefix) {
				continue
			}
			p := filepath.Join(dir, e.Name())
			info, err := os.Lstat(p)
			if err != nil || !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
				continue
			}
			if err := os.Remove(p); err == nil {
				removed++
			}
		}
	}
	if removed > 0 {
		m.logger.Info(fmt.Sprintf("swept %d stale copilot config tmp file(s)", removed),
			"agent", agentName, "max_age", maxAge.String())
	}
}
