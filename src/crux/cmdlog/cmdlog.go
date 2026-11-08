// Package cmdlog is the command log of the Crux work services (S4 in
// hivecommons/crux#114): an append-only, runtime-owned record of the
// commands a contained run executes, so a driver can check "did the agent
// really run what it said it ran?" against evidence the agent cannot write.
//
// Each run keeps its log beside its record, <workdir>/runs/<id>/commands.jsonl.
// The runner appends one started line before it dispatches a command and
// one ended line when the command is over; every line carries the next
// value of a per-log sequence number. Read folds the lines back into one
// api.CommandRecord per command. Command lines, the working directory, the
// reason and the output are scrubbed with logscrub before they are written.
//
// The log counts what the runtime dispatched (capture "runner"). Processes
// a dispatched command spawns inside the sandbox are not observed here.
package cmdlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/logscrub"
)

const (
	// FileName is the log inside a run's directory.
	FileName = "commands.jsonl"
	// DefaultHeadBytes and DefaultTailBytes are the output kept per stream
	// when the policy leaves them zero.
	DefaultHeadBytes = 4 << 10
	DefaultTailBytes = 4 << 10

	eventStarted = "started"
	eventEnded   = "ended"
	fileMode     = 0o660
)

// ErrCorrupt is a log that cannot be folded: a committed line that does not
// parse, a sequence number that does not increase, an ended line without
// its started line, or an uncommitted (torn) last line the recorder would
// otherwise append after.
var ErrCorrupt = errors.New("cmdlog: corrupt command log")

// Path is the command log of the run whose directory is runDir.
func Path(runDir string) string { return filepath.Join(runDir, FileName) }

// Policy bounds the output a record keeps per stream: the first HeadBytes
// and the last TailBytes of the scrubbed text. Zero or negative takes the
// default. The full output is the run's own artifact; the log keeps ends
// and counts.
type Policy struct {
	HeadBytes int
	TailBytes int
}

func (p Policy) withDefaults() Policy {
	if p.HeadBytes <= 0 {
		p.HeadBytes = DefaultHeadBytes
	}
	if p.TailBytes <= 0 {
		p.TailBytes = DefaultTailBytes
	}
	return p
}

// line is one entry of the log. A started line carries the command; an
// ended line names its started line (StartSeq) and carries the outcome.
type line struct {
	Seq   int64     `json:"seq"`
	Event string    `json:"event"`
	At    time.Time `json:"at"`

	Command []string `json:"command,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	Capture string   `json:"capture,omitempty"`

	StartSeq   int64              `json:"start_seq,omitempty"`
	Status     string             `json:"status,omitempty"`
	ExitCode   *int               `json:"exit_code,omitempty"`
	Reason     string             `json:"reason,omitempty"`
	DurationMs *int64             `json:"duration_ms,omitempty"`
	Stdout     *api.CommandOutput `json:"stdout,omitempty"`
	Stderr     *api.CommandOutput `json:"stderr,omitempty"`
}

// Start is a command the runner is about to dispatch.
type Start struct {
	Command []string
	// Cwd is where the runner dispatches it: for a contained run, the host
	// workspace mounted as the sandbox's working directory.
	Cwd string
	// Capture says how the command is observed; empty is api.CaptureRunner.
	Capture string
}

// End is how a dispatched command finished. Status is one of the
// api.Command* statuses; ExitCode is nil when the exit was not read.
// Stdout and Stderr are the whole streams.
type End struct {
	Status   string
	ExitCode *int
	Reason   string
	Stdout   string
	Stderr   string
}

// Recorder appends to command logs. It is the hook the S2 runner writes
// through; the zero value records with the default policy. Safe for
// concurrent use within one process; a work dir is served by one process.
type Recorder struct {
	// Policy bounds the output kept per stream.
	Policy Policy
	// Scrub redacts every recorded string; nil uses logscrub.ScrubString.
	Scrub func(string) string
	// Now stamps lines; nil uses time.Now. Durations are measured between
	// its readings, so a clock with a monotonic reading is immune to wall
	// clock steps.
	Now func() time.Time

	mu    sync.Mutex
	files map[string]*logState
}

// logState is what the recorder knows about one log with a command in
// flight: the next sequence number and the start of each open command. It
// is dropped when the last open command ends, so the map holds live runs
// only; a later append re-reads the file.
type logState struct {
	next int64
	open map[int64]time.Time
}

// Started appends a started line for s to runDir's log and returns its
// sequence number, which Ended needs.
func (r *Recorder) Started(runDir string, s Start) (int64, error) {
	if len(s.Command) == 0 {
		return 0, errors.New("cmdlog: a started command needs a command line")
	}
	capture := s.Capture
	if capture == "" {
		capture = api.CaptureRunner
	}
	command := make([]string, len(s.Command))
	for i, arg := range s.Command {
		command[i] = r.scrub(arg)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st, err := r.state(runDir)
	if err != nil {
		return 0, err
	}
	at := r.now()
	l := line{Seq: st.next, Event: eventStarted, At: at, Command: command, Cwd: r.scrub(s.Cwd), Capture: capture}
	if err := r.append(runDir, st, l); err != nil {
		if len(st.open) == 0 {
			delete(r.files, runDir)
		}
		return 0, err
	}
	st.open[l.Seq] = at
	return l.Seq, nil
}

// Ended appends the ended line of the command Started returned startSeq
// for. The command is closed whether or not the append succeeds: a log
// without its ended line folds to unfinished, which is what the runtime
// can then vouch for.
func (r *Recorder) Ended(runDir string, startSeq int64, e End) error {
	switch e.Status {
	case api.CommandCompleted, api.CommandFailed, api.CommandUnfinished, api.CommandCancelled:
	default:
		return fmt.Errorf("cmdlog: unknown command status %q", e.Status)
	}
	stdout, stderr := r.output(e.Stdout), r.output(e.Stderr)
	reason := r.scrub(e.Reason)
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.files[runDir]
	if st == nil {
		return notInFlight(startSeq)
	}
	began, ok := st.open[startSeq]
	if !ok {
		return notInFlight(startSeq)
	}
	delete(st.open, startSeq)
	if len(st.open) == 0 {
		defer delete(r.files, runDir)
	}
	at := r.now()
	return r.append(runDir, st, line{
		Seq: st.next, Event: eventEnded, At: at, StartSeq: startSeq,
		Status: e.Status, ExitCode: e.ExitCode, Reason: reason, DurationMs: new(at.Sub(began).Milliseconds()),
		Stdout: stdout, Stderr: stderr,
	})
}

func notInFlight(seq int64) error {
	return fmt.Errorf("cmdlog: no command started at seq %d is in flight", seq)
}

// state returns the cached state of runDir's log, loading the next
// sequence number from the file when nothing of it is in flight.
func (r *Recorder) state(runDir string) (*logState, error) {
	if st := r.files[runDir]; st != nil {
		return st, nil
	}
	lines, torn, err := readLines(Path(runDir))
	if err != nil {
		return nil, err
	}
	if torn {
		return nil, fmt.Errorf("%w: %s ends in an uncommitted line", ErrCorrupt, Path(runDir))
	}
	st := &logState{next: 1, open: map[int64]time.Time{}}
	if n := len(lines); n > 0 {
		st.next = lines[n-1].Seq + 1
	}
	if r.files == nil {
		r.files = map[string]*logState{}
	}
	r.files[runDir] = st
	return st, nil
}

// append writes l as one committed line and advances the sequence.
func (r *Recorder) append(runDir string, st *logState, l line) error {
	data, err := json.Marshal(l)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(Path(runDir), os.O_CREATE|os.O_APPEND|os.O_WRONLY, fileMode)
	if err != nil {
		return fmt.Errorf("cmdlog: %w", err)
	}
	_, werr := f.Write(append(data, '\n'))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("cmdlog: %w", werr)
	}
	st.next++
	return nil
}

// output scrubs text and keeps its ends per the policy.
func (r *Recorder) output(text string) *api.CommandOutput {
	text = r.scrub(text)
	p := r.Policy.withDefaults()
	out := &api.CommandOutput{Bytes: int64(len(text))}
	if len(text) <= p.HeadBytes+p.TailBytes {
		out.Head = text
		return out
	}
	head := p.HeadBytes
	for head > 0 && !utf8.RuneStart(text[head]) {
		head--
	}
	tail := len(text) - p.TailBytes
	for tail < len(text) && !utf8.RuneStart(text[tail]) {
		tail++
	}
	out.Head, out.Tail, out.Truncated = text[:head], text[tail:], true
	return out
}

func (r *Recorder) scrub(v string) string {
	if r.Scrub != nil {
		return r.Scrub(v)
	}
	return logscrub.ScrubString(v)
}

func (r *Recorder) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// readLines returns the committed lines of the log at path in order. A
// line is committed by its newline; torn reports bytes after the last one.
// A missing file is an empty log.
func readLines(path string) (lines []line, torn bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("cmdlog: %w", err)
	}
	end := bytes.LastIndexByte(data, '\n')
	torn = end+1 < len(data)
	if end < 0 {
		return nil, torn, nil
	}
	var prev int64
	for n, raw := range bytes.Split(data[:end], []byte{'\n'}) {
		var l line
		if err := json.Unmarshal(raw, &l); err != nil {
			return nil, false, fmt.Errorf("%w: %s line %d: %v", ErrCorrupt, path, n+1, err)
		}
		if l.Seq <= prev {
			return nil, false, fmt.Errorf("%w: %s line %d: seq %d does not follow %d", ErrCorrupt, path, n+1, l.Seq, prev)
		}
		prev = l.Seq
		lines = append(lines, l)
	}
	return lines, torn, nil
}
