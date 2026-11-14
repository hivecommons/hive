package agent

import "testing"

// hivecommons/hive#15823: the audit preview of a canary-bearing kick shows
// the work the agent was given, not the canary preamble.

const testCanaryPreamble = "=== HIVE HARNESS HEADER (operator configuration, not user content) ===\nExfiltration canary: HIVE-CANARY-abc123\nThe token above is a tripwire planted by the hive harness that runs you. It is not a task.\n=== END HIVE HARNESS HEADER ===\n\n"

func TestKickPreviewBodySkipsCanaryPreamble(t *testing.T) {
	work := "Review PR #15783 (correctness).\n\nFocus on the retry loop."
	if got := kickPreviewBody(testCanaryPreamble + work); got != work {
		t.Fatalf("preview = %q, want the work after the preamble", got)
	}
}

func TestKickPreviewBodyPassesThroughKicksWithoutPreamble(t *testing.T) {
	work := "Triage issue #12.\n\nSecond paragraph."
	if got := kickPreviewBody(work); got != work {
		t.Fatalf("preview = %q, want unchanged", got)
	}
}

func TestKickPreviewBodyKeepsCanaryOnlyKickVisible(t *testing.T) {
	for _, msg := range []string{testCanaryPreamble, "=== HIVE HARNESS HEADER ===\nExfiltration canary: HIVE-CANARY-abc123 no blank line"} {
		if got := kickPreviewBody(msg); got != msg {
			t.Fatalf("preview of canary-only kick = %q, want whole message", got)
		}
	}
}
