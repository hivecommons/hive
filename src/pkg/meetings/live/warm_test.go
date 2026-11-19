package live

import (
	"context"
	"sync/atomic"
	"testing"
)

// warmResearch is a dispatcher that also warms its agents.
type warmResearch struct {
	fakeResearch
	warms atomic.Int32
}

func (w *warmResearch) Warm(context.Context) { w.warms.Add(1) }

// A live meeting warms the research pool when it starts, so the first
// question does not hit a cold agent (live Discord test).
func TestLiveMeetingWarmsResearch(t *testing.T) {
	w := &warmResearch{}
	h := newHarness(t, Policy{}, func(o *Options) { o.Research = w })
	h.live()
	h.eventually("research warmed", func() bool { return w.warms.Load() == 1 })
}
