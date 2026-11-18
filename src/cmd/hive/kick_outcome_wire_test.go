package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/kickoutcome"
)

func TestKickOutcomeSenderAttributesAndFilesRefusals(t *testing.T) {
	s, _ := kickoutcome.Open("", nil)
	wf := func(agent, message string) bool { return message == "handoff" }
	send := kickOutcomeSender(s, func(agent, message string) error {
		switch message {
		case "paused":
			return errors.New("agent a cannot be kicked: it is paused")
		case "busy":
			return crux.ErrAgentBusy
		case "mystery":
			return errors.New("nobody documented this")
		}
		s.Delivered(agent, time.Now())
		return nil
	}, wf)
	_ = send("a", "handoff")
	_ = send("a", "plain")
	_ = send("a", "busy")
	_ = send("a", "mystery")
	if err := send("a", "paused"); err == nil {
		t.Fatal("the send error was swallowed")
	}
	rs := s.Records("a", 0)
	if len(rs) != 3 {
		t.Fatalf("want 2 kicks and 1 documented refusal, got %d: %+v", len(rs), rs)
	}
	if rs[2].TriggerKind != kickoutcome.TriggerWorkflowDispatch || rs[1].TriggerKind != kickoutcome.TriggerGovernor {
		t.Fatalf("triggers: %+v", rs)
	}
	if rs[0].RefusalCode != "paused" {
		t.Fatalf("refusal: %+v", rs[0])
	}
}

func TestKickOutcomeRelayActionsFileOnTheKick(t *testing.T) {
	s, _ := kickoutcome.Open("", nil)
	now := time.Now()
	s.Delivered("a", now)
	kickOutcomeAgentAction(s, github.AgentAction{Agent: "a", Kind: github.AgentActionPush, Branch: "fix/x", SHA: "abc123", At: now.Add(time.Minute)})
	s.SetClock(func() time.Time { return now.Add(2 * time.Minute) })
	if v, _ := s.LatestOf("a"); v.Kind != kickoutcome.KindPushedCommit {
		t.Fatalf("%+v", v)
	}
	kickOutcomeAgentAction(s, github.AgentAction{Agent: "a", Kind: "unknown", At: now})
}

func TestKickOutcomeDeliveredIgnoresOtherEvents(t *testing.T) {
	s, _ := kickoutcome.Open("", nil)
	kickOutcomeDelivered(s, crux.KickEvent{Agent: "a", Event: crux.KickEventQueued, At: time.Now()})
	if len(s.Records("a", 0)) != 0 {
		t.Fatal("a queued event opened a kick record")
	}
	kickOutcomeDelivered(s, crux.KickEvent{Agent: "a", Event: crux.KickEventDelivered, At: time.Now()})
	if len(s.Records("a", 0)) != 1 {
		t.Fatal("a delivered event did not")
	}
}

func TestKickOutcomeBackoffWithheldIsRefusedWithoutAgentText(t *testing.T) {
	s, _ := kickoutcome.Open("", nil)
	kickOutcomeBackoffWithheld(s, "scanner", "rate_limit", 90*time.Second, time.Now())
	latest, detail := kickOutcomeSource{s}.Recent("scanner", 10)
	b, err := json.Marshal(map[string]any{"latest": latest, "kicks": detail})
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	if !strings.Contains(out, `"refused"`) || !strings.Contains(out, "provider_backoff") {
		t.Fatalf("withheld kick not recorded as a provider_backoff refusal: %s", out)
	}
	// The reason is the hive's own text; the provider error line (agent
	// output) is never passed in, so nothing of it can be quoted.
	if strings.Contains(out, "quota exceeded") {
		t.Fatalf("agent text quoted: %s", out)
	}
	kickOutcomeBackoffWithheld(nil, "scanner", "x", 0, time.Now())
}
