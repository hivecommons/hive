package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Copilot route for the repeat-failure check (hivecommons/hive#14550, C1, C3,
// C12, C14; issue #14746).
//
// Copilot runs every shell tool call in one long-lived Bash session per shell
// id, so the neutral BASH_ENV route never sees its commands. Copilot already
// logs each call in its session log
// (~/.copilot/session-state/<id>/events.jsonl under the agent's COPILOT_HOME):
// a `tool.execution_start` with the command, then a `tool.execution_complete`
// whose `shellExecution.exitCode` is the command's exit status. Copilot sets
// `success: true` for a command that exited non-zero, so the exit code wins
// over `success`.
//
// Hive reads those logs itself on every collection while the check is on and
// appends one shared record per completed shell call to the agent's raw
// record file, which the collector then drains like any other route. Nothing
// is written into Copilot's own files, so with the check off Hive adds
// nothing (C14). Each session log is read from where the previous collection
// stopped; a log that already existed when the reader started is read from its
// end, so a Hive restart does not replay history, and a toolCallId is recorded
// at most once even when a log is rewritten and read again.
//
// The logs live under the agent's HOME and Hive reads them as itself (root in
// multi-uid mode) while the extracted commands land in a file the agent can
// read, so the reader never follows a link the agent could plant: the
// session-state tree, every session directory and every events.jsonl are
// Lstat-checked, the log is opened O_NOFOLLOW, and in multi-uid mode a log
// not owned by the agent's uid is skipped (audit F12 discipline, as on the
// record-file side).

// RepeatFailureRouteCopilotEvents names the Copilot session-log reader in a
// record's Route.
const RepeatFailureRouteCopilotEvents = "copilot_events"

const (
	// copilotRecordReadMax bounds how much of one session log a collection
	// reads; the rest is read on the next collection.
	copilotRecordReadMax = 32 << 20
	// copilotRecordSeenMax bounds the toolCallIds remembered per session.
	copilotRecordSeenMax = 8192
)

// copilotShellTools are the Copilot tools that run a shell command. The
// read_/write_/stop_ companions only talk to a running shell.
var copilotShellTools = map[string]bool{"bash": true, "powershell": true}

// copilotRecordSession is the reader's position in one session log.
type copilotRecordSession struct {
	offset int64
	// commands maps the toolCallId of a shell call that has started but not
	// completed to its command.
	commands map[string]string
	// seen holds the toolCallIds already recorded.
	seen map[string]bool
}

// copilotRecordReader follows one agent's Copilot session logs.
type copilotRecordReader struct {
	started  bool
	sessions map[string]*copilotRecordSession
}

func newCopilotRecordReader() *copilotRecordReader {
	return &copilotRecordReader{sessions: map[string]*copilotRecordSession{}}
}

// scan reads what was appended to every session log under home since the last
// scan. emit receives one record per completed shell call and observe the
// shell activity (finished false at start, true at completion). When uid > 0
// only logs owned by uid are read.
func (r *copilotRecordReader) scan(home string, uid int, emit func(RepeatFailureRecord), observe func(finished bool, at time.Time)) {
	root := filepath.Join(home, ".copilot", "session-state")
	if !copilotRecordRealDir(filepath.Join(home, ".copilot")) || !copilotRecordRealDir(root) {
		r.started = true
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		r.started = true
		return
	}
	present := make(map[string]bool, len(entries))
	for _, entry := range entries {
		// A symlinked session directory is not a directory to ReadDir.
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		present[name] = true
		info, err := os.Lstat(filepath.Join(root, name, "events.jsonl"))
		if err != nil || !info.Mode().IsRegular() || !copilotRecordOwnedBy(info, uid) {
			continue
		}
		sess := r.sessions[name]
		if sess == nil {
			sess = &copilotRecordSession{commands: map[string]string{}, seen: map[string]bool{}}
			if !r.started {
				sess.offset = info.Size()
			}
			r.sessions[name] = sess
		}
		if info.Size() < sess.offset {
			// Rewritten: read it again; seen keeps each call to one record.
			sess.offset = 0
		}
		if info.Size() > sess.offset {
			r.readSession(filepath.Join(root, name, "events.jsonl"), sess, info.Size(), emit, observe)
		}
	}
	for name := range r.sessions {
		if !present[name] {
			delete(r.sessions, name)
		}
	}
	r.started = true
}

// copilotRecordRealDir reports whether path is a directory and not a link.
func copilotRecordRealDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir()
}

// copilotRecordOwnedBy reports whether info's file belongs to uid. Any uid
// passes with uid <= 0 (single-uid mode, where Hive and the agents share one
// user) and nothing passes on a platform without Stat_t.
func copilotRecordOwnedBy(info os.FileInfo, uid int) bool {
	if uid <= 0 {
		return true
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == uid
}

func (r *copilotRecordReader) readSession(path string, sess *copilotRecordSession, size int64, emit func(RepeatFailureRecord), observe func(bool, time.Time)) {
	readAppendedLines(path, sess, size, func(line []byte) {
		sess.handleLine(line, emit, observe)
	})
}

// readAppendedLines reads the complete lines appended to the log at path
// since sess.offset (at most copilotRecordReadMax bytes, up to size) and
// passes each to handle. The offset moves past the lines handled.
func readAppendedLines(path string, sess *copilotRecordSession, size int64, handle func(line []byte)) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return
	}
	defer f.Close()
	// The log was Lstat-checked a moment ago; make sure the descriptor still
	// holds a regular file and not something swapped in since.
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return
	}
	n := min(size-sess.offset, copilotRecordReadMax)
	buf := make([]byte, n)
	read, err := f.ReadAt(buf, sess.offset)
	if err != nil && err != io.EOF {
		return
	}
	buf = buf[:read]
	end := bytes.LastIndexByte(buf, '\n')
	if end < 0 {
		if int64(read) == copilotRecordReadMax {
			// One line larger than a whole read: skip it.
			sess.offset += int64(read)
		}
		return
	}
	sess.offset += int64(end + 1)
	for _, line := range bytes.Split(buf[:end], []byte{'\n'}) {
		handle(line)
	}
}

// copilotShellArgs is the argument shape of Copilot's shell tools.
type copilotShellArgs struct {
	Command string `json:"command"`
}

// copilotShellTelemetry is the part of tool.execution_complete's
// toolTelemetry that says whether the call returned before the command ended.
type copilotShellTelemetry struct {
	Properties struct {
		ExecutionMode any `json:"executionMode"`
		Detached      any `json:"detached"`
	} `json:"properties"`
}

func copilotShellCommand(args json.RawMessage) string {
	var a copilotShellArgs
	if json.Unmarshal(args, &a) != nil {
		return ""
	}
	return a.Command
}

func (sess *copilotRecordSession) start(id, tool string, args json.RawMessage) bool {
	if id == "" || !copilotShellTools[tool] || sess.seen[id] {
		return false
	}
	if cmd := copilotShellCommand(args); strings.TrimSpace(cmd) != "" {
		sess.commands[id] = cmd
		return true
	}
	return false
}

func (sess *copilotRecordSession) handleLine(line []byte, emit func(RepeatFailureRecord), observe func(bool, time.Time)) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var ev copilotTranscriptEvent
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	ts := parseTranscriptTime(ev.Timestamp)
	switch ev.Type {
	case "assistant.message":
		for _, req := range ev.Data.ToolRequests {
			sess.start(req.ToolCallID, req.Name, req.Arguments)
		}
	case "tool.execution_start":
		if sess.start(ev.Data.ToolCallID, ev.Data.ToolName, ev.Data.Arguments) {
			observe(false, ts)
		}
	case "tool.execution_complete":
		id := ev.Data.ToolCallID
		cmd, ok := sess.commands[id]
		if !ok {
			return
		}
		delete(sess.commands, id)
		if len(sess.seen) >= copilotRecordSeenMax {
			sess.seen = map[string]bool{}
		}
		sess.seen[id] = true
		observe(true, ts)
		rec, ok := copilotShellRecord(ev, cmd, ts)
		if ok {
			emit(rec)
		}
	}
}

// copilotShellRecord builds the record of one completed shell call. A call
// that returned while its command keeps running (async or detached) has no
// result yet and is not recorded.
func copilotShellRecord(ev copilotTranscriptEvent, cmd string, ts time.Time) (RepeatFailureRecord, bool) {
	rec := RepeatFailureRecord{Backend: "copilot", Time: ts, RawCommand: cmd, Route: RepeatFailureRouteCopilotEvents}
	var shell struct {
		ExitCode *float64 `json:"exitCode"`
	}
	if len(ev.Data.ShellExecution) > 0 && json.Unmarshal(ev.Data.ShellExecution, &shell) == nil && shell.ExitCode != nil {
		code := int(*shell.ExitCode)
		rec.ExitCode = &code
		rec.Failed = code != 0
		return rec, true
	}
	var tel copilotShellTelemetry
	if len(ev.Data.ToolTelemetry) > 0 && json.Unmarshal(ev.Data.ToolTelemetry, &tel) == nil {
		if mode, _ := tel.Properties.ExecutionMode.(string); strings.EqualFold(mode, "async") {
			return rec, false
		}
		switch d := tel.Properties.Detached.(type) {
		case bool:
			if d {
				return rec, false
			}
		case string:
			if strings.EqualFold(d, "true") {
				return rec, false
			}
		}
	}
	if ev.Data.Success == nil {
		return rec, false
	}
	rec.Failed = !*ev.Data.Success
	return rec, true
}

// readCopilotRepeatFailureRecords runs every Copilot agent's session-log
// reader and appends the records to the agent's raw record file, so the
// collection that follows picks them up. Only called while the check is on.
func (m *Manager) readCopilotRepeatFailureRecords() {
	paths := m.repeatFailureRecordPaths()
	type copilotAgent struct {
		name string
		uid  int
		home string
	}
	var agents []copilotAgent
	m.mu.RLock()
	for name, a := range m.agents {
		if effectiveBackend(a) != "copilot" || paths[name] == "" {
			continue
		}
		agents = append(agents, copilotAgent{name: name, uid: a.UID, home: AgentHome(name, a.UID, "copilot")})
	}
	m.mu.RUnlock()

	mon := &m.repeatFailure
	mon.mu.Lock()
	readers := make(map[string]*copilotRecordReader, len(agents))
	for _, a := range agents {
		r := mon.copilot[a.name]
		if r == nil {
			r = newCopilotRecordReader()
		}
		readers[a.name] = r
	}
	mon.copilot = readers
	mon.mu.Unlock()

	for _, a := range agents {
		var records []RepeatFailureRecord
		readers[a.name].scan(a.home, a.uid, func(rec RepeatFailureRecord) {
			rec.Agent = a.name
			records = append(records, rec)
		}, func(finished bool, at time.Time) {
			m.ObserveRepeatFailureTool(a.name, RepeatFailureToolShell, finished, at)
		})
		if len(records) == 0 {
			continue
		}
		path := paths[a.name]
		if err := EnsureRepeatFailureRecordDir(path, a.uid); err != nil {
			m.logger.Warn("repeat-failure: cannot create the record dir for Copilot records", "agent", a.name, "error", err)
			continue
		}
		for _, rec := range records {
			if err := AppendRepeatFailureRecord(path, rec); err != nil {
				m.logger.Warn("repeat-failure: writing a Copilot command record failed", "agent", a.name, "error", err)
				break
			}
		}
	}
}
