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

// Muse Code route for the repeat-failure check (hivecommons/hive#14550, C1,
// C3, C12, C14; issue #14749).
//
// Muse Code runs shell commands in its own managed shell inside a closed
// binary, so the neutral BASH_ENV route never sees them, and its hooks load
// only from ~/.config/muse/settings.json (fleet-shared, not Hive's), a
// trust-gated project .muse/hooks.json, or a root-owned managed package;
// there is no launch flag or env var for a Hive-owned hook file. Muse Code
// already journals every session to an append-only log
// ($XDG_DATA_HOME/muse/sessions/YYYY/MM/DD/<session id>/session.jsonl,
// documented: https://dev.meta.ai/docs/cookbook/audit-agent-sessions), one
// JSON envelope per line. A shell call is an `assistant_tool_calls_committed`
// event whose tool call is named `bash` (with the command in its JSON
// `args`), then a `tool_result_batch_committed` event whose result for that
// tool_call_id is a JSON text carrying the command's `exit_code` (recorded
// from the real CLI by testdata/repeat_failure/muse/run.sh).
//
// Hive reads those logs itself on every collection while the check is on and
// appends one shared record per finished bash call to the agent's raw record
// file. Nothing is written into Muse Code's own files or launch, so with the
// check off Hive adds nothing and has nothing to remove (C14). Each log is
// read from where the previous collection stopped; a log that already existed
// when the reader started is read from its end, so a Hive restart does not
// replay history, and a tool_call_id is recorded at most once. Subagent logs
// (<session id>/subagent/<child id>/session.jsonl) are read the same way.
//
// The logs live under the agent's XDG data home and Hive reads them as itself,
// so the reader keeps the Copilot reader's discipline: every directory and log
// is Lstat-checked, the log is opened O_NOFOLLOW, and in multi-uid mode a log
// not owned by the agent's uid is skipped.

// RepeatFailureRouteMuseSessions names the Muse Code session-log reader in a
// record's Route.
const RepeatFailureRouteMuseSessions = "muse_sessions"

const (
	// museShellTool is Muse Code's shell tool; bash_input only talks to a
	// running command.
	museShellTool = "bash"
	// museSessionLog is the file name of a session's append-only log.
	museSessionLog = "session.jsonl"
	// museTerminalTimedOut is the terminal_status of a command killed at its
	// deadline.
	museTerminalTimedOut = "timed_out"
)

// museRecordReader follows one agent's Muse Code session logs. Sessions are
// keyed by the log's path relative to the sessions root.
type museRecordReader struct {
	started  bool
	sessions map[string]*copilotRecordSession
}

func newMuseRecordReader() *museRecordReader {
	return &museRecordReader{sessions: map[string]*copilotRecordSession{}}
}

// museSessionsRoot returns where Muse Code keeps the agent's session logs:
// ${XDG_DATA_HOME:-$HOME/.local/share}/muse/sessions, with XDG_DATA_HOME as
// buildAgentEnv exports it (per-agent homes) or as Hive's own environment
// passes it on (agents without a UID).
func museSessionsRoot(name string, uid int, home string) string {
	data := filepath.Join(home, xdgDataHomeRel)
	if xdgHome, ok := perAgentXDGHome(name, uid, "muse"); ok {
		data = agentXDGDataHome(xdgHome)
	} else if uid <= 0 {
		if env := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(env) {
			data = env
		}
	}
	return filepath.Join(data, "muse", "sessions")
}

// museRealSubdirs lists the subdirectories of dir that are not links.
func museRealSubdirs(dir string) []string {
	if !copilotRecordRealDir(dir) {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		// A symlinked directory is not a directory to ReadDir.
		if entry.IsDir() {
			out = append(out, entry.Name())
		}
	}
	return out
}

// museSessionLogs returns the session and subagent logs under root, relative
// to root.
func museSessionLogs(root string) []string {
	var logs []string
	for _, y := range museRealSubdirs(root) {
		for _, mo := range museRealSubdirs(filepath.Join(root, y)) {
			for _, d := range museRealSubdirs(filepath.Join(root, y, mo)) {
				day := filepath.Join(y, mo, d)
				for _, id := range museRealSubdirs(filepath.Join(root, day)) {
					sess := filepath.Join(day, id)
					logs = append(logs, filepath.Join(sess, museSessionLog))
					sub := filepath.Join(sess, "subagent")
					for _, child := range museRealSubdirs(filepath.Join(root, sub)) {
						logs = append(logs, filepath.Join(sub, child, museSessionLog))
					}
				}
			}
		}
	}
	return logs
}

// scan reads what was appended to every session log under root since the
// last scan. emit receives one record per finished bash call and observe the
// shell activity (finished false at the call, true at its result). When
// uid > 0 only logs owned by uid are read.
func (r *museRecordReader) scan(root string, uid int, emit func(RepeatFailureRecord), observe func(finished bool, at time.Time)) {
	defer func() { r.started = true }()
	if !copilotRecordRealDir(filepath.Dir(root)) {
		return
	}
	present := map[string]bool{}
	for _, rel := range museSessionLogs(root) {
		path := filepath.Join(root, rel)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || !copilotRecordOwnedBy(info, uid) {
			continue
		}
		present[rel] = true
		sess := r.sessions[rel]
		if sess == nil {
			sess = &copilotRecordSession{commands: map[string]string{}, seen: map[string]bool{}}
			if !r.started {
				sess.offset = info.Size()
			}
			r.sessions[rel] = sess
		}
		if info.Size() < sess.offset {
			// Rewritten: read it again; seen keeps each call to one record.
			sess.offset = 0
		}
		if info.Size() > sess.offset {
			r.readSession(path, sess, info.Size(), emit, observe)
		}
	}
	for rel := range r.sessions {
		if !present[rel] {
			delete(r.sessions, rel)
		}
	}
}

func (r *museRecordReader) readSession(path string, sess *copilotRecordSession, size int64, emit func(RepeatFailureRecord), observe func(bool, time.Time)) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return
	}
	defer f.Close()
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
		museHandleLine(sess, line, emit, observe)
	}
}

// museSessionEnvelope is the part of a session-log line the reader uses.
type museSessionEnvelope struct {
	// RecordedAt is in microseconds since the Unix epoch.
	RecordedAt int64 `json:"recorded_at"`
	Payload    struct {
		Event struct {
			Kind      string `json:"kind"`
			ToolCalls []struct {
				CallID string `json:"call_id"`
				Name   string `json:"name"`
				Args   string `json:"args"`
			} `json:"tool_calls"`
			Results []struct {
				ToolCallID string `json:"tool_call_id"`
				Text       string `json:"text"`
			} `json:"results"`
		} `json:"event"`
	} `json:"payload"`
}

// museShellResult is the JSON text of a finished bash call's result.
type museShellResult struct {
	ExitCode       *float64 `json:"exit_code"`
	TerminalStatus string   `json:"terminal_status"`
}

func museHandleLine(sess *copilotRecordSession, line []byte, emit func(RepeatFailureRecord), observe func(bool, time.Time)) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var env museSessionEnvelope
	if json.Unmarshal(line, &env) != nil {
		return
	}
	var ts time.Time
	if env.RecordedAt > 0 {
		ts = time.UnixMicro(env.RecordedAt)
	}
	ev := env.Payload.Event
	switch ev.Kind {
	case "assistant_tool_calls_committed":
		for _, call := range ev.ToolCalls {
			if call.Name != museShellTool {
				continue
			}
			// museShellTool is also a Copilot shell tool name, so start keeps it.
			if sess.start(call.CallID, museShellTool, json.RawMessage(call.Args)) {
				observe(false, ts)
			}
		}
	case "tool_result_batch_committed":
		for _, res := range ev.Results {
			cmd, ok := sess.commands[res.ToolCallID]
			if !ok {
				continue
			}
			delete(sess.commands, res.ToolCallID)
			if len(sess.seen) >= copilotRecordSeenMax {
				sess.seen = map[string]bool{}
			}
			sess.seen[res.ToolCallID] = true
			observe(true, ts)
			if rec, ok := museShellRecord(res.Text, cmd, ts); ok {
				emit(rec)
			}
		}
	}
}

// museShellRecord builds the record of one finished bash call. A call that
// returned while its command keeps running (no exit code yet), timed out, was
// refused, or whose result is not the shell's JSON is not recorded.
func museShellRecord(text, cmd string, ts time.Time) (RepeatFailureRecord, bool) {
	var res museShellResult
	if json.Unmarshal([]byte(text), &res) != nil || res.ExitCode == nil || *res.ExitCode < 0 ||
		strings.EqualFold(res.TerminalStatus, museTerminalTimedOut) {
		return RepeatFailureRecord{}, false
	}
	code := int(*res.ExitCode)
	return RepeatFailureRecord{
		Backend:    "muse",
		Time:       ts,
		RawCommand: cmd,
		ExitCode:   &code,
		Failed:     code != 0,
		Route:      RepeatFailureRouteMuseSessions,
	}, true
}

// readMuseRepeatFailureRecords runs every Muse Code agent's session-log
// reader and appends the records to the agent's raw record file, so the
// collection that follows picks them up. Only called while the check is on.
func (m *Manager) readMuseRepeatFailureRecords() {
	paths := m.repeatFailureRecordPaths()
	type museAgent struct {
		name string
		uid  int
		root string
	}
	var agents []museAgent
	m.mu.RLock()
	for name, a := range m.agents {
		if effectiveBackend(a) != "muse" || paths[name] == "" {
			continue
		}
		agents = append(agents, museAgent{name: name, uid: a.UID, root: museSessionsRoot(name, a.UID, AgentHome(name, a.UID, "muse"))})
	}
	m.mu.RUnlock()

	mon := &m.repeatFailure
	mon.mu.Lock()
	readers := make(map[string]*museRecordReader, len(agents))
	for _, a := range agents {
		r := mon.muse[a.name]
		if r == nil {
			r = newMuseRecordReader()
		}
		readers[a.name] = r
	}
	mon.muse = readers
	mon.mu.Unlock()

	for _, a := range agents {
		var records []RepeatFailureRecord
		readers[a.name].scan(a.root, a.uid, func(rec RepeatFailureRecord) {
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
			m.logger.Warn("repeat-failure: cannot create the record dir for Muse Code records", "agent", a.name, "error", err)
			continue
		}
		for _, rec := range records {
			if err := AppendRepeatFailureRecord(path, rec); err != nil {
				m.logger.Warn("repeat-failure: writing a Muse Code command record failed", "agent", a.name, "error", err)
				break
			}
		}
	}
}
