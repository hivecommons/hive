package agent

import (
	"strings"
	"sync"
	"unicode"

	"github.com/hivecommons/hive/crux"
)

// Output signals to kick observers (#18287). logOutputSignals has always
// logged "[STATUS]", "[FINDING]", "[COMPLETE]" and "[ERROR]" lines; they now
// also reach the fan-out kick observers with the agent name, the in-flight
// queued kick's ID, the signal kind and the (bounded) text, so a caller that
// queued a kick can follow what it produces.

const (
	// maxOutputSignalTextLen bounds Text and Detail on an output-signal
	// event, in runes.
	maxOutputSignalTextLen = 500
	// maxOutputSignalDedupe bounds the per-agent set of lines already
	// delivered for the current kick; a pane redraw re-diffs old lines.
	maxOutputSignalDedupe = 256
)

// outputSignalMarkers maps each delivered marker to its signal kind.
var outputSignalMarkers = []struct{ marker, signal string }{
	{"[STATUS]", crux.OutputSignalStatus},
	{"[FINDING]", crux.OutputSignalFinding},
	{"[COMPLETE]", crux.OutputSignalComplete},
	{"[ERROR]", crux.OutputSignalError},
}

// parseOutputSignal recognises a marker only as the first token of the line,
// after any leading CLI glyphs and whitespace ("⏺ [FINDING] x" counts). A
// marker mid-sentence (an echoed prompt explaining the convention, a quoted
// log) does not.
func parseOutputSignal(line string) (signal, text, whole string, ok bool) {
	trimmed := strings.TrimLeftFunc(line, func(r rune) bool {
		return r != '[' && !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, mk := range outputSignalMarkers {
		if rest, found := strings.CutPrefix(trimmed, mk.marker); found {
			text = boundRunes(strings.TrimSpace(rest), maxOutputSignalTextLen)
			whole = boundRunes(strings.TrimSpace(trimmed), maxOutputSignalTextLen)
			return mk.signal, text, whole, true
		}
	}
	return "", "", "", false
}

// boundRunes cuts s to at most n runes. Unlike truncateStr it adds no
// ellipsis, so the bound is exact.
func boundRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// outputSignalDedupe remembers, per agent, which signal lines were already
// delivered for the agent's current kick ID.
type outputSignalDedupe struct {
	mu      sync.Mutex
	byAgent map[string]*outputSignalSeen
}

type outputSignalSeen struct {
	kickID string
	lines  map[string]struct{}
}

// firstSighting reports whether this line has not yet been delivered for
// the agent's kickID, and records it.
func (d *outputSignalDedupe) firstSighting(agent, kickID, line string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.byAgent == nil {
		d.byAgent = make(map[string]*outputSignalSeen)
	}
	seen := d.byAgent[agent]
	if seen == nil || seen.kickID != kickID || len(seen.lines) >= maxOutputSignalDedupe {
		seen = &outputSignalSeen{kickID: kickID, lines: make(map[string]struct{})}
		d.byAgent[agent] = seen
	}
	if _, dup := seen.lines[line]; dup {
		return false
	}
	seen.lines[line] = struct{}{}
	return true
}

// emitOutputSignal delivers a recognised marker line to the fan-out kick
// observers. Safe to call with m.mu held.
func (m *Manager) emitOutputSignal(agent, line string) {
	signal, text, whole, ok := parseOutputSignal(line)
	if !ok {
		return
	}
	kickID := m.InFlightKickID(agent)
	if !m.outputSignalSeen.firstSighting(agent, kickID, whole) {
		return
	}
	m.emitKickEvent(crux.KickEvent{
		Agent:  agent,
		Event:  crux.KickEventOutputSignal,
		Detail: whole,
		KickID: kickID,
		Signal: signal,
		Text:   text,
	})
}
