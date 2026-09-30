package agent

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Backend-native resume handles (hivecommons/hive#9606).
//
// SessionID/SendResumeKick above only reach a conversation that is still
// live in its tmux pane. A pod restart kills that pane, so the reasoning
// behind a PR the agent opened can only be recovered from whatever the
// backend CLI itself persisted. Several of the CLIs the hive drives do keep
// a transcript on disk under the agent's HOME and can be pointed back at it
// by id (claude --resume, copilot --resume, codex resume, gemini --resume,
// pi --session, omp --resume).
// This file captures that id at PR-open time so pkg/prfollowup can store it
// beside the PR's pointer and hand it to whichever session picks the PR up
// later.
//
// Capture is read-only and best effort: it stats the backend's session tree
// and names the newest transcript. Nothing here launches or changes a CLI —
// resuming a whole transcript on every follow-up would also resume its full
// token cost, so the handle is offered to the agent as a command it may run,
// alongside the compact handoff note.

const (
	// resumeWalkMaxDepth bounds how deep a backend's session tree is walked
	// below its root (codex buckets sessions under YYYY/MM/DD).
	resumeWalkMaxDepth = 4
	// resumeWalkMaxEntries bounds the directory entries one capture reads, so
	// a large history directory cannot stall the PR-opened hook.
	resumeWalkMaxEntries = 2000
	// resumeIDMinLen rejects obviously non-id names (a stray "tmp" directory).
	resumeIDMinLen = 8
	// resumeIDMaxLen bounds an id read from inside a transcript.
	resumeIDMaxLen = 128
	// resumeHeaderMaxBytes bounds how much of a transcript is read to find
	// the id recorded in its first line.
	resumeHeaderMaxBytes = 64 << 10
	// geminiProjectRootMarker is the file gemini writes in each project's
	// temp directory naming the project root it belongs to.
	geminiProjectRootMarker = ".project_root"
)

// ResumeHandle names a backend-native conversation that can be resumed after
// the authoring tmux pane is gone.
type ResumeHandle struct {
	// Backend is the CLI the handle belongs to ("claude", "copilot", ...).
	Backend string
	// SessionID is the backend's own conversation id.
	SessionID string
	// Transcript is the file the backend persists that conversation in, so a
	// fresh session can read the reasoning even if resume itself fails.
	Transcript string
	// Command resumes the conversation from a shell in the agent's pane.
	Command string
	// LastActive is the transcript's modification time when it was captured.
	LastActive time.Time
	// CapturedAt is when the hive captured the handle (PR-open time); the
	// staleness clock runs from here.
	CapturedAt time.Time
}

// backendResumeLayout describes where one backend keeps its transcripts and
// how to turn a path into a resume id and command.
type backendResumeLayout struct {
	// root resolves the directory holding the backend's sessions.
	root func(home, agentName string) string
	// dirSessions is true when each session is a DIRECTORY named by its id
	// (copilot), false when each session is a FILE named by its id (claude,
	// codex).
	dirSessions bool
	// transcript is the file inside a session directory that dates it. Only
	// meaningful when dirSessions is true.
	transcript string
	// ext is the transcript file extension for file sessions.
	ext string
	// id extracts the backend's conversation id from the session file or
	// directory name; "" means the name is not a session.
	id func(name string) string
	// command renders the invocation that resumes id.
	command func(id string) string
	// idFromFile, when set, reads the real conversation id from the newest
	// transcript; id then only selects which files are sessions. Used when
	// the file name carries a shortened id (gemini).
	idFromFile func(path string) string
	// project, when set, keeps only the directories directly under root
	// that belong to the agent's working directory. The backend's session
	// tree is fleet-shared (a bridged dot-directory) or holds sessions of
	// several working directories, so an unscoped walk could name another
	// agent's or another checkout's conversation; capture without a working
	// directory then yields no handle.
	project func(home, dir, workDir string) bool
}

// backendResumeLayouts holds the backends that expose a capturable resume
// handle. A backend that is absent here simply has no handle: its PRs keep
// the handoff note alone, which is the unchanged #9583 behaviour.
//
// The headless agy runner is deliberately excluded: its conversation id lives
// in a mktemp file that is discarded on every relaunch (agy_turn.go), so a
// captured id would point at a conversation the next launch never reopens.
var backendResumeLayouts = map[string]backendResumeLayout{
	"claude": {
		root:    func(home, _ string) string { return filepath.Join(home, ".claude", "projects") },
		ext:     ".jsonl",
		id:      func(name string) string { return trimResumeName(name, "", ".jsonl") },
		command: func(id string) string { return "claude --resume " + id },
	},
	"copilot": {
		root:        func(home, _ string) string { return filepath.Join(home, ".copilot", "session-state") },
		dirSessions: true,
		transcript:  "events.jsonl",
		id:          func(name string) string { return trimResumeName(name, "", "") },
		command:     func(id string) string { return "copilot --resume " + id },
	},
	"codex": {
		root:    func(_, agentName string) string { return filepath.Join(codexHomePath(agentName), "sessions") },
		ext:     ".jsonl",
		id:      codexRolloutID,
		command: func(id string) string { return "codex resume " + id },
	},
	// gemini keeps ~/.gemini/tmp/<project>/chats/session-<ts>-<id8>.jsonl.
	// The file name only carries the first 8 characters of the session id;
	// the full id is in the transcript's first (metadata) line.
	"gemini": {
		root:       func(home, _ string) string { return filepath.Join(home, ".gemini", "tmp") },
		ext:        ".jsonl",
		id:         func(name string) string { return trimResumeName(name, "session-", ".jsonl") },
		command:    func(id string) string { return "gemini --resume " + id },
		idFromFile: geminiSessionID,
		project:    geminiProjectDir,
	},
	// pi keeps ~/.pi/agent/sessions/--<cwd>--/<ts>_<id>.jsonl, one bucket
	// per working directory, and reopens a session with --session <id>.
	"pi": {
		root:    func(home, _ string) string { return filepath.Join(home, ".pi", "agent", "sessions") },
		ext:     ".jsonl",
		id:      timestampedSessionID,
		command: func(id string) string { return "pi --session " + id },
		project: piProjectDir,
	},
	// omp (a pi fork) keeps ~/.omp/agent/sessions/<bucket>/<ts>_<id>.jsonl,
	// bucketed like pi but home-relative for directories under HOME, and
	// reopens a session with --resume <id>.
	"omp": {
		root:    func(home, _ string) string { return filepath.Join(home, ".omp", "agent", "sessions") },
		ext:     ".jsonl",
		id:      timestampedSessionID,
		command: func(id string) string { return "omp --resume " + id },
		project: ompProjectDir,
	},
}

// BackendSupportsResumeHandle reports whether a backend keeps a transcript the
// hive can capture a resume id from.
func BackendSupportsResumeHandle(backend string) bool {
	_, ok := backendResumeLayouts[strings.TrimSpace(backend)]
	return ok
}

// trimResumeName strips a prefix and extension and rejects names too short to
// be a conversation id.
func trimResumeName(name, prefix, ext string) string {
	if prefix != "" {
		if !strings.HasPrefix(name, prefix) {
			return ""
		}
		name = strings.TrimPrefix(name, prefix)
	}
	name = strings.TrimSuffix(name, ext)
	if len(name) < resumeIDMinLen || strings.HasPrefix(name, ".") {
		return ""
	}
	return name
}

// codexRolloutID pulls the conversation uuid off a codex rollout file name
// ("rollout-2026-09-30T08-05-47-<uuid>.jsonl"). The timestamp in the middle
// also contains dashes, so the id is the trailing uuid-shaped run.
func codexRolloutID(name string) string {
	base := trimResumeName(name, "rollout-", ".jsonl")
	if base == "" {
		return ""
	}
	fields := strings.Split(base, "-")
	if len(fields) < 5 {
		return ""
	}
	id := strings.Join(fields[len(fields)-5:], "-")
	if !looksLikeUUID(id) {
		return ""
	}
	return id
}

func looksLikeUUID(s string) bool {
	groups := strings.Split(s, "-")
	want := []int{8, 4, 4, 4, 12}
	if len(groups) != len(want) {
		return false
	}
	for i, g := range groups {
		if len(g) != want[i] {
			return false
		}
		for _, r := range g {
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return false
			}
		}
	}
	return true
}

// isSafeResumeID reports whether an id read from inside a transcript is safe
// to render into a shell command: a bounded run of [A-Za-z0-9_-].
func isSafeResumeID(id string) bool {
	if len(id) < resumeIDMinLen || len(id) > resumeIDMaxLen {
		return false
	}
	for _, r := range id {
		if !(r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return true
}

// geminiSessionID reads the full session id from a gemini transcript's first
// line (its metadata record) and checks it against the shortened id in the
// file name, so a truncated or foreign file never yields a handle.
func geminiSessionID(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	line, err := bufio.NewReader(io.LimitReader(f, resumeHeaderMaxBytes)).ReadBytes('\n')
	if err != nil && err != io.EOF {
		return ""
	}
	var meta struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(line, &meta) != nil || !isSafeResumeID(meta.SessionID) {
		return ""
	}
	short := trimResumeName(filepath.Base(path), "session-", ".jsonl")
	if len(short) < 8 || !strings.HasSuffix(short, meta.SessionID[:8]) {
		return ""
	}
	return meta.SessionID
}

// timestampedSessionID pulls the session id off a pi/omp session file name
// ("2026-09-30T08-05-47-123Z_<id>.jsonl"). Files that do not start with a
// date (omp's per-subagent transcripts, <AgentId>.jsonl) are not sessions,
// and an id that is not a plain [A-Za-z0-9_-] token is never offered because
// it is rendered into a shell command.
func timestampedSessionID(name string) string {
	base := strings.TrimSuffix(name, ".jsonl")
	ts, id, ok := strings.Cut(base, "_")
	if !ok || len(ts) < 11 || ts[4] != '-' || ts[10] != 'T' {
		return ""
	}
	for _, r := range ts[:4] {
		if r < '0' || r > '9' {
			return ""
		}
	}
	if !isSafeResumeID(id) {
		return ""
	}
	return id
}

// encodeAbsoluteSessionDir is pi's session bucket name for cwd: the path
// without its leading separator, with separators and colons turned into
// dashes, wrapped in "--".
func encodeAbsoluteSessionDir(cwd string) string {
	trimmed := strings.TrimLeft(filepath.Clean(cwd), `/\`)
	return "--" + strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(trimmed) + "--"
}

// piProjectDir reports whether a bucket under ~/.pi/agent/sessions belongs to
// workDir. pi names the bucket after its process cwd, which the OS reports
// with symlinks resolved, so the canonical path is accepted too.
func piProjectDir(_, dir, workDir string) bool {
	name := filepath.Base(dir)
	return name == encodeAbsoluteSessionDir(workDir) || name == encodeAbsoluteSessionDir(canonicalPath(workDir))
}

// ompProjectDir reports whether a bucket under ~/.omp/agent/sessions belongs
// to workDir. omp resolves symlinks, then names the bucket "-<relative>" for
// a directory under HOME, "-tmp-<relative>" under the temp root, and pi's
// "--<absolute>--" otherwise (also the name older releases used everywhere).
func ompProjectDir(home, dir, workDir string) bool {
	name := filepath.Base(dir)
	if name == encodeAbsoluteSessionDir(workDir) {
		return true
	}
	cwd := canonicalPath(workDir)
	if name == encodeAbsoluteSessionDir(cwd) {
		return true
	}
	encode := strings.NewReplacer("/", "-", `\`, "-", ":", "-")
	relativeName := func(base, prefix string) (string, bool) {
		if base == "" {
			return "", false
		}
		rel, err := filepath.Rel(canonicalPath(base), cwd)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return "", false
		}
		if rel == "." {
			return prefix, true
		}
		if strings.HasSuffix(prefix, "-") {
			return prefix + encode.Replace(rel), true
		}
		return prefix + "-" + encode.Replace(rel), true
	}
	if want, ok := relativeName(home, "-"); ok {
		return name == want
	}
	if want, ok := relativeName(os.TempDir(), "-tmp"); ok {
		return name == want
	}
	return false
}

// canonicalPath resolves symlinks in p when it exists, else returns it
// cleaned.
func canonicalPath(p string) string {
	p = filepath.Clean(p)
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// geminiProjectDir reports whether a directory under ~/.gemini/tmp belongs to
// workDir: current gemini names it by a registry slug and records the
// project root in a marker file; older releases named it by the SHA-256 of
// the project root.
func geminiProjectDir(_, dir, workDir string) bool {
	workDir = filepath.Clean(workDir)
	if raw, err := os.ReadFile(filepath.Join(dir, geminiProjectRootMarker)); err == nil {
		return filepath.Clean(strings.TrimSpace(string(raw))) == workDir
	}
	sum := sha256.Sum256([]byte(workDir))
	return filepath.Base(dir) == hex.EncodeToString(sum[:])
}

// CaptureResumeHandle names the newest conversation the agent's backend has
// persisted under its own HOME, or reports false when the backend keeps none
// (or has not written one yet). It never fails loudly: a missing tree, an
// unreadable directory and an unsupported backend are all "no handle", and
// the PR then keeps the handoff note alone. Backends whose session tree is
// scoped by project (gemini, pi, omp) need the agent's working directory; use
// CaptureResumeHandleIn for those.
func CaptureResumeHandle(agentName string, uid int, backend string, now time.Time) (ResumeHandle, bool) {
	return CaptureResumeHandleIn(agentName, uid, backend, "", now)
}

// CaptureResumeHandleIn is CaptureResumeHandle for an agent whose CLI runs in
// workDir. For a project-scoped backend only sessions of that project are
// considered, and an empty workDir yields no handle.
func CaptureResumeHandleIn(agentName string, uid int, backend, workDir string, now time.Time) (ResumeHandle, bool) {
	backend = strings.TrimSpace(backend)
	layout, ok := backendResumeLayouts[backend]
	if !ok {
		return ResumeHandle{}, false
	}
	if layout.project != nil && strings.TrimSpace(workDir) == "" {
		return ResumeHandle{}, false
	}
	home := AgentHome(agentName, uid, backend)
	root := layout.root(home, agentName)
	if root == "" {
		return ResumeHandle{}, false
	}
	id, path, modTime, ok := newestResumeSession(root, home, workDir, layout)
	if !ok {
		return ResumeHandle{}, false
	}
	if layout.idFromFile != nil {
		if id = layout.idFromFile(path); id == "" {
			return ResumeHandle{}, false
		}
	}
	return ResumeHandle{
		Backend:    backend,
		SessionID:  id,
		Transcript: path,
		Command:    layout.command(id),
		LastActive: modTime,
		CapturedAt: now,
	}, true
}

// newestResumeSession walks root for the most recently written session and
// returns its id, transcript path and modification time.
func newestResumeSession(root, home, workDir string, layout backendResumeLayout) (id, path string, modTime time.Time, ok bool) {
	budget := resumeWalkMaxEntries
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > resumeWalkMaxDepth || budget <= 0 {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if budget--; budget <= 0 {
				return
			}
			name := entry.Name()
			full := filepath.Join(dir, name)
			if entry.IsDir() {
				if layout.dirSessions {
					if sid := layout.id(name); sid != "" {
						consider(&id, &path, &modTime, &ok, sid, filepath.Join(full, layout.transcript))
						continue
					}
				}
				if depth == 0 && layout.project != nil && !layout.project(home, full, workDir) {
					continue
				}
				walk(full, depth+1)
				continue
			}
			if layout.dirSessions || !strings.HasSuffix(name, layout.ext) {
				continue
			}
			if sid := layout.id(name); sid != "" {
				consider(&id, &path, &modTime, &ok, sid, full)
			}
		}
	}
	walk(root, 0)
	return id, path, modTime, ok
}

// consider keeps candidate when its transcript is newer than the best so far.
func consider(id, path *string, modTime *time.Time, ok *bool, candidateID, candidatePath string) {
	info, err := os.Stat(candidatePath)
	if err != nil || info.IsDir() {
		return
	}
	if *ok && !info.ModTime().After(*modTime) {
		return
	}
	*id, *path, *modTime, *ok = candidateID, candidatePath, info.ModTime(), true
}

// ResumeHandle captures the backend-native resume handle of the named agent's
// current conversation, or false when the agent is unknown, not running, or
// on a backend with no capturable handle.
func (m *Manager) ResumeHandle(name string) (ResumeHandle, bool) {
	if m == nil {
		return ResumeHandle{}, false
	}
	m.mu.RLock()
	agent, found := m.agents[name]
	if !found || agent == nil || agent.State != StateRunning {
		m.mu.RUnlock()
		return ResumeHandle{}, false
	}
	backend, uid := effectiveBackend(agent), agent.UID
	workDir := ""
	if m.workDir != "" {
		workDir = filepath.Join(m.workDir, name)
	}
	m.mu.RUnlock()
	return CaptureResumeHandleIn(name, uid, backend, workDir, time.Now())
}

// String renders a handle for logs.
func (h ResumeHandle) String() string {
	if h.SessionID == "" {
		return ""
	}
	return fmt.Sprintf("%s session %s (%s)", h.Backend, h.SessionID, h.Transcript)
}

// TranscriptExists reports whether the captured transcript is still on disk.
func (h ResumeHandle) TranscriptExists() bool {
	if h.Transcript == "" {
		return false
	}
	_, err := os.Stat(h.Transcript)
	return err == nil
}
