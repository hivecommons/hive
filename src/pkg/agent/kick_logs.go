package agent

// Durable per-kick agent run logs (#4296, #4295).
//
// The dashboard's "full log" is a live capture-pane of the agent's CURRENT
// tmux session, so a restart — or a hive upgrade, which replaces the whole
// container — destroys every previous run's scrollback. Operators debugging
// why an agent misbehaved several kicks ago had nothing to look at.
//
// This file archives the scrollback of each kick to a durable file BEFORE the
// content is lost:
//
//   - at kick delivery (deliverKickLocked), the outgoing scrollback — the
//     previous kick's output — is snapshotted to a file and the tmux history
//     is then cleared, so each archive covers exactly one kick;
//   - at Restart, the scrollback is snapshotted BEFORE kill-session runs;
//   - at graceful shutdown (SIGTERM: pod roll, hive upgrade), every agent
//     with un-archived kick output is snapshotted via ArchiveAllKickLogs.
//
// Archives live under kickLogDir (default /data/logs/kicks — the /data PVC
// survives agent restarts, pod rolls, and hive image upgrades) as
// <agent>/<timestamp>-<reason>.log, pruned to the newest kickLogRetention
// files and kickLogMaxBytes total bytes per agent.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// kickLogDirEnv overrides the root directory per-kick log archives are
	// written under. Empty (unset) uses defaultKickLogDir.
	kickLogDirEnv = "HIVE_KICK_LOG_DIR"

	// defaultKickLogDir is on the /data persistent volume so archived kick
	// logs survive agent restarts, pod rolls, and hive image upgrades. It
	// sits beside the other durable log roots (Data.LogsDir = /data/logs).
	defaultKickLogDir = "/data/logs/kicks"

	// kickLogRetentionEnv overrides how many archived kick logs are kept per
	// agent. 0 disables archiving entirely.
	kickLogRetentionEnv = "HIVE_KICK_LOG_RETENTION"

	// defaultKickLogRetention keeps the last 10 kicks per agent — enough to
	// reach "the run several kicks ago" (#4296) without unbounded growth.
	defaultKickLogRetention = 10

	// kickLogMaxBytesEnv overrides the per-agent total size cap (in bytes)
	// across an agent's archived kick logs.
	kickLogMaxBytesEnv = "HIVE_KICK_LOG_MAX_BYTES"

	// defaultKickLogMaxBytes caps one agent's archive directory. A full
	// 50000-line scrollback of 200-column lines is ~10 MiB, so 64 MiB
	// comfortably holds the default retention of even very chatty runs.
	defaultKickLogMaxBytes = 64 << 20

	// kickLogTimestampFormat is the filename prefix. Lexicographic order of
	// this format IS chronological order, which is what pruning and listing
	// sort by. Millisecond precision keeps two archives in the same second
	// (kick storm, restart-then-kick) from colliding.
	kickLogTimestampFormat = "20060102-150405.000"

	// kickLogSuffix is the archive file extension.
	kickLogSuffix = ".log"

	// kickLogPromptSnippetLen bounds the prompt snippet recorded in an
	// archive's header, mirroring KickRecord.Snippet's truncation.
	kickLogPromptSnippetLen = 120

	// Archive directories and files are owner-only: they are written and
	// served by the hive process itself, and raw pane content may hold
	// secrets that the dashboard redacts only at serve time — peer agent
	// UIDs must not be able to read them off /data directly.
	kickLogDirMode  = os.FileMode(0o700)
	kickLogFileMode = os.FileMode(0o600)
)

// KickLogInfo describes one archived kick log, as listed by ListKickLogs and
// the dashboard's kick-history endpoint.
type KickLogInfo struct {
	// ID is the archive's filename — the handle ReadKickLog accepts.
	ID string `json:"id"`
	// Timestamp is when the archive was written (parsed from the filename,
	// falling back to the file mtime).
	Timestamp time.Time `json:"timestamp"`
	// SizeBytes is the archive size on disk.
	SizeBytes int64 `json:"size_bytes"`
	// Reason says what triggered the snapshot: "kick" (a newer kick rotated
	// this one out), "restart", or "shutdown" (pod roll / hive upgrade).
	Reason string `json:"reason"`
}

// kickLogSettingsFromEnv resolves the archive root, per-agent retention
// count, and per-agent byte cap, applying env overrides over the defaults.
// Invalid or negative overrides fall back to the defaults; retention 0 is a
// valid explicit "archiving off".
func kickLogSettingsFromEnv() (dir string, retention int, maxBytes int64) {
	dir = os.Getenv(kickLogDirEnv)
	if dir == "" {
		dir = defaultKickLogDir
	}
	retention = defaultKickLogRetention
	if v := os.Getenv(kickLogRetentionEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			retention = n
		}
	}
	maxBytes = defaultKickLogMaxBytes
	if v := os.Getenv(kickLogMaxBytesEnv); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			maxBytes = n
		}
	}
	return dir, retention, maxBytes
}

// SetKickLogDir points archives at dir (tests; empty restores the env/default
// resolution done in NewManager).
func (m *Manager) SetKickLogDir(dir string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if dir == "" {
		dir, _, _ = kickLogSettingsFromEnv()
	}
	m.kickLogDir = dir
}

// sanitizeKickLogComponent reduces a name to characters safe in a filename
// component so an agent name or reason can never smuggle a path separator
// into the archive path. Mirrors dashboard.sanitizeLogFilename.
func sanitizeKickLogComponent(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if out == "" {
		return "agent"
	}
	return out
}

// agentKickLogDir is the directory one agent's archives live in.
func (m *Manager) agentKickLogDir(name string) string {
	return filepath.Join(m.kickLogDir, sanitizeKickLogComponent(name))
}

// captureScrollbackForAgent pulls the agent's full retained scrollback, via
// the test seam when one is wired. It is the single capture path shared by
// CaptureFullLog (live "full log") and archiveKickLogLocked (durable
// snapshot), so both always see the same bytes.
func (m *Manager) captureScrollbackForAgent(agent *AgentProcess) (string, error) {
	return m.terminalSession().CaptureFullLog(agent)
}

// clearScrollbackForAgent drops the session's scrollback history (the visible
// pane is untouched) so the NEXT archive covers only the kick being delivered
// now. Only called after its content has been archived.
func (m *Manager) clearScrollbackForAgent(agent *AgentProcess) {
	m.terminalSession().ClearHistory(agent)
}

// archiveKickLogLocked snapshots the agent's current scrollback to a durable
// per-kick file and prunes the agent's archive directory to the retention
// policy. Returns true when a file was written. Callers must hold m.mu.
//
// It must run BEFORE whatever destroys the scrollback (kill-session on
// restart, container teardown on shutdown, clear-history on kick rotation) —
// that ordering is the entire point of this feature.
func (m *Manager) archiveKickLogLocked(agent *AgentProcess, reason string) bool {
	return m.archiveKickLogBytesLocked(agent, reason) > 0
}

// archiveKickLogBytesLocked is archiveKickLogLocked's implementation, reporting
// the archived scrollback size instead of a bare success bool. The turn-loss
// instrument (turn_loss.go) needs that size as its proxy for how much turn
// output a teardown discarded; every other caller only asks whether a file was
// written and goes through the bool wrapper above.
func (m *Manager) archiveKickLogBytesLocked(agent *AgentProcess, reason string) int {
	if m.kickLogRetention <= 0 || m.kickLogDir == "" {
		return 0
	}
	if agent.tmuxSession == "" {
		return 0
	}
	content, err := m.captureScrollbackForAgent(agent)
	if err != nil {
		m.logger.Warn("kick log archive: capture failed", "name", agent.Name, "reason", reason, "error", err)
		return 0
	}
	if strings.TrimSpace(content) == "" {
		return 0
	}

	now := time.Now()
	kickStarted := "unknown"
	if agent.LastKick != nil {
		kickStarted = agent.LastKick.UTC().Format(time.RFC3339)
	}
	header := fmt.Sprintf(
		"==== hive kick log ====\nagent: %s\narchived: %s\nreason: %s\nkick_started: %s\nkick_prompt: %s\n=======================\n",
		agent.Name,
		now.UTC().Format(time.RFC3339),
		reason,
		kickStarted,
		truncateStr(strings.ReplaceAll(agent.LastKickMessage, "\n", " "), kickLogPromptSnippetLen),
	)

	dir := m.agentKickLogDir(agent.Name)
	if err := os.MkdirAll(dir, kickLogDirMode); err != nil {
		m.logger.Warn("kick log archive: mkdir failed", "name", agent.Name, "dir", dir, "error", err)
		return 0
	}
	fname := now.UTC().Format(kickLogTimestampFormat) + "-" + sanitizeKickLogComponent(reason) + kickLogSuffix
	path := filepath.Join(dir, fname)
	if err := os.WriteFile(path, []byte(header+content), kickLogFileMode); err != nil {
		m.logger.Warn("kick log archive: write failed", "name", agent.Name, "path", path, "error", err)
		return 0
	}
	m.logger.Info("archived kick log", "name", agent.Name, "reason", reason, "file", fname, "bytes", len(header)+len(content))

	m.pruneKickLogs(dir)
	m.notifyKickObserver(agent.Name, KickObserverEventArchived, reason)
	// The content length, not the file length: the header is hive's own
	// framing, and counting it would credit every interruption with a constant
	// few hundred bytes of work that the agent never produced.
	return len(content)
}

// rotateKickLogOnKickLocked runs at the top of deliverKickLocked, before any
// input reaches the pane: if a previous kick's output is sitting in the
// scrollback (kickLogPending), archive it and clear the history so the new
// kick starts a fresh, cleanly-delimited log. Callers must hold m.mu.
func (m *Manager) rotateKickLogOnKickLocked(agent *AgentProcess) {
	if !agent.kickLogPending {
		return
	}
	if m.archiveKickLogLocked(agent, "kick") {
		m.clearScrollbackForAgent(agent)
	}
	agent.kickLogPending = false
}

// ArchiveAllKickLogs snapshots every agent whose session holds un-archived
// kick output. Wired to graceful shutdown (SIGTERM) so a pod roll or hive
// image upgrade — which destroys every tmux server in the container — does
// not take the in-flight kick's log with it.
func (m *Manager) ArchiveAllKickLogs(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, agent := range m.agents {
		// tearDownTurnLocked no-ops unless kickLogPending, and records what the
		// teardown discarded (#4002) as well as archiving it.
		m.tearDownTurnLocked(agent, reason)
	}
}

// pruneKickLogs enforces the retention policy on one agent's archive
// directory: keep the newest kickLogRetention files, then drop the oldest
// files until the directory total fits kickLogMaxBytes (the newest archive is
// always kept, even if it alone exceeds the cap).
func (m *Manager) pruneKickLogs(dir string) {
	infos, err := readKickLogDir(dir)
	if err != nil {
		return
	}
	// readKickLogDir returns newest-first.
	var total int64
	for i, info := range infos {
		total += info.SizeBytes
		if i == 0 {
			continue // never delete the newest archive
		}
		if i >= m.kickLogRetention || total > m.kickLogMaxBytes {
			if err := os.Remove(filepath.Join(dir, info.ID)); err != nil {
				m.logger.Warn("kick log prune failed", "file", info.ID, "error", err)
			}
		}
	}
}

// readKickLogDir lists a directory's archives newest-first. A missing
// directory is "no history yet", not an error — new code must not crash when
// less history is available than normal (#4296).
func readKickLogDir(dir string) ([]KickLogInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []KickLogInfo{}, nil
		}
		return nil, err
	}
	infos := make([]KickLogInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), kickLogSuffix) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		infos = append(infos, KickLogInfo{
			ID:        e.Name(),
			Timestamp: kickLogTimestamp(e.Name(), fi.ModTime()),
			SizeBytes: fi.Size(),
			Reason:    kickLogReason(e.Name()),
		})
	}
	// Filename timestamps sort lexicographically == chronologically; sort by
	// name descending for newest-first.
	sort.Slice(infos, func(i, j int) bool { return infos[i].ID > infos[j].ID })
	return infos, nil
}

// kickLogTimestamp parses the archive time from a filename, falling back to
// the file mtime for names that do not match the format.
func kickLogTimestamp(name string, fallback time.Time) time.Time {
	if len(name) >= len(kickLogTimestampFormat) {
		if ts, err := time.ParseInLocation(kickLogTimestampFormat, name[:len(kickLogTimestampFormat)], time.UTC); err == nil {
			return ts
		}
	}
	return fallback
}

// kickLogReason extracts the snapshot reason ("kick", "restart", "shutdown")
// from a filename of the form <timestamp>-<reason>.log.
func kickLogReason(name string) string {
	base := strings.TrimSuffix(name, kickLogSuffix)
	if len(base) > len(kickLogTimestampFormat)+1 {
		return base[len(kickLogTimestampFormat)+1:]
	}
	return ""
}

// ListKickLogs returns the agent's archived kick logs, newest first. An agent
// with no archives yet gets an empty list, never an error.
func (m *Manager) ListKickLogs(name string) ([]KickLogInfo, error) {
	m.mu.RLock()
	_, ok := m.agents[name]
	dir := m.agentKickLogDir(name)
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("agent %s not found", name)
	}
	return readKickLogDir(dir)
}

// ReadKickLog returns the content of one archived kick log by the ID that
// ListKickLogs reported. The ID is validated as a bare archive filename so a
// crafted value can never traverse out of the agent's archive directory.
func (m *Manager) ReadKickLog(name, id string) (string, error) {
	m.mu.RLock()
	_, ok := m.agents[name]
	dir := m.agentKickLogDir(name)
	m.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("agent %s not found", name)
	}
	if !validKickLogID(id) {
		return "", fmt.Errorf("invalid kick log id %q", id)
	}
	data, err := os.ReadFile(filepath.Join(dir, id))
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("kick log %s not found for agent %s", id, name)
		}
		return "", fmt.Errorf("reading kick log %s for %s: %w", id, name, err)
	}
	return string(data), nil
}

// validKickLogID accepts only names archiveKickLogLocked can produce: a bare
// filename (no separators, no dot-dot) ending in the archive suffix with a
// conservative character set.
func validKickLogID(id string) bool {
	if id == "" || !strings.HasSuffix(id, kickLogSuffix) || strings.TrimSuffix(id, kickLogSuffix) == "" {
		return false
	}
	if id != filepath.Base(id) || strings.Contains(id, "..") {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}
