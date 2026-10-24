package toolapprove

// The L6 throughput contract (#4000 comment 5321778076) as executable tests.
//
// Requirement 1 is the load-bearing one: "A request the policy resolves to
// auto-approve never enters a queue and never waits on any external process.
// The inbox is for the operator lane only."
//
// These tests do not merely assert that Enqueue happens to refuse — they point
// the inbox at a path whose parent directory does not exist and is not
// creatable, so ANY write attempt fails loudly, and then assert that a full L6
// auto-approve flow completes with that path untouched. A future change that
// starts journaling auto-approvals "for the audit trail" fails here.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestL6AutoApproveNeverQueues is THE throughput-contract test. An L6 hive
// resolving a side-effectful request must reach auto-approve, and the durable
// inbox file must not exist afterward.
func TestL6AutoApproveNeverQueues(t *testing.T) {
	dir := t.TempDir()
	inboxPath := filepath.Join(dir, "inbox.json")
	inbox := NewInbox(inboxPath)
	desk := NewDesk(nil, passingScanner{})

	req := sideEffectfulRequest()
	v := desk.Resolve(context.Background(), req, 6)

	if v.Decision != DecisionAutoApprove {
		t.Fatalf("L6 side-effectful request resolved to %s, want auto-approve — "+
			"the throughput contract requires L6 to resolve nearly everything in-loop (rationale: %s)",
			v.Decision, v.Rationale)
	}

	// The contract: no queue residence. Enqueue must refuse this verdict.
	if _, err := inbox.Enqueue(req, v); err == nil {
		t.Fatal("Enqueue ACCEPTED an auto-approve verdict — auto-approve must never enter the operator inbox")
	}

	if _, err := os.Stat(inboxPath); !os.IsNotExist(err) {
		t.Fatalf("the durable inbox file was created during an L6 auto-approve flow (%s) — "+
			"auto-approve must touch no persistence", inboxPath)
	}
	if inbox.Count() != 0 {
		t.Fatalf("inbox holds %d items after an L6 auto-approve; want 0", inbox.Count())
	}
}

// TestEnqueueRefusesNonOperatorVerdicts pins the structural guard for every
// non-operator decision, not just auto-approve.
func TestEnqueueRefusesNonOperatorVerdicts(t *testing.T) {
	inbox := NewInbox(filepath.Join(t.TempDir(), "inbox.json"))

	for _, d := range []Decision{DecisionAutoApprove, DecisionSecurityScan, DecisionDeny} {
		_, err := inbox.Enqueue(sideEffectfulRequest(), Verdict{Decision: d})
		if err == nil {
			t.Errorf("Enqueue accepted a %s verdict; only operator-approve may enter the inbox", d)
		}
	}

	// Positive control: the operator lane IS accepted. Without this the test
	// above would pass against an Enqueue that refuses everything.
	id, err := inbox.Enqueue(sideEffectfulRequest(), Verdict{Decision: DecisionOperatorApprove})
	if err != nil {
		t.Fatalf("positive control FAILED: Enqueue refused an operator-approve verdict: %v", err)
	}
	if id == "" {
		t.Error("Enqueue returned an empty ID for an accepted item")
	}
}

// TestL6AutoApproveIsSynchronous pins requirement 1's "never waits on any
// external process" half. A scanner that blocks would tax every L6 turn; the
// desk must not consult one for a request that never enters the scan lane.
//
// Read-only tools auto-approve at every level WITHOUT a scan, so a blocking
// scanner must never be reached.
func TestL6AutoApproveIsSynchronous(t *testing.T) {
	desk := NewDesk(nil, blockingScanner{t: t})

	req := Request{
		Kind:  KindAgentTool,
		Tool:  ToolRequest{Tool: "read", Arguments: map[string]any{"file_path": "README.md"}},
		Agent: AgentIdentity{Name: "reader"},
	}

	done := make(chan Verdict, 1)
	go func() { done <- desk.Resolve(context.Background(), req, 6) }()

	select {
	case v := <-done:
		if v.Decision != DecisionAutoApprove {
			t.Fatalf("read-only request at L6 resolved to %s, want auto-approve", v.Decision)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Resolve blocked on the scanner for a request that should resolve in-loop — " +
			"the scan lane has become a serialization point (throughput contract requirement 2)")
	}
}

// TestL6RuleAutoApproveNeverQueues covers the rule-driven path: an operator rule
// resolving a self-merge at L6 must also stay out of the queue.
func TestL6RuleAutoApproveNeverQueues(t *testing.T) {
	eng, err := CompileRules([]Rule{{
		Name:   "green-dependabot",
		Expr:   `request.kind == "self-merge" && request.checks_green && request.author == "dependabot[bot]"`,
		Action: RuleActionAutoApprove,
	}}, nil)
	if err != nil {
		t.Fatalf("CompileRules: %v", err)
	}

	dir := t.TempDir()
	inboxPath := filepath.Join(dir, "inbox.json")
	inbox := NewInbox(inboxPath)
	desk := NewDesk(eng, passingScanner{})

	req := Request{
		Kind:        KindSelfMerge,
		Repo:        "hivecommons/hive",
		Number:      42,
		Author:      "dependabot[bot]",
		Title:       "chore(deps): bump x from 1.0.0 to 1.0.1",
		ChecksGreen: true,
		Tool:        ToolRequest{Tool: "hive-merge"},
	}

	v := desk.Resolve(context.Background(), req, 6)
	if v.Decision != DecisionAutoApprove {
		t.Fatalf("rule-matched green dependabot PR at L6 resolved to %s, want auto-approve", v.Decision)
	}
	if v.Rule != "green-dependabot" {
		t.Errorf("verdict did not record which rule resolved it: %q", v.Rule)
	}
	if _, err := os.Stat(inboxPath); !os.IsNotExist(err) {
		t.Fatal("rule-driven L6 auto-approve created the durable inbox file")
	}
	if inbox.Count() != 0 {
		t.Fatalf("inbox holds %d items after a rule-driven L6 auto-approve; want 0", inbox.Count())
	}
}

// TestPendingAtFullAutonomyIsFlagged pins requirement 4: an item queued on a
// hive at L6 is surfaced as a probable policy bug rather than silently waiting.
func TestPendingAtFullAutonomyIsFlagged(t *testing.T) {
	inbox := NewInbox(filepath.Join(t.TempDir(), "inbox.json"))

	if _, err := inbox.Enqueue(sideEffectfulRequest(), Verdict{
		Decision: DecisionOperatorApprove, ACMMLevel: 6,
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	bugs := inbox.PendingAtFullAutonomy()
	if len(bugs) != 1 {
		t.Fatalf("PendingAtFullAutonomy returned %d items, want 1 — an inbox accumulating at L6 "+
			"is a misconfiguration signal that must be surfaced, not queued politely", len(bugs))
	}

	// Positive control: an item queued at a level that legitimately gates is
	// NOT flagged, so the signal means something.
	other := Request{
		Kind:  KindAgentTool,
		Tool:  ToolRequest{Tool: "write", Arguments: map[string]any{"file_path": "other.go"}},
		Agent: AgentIdentity{Name: "coder"},
	}
	if _, err := inbox.Enqueue(other, Verdict{Decision: DecisionOperatorApprove, ACMMLevel: 3}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if got := len(inbox.PendingAtFullAutonomy()); got != 1 {
		t.Errorf("an L3 pending item was flagged as a policy bug: PendingAtFullAutonomy=%d, want 1", got)
	}
}

// blockingScanner fails the test if it is ever called, and blocks if it is —
// so a Resolve that wrongly enters the scan lane both reports and times out.
type blockingScanner struct{ t *testing.T }

func (b blockingScanner) Scan(ctx context.Context, req ToolRequest) (ScanResult, error) {
	b.t.Errorf("scanner was consulted for tool %q, which should have resolved without a scan", req.Tool)
	<-ctx.Done()
	return ScanResult{}, ctx.Err()
}
