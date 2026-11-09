// Package cmdlog is the command log work service (hivecommons/crux#114,
// S4): a per-run, append-only record of the commands the runtime
// dispatched into a contained run, written by the runtime and never by
// the agent.
//
// The log is one commands.jsonl file in the run's directory. Each command
// contributes a "started" line when it is dispatched and an "ended" line
// once the runtime stops waiting on it, carrying the exit when it is
// known; a command with no "ended" line is in flight and reads as
// unfinished, never guessed. Readers fold the two into one
// api.CommandRecord per seq.
//
// Honest scope: a record says how the command was observed in its
// capture field. The S2 runner records what it hands to the sandbox
// launcher (api.CommandCaptureRunner). It does not see what that command
// spawns inside the sandbox: a `bash -c 'make test'` is one record, not
// one per recipe line. The runner therefore under-counts a shell that
// does many things and never over-counts; the limitation is stated in
// src/docs/design/crux-work-services.md.
package cmdlog

import (
	"errors"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/logscrub"
)

// FileName is the log inside a run's directory.
const FileName = "commands.jsonl"

// Line kinds.
const (
	EventStarted = "started"
	EventEnded   = "ended"
)

// OrphanReason is the reason on the unfinished ended line Open appends
// for a command whose started line an earlier writer never ended.
const OrphanReason = "the log was reopened with no exit recorded for this command (the runtime restarted, or its ended write failed); the exit is not knowable"

// Output policy defaults: how much of each stream a record keeps. The
// full output stays with the run (runs.StdoutArtifact); the record keeps
// enough of both ends to tell what happened without re-reading it.
const (
	DefaultHeadBytes = 4096
	DefaultTailBytes = 4096
	// MaxPageSize caps a single read; a follower pages with since_seq.
	MaxPageSize = 1000
	// DefaultPageSize is the page when the caller gives no limit.
	DefaultPageSize = 100
)

// Typed errors.
var (
	// ErrNotFound: the run directory has no log (the run never dispatched
	// a command, or the id is unknown to the caller's store).
	ErrNotFound = errors.New("cmdlog: no command log for this run")
	// ErrUnknownSeq: Ended named a seq this log never started.
	ErrUnknownSeq = errors.New("cmdlog: unknown seq")
	// ErrCorrupt: a line of the log did not parse.
	ErrCorrupt = errors.New("cmdlog: corrupt line")
)

// Policy is what a Log keeps of each command's output and how it scrubs.
type Policy struct {
	// HeadBytes and TailBytes bound the kept head and tail of each
	// stream; zero means the default, negative means none.
	HeadBytes int
	TailBytes int
	// Scrub redacts command lines and output before they are written; nil
	// uses logscrub.ScrubString.
	Scrub func(string) string
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
}

func (p Policy) defaults() Policy {
	if p.HeadBytes == 0 {
		p.HeadBytes = DefaultHeadBytes
	}
	if p.TailBytes == 0 {
		p.TailBytes = DefaultTailBytes
	}
	if p.Scrub == nil {
		p.Scrub = func(s string) string { return logscrub.ScrubString(s) }
	}
	if p.Now == nil {
		p.Now = time.Now
	}
	return p
}

// Start is what the runtime knows when it dispatches a command.
type Start struct {
	Command []string
	// Cwd is the working directory as the command sees it (the sandbox
	// mount, not the host path).
	Cwd string
	// Capture says how the command was observed; empty means
	// api.CommandCaptureRunner.
	Capture string
}

// End is what the runtime knows once the command's exit is known. Status
// empty derives from ExitCode: zero completed, non-zero failed, unknown
// unfinished.
type End struct {
	ExitCode *int
	Status   string
	// Reason explains a failed, cancelled or unfinished status.
	Reason string
	Stdout string
	Stderr string
}

// Recorder is the hook a runner appends through. Started returns the seq
// Ended must name. Implementations are safe for concurrent use.
type Recorder interface {
	Started(Start) (int64, error)
	Ended(seq int64, end End) error
}

// Opener builds the Recorder for one run's directory. runs.Service calls
// it once per run it executes; nil disables the log.
type Opener func(runDir string) (Recorder, error)

// FileOpener is the Opener that writes FileName under the run directory
// with pol.
func FileOpener(pol Policy) Opener {
	return func(runDir string) (Recorder, error) { return Open(runDir, pol) }
}

// line is one JSON object of the file: a started or ended event. Fields
// are a union; readers fold by seq.
type line struct {
	Seq     int64     `json:"seq"`
	Event   string    `json:"event"`
	At      time.Time `json:"at"`
	Command []string  `json:"command,omitempty"`
	Cwd     string    `json:"cwd,omitempty"`
	Capture string    `json:"capture,omitempty"`

	ExitCode   *int               `json:"exit_code,omitempty"`
	DurationMs *int64             `json:"duration_ms,omitempty"`
	Status     string             `json:"status,omitempty"`
	Reason     string             `json:"reason,omitempty"`
	Stdout     *api.CommandOutput `json:"stdout,omitempty"`
	Stderr     *api.CommandOutput `json:"stderr,omitempty"`
}

// deriveStatus is the status an End implies when it names none.
func deriveStatus(end End) string {
	switch {
	case end.Status != "":
		return end.Status
	case end.ExitCode == nil:
		return api.CommandUnfinished
	case *end.ExitCode == 0:
		return api.CommandCompleted
	}
	return api.CommandFailed
}

// summarize keeps the policy's head and tail of one scrubbed stream.
func summarize(text string, pol Policy) *api.CommandOutput {
	out := &api.CommandOutput{Bytes: int64(len(text))}
	head, tail := max(pol.HeadBytes, 0), max(pol.TailBytes, 0)
	if len(text) <= head+tail {
		out.Head = text
		return out
	}
	out.Truncated = true
	out.Head = text[:head]
	if tail > 0 {
		out.Tail = text[len(text)-tail:]
	}
	return out
}
