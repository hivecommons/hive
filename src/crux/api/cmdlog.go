package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Command log (hivecommons/crux#114 S4): the runtime-owned record of the
// commands a contained run executed, written by the runner and never by the
// command, so a driver can check what an agent said it ran against evidence
// the agent cannot write.
//
//	GET /v1/runs/{id}/cmdlog[?since_seq=N&limit=M]
//
// Only the run's owner or the operator may read it, the same rule as the
// run itself.

// RunCmdlogPath is the command-log route of one run.
func RunCmdlogPath(id string) string { return RunPath(id) + "/cmdlog" }

// RunCmdlogPagePath is RunCmdlogPath with the paging query; zero values are
// left out so the server defaults apply.
func RunCmdlogPagePath(id string, sinceSeq int64, limit int) string {
	q := url.Values{}
	if sinceSeq > 0 {
		q.Set("since_seq", strconv.FormatInt(sinceSeq, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if len(q) == 0 {
		return RunCmdlogPath(id)
	}
	return RunCmdlogPath(id) + "?" + q.Encode()
}

// Page bounds of GET /v1/runs/{id}/cmdlog: limit 0 (or absent) takes the
// default, a larger one is capped.
const (
	CmdlogDefaultLimit = 100
	CmdlogMaxLimit     = 1000
)

// Command statuses, the closed set every command record reports. They mean
// what the run statuses of the same names mean: completed is a command
// whose exit was read (whatever its code), failed one that never started or
// timed out, cancelled one stopped by the caller, and unfinished one whose
// end the runtime did not observe (still running, or the server stopped
// while it ran).
const (
	CommandCompleted  = "completed"
	CommandFailed     = "failed"
	CommandUnfinished = "unfinished"
	CommandCancelled  = "cancelled"
)

// CaptureRunner says the runtime observed the command because it dispatched
// it: the command a contained run was asked to execute. Processes that
// command spawns inside the sandbox (a shell's children, a build tool's
// compilers) are not individually observed and do not appear.
const CaptureRunner = "runner"

// CommandOutput describes one output stream of a command. Bytes counts the
// whole scrubbed stream (the same bytes the run keeps as its stdout.txt or
// stderr.txt artifact). When it fits the server's head+tail policy Head is
// the whole stream; otherwise Head and Tail are its ends and Truncated is
// set.
type CommandOutput struct {
	Bytes     int64  `json:"bytes"`
	Head      string `json:"head,omitempty"`
	Tail      string `json:"tail,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// CommandRecord is one command, folded from its started and ended lines.
// Seq is the started line's sequence number and identifies the command
// within its run; EndSeq, EndedAt, DurationMs, ExitCode and the outputs are
// absent until an ended line is recorded. ExitCode is present only when
// the exit was read, never guessed.
type CommandRecord struct {
	Seq        int64          `json:"seq"`
	EndSeq     int64          `json:"end_seq,omitempty"`
	Command    []string       `json:"command"`
	Cwd        string         `json:"cwd,omitempty"`
	Capture    string         `json:"capture"`
	Status     string         `json:"status"`
	StartedAt  time.Time      `json:"started_at"`
	EndedAt    *time.Time     `json:"ended_at,omitempty"`
	DurationMs *int64         `json:"duration_ms,omitempty"`
	ExitCode   *int           `json:"exit_code,omitempty"`
	Reason     string         `json:"reason,omitempty"`
	Stdout     *CommandOutput `json:"stdout,omitempty"`
	Stderr     *CommandOutput `json:"stderr,omitempty"`
}

// CommandLog is one page of GET /v1/runs/{id}/cmdlog: the commands with a
// line after since_seq, ordered by their latest line. A command that was
// unfinished on one page comes back on a later one once it ends. NextSeq
// is the since_seq of the next page; More says the page was cut at limit.
type CommandLog struct {
	Run      string          `json:"run"`
	Commands []CommandRecord `json:"commands"`
	NextSeq  int64           `json:"next_seq"`
	More     bool            `json:"more,omitempty"`
}

// RunCommandLog is GET /v1/runs/{id}/cmdlog: one page of the run's
// command log (sinceSeq 0 from the start, limit 0 for the server default).
func (c *Client) RunCommandLog(ctx context.Context, id string, sinceSeq int64, limit int) (CommandLog, error) {
	var out CommandLog
	err := c.do(ctx, http.MethodGet, RunCmdlogPagePath(id, sinceSeq, limit), nil, &out)
	return out, err
}
