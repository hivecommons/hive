package github

import (
	"strings"
	"sync"
	"time"
)

// MergeSweepTimes is an observational clock, never an input to merge admission.
// NextSweepAt is the scheduled floor pass; events can cause an earlier pass.
type MergeSweepTimes struct {
	SweepRunning bool       `json:"sweep_running"`
	LastSweepAt  *time.Time `json:"last_sweep_at"`
	NextSweepAt  *time.Time `json:"next_sweep_at"`
}

type mergeSweepLane struct {
	last, next time.Time
	running    int
}

// MergeSweepClock belongs to a client so separate hives cannot share clocks.
// All lanes may update it concurrently with dashboard reads.
type MergeSweepClock struct {
	mu     sync.Mutex
	lanes  map[string]mergeSweepLane
	active map[string]map[int]int
}

func (c *Client) MergeSweepClock() *MergeSweepClock { return &c.mergeSweepClock }

func (c *MergeSweepClock) Schedule(lane string, next time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lanes == nil {
		c.lanes = make(map[string]mergeSweepLane)
	}
	l := c.lanes[lane]
	l.next = next
	c.lanes[lane] = l
}

// Begin records a whole pass; the returned function records completion even
// when the pass exits early or fails. Overlapping lanes retain their clocks.
func (c *MergeSweepClock) Begin(lane string) func() {
	c.mu.Lock()
	if c.lanes == nil {
		c.lanes = make(map[string]mergeSweepLane)
	}
	l := c.lanes[lane]
	l.running++
	c.lanes[lane] = l
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		l := c.lanes[lane]
		l.running--
		l.last = time.Now().UTC()
		c.lanes[lane] = l
	}
}

// Working marks only the PR currently being evaluated, rather than every PR
// in a running sweep. A counter handles concurrent evaluations of the same PR.
func (c *MergeSweepClock) Working(repo string, number int) func() {
	repo = strings.ToLower(repo)
	c.mu.Lock()
	if c.active == nil {
		c.active = make(map[string]map[int]int)
	}
	if c.active[repo] == nil {
		c.active[repo] = make(map[int]int)
	}
	c.active[repo][number]++
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.active[repo][number]--
		if c.active[repo][number] == 0 {
			delete(c.active[repo], number)
		}
	}
}

func (c *MergeSweepClock) Snapshot(repo string, number int) MergeSweepTimes {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := MergeSweepTimes{SweepRunning: c.active[strings.ToLower(repo)][number] > 0}
	for _, l := range c.lanes {
		if !l.last.IsZero() && (out.LastSweepAt == nil || l.last.After(*out.LastSweepAt)) {
			t := l.last
			out.LastSweepAt = &t
		}
		if !l.next.IsZero() && (out.NextSweepAt == nil || l.next.Before(*out.NextSweepAt)) {
			t := l.next
			out.NextSweepAt = &t
		}
	}
	return out
}
