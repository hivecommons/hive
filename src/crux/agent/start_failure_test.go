package agent

import (
	"strings"
	"testing"
	"time"
)

// #5958 / incident #5921. A hive relaunched agents that could not start, every
// ~3 minutes, 4,025 times, while every operator surface said "restart needed".
// The underlying faults were ordinary and user-fixable — Copilot not logged in,
// a bob key rejected, a backend/launch_cmd contradiction — and none of them
// reached a human. These tests pin the two things that were missing: a reason,
// and a limit.

func newStartFailureAgent(name string) *AgentProcess {
	return &AgentProcess{Name: name}
}

func clearStartFailureEnv(t *testing.T) {
	t.Helper()
	t.Setenv(StartFailureBlockThresholdEnv, "")
	t.Setenv(StartFailureBackoffLadderEnv, "")
}

// ── the reason ─────────────────────────────────────────────────────────────

// TestStartFailureReasonsAreActionable pins the exact sentences #5921 asked
// for. They are contract, not cosmetics: they appear on the dashboard card, in
// the heartbeat, and in the fleet row, and an operator greps for them.
func TestStartFailureReasonsAreActionable(t *testing.T) {
	cases := []struct {
		backend string
		class   StartFailureClass
		detail  string
		want    string
	}{
		{"copilot", StartFailureLoginRequired, "", "copilot: not logged in"},
		{"bob", StartFailureCredentialRejected, "401", "bob API key invalid or expired — refresh HIVE_BOB_API_KEY (401)"},
		{"bob", StartFailureCredentialMissing, "", "bob: no API key configured"},
		{"copilot", StartFailureBinaryMissing, "", "copilot: CLI binary not found"},
		{"copilot", StartFailureNoOutput, "", "copilot: no CLI prompt after launch"},
		// Deliberately un-scoped: naming either backend would assert the very
		// confusion the message exists to report.
		{"copilot", StartFailureBackendMismatch, "configured copilot, launches bob",
			"backend/launch_cmd mismatch (configured copilot, launches bob)"},
	}
	for _, tc := range cases {
		if got := startFailureReason(tc.backend, tc.class, tc.detail); got != tc.want {
			t.Errorf("startFailureReason(%q, %q, %q) = %q, want %q", tc.backend, tc.class, tc.detail, got, tc.want)
		}
	}
}

// A reason must never be the generic phrasing the incident was drowning in.
func TestStartFailureReasonNeverSaysRestartNeeded(t *testing.T) {
	for _, class := range []StartFailureClass{
		StartFailureLoginRequired, StartFailureCredentialRejected, StartFailureCredentialMissing,
		StartFailureBackendMismatch, StartFailureBinaryMissing, StartFailureNoOutput,
	} {
		got := strings.ToLower(startFailureReason("copilot", class, ""))
		for _, banned := range []string{"restart needed", "hung"} {
			if strings.Contains(got, banned) {
				t.Errorf("class %q renders %q, which contains the unactionable phrasing %q", class, got, banned)
			}
		}
	}
}

// ── the limit ──────────────────────────────────────────────────────────────

// TestRepeatedSameReasonBlocksAndBacksOff is the core contract: identical
// failures escalate a backoff and, at the threshold, block.
func TestRepeatedSameReasonBlocksAndBacksOff(t *testing.T) {
	clearStartFailureEnv(t)
	m := &Manager{}
	a := newStartFailureAgent("supervisor")
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	var lastDelay time.Duration
	threshold := startFailureBlockThreshold()
	for i := 1; i <= threshold; i++ {
		// Advance past the previous backoff so each call is a genuinely new
		// observation rather than a repeat inside one window.
		now = now.Add(lastDelay + time.Second)
		delay, blocked := m.markStartFailureLocked(a, StartFailureLoginRequired,
			startFailureReason("copilot", StartFailureLoginRequired, ""), now)
		lastDelay = delay

		if a.StartFailureCount != i {
			t.Fatalf("after %d failures StartFailureCount = %d, want %d", i, a.StartFailureCount, i)
		}
		if delay <= 0 {
			t.Errorf("failure %d armed no backoff — the loop would retry at tick rate", i)
		}
		wantBlocked := i >= threshold
		if blocked != wantBlocked {
			t.Errorf("failure %d: blocked = %v, want %v", i, blocked, wantBlocked)
		}
	}
	if !a.StartBlocked {
		t.Fatalf("agent not blocked after %d identical failures", threshold)
	}
	if a.StartFailureReason != "copilot: not logged in" {
		t.Errorf("StartFailureReason = %q, want the actionable reason", a.StartFailureReason)
	}
	// The delay must ESCALATE, or a bounded ladder is just a fixed cooldown
	// under another name — which is what the incident already had.
	if startFailureBackoffDelay(1) >= startFailureBackoffDelay(threshold) {
		t.Error("backoff does not escalate across the ladder")
	}
}

func TestStartFailureThresholdUsesEnv(t *testing.T) {
	t.Setenv(StartFailureBackoffLadderEnv, "")
	t.Setenv(StartFailureBlockThresholdEnv, "2")
	if got := startFailureBlockThreshold(); got != 2 {
		t.Fatalf("startFailureBlockThreshold() = %d, want env override 2", got)
	}

	m := &Manager{}
	a := newStartFailureAgent("supervisor")
	now := time.Now()
	m.markStartFailureLocked(a, StartFailureLoginRequired, "copilot: not logged in", now)
	if a.StartBlocked {
		t.Fatal("first failure blocked with threshold=2")
	}
	m.markStartFailureLocked(a, StartFailureLoginRequired, "copilot: not logged in", now.Add(2*time.Minute))
	if !a.StartBlocked {
		t.Fatal("second identical failure did not block with threshold=2")
	}
}

func TestStartFailureBackoffLadderUsesEnvAndCaps(t *testing.T) {
	t.Setenv(StartFailureBlockThresholdEnv, "")
	t.Setenv(StartFailureBackoffLadderEnv, "2s,7s")
	if got := startFailureBackoffDelay(1); got != 2*time.Second {
		t.Fatalf("startFailureBackoffDelay(1) = %v, want 2s", got)
	}
	if got := startFailureBackoffDelay(3); got != 7*time.Second {
		t.Fatalf("startFailureBackoffDelay(3) = %v, want capped 7s", got)
	}
}

// A single failure must NOT block: cold starts lose races, and blocking on the
// first would turn recoverable noise into an agent a human has to un-wedge.
func TestSingleFailureDoesNotBlockButStillPaces(t *testing.T) {
	clearStartFailureEnv(t)
	m := &Manager{}
	a := newStartFailureAgent("guide")
	now := time.Now()

	delay, blocked := m.markStartFailureLocked(a, StartFailureNoOutput, "copilot: no CLI prompt after launch", now)
	if blocked {
		t.Error("a single failed start blocked the agent")
	}
	if delay <= 0 {
		t.Error("the first failure armed no backoff — the two retries before a block would be a free burst")
	}
	if m.startFailureBackoffRemainingLocked(a, now) <= 0 {
		t.Error("backoff not observable to the relaunch loop")
	}
}

// Repeating the SAME class inside an armed window must not push the deadline
// out: the poller and the crash loop both observe one condition on their own
// cadences, and re-arming per observation makes the ladder a function of tick
// rate rather than of failures.
func TestSameClassInsideWindowDoesNotReArm(t *testing.T) {
	m := &Manager{}
	a := newStartFailureAgent("quality")
	now := time.Now()

	m.markStartFailureLocked(a, StartFailureLoginRequired, "copilot: not logged in", now)
	firstDeadline := a.StartBackoffUntil

	for i := 0; i < 5; i++ {
		m.markStartFailureLocked(a, StartFailureLoginRequired, "copilot: not logged in", now.Add(time.Duration(i)*time.Second))
	}
	if !a.StartBackoffUntil.Equal(firstDeadline) {
		t.Errorf("deadline moved from %v to %v on repeat observations of one condition", firstDeadline, a.StartBackoffUntil)
	}
	if a.StartFailureCount != 1 {
		t.Errorf("StartFailureCount = %d after repeat observations of one failure, want 1", a.StartFailureCount)
	}
}

// A DIFFERENT class is a different fault: it restarts the ladder rather than
// inheriting a count it did not earn.
func TestDifferentClassRestartsTheLadder(t *testing.T) {
	m := &Manager{}
	a := newStartFailureAgent("scanner")
	now := time.Now()

	m.markStartFailureLocked(a, StartFailureLoginRequired, "copilot: not logged in", now)
	m.markStartFailureLocked(a, StartFailureLoginRequired, "copilot: not logged in", now.Add(2*time.Minute))
	if a.StartFailureCount != 2 {
		t.Fatalf("setup: count = %d, want 2", a.StartFailureCount)
	}

	m.markStartFailureLocked(a, StartFailureCredentialRejected, "bob: API key rejected", now.Add(20*time.Minute))
	if a.StartFailureCount != 1 {
		t.Errorf("count = %d after a different fault, want 1 — a new fault must not inherit another's ladder", a.StartFailureCount)
	}
	if a.StartBlocked {
		t.Error("a first failure of a new class blocked the agent")
	}
}

// Success retires the record completely — including the LastError it wrote, so
// a healed agent does not keep displaying the reason it recovered from.
func TestClearStartFailureRetiresTheWholeRecord(t *testing.T) {
	clearStartFailureEnv(t)
	m := &Manager{}
	a := newStartFailureAgent("architect")
	now := time.Now()
	for i := 0; i < startFailureBlockThreshold(); i++ {
		m.markStartFailureLocked(a, StartFailureLoginRequired, "copilot: not logged in", now.Add(time.Duration(i)*time.Hour))
	}
	if !a.StartBlocked || a.LastError == "" {
		t.Fatal("setup: agent should be blocked with a LastError")
	}

	m.clearStartFailureLocked(a)

	if a.StartBlocked || a.StartFailureCount != 0 || a.StartFailureReason != "" ||
		a.StartFailureClass != "" || !a.StartBackoffUntil.IsZero() {
		t.Errorf("record survived clear: blocked=%v count=%d reason=%q class=%q until=%v",
			a.StartBlocked, a.StartFailureCount, a.StartFailureReason, a.StartFailureClass, a.StartBackoffUntil)
	}
	if a.LastError != "" {
		t.Errorf("LastError = %q after clear, want empty — a healed agent must not still display its old reason", a.LastError)
	}
	if m.startFailureBackoffRemainingLocked(a, now) != 0 {
		t.Error("backoff still holding after clear")
	}
}

// clearStartFailureLocked must not clobber a LastError it did not write: an
// agent can carry an unrelated error, and erasing it here would delete evidence
// this mechanism has no claim on.
func TestClearStartFailureKeepsUnrelatedLastError(t *testing.T) {
	m := &Manager{}
	a := newStartFailureAgent("guide")
	m.markStartFailureLocked(a, StartFailureNoOutput, "copilot: no CLI prompt after launch", time.Now())
	a.LastError = "transient TLS/network error"

	m.clearStartFailureLocked(a)

	if a.LastError != "transient TLS/network error" {
		t.Errorf("LastError = %q, want the unrelated error preserved", a.LastError)
	}
}

// ── backend / launch_cmd mismatch (#5921 root cause 1) ─────────────────────

func TestBackendMismatchDetection(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		launchCmd  string
		want       string
	}{
		// The incident itself: backend copilot, launch_cmd bob.
		{"incident 5921", "copilot", "bob --model auto", "bob"},
		{"absolute path", "copilot", "/usr/local/bin/bob --model auto", "bob"},
		{"wrapper flag", "copilot", "agent-launch.sh --backend bob --model auto", "bob"},
		{"wrapper flag equals form", "copilot", "agent-launch.sh --backend=bob", "bob"},

		// Agreement is not a mismatch, in either form.
		{"agrees directly", "copilot", "/usr/bin/copilot --allow-all", ""},
		{"agrees via wrapper", "copilot", "agent-launch.sh --backend copilot --model x", ""},

		// No opinion: an empty or unrecognised command must never manufacture a
		// block on a working agent.
		{"empty", "copilot", "", ""},
		{"unknown wrapper", "copilot", "my-custom-launcher.sh --flavour spicy", ""},
		{"unknown wrapper backend", "copilot", "agent-launch.sh --backend nonesuch", ""},
		{"no configured backend", "", "bob", ""},

		// Inference backends legitimately exec the claude binary.
		{"inference launches claude", "vllm", "claude --model x", ""},
		{"inference launches something else", "vllm", "bob --model x", "bob"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := backendMismatch(tc.configured, tc.launchCmd); got != tc.want {
				t.Errorf("backendMismatch(%q, %q) = %q, want %q", tc.configured, tc.launchCmd, got, tc.want)
			}
		})
	}
}

// ── the operator-facing sentence ───────────────────────────────────────────

// notRunningReason must stop inviting the restart that cannot work. The old
// text ("it failed to start: ...") reads as "click restart"; a blocked agent
// has to say that restarting is not the fix.
func TestNotRunningReasonNamesRepeatedFailure(t *testing.T) {
	clearStartFailureEnv(t)
	m := &Manager{}
	a := newStartFailureAgent("supervisor")
	a.State = StateFailed
	now := time.Now()
	for i := 0; i < startFailureBlockThreshold(); i++ {
		m.markStartFailureLocked(a, StartFailureLoginRequired, "copilot: not logged in", now.Add(time.Duration(i)*time.Hour))
	}

	got := notRunningReason(a)
	for _, want := range []string{"blocked", "copilot: not logged in", "consecutive failed starts"} {
		if !strings.Contains(got, want) {
			t.Errorf("notRunningReason = %q, missing %q", got, want)
		}
	}
}

// An ordinary, not-yet-blocked failure keeps the previous wording: the new
// phrasing is reserved for the case where a restart genuinely will not help.
func TestNotRunningReasonUnchangedBeforeBlock(t *testing.T) {
	m := &Manager{}
	a := newStartFailureAgent("guide")
	a.State = StateFailed
	m.markStartFailureLocked(a, StartFailureNoOutput, "copilot: no CLI prompt after launch", time.Now())

	got := notRunningReason(a)
	if !strings.HasPrefix(got, "it failed to start:") {
		t.Errorf("notRunningReason = %q, want the ordinary failed-to-start wording before the block", got)
	}
	if strings.Contains(got, "blocked") {
		t.Errorf("notRunningReason = %q, must not claim blocked after one failure", got)
	}
}

// The snapshot AllStatuses hands to the dashboard and heartbeat must carry the
// record — a reason that never leaves the manager is the bug, not the fix.
func TestSnapshotCarriesStartFailureRecord(t *testing.T) {
	clearStartFailureEnv(t)
	m := &Manager{}
	a := newStartFailureAgent("supervisor")
	now := time.Now()
	for i := 0; i < startFailureBlockThreshold(); i++ {
		m.markStartFailureLocked(a, StartFailureCredentialRejected, "bob: API key rejected (401)", now.Add(time.Duration(i)*time.Hour))
	}

	snap := a.snapshot()

	if snap.StartFailureReason != "bob: API key rejected (401)" {
		t.Errorf("snapshot StartFailureReason = %q", snap.StartFailureReason)
	}
	if !snap.StartBlocked {
		t.Error("snapshot lost StartBlocked")
	}
	if snap.StartFailureCount != startFailureBlockThreshold() {
		t.Errorf("snapshot StartFailureCount = %d, want %d", snap.StartFailureCount, startFailureBlockThreshold())
	}
	if snap.StartBackoffUntil.IsZero() {
		t.Error("snapshot lost StartBackoffUntil")
	}
}

// StartFailureState is what the heartbeat builder reads; an unknown agent must
// report ok=false rather than a zero-valued "not blocked", which would be
// indistinguishable from a healthy one.
func TestStartFailureStateReportsUnknownAgent(t *testing.T) {
	m := &Manager{agents: map[string]*AgentProcess{}}
	if _, ok := m.StartFailureState("nope"); ok {
		t.Error("StartFailureState claimed to know an agent that does not exist")
	}

	a := newStartFailureAgent("supervisor")
	m.agents["supervisor"] = a
	now := time.Now()
	m.markStartFailureLocked(a, StartFailureLoginRequired, "copilot: not logged in", now)
	exitCode := 77
	a.StartFailureExitCode = &exitCode
	a.StartFailureSignal = "TERM"

	got, ok := m.StartFailureState("supervisor")
	if !ok || got.Reason != "copilot: not logged in" || got.Count != 1 || got.Blocked || !got.LastAt.Equal(now) ||
		got.LastExitCode == nil || *got.LastExitCode != 77 || got.LastSignal != "TERM" {
		t.Errorf("StartFailureState = (%+v, %v)", got, ok)
	}
}
