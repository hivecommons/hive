package agent

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
)

func TestKickObserverArchiveDetailCarriesSource(t *testing.T) {
	detail := kickObserverArchiveDetail("kick", "mention")
	if detail != "kick source=mention" {
		t.Fatalf("detail = %q", detail)
	}
	if got := KickObserverDetailReason(detail); got != "kick" {
		t.Fatalf("reason = %q", got)
	}
	if got := KickObserverDetailSource(detail); got != "mention" {
		t.Fatalf("source = %q", got)
	}
	if got := kickObserverArchiveDetail("kick", ""); got != "kick" {
		t.Fatalf("empty source detail = %q", got)
	}
}

// legacyKickObsEvent is one (agent, event, detail) callback.
type legacyKickObsEvent [3]string

func recvLegacyKickObsEvent(t *testing.T, name string, ch <-chan legacyKickObsEvent) legacyKickObsEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(kickObsTestWait):
		t.Fatalf("%s observer never received an event", name)
	}
	return legacyKickObsEvent{}
}

// TestLegacyKickObservers_BothFireOnSameKick pins the v6 wiring after the
// convergence with v5's fan-out: the mention responder and the Linear agent
// both register with crux.AddLegacyKickObserver, and on the same kick each
// receives the same events, in emission order, with the archive detail still
// carrying the kick source. Events the single-slot observer never saw
// (queue, output signal) are filtered out, and the single slot no longer
// interferes with either.
func TestLegacyKickObservers_BothFireOnSameKick(t *testing.T) {
	m := &Manager{}
	mentionCh := make(chan legacyKickObsEvent, 8)
	linearCh := make(chan legacyKickObsEvent, 8)
	removeMention := crux.AddLegacyKickObserver(m, func(a, e, d string) { mentionCh <- legacyKickObsEvent{a, e, d} })
	defer removeMention()
	removeLinear := crux.AddLegacyKickObserver(m, func(a, e, d string) { linearCh <- legacyKickObsEvent{a, e, d} })
	defer removeLinear()

	// The single slot is independent: installing and clearing it must not
	// touch either fan-out observer (v6's old chaining had nil clear both).
	m.SetKickObserver(func(string, string, string) {})
	m.SetKickObserver(nil)

	const source = "mention:IC_1"
	m.emitKickEvent(crux.KickEvent{Agent: "scanner", Event: crux.KickEventQueued, KickID: "k1"})
	m.notifyKickObserver("scanner", KickObserverEventDelivered, source)
	m.emitKickEvent(crux.KickEvent{Agent: "scanner", Event: crux.KickEventOutputSignal, Signal: crux.OutputSignalFinding})
	m.notifyKickObserver("scanner", KickObserverEventArchived, kickObserverArchiveDetail("kick", source))
	m.notifyKickObserver("scanner", "kick-dropped", "mention:IC_2")

	want := []legacyKickObsEvent{
		{"scanner", KickObserverEventDelivered, source},
		{"scanner", KickObserverEventArchived, "kick source=" + source},
		{"scanner", "kick-dropped", "mention:IC_2"},
	}
	for name, ch := range map[string]chan legacyKickObsEvent{"mention": mentionCh, "linear": linearCh} {
		for i, w := range want {
			if got := recvLegacyKickObsEvent(t, name, ch); got != w {
				t.Fatalf("%s event %d = %v, want %v", name, i, got, w)
			}
		}
		if got := KickObserverDetailSource(want[1][2]); got != source {
			t.Fatalf("archive detail source = %q, want %q", got, source)
		}
		select {
		case extra := <-ch:
			t.Fatalf("%s observer received a non-legacy event %v", name, extra)
		case <-time.After(50 * time.Millisecond):
		}
	}

	removeLinear()
	m.notifyKickObserver("scanner", KickObserverEventDelivered, "governor")
	if got := recvLegacyKickObsEvent(t, "mention", mentionCh); got[2] != "governor" {
		t.Fatalf("mention event after removing linear = %v", got)
	}
	select {
	case ev := <-linearCh:
		t.Fatalf("removed linear observer received %v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}
