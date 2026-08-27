package agent

// Cross-UID reads and writes of .claude.json for session adoption (#4637).
//
// THE DEFECT: #4619 gave every per-UID interactive agent its own HOME and
// seeded a signed-in .claude.json into it, "so existing hives migrate without
// any operator step". That works for a MIGRATING hive, where the signed-in
// source is the legacy /data/home/.claude.json the hive process itself can
// read. It cannot work on a FRESH install: the only signed-in file in
// existence is the one agent A's own CLI just wrote, owned by A's UID at mode
// 0600. inspectClaudeSession on that file returns claudeSessionUnreadable, so
// findSignedInClaudeSession sees no source, agent B stays at the login menu,
// and the operator logs in once per agent — while the docs promise "log in
// once per method" (docs/agent-configuration.md).
//
// THE FIX: stop asking the hive process to read what only an agent UID can
// read. Every other cross-UID operation in this package already goes through
// su-exec — tmuxCmd, setupCodexHome, the tmux-dir chmod — and this is the same
// shape. When a direct read hits EACCES, re-read as the file's OWNER; when a
// direct write hits EACCES, write as the TARGET agent. Adoption then
// propagates a fresh login across the fleet and the documented contract holds
// on fresh installs too.
//
// POSTURE: every helper here is best-effort and fails back to the direct
// result. su-exec is absent in unit tests and on developer laptops, unreadable
// stays unreadable when the fallback cannot run, and no caller may treat a
// failed fallback as evidence of anything. Files are Lstat'd (never followed)
// and must be regular, under the size cap, and owned by a NON-ROOT uid before
// a helper runs — a symlink or a root-owned file at a sibling path must never
// be able to turn this into "run something as uid 0".

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// claudeSessionAdoptMaxBytes caps how much of a .claude.json this will carry
// across UIDs. Claude Code accumulates per-project history in the file, so it
// grows without a natural bound; a few MiB is far above any observed session
// file while keeping a rogue or corrupted path from being slurped into memory.
const claudeSessionAdoptMaxBytes = 8 << 20

// claudeSessionAdoptTimeout bounds one su-exec helper invocation. The helpers
// are cat/sh against a local file: anything slower than this is a wedged
// process, not slow I/O, and a launch must not block on it.
const claudeSessionAdoptTimeout = 10 * time.Second

// runAsAgentUser runs argv as the su-exec user spec, feeds it stdin, and
// returns its stdout.
//
// Var (not func) as a TEST SEAM, matching the sharedAgentHome convention: unit
// tests cannot create files owned by another UID, so they substitute a runner
// that stands in for the privileged read. The production body is deliberately
// the same su-exec invocation setupCodexHome uses.
var runAsAgentUser = func(spec string, stdin []byte, argv ...string) ([]byte, error) {
	if spec == "" || len(argv) == 0 {
		return nil, errors.New("runAsAgentUser: empty user spec or argv")
	}
	if _, err := exec.LookPath("su-exec"); err != nil {
		return nil, fmt.Errorf("su-exec unavailable: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), claudeSessionAdoptTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "su-exec", append([]string{spec}, argv...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, outputErr("su-exec "+spec, err, stderr.Bytes())
	}
	return stdout.Bytes(), nil
}

// claudeSessionOwnerSpec returns the su-exec user spec for the owner of the
// session file at path, or an error explaining why no helper may run for it.
//
// Fails CLOSED, unlike inferenceHomeIsOwnedBy: the result decides whose
// identity a subprocess runs under, so every doubt (not a regular file, a
// symlink, oversized, no unix ownership, owned by root) is a refusal rather
// than a guess. Lstat, never Stat — a symlink planted at a sibling's
// .claude.json must not resolve to the ownership of whatever it points at.
func claudeSessionOwnerSpec(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file (mode %s)", path, info.Mode())
	}
	if info.Size() > claudeSessionAdoptMaxBytes {
		return "", fmt.Errorf("%s is %d bytes, over the %d-byte adoption cap", path, info.Size(), claudeSessionAdoptMaxBytes)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("%s: no unix ownership available", path)
	}
	if st.Uid == 0 {
		return "", fmt.Errorf("%s is owned by uid 0; refusing to run a helper as root", path)
	}
	return fmt.Sprintf("%d:%d", st.Uid, st.Gid), nil
}

// inspectClaudeSessionForAdoption classifies path exactly as
// inspectClaudeSession does, except that claudeSessionUnreadable is retried as
// the file's owner instead of being reported as a dead end.
//
// Both callers in the seeding path need this and for opposite reasons: a
// SOURCE that is unreadable is a login this fleet is failing to propagate, and
// a TARGET that is unreadable may be the agent's OWN signed-in session — which
// adopt-only must never overwrite. Before this, an unreadable target fell
// through to "not signed in" and was clobbered by a sibling's identity.
func (m *Manager) inspectClaudeSessionForAdoption(path string) claudeSessionInfo {
	info := inspectClaudeSession(path)
	if info.State != claudeSessionUnreadable {
		return info
	}
	data, err := m.readClaudeSessionAsOwner(path)
	if err != nil {
		// Debug, not Warn: on any deployment without su-exec this is the
		// ordinary outcome, and diagnoseStuckLogin already reports the
		// unreadable state where an operator will see it.
		m.logger.Debug("could not read agent-owned claude session",
			"path", path, "error", err)
		return info
	}
	return classifyClaudeSession(path, data)
}

// readClaudeSessionAsOwner reads path through su-exec as the file's owner.
func (m *Manager) readClaudeSessionAsOwner(path string) ([]byte, error) {
	spec, err := claudeSessionOwnerSpec(path)
	if err != nil {
		return nil, err
	}
	data, err := runAsAgentUser(spec, nil, "cat", path)
	if err != nil {
		return nil, err
	}
	if len(data) > claudeSessionAdoptMaxBytes {
		// The Lstat cap above is advisory — the file can grow between the stat
		// and the read — so re-check what actually came back.
		return nil, fmt.Errorf("%s returned %d bytes, over the %d-byte adoption cap",
			path, len(data), claudeSessionAdoptMaxBytes)
	}
	return data, nil
}

// readClaudeSessionForAdoption reads a seed source, falling back to a read as
// the file's owner when this process is not permitted to open it.
func (m *Manager) readClaudeSessionForAdoption(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return data, err
	}
	owned, oerr := m.readClaudeSessionAsOwner(path)
	if oerr != nil {
		return nil, errors.Join(err, oerr)
	}
	return owned, nil
}

// writeClaudeSessionForAgent writes the adopted session to target, falling
// back to a write performed BY the owning agent when this process cannot write
// it directly — which is the fresh-install case, where target is the agent's
// own 0600 file.
//
// The fallback stages through a temp file in the target's own directory and
// renames, so a helper that dies mid-write cannot leave the agent holding a
// truncated .claude.json. target is passed as $0 rather than interpolated into
// the script, so a path is data to the shell and never code.
//
// The staged file is chmod'ed to claudeSessionSeedFileMode — the SAME mode the
// direct write above uses, and deliberately not the 0600 the agent's own CLI
// would produce. A seeded session has to stay readable by the hive uid:
// #4636's live investigation found that a CLI spawned by the manager against a
// session file the hive process cannot read treats the home as a fresh install
// and rewrites the signed-in identity away. Agents whose files had been seeded
// at this mode survived that; agents holding an agent-written 0600 file did
// not. Both write paths must therefore land on the same mode or adoption would
// reintroduce the clobber it exists to prevent. The containment is the home
// itself (0700, agent-owned — see tightenInteractiveHome), exactly the
// inferenceConfigFileMode trade-off.
func (m *Manager) writeClaudeSessionForAgent(target string, data []byte, spec string) error {
	err := os.WriteFile(target, data, claudeSessionSeedFileMode)
	if err == nil || spec == "" || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	stageAndRename := fmt.Sprintf(
		`tmp="$0.hive-seed.$$"; cat >"$tmp" && chmod %#o "$tmp" && mv -f "$tmp" "$0" `+
			`|| { rm -f "$tmp"; exit 1; }`, claudeSessionSeedFileMode)
	if _, werr := runAsAgentUser(spec, data, "sh", "-c", stageAndRename, target); werr != nil {
		return errors.Join(err, werr)
	}
	return nil
}
