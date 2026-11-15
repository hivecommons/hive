package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
)

func TestParseOutputSignal(t *testing.T) {
	cases := []struct {
		line, signal, text string
		ok                 bool
	}{
		{"[FINDING] vLLM 0.9 adds prefix caching", crux.OutputSignalFinding, "vLLM 0.9 adds prefix caching", true},
		{"⏺ [COMPLETE] three sources agree", crux.OutputSignalComplete, "three sources agree", true},
		{"   [STATUS] reading the docs", crux.OutputSignalStatus, "reading the docs", true},
		{"● [ERROR] site unreachable", crux.OutputSignalError, "site unreachable", true},
		{"[FINDING]", crux.OutputSignalFinding, "", true},
		{"emit lines that start with [FINDING] for each result", "", "", false},
		{"[HEARTBEAT] alive", "", "", false},
		{"nothing here", "", "", false},
	}
	for _, c := range cases {
		signal, text, _, ok := parseOutputSignal(c.line)
		if ok != c.ok || signal != c.signal || text != c.text {
			t.Errorf("parseOutputSignal(%q) = (%q, %q, %v), want (%q, %q, %v)", c.line, signal, text, ok, c.signal, c.text, c.ok)
		}
	}
	_, text, whole, _ := parseOutputSignal("[FINDING] " + strings.Repeat("x", 2*maxOutputSignalTextLen))
	if n := len([]rune(text)); n > maxOutputSignalTextLen {
		t.Errorf("text is %d runes, want at most %d", n, maxOutputSignalTextLen)
	}
	if n := len([]rune(whole)); n > maxOutputSignalTextLen {
		t.Errorf("whole line is %d runes, want at most %d", n, maxOutputSignalTextLen)
	}
}

func TestLogOutputSignals_DeliversToObserversWithKickID(t *testing.T) {
	m := newKickQueueManager(t, func(string) bool { return true }, func(string, string) error { return nil })
	events := make(chan crux.KickEvent, 16)
	defer m.AddKickObserver(func(ev crux.KickEvent) {
		if ev.Event == crux.KickEventOutputSignal {
			events <- ev
		}
	})()

	// Outside any queued kick: delivered with an empty kick id.
	m.logOutputSignals(kqTestAgent, "[STATUS] warming up")
	ev := recvKickEvent(t, events)
	if ev.Signal != crux.OutputSignalStatus || ev.KickID != "" || ev.Agent != kqTestAgent || ev.Text != "warming up" || ev.Detail != "[STATUS] warming up" {
		t.Fatalf("status event = %+v", ev)
	}

	id, _ := m.EnqueueKick(kqTestAgent, "meet-1.r1", "research")
	waitKickPhase(t, m, id, crux.KickPhaseDelivered)
	m.logOutputSignals(kqTestAgent, "⏺ [FINDING] answer one")
	m.logOutputSignals(kqTestAgent, "⏺ [FINDING] answer one") // pane redraw
	m.logOutputSignals(kqTestAgent, "git push origin main")   // logged only
	m.logOutputSignals(kqTestAgent, "[COMPLETE] done")
	first, second := recvKickEvent(t, events), recvKickEvent(t, events)
	if first.Signal != crux.OutputSignalFinding || first.KickID != id || first.Text != "answer one" {
		t.Fatalf("finding event = %+v", first)
	}
	if second.Signal != crux.OutputSignalComplete || second.KickID != id {
		t.Fatalf("second event = %+v, want the complete signal (the redraw must be deduplicated)", second)
	}
	select {
	case extra := <-events:
		t.Fatalf("unexpected extra event %+v", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestOutputSignalDedupe_ResetsPerKickAndBound(t *testing.T) {
	var d outputSignalDedupe
	if !d.firstSighting("a", "k1", "[FINDING] x") || d.firstSighting("a", "k1", "[FINDING] x") {
		t.Fatal("same line within one kick must be delivered once")
	}
	if !d.firstSighting("a", "k2", "[FINDING] x") {
		t.Fatal("a new kick must reset the dedupe set")
	}
	if !d.firstSighting("b", "k2", "[FINDING] x") {
		t.Fatal("dedupe must be per agent")
	}
	for i := 0; i < maxOutputSignalDedupe; i++ {
		d.firstSighting("c", "k", strings.Repeat("y", i+1))
	}
	if !d.firstSighting("c", "k", "y") {
		t.Fatal("a full dedupe set must reset rather than grow")
	}
}
