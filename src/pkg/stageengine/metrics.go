package stageengine

import (
	"sort"
	"sync"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Metric series names (ADR-0025 §Metrics). The dashboard's /metrics writer
// renders Counters.Snapshot under these names.
const (
	MetricDerivations     = "stage_derivations_total"            // {kind,stage}
	MetricAnomaly         = "stage_transition_anomaly_total"     // {kind,from,to}
	MetricWriterWrites    = "stage_writer_writes_total"          // {kind,op}
	MetricDuplicateHealed = "stage_duplicate_pills_healed_total" // {kind}
)

// Writer ops recorded on MetricWriterWrites.
const (
	OpAdd    = "add"
	OpRemove = "remove"
)

// Metrics receives the reconciler's counters. Implementations must be safe
// for concurrent use.
type Metrics interface {
	Derivation(kind stagelabels.Item, stage stagelabels.Stage)
	TransitionAnomaly(kind stagelabels.Item, from, to stagelabels.Stage)
	WriterWrite(kind stagelabels.Item, op string, n int)
	DuplicateHealed(kind stagelabels.Item)
}

// Sample is one labelled counter value.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  int
}

// Counters is the in-process Metrics implementation, in the pattern of the
// other hive lanes (a mutex-guarded struct the dashboard snapshots on
// scrape; no prometheus client dependency).
type Counters struct {
	mu     sync.Mutex
	values map[string]map[string]int // name → label key → value
	labels map[string]map[string]map[string]string
}

// NewCounters returns an empty counter set.
func NewCounters() *Counters {
	return &Counters{values: map[string]map[string]int{}, labels: map[string]map[string]map[string]string{}}
}

var defaultCounters = NewCounters()

// DefaultCounters is the process-wide counter set Options.Metrics defaults
// to; the dashboard reads it for /metrics and the tracker status strip.
func DefaultCounters() *Counters { return defaultCounters }

func (c *Counters) add(name string, n int, kv ...string) {
	labels := make(map[string]string, len(kv)/2)
	key := ""
	for i := 0; i+1 < len(kv); i += 2 {
		labels[kv[i]] = kv[i+1]
		key += kv[i] + "=" + kv[i+1] + ","
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.values[name] == nil {
		c.values[name] = map[string]int{}
		c.labels[name] = map[string]map[string]string{}
	}
	c.values[name][key] += n
	c.labels[name][key] = labels
}

func stageLabel(s stagelabels.Stage) string {
	if s == stagelabels.None {
		return "none"
	}
	return s.Slug()
}

// Derivation implements Metrics.
func (c *Counters) Derivation(kind stagelabels.Item, stage stagelabels.Stage) {
	c.add(MetricDerivations, 1, "kind", kind.String(), "stage", stageLabel(stage))
}

// TransitionAnomaly implements Metrics.
func (c *Counters) TransitionAnomaly(kind stagelabels.Item, from, to stagelabels.Stage) {
	c.add(MetricAnomaly, 1, "kind", kind.String(), "from", stageLabel(from), "to", stageLabel(to))
}

// WriterWrite implements Metrics.
func (c *Counters) WriterWrite(kind stagelabels.Item, op string, n int) {
	if n <= 0 {
		return
	}
	c.add(MetricWriterWrites, n, "kind", kind.String(), "op", op)
}

// DuplicateHealed implements Metrics.
func (c *Counters) DuplicateHealed(kind stagelabels.Item) {
	c.add(MetricDuplicateHealed, 1, "kind", kind.String())
}

// Snapshot returns every counter, sorted by name then label key, for the
// metrics writer.
func (c *Counters) Snapshot() []Sample {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Sample
	for name, byKey := range c.values {
		for key, v := range byKey {
			labels := make(map[string]string, len(c.labels[name][key]))
			for k, lv := range c.labels[name][key] {
				labels[k] = lv
			}
			out = append(out, Sample{Name: name, Labels: labels, Value: v})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return labelKey(out[i].Labels) < labelKey(out[j].Labels)
	})
	return out
}

// Total sums one series across labels (tests and the status strip).
func (c *Counters) Total(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.values[name] {
		n += v
	}
	return n
}

func labelKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s := ""
	for _, k := range keys {
		s += k + "=" + labels[k] + ","
	}
	return s
}
