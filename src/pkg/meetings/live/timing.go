package live

import (
	"slices"
	"time"

	"github.com/hivecommons/hive/pkg/meetings/betty"
)

// Persona answer timings in the live state (hivecommons/hive#18590): the
// meeting detail API (GET /api/meetings/{id}/live, `live.persona_timings`
// and `live.persona_latency`) shows every recent answer's stages, heard to
// retrieval to model to answer ready to sent, and a summary against the
// answer budgets.

// maxPersonaTimings bounds the answers kept with their timings.
const maxPersonaTimings = 20

// PersonaTiming is one persona answer's stages.
type PersonaTiming struct {
	betty.Timing
	Trigger string `json:"trigger"`
	Kind    string `json:"kind"`
	// DeliverMS is answer ready to sent (chat) or handed to the meeting's
	// voice (spoken), including any wait behind the chat rate.
	DeliverMS int64 `json:"deliver_ms"`
	// SayMS is how long the meeting took to accept the speech (TTS on
	// Discord synthesises before it accepts), for a spoken answer.
	SayMS int64 `json:"say_ms,omitempty"`
	// TotalMS is heard to sent.
	TotalMS int64 `json:"total_ms"`
}

// PersonaLatency sums the persona's answer timings: counts over the
// meeting, percentiles over the kept answers.
type PersonaLatency struct {
	Answers    int   `json:"answers"`
	OverBudget int   `json:"over_budget"`
	P50MS      int64 `json:"p50_ms"`
	MaxMS      int64 `json:"max_ms"`
	// LastAckSayMS is how long the last spoken acknowledgement took to be
	// accepted.
	LastAckSayMS int64 `json:"last_ack_say_ms,omitempty"`
}

// recordTiming adds a persona answer's timings once it is out.
func (s *session) recordTiming(rep betty.Reply) {
	if rep.Timing == nil {
		return
	}
	now := s.now()
	ready := rep.Timing.HeardAt.Add(time.Duration(rep.Timing.AnswerMS) * time.Millisecond)
	t := PersonaTiming{Timing: *rep.Timing, Trigger: rep.Trigger, Kind: rep.Kind,
		DeliverMS: max(0, now.Sub(ready).Milliseconds()), TotalMS: max(0, now.Sub(rep.Timing.HeardAt).Milliseconds())}
	if rep.Kind == betty.KindVoice {
		t.SayMS = s.lastSay.Milliseconds()
	}
	s.state.PersonaTimings = append(s.state.PersonaTimings, t)
	if n := len(s.state.PersonaTimings); n > maxPersonaTimings {
		s.state.PersonaTimings = s.state.PersonaTimings[n-maxPersonaTimings:]
	}
	l := s.latency()
	l.Answers++
	if t.OverBudget {
		l.OverBudget++
	}
	totals := make([]int64, 0, len(s.state.PersonaTimings))
	for _, x := range s.state.PersonaTimings {
		totals = append(totals, x.TotalMS)
	}
	slices.Sort(totals)
	l.P50MS, l.MaxMS = totals[len(totals)/2], totals[len(totals)-1]
}

// recordAckSay notes how long a spoken acknowledgement took to go out.
func (s *session) recordAckSay() {
	s.latency().LastAckSayMS = s.lastSay.Milliseconds()
}

func (s *session) latency() *PersonaLatency {
	if s.state.PersonaLatency == nil {
		s.state.PersonaLatency = &PersonaLatency{}
	}
	return s.state.PersonaLatency
}

// timedSay calls the meeting's say and keeps how long it took.
func (s *session) timedSay(say func() error) error {
	start := time.Now()
	err := say()
	s.lastSay = time.Since(start)
	return err
}
