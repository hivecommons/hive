package agent

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RepeatFailureResult preserves the distinction between a numeric exit status
// and a backend that only reports success/failure. A nil ExitCode uses Failed.
type RepeatFailureResult struct {
	ExitCode *int
	Failed   bool
}

func (r RepeatFailureResult) key() string {
	if r.ExitCode != nil {
		return "exit:" + strconv.Itoa(*r.ExitCode)
	}
	return "failed"
}

// RepeatFailureNote contains only caller-supplied scrubbed, normalized text.
// Latest is the last occurrence of the result that raised the note.
type RepeatFailureNote struct {
	Command string
	Result  RepeatFailureResult
	Count   int
	Latest  time.Time
}

type repeatFailureHistory struct {
	results map[string][]time.Time
	note    *RepeatFailureNote
}

// RepeatFailureDetector is a passive, in-memory sliding window. It has no
// Manager reference or lifecycle callbacks: consumers decide how to display and
// log the returned notes, and cannot pause an agent through this API.
//
// Callers must scrub and normalize commands before Record. Agents are keyed by
// session identity; Forget releases their state when a session is removed.
type RepeatFailureDetector struct {
	mu        sync.Mutex
	threshold int
	window    time.Duration
	exclude   []string
	agents    map[string]map[string]*repeatFailureHistory
}

// NewRepeatFailureDetector uses the initial-release defaults for nonpositive
// threshold/window. A nil exclusion list selects the defaults; an empty list
// explicitly disables exclusions. Construction does not enable any producer.
func NewRepeatFailureDetector(threshold int, window time.Duration, exclude []string) *RepeatFailureDetector {
	if threshold <= 0 {
		threshold = 5
	}
	if window <= 0 {
		window = 10 * time.Minute
	}
	if exclude == nil {
		exclude = []string{"gh pr checks", "gh run watch"}
	}
	return &RepeatFailureDetector{threshold: threshold, window: window, exclude: append([]string(nil), exclude...), agents: make(map[string]map[string]*repeatFailureHistory)}
}

// Record returns a note only when a command first crosses the threshold. now
// is the observation time supplied by the collector's clock, in arrival order.
// Success clears every failure result for that command. Different exit codes
// never contribute to one another's threshold.
func (d *RepeatFailureDetector) Record(agent, command string, result RepeatFailureResult, now time.Time) *RepeatFailureNote {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prune(agent, now)
	if command == "" {
		return nil
	}
	for _, prefix := range d.exclude {
		if strings.HasPrefix(command, prefix) {
			return nil
		}
	}
	failed := result.Failed
	if result.ExitCode != nil {
		failed = *result.ExitCode != 0
	}
	if !failed {
		delete(d.agents[agent], command)
		if len(d.agents[agent]) == 0 {
			delete(d.agents, agent)
		}
		return nil
	}
	commands := d.agents[agent]
	if commands == nil {
		commands = make(map[string]*repeatFailureHistory)
		d.agents[agent] = commands
	}
	history := commands[command]
	if history == nil {
		history = &repeatFailureHistory{results: make(map[string][]time.Time)}
		commands[command] = history
	}
	key := result.key()
	times := append(history.results[key], now)
	history.results[key] = times
	if history.note != nil {
		if history.note.Result.key() == key {
			history.note.Count = len(times)
			history.note.Latest = now
		}
		return nil
	}
	if len(times) < d.threshold {
		return nil
	}
	note := RepeatFailureNote{Command: command, Result: copyRepeatFailureResult(result), Count: len(times), Latest: now}
	history.note = &note
	copy := note
	copy.Result = copyRepeatFailureResult(note.Result)
	return &copy
}

func copyRepeatFailureResult(r RepeatFailureResult) RepeatFailureResult {
	if r.ExitCode != nil {
		code := *r.ExitCode
		r.ExitCode = &code
	}
	return r
}

// Notes expires stale notes even when no new records arrive. The newest note
// comes first, so a row can show it and len(notes)-1 additional commands.
func (d *RepeatFailureDetector) Notes(agent string, now time.Time) []RepeatFailureNote {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prune(agent, now)
	var notes []RepeatFailureNote
	for _, h := range d.agents[agent] {
		if h.note != nil {
			n := *h.note
			n.Result = copyRepeatFailureResult(n.Result)
			notes = append(notes, n)
		}
	}
	sort.Slice(notes, func(i, j int) bool {
		if notes[i].Latest.Equal(notes[j].Latest) {
			return notes[i].Command < notes[j].Command
		}
		return notes[i].Latest.After(notes[j].Latest)
	})
	return notes
}

func (d *RepeatFailureDetector) prune(agent string, now time.Time) {
	cutoff := now.Add(-d.window)
	for command, h := range d.agents[agent] {
		for key, times := range h.results {
			first := 0
			for first < len(times) && !times[first].After(cutoff) {
				first++
			}
			if first == len(times) {
				delete(h.results, key)
			} else {
				h.results[key] = times[first:]
			}
		}
		if h.note != nil {
			if !h.note.Latest.After(cutoff) {
				h.note = nil
			} else {
				h.note.Count = len(h.results[h.note.Result.key()])
			}
		}
		if len(h.results) == 0 && h.note == nil {
			delete(d.agents[agent], command)
		}
	}
	if len(d.agents[agent]) == 0 {
		delete(d.agents, agent)
	}
}

// Forget discards all observations when the owning agent session ends.
func (d *RepeatFailureDetector) Forget(agent string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.agents, agent)
}
