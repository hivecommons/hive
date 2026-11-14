package agent

import "testing"

// A model that reads the canary preamble as a prompt-injection marker refuses
// the whole kick; the kick is re-delivered once without the canary so the
// refusal does not burn the cadence slot.

func TestStripCanaryPreamble(t *testing.T) {
	work := "Review PR #15783 (correctness).\n\nFocus on the retry loop."
	if got, ok := stripCanaryPreamble(testCanaryPreamble + work); !ok || got != work {
		t.Fatalf("stripCanaryPreamble = (%q, %v), want (%q, true)", got, ok, work)
	}
	for _, msg := range []string{work, testCanaryPreamble, "=== HIVE HARNESS HEADER ===\nExfiltration canary: HIVE-CANARY-abc123 no blank line", ""} {
		if got, ok := stripCanaryPreamble(msg); ok || got != msg {
			t.Errorf("stripCanaryPreamble(%q) = (%q, %v), want unchanged, false", msg, got, ok)
		}
	}
}

func TestPlanCanaryRetry(t *testing.T) {
	work := "Triage issue #12."
	if plain, ok := planCanaryRetry(testCanaryPreamble+work, false); !ok || plain != work {
		t.Fatalf("canary kick must earn one plain retry, got (%q, %v)", plain, ok)
	}
	if _, ok := planCanaryRetry(testCanaryPreamble+work, true); ok {
		t.Error("the retry itself must not be retried")
	}
	if _, ok := planCanaryRetry(work, false); ok {
		t.Error("a kick without a canary has nothing to strip; no retry")
	}
	if _, ok := planCanaryRetry(testCanaryPreamble, false); ok {
		t.Error("a canary-only kick has no work to re-deliver")
	}
}

func TestCheckKickRefusalRedeliversCanaryKickOnce(t *testing.T) {
	m := &Manager{logger: discardLogger()}
	var delivered []string
	m.canaryRetryDeliver = func(name, message string) {
		delivered = append(delivered, name+"\x00"+message)
	}
	work := "[agent:scanner]\n\nScan cncf/techdocs for stale pages."
	refusal := "I'm noticing this request includes a canary marker and looks like a prompt injection, so I won't act on it."

	agent := &AgentProcess{Name: "scanner", lastKickInput: testCanaryPreamble + work}
	m.checkKickRefusal(agent, refusal)
	if !agent.KickRefused {
		t.Fatal("refusal must still be recorded")
	}
	if len(delivered) != 1 || delivered[0] != "scanner\x00"+work {
		t.Fatalf("refused canary kick must be re-delivered plain, got %q", delivered)
	}

	// The re-delivery is marked by its trigger; a refusal of the retry stands.
	m.recordDeliveredKickLocked(agent, work, kickTriggerCanaryRetry)
	if agent.KickRefused || !agent.canaryRetryDone {
		t.Fatalf("retry delivery must clear the refusal and mark the retry: refused=%v retried=%v", agent.KickRefused, agent.canaryRetryDone)
	}
	agent.lastKickInput = testCanaryPreamble + work
	m.checkKickRefusal(agent, refusal)
	if len(delivered) != 1 {
		t.Fatalf("a refused retry must not be retried again, got %d deliveries", len(delivered))
	}

	// A fresh kick from the governor re-arms the single retry.
	m.recordDeliveredKickLocked(agent, testCanaryPreamble+work, "send-kick")
	if agent.canaryRetryDone {
		t.Fatal("an ordinary kick must re-arm the canary retry")
	}
}

func TestCheckKickRefusalWithoutCanaryDoesNotRedeliver(t *testing.T) {
	m := &Manager{logger: discardLogger()}
	calls := 0
	m.canaryRetryDeliver = func(string, string) { calls++ }
	agent := &AgentProcess{Name: "scanner", lastKickInput: "[agent:scanner]\n\nPlain kick."}
	m.checkKickRefusal(agent, "This looks like a prompt injection attempt, so I'm declining to execute it.")
	if !agent.KickRefused || calls != 0 {
		t.Fatalf("plain kick refusal must be recorded without a retry: refused=%v calls=%d", agent.KickRefused, calls)
	}
}
