package automerge

import (
	"time"

	hgithub "github.com/hivecommons/hive/pkg/github"
)

// Optional transport capability keeps test transports and other forges usable.
func (c *Engine) sweepClock() *hgithub.MergeSweepClock {
	if t, ok := c.transport.(interface {
		MergeSweepClock() *hgithub.MergeSweepClock
	}); ok {
		return t.MergeSweepClock()
	}
	return nil
}
func (c *Engine) scheduleSweep(next time.Time) {
	if clock := c.sweepClock(); clock != nil {
		clock.Schedule("lanes", next)
	}
}
func (c *Engine) beginSweep() func() {
	if clock := c.sweepClock(); clock != nil {
		return clock.Begin("lanes")
	}
	return func() {}
}
func (c *Engine) workingPR(repo string, number int) func() {
	if clock := c.sweepClock(); clock != nil {
		return clock.Working(repo, number)
	}
	return func() {}
}
