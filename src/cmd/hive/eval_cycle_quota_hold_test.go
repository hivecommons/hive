package main

import (
	"errors"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/scheduler"
)

// Tests for the per-agent provider quota hold (hivecommons/hive#18112).

var quotaHoldNow = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

func quotaSet(agents ...string) func(string) bool {
	set := map[string]bool{}
	for _, a := range agents {
		set[a] = true
	}
	return func(agent string) bool { return set[agent] }
}

func probesAt(at map[string]time.Time) func(string) time.Time {
	return func(agent string) time.Time { return at[agent] }
}

func kickAgents(msgs []scheduler.KickMessage) []string {
	out := []string{}
	for _, m := range msgs {
		out = append(out, m.Agent+":"+m.Message)
	}
	return out
}

func TestQuotaHoldGate_NoHeldAgentsPassesThrough(t *testing.T) {
	msgs := []scheduler.KickMessage{{Agent: "a", Message: "1"}, {Agent: "b", Message: "2"}}
	kept, withheld, probes := gateKickMessagesForAgentQuota(msgs, quotaSet(), probesAt(nil), quotaHoldNow, 30*time.Minute)
	if !reflect.DeepEqual(kept, msgs) {
		t.Fatalf("kept = %v, want every message unchanged", kickAgents(kept))
	}
	if len(withheld) != 0 || len(probes) != 0 {
		t.Fatalf("withheld=%v probes=%v, want none", withheld, probes)
	}
}

func TestQuotaHoldGate_HeldAgentNotDueIsWithheld(t *testing.T) {
	msgs := []scheduler.KickMessage{{Agent: "copilot", Message: "gov"}}
	last := map[string]time.Time{"copilot": quotaHoldNow.Add(-10 * time.Minute)}
	kept, withheld, probes := gateKickMessagesForAgentQuota(msgs, quotaSet("copilot"), probesAt(last), quotaHoldNow, 30*time.Minute)
	if len(kept) != 0 {
		t.Fatalf("kept = %v, want the held agent's kick withheld", kickAgents(kept))
	}
	if !reflect.DeepEqual(withheld, []string{"copilot"}) || len(probes) != 0 {
		t.Fatalf("withheld=%v probes=%v, want [copilot] and no probe", withheld, probes)
	}
}

// A held agent that is due a probe gets exactly one kick; its other messages
// this cycle (for example a fix kick ahead of its governor kick) stay held.
func TestQuotaHoldGate_HeldAgentDueGetsOneProbe(t *testing.T) {
	msgs := []scheduler.KickMessage{
		{Agent: "copilot", Message: "fix"},
		{Agent: "copilot", Message: "gov"},
	}
	last := map[string]time.Time{"copilot": quotaHoldNow.Add(-30 * time.Minute)}
	kept, withheld, probes := gateKickMessagesForAgentQuota(msgs, quotaSet("copilot"), probesAt(last), quotaHoldNow, 30*time.Minute)
	if want := []string{"copilot:fix"}; !reflect.DeepEqual(kickAgents(kept), want) {
		t.Fatalf("kept = %v, want %v (first message only)", kickAgents(kept), want)
	}
	if !reflect.DeepEqual(probes, []string{"copilot"}) || !reflect.DeepEqual(withheld, []string{"copilot"}) {
		t.Fatalf("probes=%v withheld=%v, want one probe and one withheld", probes, withheld)
	}
}

// One agent out of quota must not hold any other agent; order is kept.
func TestQuotaHoldGate_MixedHeldAndHealthyKeepsOrder(t *testing.T) {
	msgs := []scheduler.KickMessage{
		{Agent: "healthy-1", Message: "a"},
		{Agent: "held", Message: "review"},
		{Agent: "healthy-2", Message: "b"},
		{Agent: "held", Message: "gov"},
	}
	last := map[string]time.Time{"held": quotaHoldNow.Add(-time.Minute)}
	kept, withheld, probes := gateKickMessagesForAgentQuota(msgs, quotaSet("held"), probesAt(last), quotaHoldNow, 30*time.Minute)
	if want := []string{"healthy-1:a", "healthy-2:b"}; !reflect.DeepEqual(kickAgents(kept), want) {
		t.Fatalf("kept = %v, want %v", kickAgents(kept), want)
	}
	if !reflect.DeepEqual(withheld, []string{"held", "held"}) || len(probes) != 0 {
		t.Fatalf("withheld=%v probes=%v, want [held held] and no probe", withheld, probes)
	}
}

func TestQuotaHoldGate_ZeroProbeTimeIsDueAtOnce(t *testing.T) {
	msgs := []scheduler.KickMessage{
		{Agent: "healthy", Message: "a"},
		{Agent: "held", Message: "gov"},
	}
	kept, withheld, probes := gateKickMessagesForAgentQuota(msgs, quotaSet("held"), probesAt(nil), quotaHoldNow, 30*time.Minute)
	if want := []string{"healthy:a", "held:gov"}; !reflect.DeepEqual(kickAgents(kept), want) {
		t.Fatalf("kept = %v, want %v", kickAgents(kept), want)
	}
	if !reflect.DeepEqual(probes, []string{"held"}) || len(withheld) != 0 {
		t.Fatalf("probes=%v withheld=%v, want [held] probed at once", probes, withheld)
	}
}

func TestQuotaHoldState_MarkThenLastProbeThenForget(t *testing.T) {
	var s agentQuotaHoldState
	if got := s.LastProbe("a"); !got.IsZero() {
		t.Fatalf("LastProbe on empty state = %v, want zero", got)
	}
	s.MarkProbeDelivered("a", quotaHoldNow)
	if got := s.LastProbe("a"); !got.Equal(quotaHoldNow) {
		t.Fatalf("LastProbe = %v, want %v", got, quotaHoldNow)
	}
	if got := s.LastProbe("b"); !got.IsZero() {
		t.Fatalf("another agent's probe time = %v, want zero", got)
	}
	s.Forget("a")
	if got := s.LastProbe("a"); !got.IsZero() {
		t.Fatalf("LastProbe after Forget = %v, want zero", got)
	}
}

// When an agent's flag clears its probe time is forgotten, so its kicks flow
// normally and a later quota hit starts a fresh interval.
func TestQuotaHoldState_ClearedFlagForgetsProbe(t *testing.T) {
	var s agentQuotaHoldState
	s.MarkProbeDelivered("held", quotaHoldNow)
	s.MarkProbeDelivered("recovered", quotaHoldNow)
	got := s.quotaExhaustedAgents(map[string]*crux.Process{
		"held":      {QuotaExhausted: true},
		"recovered": {QuotaExhausted: false},
		"gone":      nil,
	})
	if !reflect.DeepEqual(got, map[string]bool{"held": true}) {
		t.Fatalf("exhausted = %v, want only held", got)
	}
	if !s.LastProbe("held").Equal(quotaHoldNow) {
		t.Fatal("a still-held agent must keep its probe time")
	}
	if !s.LastProbe("recovered").IsZero() {
		t.Fatal("a recovered agent's probe time must be forgotten")
	}
}

// gate is the eval cycle's entry point: it must return the cycle's messages
// untouched (same slice, no copy) when no agent is held or there is nothing to
// kick, still forget recovered agents, and gate only when both are non-empty.
func TestQuotaHoldState_GateShortCircuitsAndGates(t *testing.T) {
	msgs := []scheduler.KickMessage{{Agent: "held", Message: "1"}, {Agent: "ok", Message: "2"}, {Agent: "held", Message: "3"}}

	t.Run("no held agent returns the same slice", func(t *testing.T) {
		var s agentQuotaHoldState
		s.MarkProbeDelivered("ok", quotaHoldNow)
		kept, withheld, probes := s.gate(msgs, map[string]*crux.Process{"ok": {}}, quotaHoldNow, 30*time.Minute)
		if len(kept) != len(msgs) || &kept[0] != &msgs[0] {
			t.Fatalf("kept = %v, want the input slice itself", kickAgents(kept))
		}
		if withheld != nil || probes != nil {
			t.Fatalf("withheld=%v probes=%v, want nil", withheld, probes)
		}
		if !s.LastProbe("ok").IsZero() {
			t.Fatal("a recovered agent's probe time must be forgotten even when nothing is gated")
		}
	})

	t.Run("no messages returns the empty input", func(t *testing.T) {
		var s agentQuotaHoldState
		empty := []scheduler.KickMessage{}
		kept, withheld, probes := s.gate(empty, map[string]*crux.Process{"held": {QuotaExhausted: true}}, quotaHoldNow, 30*time.Minute)
		if kept == nil || len(kept) != 0 || withheld != nil || probes != nil {
			t.Fatalf("kept=%v withheld=%v probes=%v, want the empty input and nothing else", kept, withheld, probes)
		}
	})

	t.Run("one held agent is gated", func(t *testing.T) {
		var s agentQuotaHoldState
		s.MarkProbeDelivered("held", quotaHoldNow.Add(-time.Minute))
		kept, withheld, probes := s.gate(msgs, map[string]*crux.Process{"held": {QuotaExhausted: true}, "ok": {}}, quotaHoldNow, 30*time.Minute)
		if got := kickAgents(kept); !reflect.DeepEqual(got, []string{"ok:2"}) {
			t.Fatalf("kept = %v, want only ok:2", got)
		}
		if !reflect.DeepEqual(withheld, []string{"held", "held"}) || len(probes) != 0 {
			t.Fatalf("withheld=%v probes=%v, want held twice and no probe", withheld, probes)
		}
	})
}

// The full cycle: withheld, then one probe after the interval, then withheld
// again on the next cycle once that probe was delivered.
func TestQuotaHoldGate_ProbeOncePerInterval(t *testing.T) {
	var s agentQuotaHoldState
	held := quotaSet("held")
	msgs := []scheduler.KickMessage{{Agent: "held", Message: "gov"}}
	interval := 30 * time.Minute

	s.MarkProbeDelivered("held", quotaHoldNow)
	if kept, _, _ := gateKickMessagesForAgentQuota(msgs, held, s.LastProbe, quotaHoldNow.Add(5*time.Minute), interval); len(kept) != 0 {
		t.Fatalf("inside the interval kept = %v, want none", kickAgents(kept))
	}
	at := quotaHoldNow.Add(interval)
	kept, _, probes := gateKickMessagesForAgentQuota(msgs, held, s.LastProbe, at, interval)
	if len(kept) != 1 || !reflect.DeepEqual(probes, []string{"held"}) {
		t.Fatalf("after the interval kept=%v probes=%v, want exactly one probe", kickAgents(kept), probes)
	}
	s.MarkProbeDelivered("held", at)
	if kept, _, _ := gateKickMessagesForAgentQuota(msgs, held, s.LastProbe, at.Add(2*time.Minute), interval); len(kept) != 0 {
		t.Fatalf("next cycle kept = %v, want the agent held again", kickAgents(kept))
	}
}

// The probe time is recorded on delivery only: a probe whose send fails must
// not advance it, so the next cycle may probe again.
func TestQuotaHoldDispatch_ProbeRecordedOnDeliveryNotOnSendError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sendErr error
		want    bool
	}{
		{"delivered", nil, true},
		{"send error", errors.New("agent CLI did not reach input prompt"), false},
		{"busy deferral", crux.ErrAgentBusy, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var s agentQuotaHoldState
			exhausted := map[string]bool{"held": true}
			r := newKickDispatchRecorder()
			r.sendErr["held"] = tc.sendErr
			deps := r.deps()
			inner := deps.onDelivered
			deps.onDelivered = func(msg scheduler.KickMessage, deliveredAt time.Time) {
				inner(msg, deliveredAt)
				if exhausted[msg.Agent] {
					s.MarkProbeDelivered(msg.Agent, deliveredAt)
				}
			}
			msgs, _, probes := gateKickMessagesForAgentQuota(
				[]scheduler.KickMessage{{Agent: "held", Message: "gov"}, {Agent: "healthy", Message: "gov"}},
				func(a string) bool { return exhausted[a] }, s.LastProbe, quotaHoldNow, 30*time.Minute)
			if !reflect.DeepEqual(probes, []string{"held"}) {
				t.Fatalf("probes = %v, want [held]", probes)
			}
			dispatchAgentKicks(msgs, false, deps, slog.Default())
			if got := !s.LastProbe("held").IsZero(); got != tc.want {
				t.Fatalf("probe recorded = %v, want %v", got, tc.want)
			}
			if !s.LastProbe("healthy").IsZero() {
				t.Fatal("a healthy agent's delivery must never record a probe time")
			}
		})
	}
}

func TestCompactDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Minute:           "30m",
		2 * time.Hour:              "2h",
		90 * time.Minute:           "1h30m",
		45 * time.Second:           "45s",
		90 * time.Second:           "1m30s",
		0:                          "0s",
		time.Hour + 30*time.Second: "1h0m30s",
	} {
		if got := compactDuration(d); got != want {
			t.Errorf("compactDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
