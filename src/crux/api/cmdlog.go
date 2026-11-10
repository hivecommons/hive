package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Command log (hivecommons/crux#114 S4): the runtime-owned record of the
// commands a contained run dispatched.
//
//	GET /v1/runs/{id}/cmdlog?since_seq=N&limit=M   records after seq N,
//	                                                oldest first, at most M
//
// The route admits the run's owner and the operator, like the run itself.
// Follow the log by passing the page's next_seq back as since_seq.

// RunCmdlogPath is the route of one run's command log.
func RunCmdlogPath(id string) string { return RunPath(id) + "/cmdlog" }

// RunCmdlogQuery is RunCmdlogPath with its paging parameters; zero values
// are omitted and take the server defaults.
func RunCmdlogQuery(id string, sinceSeq int64, limit int) string {
	q := url.Values{}
	if sinceSeq > 0 {
		q.Set("since_seq", strconv.FormatInt(sinceSeq, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := RunCmdlogPath(id)
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return path
}

// Command statuses, the closed set every record reports. Unfinished is a
// command whose exit is not known; it is never promoted by a timeout.
const (
	CommandCompleted  = "completed"
	CommandFailed     = "failed"
	CommandUnfinished = "unfinished"
	CommandCancelled  = "cancelled"
)

// Capture values: how the runtime observed the command.
const (
	// CommandCaptureRunner is the command the contained-run runner handed
	// to the sandbox launcher. Whatever that command spawned inside the
	// sandbox is not observed separately.
	CommandCaptureRunner = "runner"
	// CommandCaptureHook is reserved for an in-sandbox shell hook that
	// reports each invocation; no shipped backend writes it yet.
	CommandCaptureHook = "hook"
)

// CommandOutput summarises one stream: its full length and the kept head
// and tail. Truncated is set when the stream was longer than head+tail;
// the full scrubbed output is the run's stdout.txt / stderr.txt artifact.
type CommandOutput struct {
	Bytes     int64  `json:"bytes"`
	Head      string `json:"head,omitempty"`
	Tail      string `json:"tail,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// CommandRecord is one dispatched command. EndedAt is absent while the
// command is in flight; ExitCode is absent whenever the exit is not known,
// so an unfinished record can carry EndedAt without ExitCode.
type CommandRecord struct {
	Seq       int64      `json:"seq"`
	Command   []string   `json:"command"`
	Cwd       string     `json:"cwd,omitempty"`
	Capture   string     `json:"capture"`
	Status    string     `json:"status"`
	Reason    string     `json:"reason,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	// DurationMs is EndedAt minus StartedAt, present with EndedAt.
	DurationMs *int64         `json:"duration_ms,omitempty"`
	ExitCode   *int           `json:"exit_code,omitempty"`
	Stdout     *CommandOutput `json:"stdout,omitempty"`
	Stderr     *CommandOutput `json:"stderr,omitempty"`
}

// CommandPage is GET /v1/runs/{id}/cmdlog. NextSeq is the seq to pass as
// since_seq for the following page; More says one exists now. A caller
// following a live run keeps polling with NextSeq after More is false.
// NextSeq never advances past a record with no EndedAt, so a follower
// re-receives an in-flight command until it ends (idempotent by seq); a
// record with EndedAt is terminal whatever its Status. More is never set
// on a page whose NextSeq equals the since_seq it answered, so paging
// while More is true always makes progress.
type CommandPage struct {
	Run      string          `json:"run"`
	Commands []CommandRecord `json:"commands"`
	NextSeq  int64           `json:"next_seq"`
	More     bool            `json:"more,omitempty"`
}

// RunCmdlog is GET /v1/runs/{id}/cmdlog.
func (c *Client) RunCmdlog(ctx context.Context, id string, sinceSeq int64, limit int) (CommandPage, error) {
	var out CommandPage
	err := c.Do(ctx, http.MethodGet, RunCmdlogQuery(id, sinceSeq, limit), nil, &out)
	return out, err
}
