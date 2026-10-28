package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
// replay history (a log a cap hid from that first scan is instead read from
// the start but drops records dated before the reader started), and a tool_call_id is recorded at most once. Subagent logs
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
	// museDirEntriesMax bounds the subdirectories kept from one directory of
	// the agent-writable sessions tree: the museDirEntriesMax newest names.
	museDirEntriesMax = 1024
	// museSessionLogsMax bounds the session logs one scan follows; the oldest
	// days are the ones left out.
	museSessionLogsMax = 4096
	// museDirListingsMax bounds the directories one scan lists, so a tree of
	// empty year/month/day directories costs at most this many listings.
	museDirListingsMax = 2 * museSessionLogsMax
	// museDisplacedSessionsMax bounds the followed sessions a truncated walk
	// left out that the reader keeps (newest names first) while their logs
	// exist, so the sessions map and the stats a scan spends on them stay
	// bounded however many newer-named sessions displace them.
	museDisplacedSessionsMax = museSessionLogsMax
)

// museRecordReader follows one agent's Muse Code session logs. Sessions are
// keyed by the log's path relative to the sessions root.
type museRecordReader struct {
	started bool
	// startedAt is when the first scan began.
	startedAt time.Time
	sessions  map[string]*copilotRecordSession
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

// museRealSubdirs lists the subdirectories of dir that are not links, newest
// name first, keeping at most the museDirEntriesMax newest. truncated reports
// that some were left out. Names are taken from the directory listing alone
// (no per-entry stat); a batch is trimmed once 2*museDirEntriesMax names are
// held, so that is the most held at a time.
func museRealSubdirs(dir string) (out []string, truncated bool) {
	// Also checked by museWalk.subdirs, which charges the listing budget
	// only for directories that exist; this keeps direct callers safe.
	if !copilotRecordRealDir(dir) {
		return nil, false
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	keepNewest := func() {
		sort.Sort(sort.Reverse(sort.StringSlice(out)))
		if len(out) > museDirEntriesMax {
			out = out[:museDirEntriesMax]
			truncated = true
		}
	}
	for {
		entries, err := f.ReadDir(museDirEntriesMax)
		for _, entry := range entries {
			// A symlinked directory is not a directory to ReadDir.
			if entry.IsDir() {
				out = append(out, entry.Name())
			}
		}
		if len(out) >= 2*museDirEntriesMax {
			keepNewest()
		}
		if err != nil || len(entries) == 0 {
			break
		}
	}
	keepNewest()
	return out, truncated
}

// museWalk is one scan's walk of the sessions tree: how many directories it
// has listed and whether any cap left part of the tree out.
type museWalk struct {
	listings  int
	truncated bool
}

// subdirs is museRealSubdirs charged to the walk's listing budget. A missing
// or linked dir is checked here first so it costs no listing.
func (w *museWalk) subdirs(dir string) []string {
	if !copilotRecordRealDir(dir) {
		return nil
	}
	if w.listings >= museDirListingsMax {
		w.truncated = true
		return nil
	}
	w.listings++
	out, truncated := museRealSubdirs(dir)
	if truncated {
		w.truncated = true
	}
	return out
}

// museSessionLogs returns the session and subagent logs under root, relative
// to root, at most museSessionLogsMax of them, newest days first. truncated
// reports that a cap (per directory, logs, or listings) left logs out, so a
// log missing from the result is not necessarily gone.
func museSessionLogs(root string) (logs []string, truncated bool) {
	w := &museWalk{}
	add := func(rel string) bool {
		if len(logs) >= museSessionLogsMax {
			w.truncated = true
			return false
		}
		logs = append(logs, rel)
		return true
	}
	for _, y := range w.subdirs(root) {
		for _, mo := range w.subdirs(filepath.Join(root, y)) {
			for _, d := range w.subdirs(filepath.Join(root, y, mo)) {
				day := filepath.Join(y, mo, d)
				for _, id := range w.subdirs(filepath.Join(root, day)) {
					sess := filepath.Join(day, id)
					if !add(filepath.Join(sess, museSessionLog)) {
						return logs, w.truncated
					}
					sub := filepath.Join(sess, "subagent")
					for _, child := range w.subdirs(filepath.Join(root, sub)) {
						if !add(filepath.Join(sub, child, museSessionLog)) {
							return logs, w.truncated
						}
					}
				}
			}
		}
	}
	return logs, w.truncated
}

// scan reads what was appended to every session log under root since the
// last scan. emit receives one record per finished bash call and observe the
// shell activity (finished false at the call, true at its result). When
// uid > 0 only logs owned by uid are read.
func (r *museRecordReader) scan(root string, uid int, emit func(RepeatFailureRecord), observe func(finished bool, at time.Time)) {
	if !r.started && r.startedAt.IsZero() {
		r.startedAt = time.Now()
	}
	defer func() { r.started = true }()
	if !copilotRecordRealDir(filepath.Dir(root)) {
		return
	}
	present := map[string]bool{}
	logs, truncated := museSessionLogs(root)
	for _, rel := range logs {
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
			} else {
				// A log first reached after the first scan is new, or a
				// pre-existing one a cap hid; either way its records from
				// before the reader started are history.
				sess.since = r.startedAt
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
	// Followed sessions the walk did not reach. A scan that saw the whole
	// tree forgets them: they are gone. A truncated walk may have left a log
	// out by a cap, not deletion, so its offset is kept while the log exists
	// and its return replays nothing; newest names first, at most
	// museDisplacedSessionsMax of them, so neither the map nor the stats
	// spent here grow with the sessions an agent keeps minting.
	var displaced []string
	for rel := range r.sessions {
		if !present[rel] {
			displaced = append(displaced, rel)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(displaced)))
	kept := 0
	for _, rel := range displaced {
		if truncated && kept < museDisplacedSessionsMax {
			if info, err := os.Lstat(filepath.Join(root, rel)); err == nil && info.Mode().IsRegular() {
				kept++
				continue
			}
		}
		delete(r.sessions, rel)
	}
}

func (r *museRecordReader) readSession(path string, sess *copilotRecordSession, size int64, emit func(RepeatFailureRecord), observe func(bool, time.Time)) {
	readAppendedLines(path, sess, size, func(line []byte) {
		museHandleLine(sess, line, emit, observe)
	})
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
	// history is a line from before the reader started in a log it first
	// reached later: tracked so its call pairs with its result, but neither
	// recorded nor reported as shell activity.
	history := !sess.since.IsZero() && !ts.IsZero() && ts.Before(sess.since)
	ev := env.Payload.Event
	switch ev.Kind {
	case "assistant_tool_calls_committed":
		for _, call := range ev.ToolCalls {
			if call.Name != museShellTool {
				continue
			}
			// museShellTool is also a Copilot shell tool name, so start keeps it.
			if sess.start(call.CallID, museShellTool, json.RawMessage(call.Args)) && !history {
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
			if history {
				continue
			}
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
