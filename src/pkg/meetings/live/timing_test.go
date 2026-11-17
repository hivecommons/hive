package live

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/betty"
)

// The live state (the meeting detail API's `live`) keeps each answer's
// stage timings and a latency summary.
func TestPersonaTimingsInLiveState(t *testing.T) {
	h := personaHarness(t, Policy{ChatPostsPerMinute: 20}, config.MeetingsPersonaConfig{}, &personaLLM{})
	h.live()
	h.line(meetings.EventChat, "Bo", "Betty, when does the release ship?", false)
	h.eventually("answer", h.posted("The release ships Friday."))
	h.eventually("timing recorded", func() bool { return len(h.state().PersonaTimings) == 1 })
	st := h.state()
	tm := st.PersonaTimings[0]
	if tm.Source != betty.SourceRetrieval || tm.Trigger != betty.TriggerAddressed || tm.Kind != betty.KindChat ||
		tm.TotalMS < tm.AnswerMS || tm.BudgetMS == 0 {
		t.Fatalf("timing %+v", tm)
	}
	if l := st.PersonaLatency; l == nil || l.Answers != 1 || l.MaxMS != tm.TotalMS || l.P50MS != tm.TotalMS {
		t.Fatalf("latency %+v", st.PersonaLatency)
	}
}

func TestRecordTimingKeepsTheLastAnswers(t *testing.T) {
	_, s := timingSession(t)
	for i := range maxPersonaTimings + 5 {
		s.recordTiming(betty.Reply{Trigger: betty.TriggerAddressed, Timing: &betty.Timing{HeardAt: s.now(), AnswerMS: int64(i), OverBudget: i%2 == 0}})
	}
	if n := len(s.state.PersonaTimings); n != maxPersonaTimings {
		t.Fatalf("kept %d timings", n)
	}
	if l := s.state.PersonaLatency; l.Answers != maxPersonaTimings+5 || l.OverBudget != (maxPersonaTimings+5+1)/2 {
		t.Fatalf("latency %+v", l)
	}
	s.recordTiming(betty.Reply{Trigger: betty.TriggerAck})
	if l := s.state.PersonaLatency; l.Answers != maxPersonaTimings+5 {
		t.Fatal("a reply without timings is not counted")
	}
}

func timingSession(t *testing.T) (*harness, *session) {
	t.Helper()
	h := newHarness(t, Policy{}, nil)
	return h, h.r.newSession(h.m, h.spek)
}
