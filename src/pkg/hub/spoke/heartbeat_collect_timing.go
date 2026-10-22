package spoke

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// collectTimingTopN is how many of the slowest collector sections are named
// in the timeout WARN. Enough to see a tail, few enough to read in a log line.
const collectTimingTopN = 5

// collectTimings records how long each named section of one heartbeat
// collect() took, so that when the collect exceeds its budget the WARN can
// say WHICH sub-collector ate it instead of leaving the operator to bisect
// a 60-field closure. Sections still in flight at timeout time are reported
// with their elapsed-so-far duration and marked as such — the in-flight one
// is almost always the culprit (a bundle walk on NFS, a GitHub call).
type collectTimings struct {
	mu       sync.Mutex
	started  time.Time
	finished map[string]time.Duration
	inFlight map[string]time.Time
}

func newCollectTimings() *collectTimings {
	return &collectTimings{
		started:  time.Now(),
		finished: make(map[string]time.Duration),
		inFlight: make(map[string]time.Time),
	}
}

// currentCollectTimings is the recorder for the collect() currently running
// under collectWithTimeout. The collect closure is a plain StatusCollector with
// no way to receive a recorder, so TimedSection finds it here. Set before the
// collect goroutine starts and cleared when collectWithTimeout returns, so a
// section that straggles past the timeout (the goroutine cannot be cancelled)
// records into nothing rather than into the next beat's recorder.
var currentCollectTimings atomic.Pointer[collectTimings]

func (c *collectTimings) begin(name string) {
	c.mu.Lock()
	c.inFlight[name] = time.Now()
	c.mu.Unlock()
}

func (c *collectTimings) end(name string) {
	c.mu.Lock()
	if start, ok := c.inFlight[name]; ok {
		delete(c.inFlight, name)
		c.finished[name] += time.Since(start)
	}
	c.mu.Unlock()
}

// collectSectionTiming is one row of the timeout report.
type collectSectionTiming struct {
	Name     string
	Elapsed  time.Duration
	InFlight bool
}

// slowest returns up to n sections ordered by descending elapsed time, with
// in-flight sections measured up to now.
func (c *collectTimings) slowest(n int) []collectSectionTiming {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	out := make([]collectSectionTiming, 0, len(c.finished)+len(c.inFlight))
	for name, d := range c.finished {
		out = append(out, collectSectionTiming{Name: name, Elapsed: d})
	}
	for name, start := range c.inFlight {
		out = append(out, collectSectionTiming{Name: name, Elapsed: now.Sub(start), InFlight: true})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Elapsed != out[j].Elapsed {
			return out[i].Elapsed > out[j].Elapsed
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// summary renders the slowest sections as a single log attribute value, e.g.
// "evidence=27.3s(in-flight) agents=1.2s owner=310ms". Empty when the collect
// closure recorded no sections (an uninstrumented collector).
func (c *collectTimings) summary(n int) string {
	rows := c.slowest(n)
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		s := fmt.Sprintf("%s=%s", r.Name, r.Elapsed.Round(time.Millisecond))
		if r.InFlight {
			s += "(in-flight)"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

// TimedSection runs fn as a named section of the heartbeat collect currently
// running under collectWithTimeout, recording its duration for the timeout
// report. Outside a collect (no recorder installed) it just runs fn. Generic so
// a collector of any return type can be wrapped without an adapter:
//
//	evidence := spoke.TimedSection("evidence", b.dashSrv.EvidenceHeartbeat)
func TimedSection[T any](name string, fn func() T) T {
	c := currentCollectTimings.Load()
	if c == nil {
		return fn()
	}
	c.begin(name)
	defer c.end(name)
	return fn()
}

// logCollectTimeoutBreakdown emits the per-section WARN that names the slow
// collectors behind a timed-out collect. Separate from the timeout WARN itself
// so the existing message (and anything grepping for it) is unchanged.
func logCollectTimeoutBreakdown(logger *slog.Logger, c *collectTimings, timeout time.Duration) {
	if logger == nil || c == nil {
		return
	}
	s := c.summary(collectTimingTopN)
	if s == "" {
		return
	}
	logger.Warn("hub heartbeat collect budget exceeded — slowest collectors (in-flight = still running at timeout)",
		"timeout", timeout.String(),
		"slowest", s)
}
