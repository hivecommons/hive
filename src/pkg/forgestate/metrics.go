package forgestate

import (
	"sort"
	"strings"
	"sync"
)

// Metric series names. The dashboard's /metrics writer can render
// Counters.Snapshot under these names once a reader wires it.
const (
	MetricDrift        = "forgestate_drift"               // gauge {repo}
	MetricStale        = "forgestate_stale"               // gauge {repo}: 1 while stale
	MetricReconcile    = "forgestate_reconcile_total"     // {repo,result}
	MetricWebhookPatch = "forgestate_webhook_patch_total" // {repo,result}
	MetricWriteThrough = "forgestate_writethrough_total"  // {repo,op}
	MetricReadThrough  = "forgestate_readthrough_total"   // {repo,result}
	MetricBudgetDenied = "forgestate_budget_denied_total" // {bucket}
)

// Result labels recorded on the *_total series.
const (
	resultOK          = "ok"
	resultNotModified = "not_modified"
	resultError       = "error"
	resultDenied      = "denied"
	resultIgnored     = "ignored"
	resultRemoved     = "removed"
	resultCached      = "cached"
	resultMiss        = "miss"
)

// Metrics receives the cache's counters and gauges. Implementations must be
// safe for concurrent use.
type Metrics interface {
	Drift(repo string, n int)
	Stale(repo string, stale bool)
	Reconcile(repo, result string)
	WebhookPatch(repo, result string)
	WriteThrough(repo, op string)
	ReadThrough(repo, result string)
	BudgetDenied(bucket Bucket)
}

// Sample is one labelled metric value.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  int
}

// Counters is the in-process Metrics implementation (a mutex-guarded map the
// dashboard snapshots on scrape; no prometheus client dependency), in the
// pattern of stageengine.Counters.
type Counters struct {
	mu     sync.Mutex
	values map[string]map[string]int
	labels map[string]map[string]map[string]string
}

// NewCounters returns an empty set.
func NewCounters() *Counters {
	return &Counters{values: map[string]map[string]int{}, labels: map[string]map[string]map[string]string{}}
}

var defaultCounters = NewCounters()

// DefaultCounters is the process-wide set Options.Metrics defaults to.
func DefaultCounters() *Counters { return defaultCounters }

func labelKey(kv []string) (string, map[string]string) {
	labels := make(map[string]string, len(kv)/2)
	var b strings.Builder
	for i := 0; i+1 < len(kv); i += 2 {
		labels[kv[i]] = kv[i+1]
		b.WriteString(kv[i] + "=" + kv[i+1] + ",")
	}
	return b.String(), labels
}

func (c *Counters) apply(name string, set bool, n int, kv ...string) {
	key, labels := labelKey(kv)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.values[name] == nil {
		c.values[name] = map[string]int{}
		c.labels[name] = map[string]map[string]string{}
	}
	if set {
		c.values[name][key] = n
	} else {
		c.values[name][key] += n
	}
	c.labels[name][key] = labels
}

// Value returns one series' value (0 when never recorded).
func (c *Counters) Value(name string, kv ...string) int {
	key, _ := labelKey(kv)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[name][key]
}

// Drift implements Metrics.
func (c *Counters) Drift(repo string, n int) { c.apply(MetricDrift, true, n, "repo", repo) }

// Stale implements Metrics.
func (c *Counters) Stale(repo string, stale bool) {
	v := 0
	if stale {
		v = 1
	}
	c.apply(MetricStale, true, v, "repo", repo)
}

// Reconcile implements Metrics.
func (c *Counters) Reconcile(repo, result string) {
	c.apply(MetricReconcile, false, 1, "repo", repo, "result", result)
}

// WebhookPatch implements Metrics.
func (c *Counters) WebhookPatch(repo, result string) {
	c.apply(MetricWebhookPatch, false, 1, "repo", repo, "result", result)
}

// WriteThrough implements Metrics.
func (c *Counters) WriteThrough(repo, op string) {
	c.apply(MetricWriteThrough, false, 1, "repo", repo, "op", op)
}

// ReadThrough implements Metrics.
func (c *Counters) ReadThrough(repo, result string) {
	c.apply(MetricReadThrough, false, 1, "repo", repo, "result", result)
}

// BudgetDenied implements Metrics.
func (c *Counters) BudgetDenied(bucket Bucket) {
	c.apply(MetricBudgetDenied, false, 1, "bucket", string(bucket))
}

// Snapshot returns every series sorted by name then label key.
func (c *Counters) Snapshot() []Sample {
	c.mu.Lock()
	defer c.mu.Unlock()
	names := make([]string, 0, len(c.values))
	for n := range c.values {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []Sample
	for _, n := range names {
		keys := make([]string, 0, len(c.values[n]))
		for k := range c.values[n] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			labels := make(map[string]string, len(c.labels[n][k]))
			for lk, lv := range c.labels[n][k] {
				labels[lk] = lv
			}
			out = append(out, Sample{Name: n, Labels: labels, Value: c.values[n][k]})
		}
	}
	return out
}
