package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Claude Code's first-run screens, and why a subscription agent needs a seed.
//
// A claude-backend agent signed in with a subscription (not routed through an
// inference endpoint) runs Claude Code in its own per-agent HOME. When Claude
// Code has never run in that HOME, it opens on two screens before its input
// box:
//
//  1. "Security notes: … Press Enter to continue…", shown until
//     hasCompletedOnboarding is set in ~/.claude.json;
//  2. "Quick safety check: Is this a project you created or one you trust?",
//     shown until projects[<cwd>].hasTrustDialogAccepted is true, with
//     "No, exit" PRE-SELECTED.
//
// The startup kick cannot be delivered while either is up, and any Enter that
// reaches the second screen before the trust watcher does (an operator kick,
// for example) answers "No, exit": the CLI quits and the rest of the prompt is
// typed into bash. Inference agents never see either screen because
// ensureClaudeSettings seeds their HOME; this gives a subscription agent the
// same two answers, which are the ones hive's own watchers would type anyway
// (see the "claude workspace trust" entry in panesignal.BlockingPrompts).
//
// The file is NOT hive's. It holds the agent's signed-in identity
// (oauthAccount, see claude_session_state.go) and whatever else Claude Code
// keeps there, it may be owned by the agent UID at 0600, and on a hive that
// runs unprivileged it may be unreadable. So, unlike the inference seeders,
// this one is strictly merge-only and never repairs anything:
//
//   - only hasCompletedOnboarding and projects[<dir>].hasTrustDialogAccepted
//     are ever set, and only to true; every other key and value is kept;
//   - a file hive cannot read, cannot parse, or cannot replace with the same
//     owner and mode is left exactly as it is (logged once, launch goes on);
//   - a symlink or other non-regular file is refused, never followed;
//   - a new file is created only where it can be handed to the agent UID
//     through the open descriptor (or where hive is unprivileged and the
//     home is shared-writable, the seedClaudeSessionForAgent trade-off), so
//     a root hive never leaves a root-owned file in an agent's HOME.

// claudeTrustAcceptedKey is the per-project ~/.claude.json key that records a
// "Yes, I trust this folder" answer.
const claudeTrustAcceptedKey = "hasTrustDialogAccepted"

// claudeFirstRunSeedTempInfix names the staging file a seed is written to
// before it is renamed over ~/.claude.json, in the same directory so the
// rename is atomic.
const claudeFirstRunSeedTempInfix = ".hive-first-run."

// errClaudeFirstRunSymlink reports a symlink where ~/.claude.json should be.
var errClaudeFirstRunSymlink = errors.New("is a symlink; refusing to follow it")

// errClaudeFirstRunChanged reports that ~/.claude.json changed between the
// read and the rename, so the merged copy may be stale and is not published.
var errClaudeFirstRunChanged = errors.New("changed while being seeded; left for the next launch")

// claudeFirstRunSkipLog remembers, per agent, the last reason the seed was
// skipped, so an agent whose file hive can never touch (an unreadable 0600
// file on an unprivileged hive) logs one WARN rather than one per launch.
type claudeFirstRunSkipLog struct {
	mu   sync.Mutex
	last map[string]string
}

// shouldWarn reports whether reason is new for agentName.
func (s *claudeFirstRunSkipLog) shouldWarn(agentName, reason string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		s.last = make(map[string]string)
	}
	if s.last[agentName] == reason {
		return false
	}
	s.last[agentName] = reason
	return true
}

// clear forgets agentName's last skip reason after a seed that succeeded.
func (s *claudeFirstRunSkipLog) clear(agentName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.last, agentName)
}

// seedClaudeFirstRunForAgent gives a subscription claude agent's own
// ~/.claude.json the onboarding and workspace-trust answers, so Claude Code
// starts at its input box. Best-effort: every failure is logged and the
// launch continues (the trust watcher still answers the trust dialog).
//
// Runs only where the HOME is the agent's own (a per-UID agent on the
// per-agent layout). With no agent UID the HOME is the hive process's own, and
// under HIVE_SHARED_AGENT_HOME=1 it is one file every running agent's CLI
// rewrites (#4596); neither is a file one agent's launch should rewrite.
//
// Must run after setupInteractiveHome, whose session adoption may have just
// written this file: the seed merges into the adopted copy.
func (m *Manager) seedClaudeFirstRunForAgent(agent *AgentProcess) {
	home, ok := perAgentXDGHome(agent.Name, agent.UID, "claude")
	if !ok {
		m.logger.Debug("claude first-run seed skipped: HOME is not per-agent",
			"agent", agent.Name, "uid", agent.UID)
		return
	}
	path := claudeSessionFile(home)
	written, err := seedClaudeFirstRunFile(path, m.claudeTrustedProjectDirs(agent.Name), agent.UID)
	if err != nil {
		reason := err.Error()
		if m.claudeFirstRunSkip.shouldWarn(agent.Name, reason) {
			m.logger.Warn("claude first-run seed skipped; Claude Code may show its onboarding and trust screens on this launch",
				"agent", agent.Name, "path", path, "reason", reason)
		} else {
			m.logger.Debug("claude first-run seed skipped",
				"agent", agent.Name, "path", path, "reason", reason)
		}
		return
	}
	m.claudeFirstRunSkip.clear(agent.Name)
	if written {
		m.logger.Info("seeded claude onboarding and workspace trust for agent",
			"agent", agent.Name, "path", path)
	}
}

// seedClaudeFirstRunFile merges the first-run answers into the .claude.json at
// path, creating it for agentUID when it does not exist. It reports whether
// the file was written; an error means it was left untouched.
func seedClaudeFirstRunFile(path string, projectDirs []string, agentUID int) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		data, _, merr := mergeClaudeFirstRunSeed(nil, projectDirs)
		if merr != nil {
			return false, merr
		}
		if cerr := createClaudeFirstRunFile(path, data, agentUID); cerr != nil {
			return false, cerr
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, errClaudeFirstRunSymlink
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("not a regular file (mode %s)", info.Mode())
	}
	data, orig, err := readClaudeFirstRunFile(path)
	if err != nil {
		// Includes EACCES: an unreadable file is very likely the agent's own
		// 0600 sign-in, and "cannot read" must never become "start fresh".
		return false, err
	}
	out, changed, err := mergeClaudeFirstRunSeed(data, projectDirs)
	if err != nil || !changed {
		return false, err
	}
	if err := replaceClaudeFirstRunFile(path, out, orig); err != nil {
		return false, err
	}
	return true, nil
}

// mergeClaudeFirstRunSeed returns data with hasCompletedOnboarding and
// projects[dir].hasTrustDialogAccepted set to true for every dir, and whether
// anything changed. nil data means "no file yet". Everything else is carried
// over as raw JSON, so values hive does not interpret are never re-encoded
// through Go types (no float64 rounding, no dropped fields). A file that is
// not a JSON object, or whose projects value is not an object, is an error:
// hive does not know what it is looking at and leaves it alone. A project
// entry that is not an object is skipped for the same reason.
func mergeClaudeFirstRunSeed(data []byte, projectDirs []string) ([]byte, bool, error) {
	top := map[string]json.RawMessage{}
	if data != nil {
		if err := json.Unmarshal(data, &top); err != nil {
			return nil, false, fmt.Errorf("not parseable as a JSON object: %w", err)
		}
		if top == nil {
			return nil, false, errors.New("not a JSON object")
		}
	}
	changed := false
	if !jsonValueIsTrue(top[claudeOnboardingKey]) {
		top[claudeOnboardingKey] = json.RawMessage("true")
		changed = true
	}

	projects := map[string]json.RawMessage{}
	if raw, ok := top[claudeProjectsKey]; ok {
		if err := json.Unmarshal(raw, &projects); err != nil || projects == nil {
			return nil, false, fmt.Errorf("%q is not a JSON object", claudeProjectsKey)
		}
	}
	projectsChanged := false
	for _, dir := range projectDirs {
		if dir == "" {
			continue
		}
		entry := map[string]json.RawMessage{}
		if raw, ok := projects[dir]; ok {
			if err := json.Unmarshal(raw, &entry); err != nil || entry == nil {
				continue
			}
		}
		if jsonValueIsTrue(entry[claudeTrustAcceptedKey]) {
			continue
		}
		entry[claudeTrustAcceptedKey] = json.RawMessage("true")
		encoded, err := marshalJSONNoEscape(entry)
		if err != nil {
			return nil, false, err
		}
		projects[dir] = encoded
		projectsChanged = true
	}
	if projectsChanged {
		encoded, err := marshalJSONNoEscape(projects)
		if err != nil {
			return nil, false, err
		}
		top[claudeProjectsKey] = encoded
		changed = true
	}
	if !changed {
		return nil, false, nil
	}
	out, err := marshalJSONNoEscape(top)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// marshalJSONNoEscape is json.Marshal without HTML escaping, so the strings
// Claude Code wrote ("<", ">", "&" in history and paths) come back byte-equal.
func marshalJSONNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// claudeFirstRunFileState is what the seed read, kept so the replacement
// carries the same owner and mode and so a concurrent change is detected.
type claudeFirstRunFileState struct {
	uid, gid int
	perm     os.FileMode
	info     os.FileInfo
}

// readClaudeFirstRunFile reads path through a descriptor opened without
// following symlinks (and without blocking on a planted FIFO), and returns
// the owner and mode of the file it actually read.
func readClaudeFirstRunFile(path string) ([]byte, claudeFirstRunFileState, error) {
	var state claudeFirstRunFileState
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, state, fmt.Errorf("read: %w", err)
	}
	defer func() { _ = f.Close() }() // read-only fd; nothing to lose on close error
	info, err := f.Stat()
	if err != nil {
		return nil, state, fmt.Errorf("stat: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, state, fmt.Errorf("not a regular file (mode %s)", info.Mode())
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, state, errors.New("owner unknown")
	}
	data, err := io.ReadAll(io.LimitReader(f, claudeSessionAdoptMaxBytes+1))
	if err != nil {
		return nil, state, fmt.Errorf("read: %w", err)
	}
	if len(data) > claudeSessionAdoptMaxBytes {
		return nil, state, fmt.Errorf("over the %d-byte cap", claudeSessionAdoptMaxBytes)
	}
	state = claudeFirstRunFileState{uid: int(st.Uid), gid: int(st.Gid), perm: info.Mode().Perm(), info: info}
	return data, state, nil
}

// replaceClaudeFirstRunFile atomically replaces path with data, keeping the
// owner and mode recorded in orig. The staging file is given that owner and
// mode through its own descriptor before anything is written to it; when hive
// is not allowed to (an unprivileged hive and an agent-owned file), the seed
// is abandoned rather than publishing a file with a different owner.
func replaceClaudeFirstRunFile(path string, data []byte, orig claudeFirstRunFileState) error {
	return publishClaudeFirstRunFile(path, data, func(f *os.File) error {
		if err := f.Chown(orig.uid, orig.gid); err != nil {
			return fmt.Errorf("cannot keep owner %d:%d: %w", orig.uid, orig.gid, err)
		}
		if err := f.Chmod(orig.perm); err != nil {
			return fmt.Errorf("cannot keep mode %v: %w", orig.perm, err)
		}
		return nil
	}, func() error {
		now, err := os.Lstat(path)
		if err != nil || !os.SameFile(now, orig.info) || now.Size() != orig.info.Size() ||
			!now.ModTime().Equal(orig.info.ModTime()) {
			return errClaudeFirstRunChanged
		}
		return nil
	})
}

// createClaudeFirstRunFile creates path holding data for agentUID. The file is
// chowned to the agent through its descriptor. When that chown is refused and
// hive is root (so the file would stay root-owned) nothing is created; an
// unprivileged hive keeps it at claudeSessionSeedFileMode, the same trade-off
// seedClaudeSessionForAgent makes for an adopted session in a home the hive
// could not chown either.
func createClaudeFirstRunFile(path string, data []byte, agentUID int) error {
	return publishClaudeFirstRunFile(path, data, func(f *os.File) error {
		if err := f.Chown(agentUID, -1); err != nil && os.Geteuid() == 0 {
			return fmt.Errorf("cannot give the new file to uid %d: %w", agentUID, err)
		}
		if err := f.Chmod(claudeSessionSeedFileMode); err != nil {
			return fmt.Errorf("chmod: %w", err)
		}
		return nil
	}, func() error {
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			return errClaudeFirstRunChanged
		}
		return nil
	})
}

// publishClaudeFirstRunFile stages data next to path and renames it into
// place. prepare sets the staging file's owner and mode through its
// descriptor; stillCurrent re-checks path just before the rename. The staging
// file is removed on every failure, so a failed seed leaves nothing behind.
func publishClaudeFirstRunFile(path string, data []byte, prepare func(*os.File) error, stillCurrent func() error) (err error) {
	tmpPath := path + claudeFirstRunSeedTempInfix +
		strconv.Itoa(os.Getpid()) + "." + strconv.FormatInt(time.Now().UnixNano(), 36)
	f, err := os.OpenFile(tmpPath,
		os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("stage in %s: %w", filepath.Dir(path), err)
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if err = prepare(f); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if err = stillCurrent(); err != nil {
		return err
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
