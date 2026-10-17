package hivestate

import (
	"fmt"
	"sort"
	"time"
)

// Observation kinds; the kind is half of an Observation's Fingerprint.
const (
	KindQueueGrowth = "queue-growth"
	KindHoldGrowth  = "hold-growth"
	KindSLA         = "sla-violations"
	KindAgentIdle   = "agent-idle"
	KindThrashing   = "agent-thrashing"
	KindTokens      = "token-efficiency"
)

// Detector thresholds. Every detector fails closed below its minimum coverage.
const (
	week = 7 * 24 * time.Hour

	minWeekCoverageHours = 5 * 24 // 7 d checks need 5 days of samples

	queueGrowthRatio   = 1.5  // p90 depth up >= 50 % week-over-week
	holdGrowthDays     = 3    // consecutive daily increases of the median hold
	slaRateThreshold   = 0.25 // fraction of samples with a violation
	slaRiseRatio       = 1.5  // week-over-week rise factor ...
	slaRiseMinDelta    = 0.05 // ... and absolute rise
	idleRatioThreshold = 0.90
	thrashKicksPerDay  = 500.0 // about one kick per 120 s cycle is 720/day
	tokenGrowthRatio   = 1.5
	queueFlatTolerance = 0.2 // relative; at least 1 item of slack
)

// Observation is one hive-wide finding derived from the snapshot series.
// Fingerprint is stable per hive and kind so a sink can dedupe on it. Series
// is the daily series that supports the finding, oldest first.
type Observation struct {
	Kind           string
	Title          string
	Evidence       string
	SuspectedCause string
	NextStep       string
	Fingerprint    string
	Series         []float64
}

// Observe runs every detector over buckets as of now and returns the
// observations that fire, in a fixed order. It needs buckets going back 14
// days for the week-over-week checks, and returns nothing for an empty series
// or when a detector's windows are too thinly covered.
//
// There is no mode detector: Bucket does not carry Snapshot.Mode.
func Observe(buckets []Bucket, now time.Time, hiveID string) []Observation {
	sorted := append([]Bucket(nil), buckets...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Start.Before(sorted[j].Start) })
	cur := summarizeWindow(sorted, now, week)
	if cur.Samples == 0 || cur.CoverageHours < minWeekCoverageHours {
		return nil
	}
	prev := priorWeek(sorted, now)

	var out []Observation
	add := func(o *Observation) {
		if o != nil {
			o.Fingerprint = hiveID + ":" + o.Kind
			out = append(out, *o)
		}
	}
	add(detectQueueGrowth(sorted, now, cur, prev))
	add(detectHoldGrowth(sorted, now))
	add(detectSLA(sorted, now, cur, prev))
	idle, thrash := detectAgents(sorted, now, cur)
	add(idle)
	add(thrash)
	add(detectTokens(sorted, now, cur, prev))
	return out
}

// priorWeek summarizes the 7 days before the last 7 days.
func priorWeek(sorted []Bucket, now time.Time) Window {
	end := now.Add(-week)
	i := sort.Search(len(sorted), func(i int) bool { return !sorted[i].Start.Before(end) })
	return summarizeWindow(sorted[:i], end, week)
}

func priorUsable(prev Window) bool {
	return prev.Samples > 0 && prev.CoverageHours >= minWeekCoverageHours
}

// dailyBuckets merges buckets into one aggregate per 24 h day ending at now,
// oldest first; a day without samples has Samples == 0.
func dailyBuckets(sorted []Bucket, now time.Time, days int) []Bucket {
	out := make([]Bucket, days)
	for _, b := range sorted {
		if b.Start.After(now) {
			continue
		}
		back := int(now.Sub(b.Start) / (24 * time.Hour))
		if back >= days {
			continue
		}
		out[days-1-back].merge(b)
	}
	return out
}

func dailySeries(sorted []Bucket, now time.Time, days int, f func(Bucket) float64) []float64 {
	daily := dailyBuckets(sorted, now, days)
	out := make([]float64, len(daily))
	for i, d := range daily {
		if d.Samples > 0 {
			out[i] = f(d)
		}
	}
	return out
}

func depthP90(b Bucket) float64 {
	return float64(quantiles(b.QueueIssues).P90 + quantiles(b.QueuePRs).P90)
}

func detectQueueGrowth(sorted []Bucket, now time.Time, cur, prev Window) *Observation {
	if !priorUsable(prev) {
		return nil
	}
	c := float64(cur.QueueIssues.P90 + cur.QueuePRs.P90)
	p := float64(prev.QueueIssues.P90 + prev.QueuePRs.P90)
	if p <= 0 || c < p*queueGrowthRatio {
		return nil
	}
	return &Observation{
		Kind:           KindQueueGrowth,
		Title:          "Queue depth is growing week-over-week",
		Evidence:       fmt.Sprintf("p90 queue depth (issues + PRs) is %.0f over the last 7 d versus %.0f in the prior 7 d (+%.0f%%).", c, p, (c/p-1)*100),
		SuspectedCause: "Intake is outpacing throughput, or work is stalling in review or merge.",
		NextStep:       "Check which queue grew and whether agents are idle or blocked on it.",
		Series:         dailySeries(sorted, now, 14, depthP90),
	}
}

func detectHoldGrowth(sorted []Bucket, now time.Time) *Observation {
	daily := dailyBuckets(sorted, now, 7)
	med := make([]float64, len(daily))
	for i, d := range daily {
		if d.Samples > 0 {
			med[i] = float64(quantiles(d.QueueHold).Median)
		}
	}
	n := len(daily)
	for i := n - holdGrowthDays - 1; i < n; i++ {
		if i < 0 || daily[i].Samples == 0 {
			return nil
		}
	}
	for i := n - holdGrowthDays; i < n; i++ {
		if med[i] <= med[i-1] {
			return nil
		}
	}
	return &Observation{
		Kind:           KindHoldGrowth,
		Title:          "Held queue items have grown for several days",
		Evidence:       fmt.Sprintf("Daily median hold count rose %d days in a row: %.0f → %.0f.", holdGrowthDays, med[n-holdGrowthDays-1], med[n-1]),
		SuspectedCause: "Items are being held faster than they are released or resolved.",
		NextStep:       "Review the held items for a shared blocker.",
		Series:         med,
	}
}

func detectSLA(sorted []Bucket, now time.Time, cur, prev Window) *Observation {
	over := cur.SLAViolationRate > slaRateThreshold
	rising := priorUsable(prev) && cur.SLAViolationRate-prev.SLAViolationRate >= slaRiseMinDelta &&
		cur.SLAViolationRate >= prev.SLAViolationRate*slaRiseRatio
	if !over && !rising {
		return nil
	}
	ev := fmt.Sprintf("%.0f%% of samples in the last 7 d had an SLA violation", cur.SLAViolationRate*100)
	if priorUsable(prev) {
		ev += fmt.Sprintf(" (prior 7 d: %.0f%%)", prev.SLAViolationRate*100)
	}
	return &Observation{
		Kind:           KindSLA,
		Title:          "SLA violations are frequent or rising",
		Evidence:       ev + ".",
		SuspectedCause: "Work is waiting longer than its SLA, typically from capacity shortfall or a stuck stage.",
		NextStep:       "Identify the oldest violating items and what they are waiting on.",
		Series: dailySeries(sorted, now, 14, func(b Bucket) float64 {
			return float64(b.SLAViolatedSamples) / float64(b.Samples)
		}),
	}
}

func detectAgents(sorted []Bucket, now time.Time, cur Window) (idle, thrash *Observation) {
	if cur.AgentIdleRatio <= idleRatioThreshold {
		return nil, nil
	}
	series := dailySeries(sorted, now, 7, func(b Bucket) float64 {
		if b.AgentSamples == 0 {
			return 0
		}
		return float64(b.IdleAgentSamples) / float64(b.AgentSamples)
	})
	if cur.KicksPerDay >= thrashKicksPerDay {
		return nil, &Observation{
			Kind:           KindThrashing,
			Title:          "Agents are being kicked constantly but stay idle",
			Evidence:       fmt.Sprintf("Agents were idle %.0f%% of agent-samples over 7 d while kicked %.0f times per day.", cur.AgentIdleRatio*100, cur.KicksPerDay),
			SuspectedCause: "Kicks are not producing work: nothing eligible to do, or the agent fails to start.",
			NextStep:       "Inspect recent kicked agents for empty runs and stop kicking them when there is no work.",
			Series:         series,
		}
	}
	return &Observation{
		Kind:           KindAgentIdle,
		Title:          "Agents have been idle almost all week",
		Evidence:       fmt.Sprintf("Agents were idle %.0f%% of agent-samples over the last 7 d.", cur.AgentIdleRatio*100),
		SuspectedCause: "More agents are configured than the queue can feed.",
		NextStep:       "Consider pausing or removing agents with nothing to do.",
		Series:         series,
	}, nil
}

// detectTokens uses median queue depth as a proxy for throughput: Bucket does
// not carry merged-PR counts, so a flat queue stands in for flat output.
func detectTokens(sorted []Bucket, now time.Time, cur, prev Window) *Observation {
	if !priorUsable(prev) || prev.TokensPerDay <= 0 || cur.TokensPerDay < prev.TokensPerDay*tokenGrowthRatio {
		return nil
	}
	c := float64(cur.QueueIssues.Median + cur.QueuePRs.Median)
	p := float64(prev.QueueIssues.Median + prev.QueuePRs.Median)
	tol := queueFlatTolerance * p
	if tol < 1 {
		tol = 1
	}
	if c-p > tol || p-c > tol {
		return nil
	}
	return &Observation{
		Kind:           KindTokens,
		Title:          "Token spend is up with no change in throughput",
		Evidence:       fmt.Sprintf("Tokens per day are %.0f over the last 7 d versus %.0f in the prior 7 d, while median queue depth is flat (%.0f vs %.0f).", cur.TokensPerDay, prev.TokensPerDay, c, p),
		SuspectedCause: "Agents are spending more per unit of work: larger contexts, retries, or a costlier model.",
		NextStep:       "Compare per-agent token use across the two weeks.",
		Series: dailySeries(sorted, now, 14, func(b Bucket) float64 {
			return float64(b.Tokens)
		}),
	}
}
