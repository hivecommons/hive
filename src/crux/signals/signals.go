// Package signals is the turn-signal work service (hivecommons/crux#114
// S5): a per-run, append-only record of what the runner, the gate
// service and the run's own driver said at turn boundaries — started,
// thinking, tool, blocked, needs-input, input, done — so a driver that
// missed the live event learns from the saved record where the turn is
// and how it ended.
//
// The log is one signals.jsonl file in the run's directory, one JSON
// object per signal, numbered by seq. Readers page with since_seq the way
// the S4 command log does. A signal is a statement about where the turn
// is; it is never a verdict on the work: done carries the run's terminal
// status and nothing promotes it to success.
//
// Who may write what is closed by source: the runner alone writes
// started and done; the gate service writes needs-input, input and
// blocked; a caller writes any of api.CallerSignalKinds. The agent inside
// the sandbox has no handle on the file (0600 in the runtime's run dir).
//
// The package also keeps the RFC's per-agent kick outcome: OutcomeStore
// persists the last crux.KickOutcome of every agent under
// <workdir>/signals/agents/ so GET /v1/agents/{name}/kick-outcome answers
// after a restart.
package signals

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/logscrub"
)

const (
	// FileName is the log inside a run's directory.
	FileName = "signals.jsonl"
	// DirName is the per-concern directory under the work dir that holds
	// the per-agent kick outcomes.
	DirName = "signals"
	// AgentsDirName holds one <agent>.json per agent under DirName.
	AgentsDirName = "agents"

	// MaxMessageBytes bounds a signal's message after scrubbing.
	MaxMessageBytes = 1024
	// MaxDataBytes bounds a signal's data payload once encoded.
	MaxDataBytes = 16 << 10
	// MaxPageSize caps a single read; a follower pages with since_seq.
	MaxPageSize = 1000
	// DefaultPageSize is the page when the caller gives no limit.
	DefaultPageSize = 100
)

// Typed errors.
var (
	// ErrNotFound: the run directory has no signal log yet.
	ErrNotFound = errors.New("signals: no signal log for this run")
	// ErrBadRequest: the signal is malformed (unknown kind, oversize
	// message, data that is not one JSON value).
	ErrBadRequest = errors.New("signals: bad request")
	// ErrDataTooLarge: data over MaxDataBytes once encoded.
	ErrDataTooLarge = errors.New("signals: data too large")
	// ErrKindNotAllowed: a kind the source may not write (a caller
	// claiming started or done).
	ErrKindNotAllowed = errors.New("signals: kind not allowed for this source")
	// ErrCorrupt: a line of the log did not parse.
	ErrCorrupt = errors.New("signals: corrupt line")
)

// Kinds is the closed set of signal kinds.
var Kinds = []string{api.SignalStarted, api.SignalThinking, api.SignalTool, api.SignalBlocked, api.SignalNeedsInput, api.SignalInput, api.SignalDone}

// allowed maps each source onto the kinds it may write.
var allowed = map[string][]string{
	api.SignalSourceRunner: {api.SignalStarted, api.SignalDone},
	api.SignalSourceGate:   {api.SignalNeedsInput, api.SignalInput, api.SignalBlocked},
	api.SignalSourceCaller: api.CallerSignalKinds,
}

// KnownKind reports whether kind is one of Kinds.
func KnownKind(kind string) bool {
	for _, k := range Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// Allowed reports whether source may write kind.
func Allowed(source, kind string) bool {
	for _, k := range allowed[source] {
		if k == kind {
			return true
		}
	}
	return false
}

// Policy is how a Log scrubs and stamps what it writes.
type Policy struct {
	// Scrub redacts message text and the string leaves of data before
	// they are written; nil uses logscrub.ScrubString.
	Scrub func(string) string
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
}

func (p Policy) defaults() Policy {
	if p.Scrub == nil {
		p.Scrub = func(s string) string { return logscrub.ScrubString(s) }
	}
	if p.Now == nil {
		p.Now = time.Now
	}
	return p
}

// Signal is what an emitter hands to a Recorder. Source and Kind are
// required; By names the principal behind a caller signal.
type Signal struct {
	Kind    string
	Source  string
	By      string
	Message string
	Data    json.RawMessage
}

// Recorder is the hook emitters append through. Emit returns the record
// as written. Implementations are safe for concurrent use.
type Recorder interface {
	Emit(Signal) (api.Signal, error)
}

// Opener builds the Recorder for one run's directory. runs.Service calls
// it per run it executes or receives a signal for; nil disables signals.
type Opener func(runDir string) (Recorder, error)

// FileOpener is the Opener that writes FileName under the run directory
// with pol.
func FileOpener(pol Policy) Opener {
	return func(runDir string) (Recorder, error) { return Open(runDir, pol) }
}

// normalize validates sig against the closed sets and bounds, scrubs its
// text and returns the fields to write.
func normalize(sig Signal, scrub func(string) string) (Signal, error) {
	switch {
	case !KnownKind(sig.Kind):
		return Signal{}, fmt.Errorf("%w: kind %q is not one of %v", ErrBadRequest, sig.Kind, Kinds)
	case allowed[sig.Source] == nil:
		return Signal{}, fmt.Errorf("%w: source %q is unknown", ErrBadRequest, sig.Source)
	case !Allowed(sig.Source, sig.Kind):
		return Signal{}, fmt.Errorf("%w: %s may not emit %s", ErrKindNotAllowed, sig.Source, sig.Kind)
	}
	out := Signal{Kind: sig.Kind, Source: sig.Source, By: sig.By, Message: scrub(sig.Message)}
	if len(out.Message) > MaxMessageBytes {
		return Signal{}, fmt.Errorf("%w: message is %d bytes, over %d", ErrBadRequest, len(out.Message), MaxMessageBytes)
	}
	data, err := normalizeData(sig.Data, scrub)
	if err != nil {
		return Signal{}, err
	}
	out.Data = data
	return out, nil
}

// normalizeData checks raw is one JSON value within MaxDataBytes and
// returns it re-encoded with its string leaves scrubbed. Absent and null
// are nil.
func normalizeData(raw json.RawMessage, scrub func(string) string) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	if len(raw) > MaxDataBytes {
		return nil, fmt.Errorf("%w: %d bytes, over %d", ErrDataTooLarge, len(raw), MaxDataBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%w: data is not JSON: %v", ErrBadRequest, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: data must be a single JSON value", ErrBadRequest)
	}
	if v == nil {
		return nil, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(scrubValue(v, scrub)); err != nil {
		return nil, fmt.Errorf("%w: data: %v", ErrBadRequest, err)
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// scrubValue redacts credential-shaped strings in every string leaf.
func scrubValue(v any, scrub func(string) string) any {
	switch t := v.(type) {
	case string:
		return scrub(t)
	case map[string]any:
		for k, x := range t {
			t[k] = scrubValue(x, scrub)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = scrubValue(x, scrub)
		}
		return t
	}
	return v
}
