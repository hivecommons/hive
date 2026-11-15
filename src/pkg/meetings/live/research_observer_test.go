package live

import (
	"context"
	"strings"
	"testing"
)

// An in-process runtime can finish a kick before Dispatch returns. The
// observer must already be installed and route those results to the session.
type immediateResearch struct {
	fakeResearch
	fn      func(ResearchSignal)
	removed chan struct{}
	fail    bool
}

func (f *immediateResearch) ObserveResearch(fn func(ResearchSignal)) func() {
	f.fn = fn
	return func() { close(f.removed) }
}

func (f *immediateResearch) Dispatch(ctx context.Context, q string, meta map[string]string) (string, error) {
	kick, err := f.fakeResearch.Dispatch(ctx, q, meta)
	if err != nil {
		return kick, err
	}
	f.fn(ResearchSignal{KickID: kick, MeetingID: meta["meeting_id"], Kind: SignalFinding, Text: "published finding"})
	kind, text := SignalComplete, "research summary"
	if f.fail {
		kind, text = SignalError, "research failed"
	}
	f.fn(ResearchSignal{KickID: kick, MeetingID: meta["meeting_id"], Kind: kind, Text: text})
	return kick, nil
}

func TestObservedResearchBeforeDispatchReturns(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "complete"
		if fail {
			name = "failed"
		}
		t.Run(name, func(t *testing.T) {
			research := &immediateResearch{removed: make(chan struct{}), fail: fail}
			h := newHarness(t, Policy{ResearchPerMeeting: 2}, func(o *Options) { o.Research = research })
			h.live()
			h.spek.intent(`{"type":"research","rid":"r1","query":"meeting question"}`)
			want := ResearchDone
			if fail {
				want = ResearchFailed
			}
			h.eventually("terminal research result", func() bool {
				st := h.state()
				return len(st.Research) == 1 && st.Research[0].Status == want
			})
			it := h.state().Research[0]
			if len(it.Findings) != 1 || it.Findings[0] != "published finding" {
				t.Fatalf("findings lost: %+v", it)
			}
			if !h.spek.has(`"status":"running"`) || !h.spek.has(`"status":"`+want+`"`) {
				t.Fatalf("missing research updates: %v", h.spek.stdin())
			}
			if !fail {
				h.eventually("knowledge fact", func() bool { return h.notes.count() == 1 })
				h.eventually("chat summary", func() bool {
					posts, _ := h.mt.got()
					return len(posts) == 1 && strings.Contains(posts[0], "research summary")
				})
			}
			h.r.Close()
			select {
			case <-research.removed:
			default:
				t.Fatal("research observer survived runner shutdown")
			}
		})
	}
}
