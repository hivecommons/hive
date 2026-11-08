// Package signals is the turn-signal work service (hivecommons/crux#114,
// S5): where a contained run's turn is — launched, thinking, running a
// tool, waiting on a person, blocked — and how it ended, kept with the run
// so a driver that missed the live event learns it from the saved record.
//
// A run's signals are one append-only signals.jsonl in the run's directory,
// one line per signal with a seq that only grows. Three sources append:
// the S2 runner (started when the sandbox launches, done on every terminal
// path), a gate opened for the run (needs-input, input, blocked) and the
// run's owner through the API (thinking, tool, blocked, needs-input,
// input). The kind set is closed, done is always the last signal, and the
// message and data are scrubbed and bounded before they are written.
//
// The package also keeps the RFC's S5 proper: the last KickOutcome of each
// agent, saved under <workdir>/signals/agents/ (AgentOutcomes).
package signals

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/logscrub"
)

const (
	// FileName is the log inside a run's directory.
	FileName = "signals.jsonl"
	// DirName is the per-concern directory under the work dir; it holds
	// AgentsDirName.
	DirName = "signals"
	// AgentsDirName holds one saved kick outcome per agent.
	AgentsDirName = "agents"
	// MaxMessageBytes bounds a signal's message.
	MaxMessageBytes = 1024
	// MaxDataBytes bounds a signal's data once encoded.
	MaxDataBytes = 8 << 10
	// DefaultPageSize is the page when the caller gives no limit.
	DefaultPageSize = 100
	// MaxPageSize caps a single read; a follower pages with since_seq.
	MaxPageSize = 1000
)

// Typed errors.
var (
	// ErrBadRequest: an unknown kind or source, a kind the source may not
	// append, a message or data over its bound, data that is not one JSON
	// value.
	ErrBadRequest = errors.New("signals: bad request")
	// ErrClosed: the run's done signal is recorded; nothing follows it.
	ErrClosed = errors.New("signals: the run's done signal is already recorded")
	// ErrNotFound: the run directory holds no signal log, or no outcome is
	// saved for the agent.
	ErrNotFound = errors.New("signals: not found")
	// ErrCorrupt: a line of a log or a saved record did not parse.
	ErrCorrupt = errors.New("signals: corrupt record")
)

// kinds is the closed kind set.
var kinds = map[string]bool{
	api.SignalStarted: true, api.SignalThinking: true, api.SignalTool: true, api.SignalBlocked: true,
	api.SignalNeedsInput: true, api.SignalInput: true, api.SignalDone: true, api.SignalStep: true,
}

// bySource is what each source may append. The runner owns the run's
// boundaries and the steps an agent session ran (S6); a gate reports a
// person being asked, answering or not answering in time; the caller
// reports everything in between but never a boundary, so a done signal
// always means the runner saw the run end.
var bySource = map[string]map[string]bool{
	api.SignalSourceRunner: {api.SignalStarted: true, api.SignalDone: true, api.SignalStep: true},
	api.SignalSourceGate:   {api.SignalNeedsInput: true, api.SignalInput: true, api.SignalBlocked: true},
	api.SignalSourceCaller: {api.SignalThinking: true, api.SignalTool: true, api.SignalBlocked: true, api.SignalNeedsInput: true, api.SignalInput: true},
}

// Allowed reports whether source may append kind.
func Allowed(source, kind string) bool { return bySource[source][kind] }

// Signal is what a source appends; Seq and At are assigned by the Book.
type Signal struct {
	Kind    string
	Source  string
	Message string
	// Data is one JSON value, at most MaxDataBytes encoded.
	Data json.RawMessage
}

// Policy is how a Book scrubs and stamps signals.
type Policy struct {
	// Scrub redacts the message and the string leaves of the data; nil
	// uses logscrub.ScrubString.
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

// normalize validates in and returns the message and data to write. A
// caller's oversized message is refused; the runner's and a gate's (a
// failure reason, a gate subject) are cut to the bound instead, so a long
// reason never costs the run its done signal.
func normalize(in Signal, scrub func(string) string) (string, json.RawMessage, error) {
	if !kinds[in.Kind] {
		return "", nil, fmt.Errorf("%w: kind %q is not one of started, thinking, tool, blocked, needs-input, input, done", ErrBadRequest, in.Kind)
	}
	if _, ok := bySource[in.Source]; !ok {
		return "", nil, fmt.Errorf("%w: source %q is not runner, gate or caller", ErrBadRequest, in.Source)
	}
	if !Allowed(in.Source, in.Kind) {
		return "", nil, fmt.Errorf("%w: a %s may not append a %s signal", ErrBadRequest, in.Source, in.Kind)
	}
	msg := strings.TrimSpace(in.Message)
	if in.Source == api.SignalSourceCaller && len(msg) > MaxMessageBytes {
		return "", nil, fmt.Errorf("%w: message is %d bytes, over %d", ErrBadRequest, len(msg), MaxMessageBytes)
	}
	// Scrub before cutting, so a cut never splits a secret into a shape
	// the scrubber no longer recognises.
	msg = truncate(scrub(msg), MaxMessageBytes)
	data, err := normalizeData(in.Data, scrub)
	if err != nil {
		return "", nil, err
	}
	return msg, data, nil
}

// normalizeData checks the payload is one JSON value no larger than
// MaxDataBytes and returns it compactly encoded with its string leaves
// scrubbed. JSON null and an absent payload are nil.
func normalizeData(raw json.RawMessage, scrub func(string) string) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
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
	out, err := encode(scrubValue(v, scrub))
	if err != nil {
		return nil, fmt.Errorf("%w: data: %v", ErrBadRequest, err)
	}
	if len(out) > MaxDataBytes {
		return nil, fmt.Errorf("%w: data is %d bytes encoded, over %d", ErrBadRequest, len(out), MaxDataBytes)
	}
	return out, nil
}

// encode marshals v compactly without HTML escaping.
func encode(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// scrubValue redacts credential-shaped strings in every string leaf.
func scrubValue(v any, scrub func(string) string) any {
	switch t := v.(type) {
	case string:
		return scrub(t)
	case map[string]any:
		for k, e := range t {
			t[k] = scrubValue(e, scrub)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = scrubValue(e, scrub)
		}
		return t
	}
	return v
}

// truncate cuts s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
