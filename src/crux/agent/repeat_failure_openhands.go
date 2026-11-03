package agent

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// OpenHands route for the repeat-failure check (hivecommons/hive#14550, C1,
// C3, C12, C14; issue #14748).
//
// OpenHands keeps one interactive bash open in tmux, so the neutral BASH_ENV
// route never sees its commands. The OpenHands CLI already saves every
// conversation under ~/.openhands/conversations (documented:
// https://docs.openhands.dev/openhands/usage/cli/resume), one directory per
// conversation with one JSON file per event in events/, named
// event-<index>-<event id>.json (documented:
// https://docs.openhands.dev/sdk/guides/convo-persistence). A shell call is an
// `ActionEvent` of the `terminal` tool followed by an `ObservationEvent` whose
// `observation` is a `TerminalObservation` carrying the `command` and its
// `exit_code` (recorded from the real CLI by
// testdata/repeat_failure/openhands/run.sh). OpenHands sets `is_error: false`
// for a command that exited non-zero, so the exit code is the result.
//
// Hive reads those events itself on every collection while the check is on
// and appends one shared record per finished terminal command to the agent's
// raw record file. Nothing is written into OpenHands' own files or launch, so
// with the check off Hive adds nothing and has nothing to remove (C14). Each
// conversation is read from the event after the last one read; a
// conversation that already existed when the reader started is read from its
// end, so a Hive restart does not replay history, and a tool_call_id is
// recorded at most once.
//
// The events live under the agent's HOME and Hive reads them as itself, so
// the reader never follows a link the agent could plant (the same discipline
// as the Copilot reader): the .openhands tree, the conversations tree, every
// conversation and events directory and every event file are Lstat-checked,
// event files are opened O_NOFOLLOW, and in multi-uid mode an event file not
// owned by the agent's uid is skipped.

// RepeatFailureRouteOpenHandsEvents names the OpenHands saved-events reader
// in a record's Route.
const RepeatFailureRouteOpenHandsEvents = "openhands_events"

const (
	// openhandsEventFileMax bounds one event file; a larger one is skipped.
	openhandsEventFileMax = 8 << 20
	// openhandsEventsPerScan bounds the events one collection reads per
	// conversation; the rest are read on the next collection.
	openhandsEventsPerScan = 4096
	// openhandsRecordSeenMax bounds the tool_call_ids remembered per
	// conversation.
	openhandsRecordSeenMax = 8192
	// openhandsTerminalTool is the OpenHands shell tool.
	openhandsTerminalTool = "terminal"
)

// openhandsRecordConversation is the reader's position in one conversation.
type openhandsRecordConversation struct {
	// next is the index of the first event not read yet.
	next int
	// seen holds the tool_call_ids already recorded.
	seen map[string]bool
}

// openhandsRecordReader follows one agent's OpenHands conversations.
type openhandsRecordReader struct {
	started       bool
	conversations map[string]*openhandsRecordConversation
}

func newOpenHandsRecordReader() *openhandsRecordReader {
	return &openhandsRecordReader{conversations: map[string]*openhandsRecordConversation{}}
}

// openhandsEvent is the part of a saved OpenHands event the reader uses.
type openhandsEvent struct {
	Kind        string `json:"kind"`
	Timestamp   string `json:"timestamp"`
	ToolName    string `json:"tool_name"`
	ToolCallID  string `json:"tool_call_id"`
	Observation struct {
		Kind     string   `json:"kind"`
		Command  string   `json:"command"`
		ExitCode *float64 `json:"exit_code"`
		Timeout  bool     `json:"timeout"`
	} `json:"observation"`
}

// openhandsEventIndex parses the index out of an event-<index>-<id>.json
// file name.
func openhandsEventIndex(name string) (int, bool) {
	if !strings.HasPrefix(name, "event-") || !strings.HasSuffix(name, ".json") {
		return 0, false
	}
	rest := strings.TrimPrefix(name, "event-")
	dash := strings.IndexByte(rest, '-')
	if dash <= 0 {
		return 0, false
	}
	n, err := strconv.Atoi(rest[:dash])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// scan reads the events saved in every conversation under home since the
// last scan. emit receives one record per finished terminal command and
// observe the shell activity (finished false at the action, true at the
// observation). When uid > 0 only event files owned by uid are read.
func (r *openhandsRecordReader) scan(home string, uid int, emit func(RepeatFailureRecord), observe func(finished bool, at time.Time)) {
	defer func() { r.started = true }()
	root := filepath.Join(home, ".openhands", "conversations")
	if !copilotRecordRealDir(filepath.Join(home, ".openhands")) || !copilotRecordRealDir(root) {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	present := make(map[string]bool, len(entries))
	for _, entry := range entries {
		// A symlinked conversation directory is not a directory to ReadDir.
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		present[name] = true
		events := filepath.Join(root, name, "events")
		if !copilotRecordRealDir(events) {
			continue
		}
		files := openhandsEventFiles(events)
		conv := r.conversations[name]
		if conv == nil {
			conv = &openhandsRecordConversation{seen: map[string]bool{}}
			if !r.started {
				for idx := range files {
					conv.next = max(conv.next, idx+1)
				}
			}
			r.conversations[name] = conv
		}
		r.readConversation(events, files, conv, uid, emit, observe)
	}
	for name := range r.conversations {
		if !present[name] {
			delete(r.conversations, name)
		}
	}
}

// openhandsEventFiles maps each event index in dir to its file name.
func openhandsEventFiles(dir string) map[int]string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	files := make(map[int]string, len(entries))
	for _, entry := range entries {
		if idx, ok := openhandsEventIndex(entry.Name()); ok {
			files[idx] = entry.Name()
		}
	}
	return files
}

func (r *openhandsRecordReader) readConversation(dir string, files map[int]string, conv *openhandsRecordConversation, uid int, emit func(RepeatFailureRecord), observe func(bool, time.Time)) {
	last := -1
	for idx := range files {
		last = max(last, idx)
	}
	for read := 0; conv.next <= last && read < openhandsEventsPerScan; read++ {
		name, ok := files[conv.next]
		if !ok {
			// A gap: skip the missing index.
			conv.next++
			continue
		}
		ev, ok := openhandsReadEvent(filepath.Join(dir, name), uid)
		if !ok {
			if conv.next == last {
				// Possibly still being written: try again next collection.
				return
			}
			conv.next++
			continue
		}
		conv.next++
		conv.handle(ev, emit, observe)
	}
}

// openhandsReadEvent reads one event file without following links.
func openhandsReadEvent(path string, uid int) (openhandsEvent, bool) {
	var ev openhandsEvent
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || !copilotRecordOwnedBy(info, uid) || info.Size() > openhandsEventFileMax {
		return ev, false
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return ev, false
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return ev, false
	}
	data, err := io.ReadAll(io.LimitReader(f, openhandsEventFileMax+1))
	if err != nil || len(data) > openhandsEventFileMax {
		return ev, false
	}
	if json.Unmarshal(data, &ev) != nil {
		return ev, false
	}
	return ev, true
}

// parseOpenHandsTime parses an event timestamp. OpenHands writes local time
// without a zone (2026-10-28T01:25:50.034272).
func parseOpenHandsTime(s string) time.Time {
	if ts := parseTranscriptTime(s); !ts.IsZero() {
		return ts
	}
	ts, err := time.ParseInLocation("2006-01-02T15:04:05.999999999", s, time.Local)
	if err != nil {
		return time.Time{}
	}
	return ts
}

func (conv *openhandsRecordConversation) handle(ev openhandsEvent, emit func(RepeatFailureRecord), observe func(bool, time.Time)) {
	if ev.ToolName != openhandsTerminalTool || ev.ToolCallID == "" || conv.seen[ev.ToolCallID] {
		return
	}
	ts := parseOpenHandsTime(ev.Timestamp)
	switch ev.Kind {
	case "ActionEvent":
		observe(false, ts)
	case "ObservationEvent":
		if ev.Observation.Kind != "TerminalObservation" {
			return
		}
		if len(conv.seen) >= openhandsRecordSeenMax {
			conv.seen = map[string]bool{}
		}
		conv.seen[ev.ToolCallID] = true
		observe(true, ts)
		if rec, ok := openhandsShellRecord(ev, ts); ok {
			emit(rec)
		}
	}
}

// openhandsShellRecord builds the record of one finished terminal command. A
// command that timed out or is still running has no exit code yet (OpenHands
// writes -1) and is not recorded.
func openhandsShellRecord(ev openhandsEvent, ts time.Time) (RepeatFailureRecord, bool) {
	obs := ev.Observation
	if strings.TrimSpace(obs.Command) == "" || obs.Timeout || obs.ExitCode == nil || *obs.ExitCode < 0 {
		return RepeatFailureRecord{}, false
	}
	code := int(*obs.ExitCode)
	return RepeatFailureRecord{
		Backend:    "openhands",
		Time:       ts,
		RawCommand: obs.Command,
		ExitCode:   &code,
		Failed:     code != 0,
		Route:      RepeatFailureRouteOpenHandsEvents,
	}, true
}

// readOpenHandsRepeatFailureRecords runs every OpenHands agent's
// saved-events reader and appends the records to the agent's raw record
// file, so the collection that follows picks them up. Only called while the
// check is on.
func (m *Manager) readOpenHandsRepeatFailureRecords() {
	paths := m.repeatFailureRecordPaths()
	type openhandsAgent struct {
		name string
		uid  int
		home string
	}
	var agents []openhandsAgent
	m.mu.RLock()
	for name, a := range m.agents {
		if effectiveBackend(a) != "openhands" || paths[name] == "" {
			continue
		}
		agents = append(agents, openhandsAgent{name: name, uid: a.UID, home: AgentHome(name, a.UID, "openhands")})
	}
	m.mu.RUnlock()

	mon := &m.repeatFailure
	mon.mu.Lock()
	readers := make(map[string]*openhandsRecordReader, len(agents))
	for _, a := range agents {
		r := mon.openhands[a.name]
		if r == nil {
			r = newOpenHandsRecordReader()
		}
		readers[a.name] = r
	}
	mon.openhands = readers
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
			m.logger.Warn("repeat-failure: cannot create the record dir for OpenHands records", "agent", a.name, "error", err)
			continue
		}
		for _, rec := range records {
			if err := AppendRepeatFailureRecord(path, rec); err != nil {
				m.logger.Warn("repeat-failure: writing an OpenHands command record failed", "agent", a.name, "error", err)
				break
			}
		}
	}
}
