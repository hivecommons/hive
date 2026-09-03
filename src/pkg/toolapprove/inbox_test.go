package toolapprove

// Durability and idempotency of the operator lane.
//
// The requirement these encode: an approval that waits overnight must survive a
// spoke roll. Hives auto-roll frequently, so "restart" is not an exotic case —
// it is the normal case for anything that waits more than a few hours.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeFileHelper seeds a file, creating parent directories as needed.
func writeFileHelper(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// pendingRequest builds a distinct operator-lane request.
func pendingRequest(n int) Request {
	return Request{
		Kind:   KindSelfMerge,
		Repo:   "hivecommons/hive",
		Number: n,
		Author: "hive-app[bot]",
		Title:  fmt.Sprintf("fix: thing %d", n),
		Tool:   ToolRequest{Tool: "hive-merge"},
		Agent:  AgentIdentity{Name: "sweep"},
	}
}

func operatorVerdict() Verdict {
	return Verdict{Decision: DecisionOperatorApprove, ACMMLevel: 4, Tool: "hive-merge"}
}

// TestInboxSurvivesRestart is the persistence round-trip: items queued by one
// process are visible to a NEW Inbox over the same path, which is exactly what
// a spoke roll looks like from the inbox's point of view.
func TestInboxSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approvals", "inbox.json")

	// Process 1: queue three approvals.
	first := NewInbox(path)
	var ids []string
	for n := 1; n <= 3; n++ {
		id, err := first.Enqueue(pendingRequest(n), operatorVerdict())
		if err != nil {
			t.Fatalf("Enqueue #%d: %v", n, err)
		}
		ids = append(ids, id)
	}
	if first.Count() != 3 {
		t.Fatalf("pre-restart count = %d, want 3", first.Count())
	}

	// Process 2: a brand-new Inbox over the same durable path — the simulated
	// restart. Nothing is carried over in memory.
	second := NewInbox(path)
	if got := second.Count(); got != 3 {
		t.Fatalf("post-restart count = %d, want 3 — pending approvals did not survive the roll", got)
	}
	for _, id := range ids {
		if _, ok := second.Get(id); !ok {
			t.Errorf("approval %s vanished across restart", id)
		}
	}

	// Resolving after the restart works, and the resolution ALSO persists.
	if _, err := second.Resolve(ids[0], true, "operator", "looks good"); err != nil {
		t.Fatalf("post-restart Resolve: %v", err)
	}
	third := NewInbox(path)
	if got := third.Count(); got != 2 {
		t.Fatalf("after resolving 1 of 3, post-restart count = %d, want 2", got)
	}
	if _, ok := third.ResolvedRecordFor(ids[0]); !ok {
		t.Error("the resolved-verdict journal did not survive the restart — a re-delivered grant could double-execute")
	}
}

// TestGrantedVerdictDoesNotDoubleExecute is the idempotency requirement stated
// directly: a granted verdict re-delivered must not execute twice.
func TestGrantedVerdictDoesNotDoubleExecute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.json")
	inbox := NewInbox(path)

	req := pendingRequest(7)
	id, err := inbox.Enqueue(req, operatorVerdict())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// The operator grants it, and the producer executes.
	if _, err := inbox.Resolve(id, true, "operator", ""); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := inbox.MarkExecuted(id); err != nil {
		t.Fatalf("MarkExecuted: %v", err)
	}

	// RE-DELIVERY. The identical request arrives again — same fields, so the
	// derived key is identical.
	if got := DeriveIdempotencyKey(req); got != id {
		t.Fatalf("re-delivery derived a DIFFERENT key (%s vs %s) — idempotency depends on the key being stable", got, id)
	}

	// Enqueue must refuse to re-queue it, and say why.
	_, err = inbox.Enqueue(req, operatorVerdict())
	if !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("re-delivered granted request was re-queued (err=%v) — it must be recognized as already resolved", err)
	}

	// And a producer consulting the journal sees it already ran.
	rec, ok := inbox.ResolvedRecordFor(id)
	if !ok {
		t.Fatal("journal lost the resolved record")
	}
	if !rec.Approved || !rec.Executed {
		t.Errorf("journal record wrong: approved=%v executed=%v, want both true", rec.Approved, rec.Executed)
	}

	// Resolving again must not journal a second time.
	if _, err := inbox.Resolve(id, true, "operator", ""); !errors.Is(err, ErrAlreadyResolved) {
		t.Errorf("second Resolve of the same ID succeeded (err=%v) — that is the double-execution the journal prevents", err)
	}
}

// TestIdempotencyKeyDistinguishesDifferentRequests is the positive control for
// the test above: two genuinely different requests must NOT collide, or
// "idempotency" would just be "everything is a duplicate".
func TestIdempotencyKeyDistinguishesDifferentRequests(t *testing.T) {
	a := DeriveIdempotencyKey(pendingRequest(1))
	b := DeriveIdempotencyKey(pendingRequest(2))
	if a == b {
		t.Fatal("two different PRs derived the same idempotency key — distinct requests would be silently dropped")
	}

	// Argument ORDER must not change the key: map iteration order is random, so
	// a key that depended on it would make re-delivery detection flaky.
	r1 := Request{Kind: KindAgentTool, Tool: ToolRequest{
		Tool:      "write",
		Arguments: map[string]any{"a": "1", "b": "2", "c": "3"},
	}}
	first := DeriveIdempotencyKey(r1)
	for i := 0; i < 20; i++ {
		if DeriveIdempotencyKey(r1) != first {
			t.Fatal("idempotency key is not stable across calls — map iteration order is leaking into the hash")
		}
	}
}

// TestExplicitIdempotencyKeyWins pins that a caller-supplied key overrides the
// derived one, which is how a producer coordinates with an external notion of
// identity (e.g. stage 2's turn journal).
func TestExplicitIdempotencyKeyWins(t *testing.T) {
	req := pendingRequest(3)
	req.IdempotencyKey = "turn-abc-op-4"
	if got := DeriveIdempotencyKey(req); got != "turn-abc-op-4" {
		t.Errorf("explicit IdempotencyKey ignored: got %q", got)
	}
}

// TestResolveUnknownIDFails pins that resolving something that was never queued
// is an error rather than a silent success.
func TestResolveUnknownIDFails(t *testing.T) {
	inbox := NewInbox(filepath.Join(t.TempDir(), "inbox.json"))
	if _, err := inbox.Resolve("nope", true, "operator", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("Resolve of an unknown ID returned %v, want ErrNotFound", err)
	}
}

// TestEnqueueIsIdempotentWhilePending pins that the sweep re-seeing the same PR
// every tick produces ONE inbox row, not one per tick. Without this the panel
// would fill with duplicates of a single waiting decision.
func TestEnqueueIsIdempotentWhilePending(t *testing.T) {
	inbox := NewInbox(filepath.Join(t.TempDir(), "inbox.json"))
	req := pendingRequest(9)

	var firstID string
	for i := 0; i < 5; i++ {
		id, err := inbox.Enqueue(req, operatorVerdict())
		if err != nil {
			t.Fatalf("Enqueue tick %d: %v", i, err)
		}
		if firstID == "" {
			firstID = id
		} else if id != firstID {
			t.Fatalf("tick %d produced a different ID (%s vs %s)", i, id, firstID)
		}
	}
	if got := inbox.Count(); got != 1 {
		t.Fatalf("five identical enqueues produced %d rows, want 1", got)
	}
}

// TestRuleChipsFromPending pins the dashboard's filter-chip source.
func TestRuleChipsFromPending(t *testing.T) {
	inbox := NewInbox(filepath.Join(t.TempDir(), "inbox.json"))

	v1 := operatorVerdict()
	v1.Rule = "needs-human-for-workflows"
	v2 := operatorVerdict()
	v2.Rule = "large-diff"
	v3 := operatorVerdict() // no rule — base policy

	for i, v := range []Verdict{v1, v2, v3, v1} {
		if _, err := inbox.Enqueue(pendingRequest(100+i), v); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	chips := inbox.RuleChips()
	if len(chips) != 2 {
		t.Fatalf("RuleChips = %v, want 2 distinct names (duplicates collapsed, base-policy items excluded)", chips)
	}
	if chips[0] != "large-diff" || chips[1] != "needs-human-for-workflows" {
		t.Errorf("RuleChips not sorted deterministically: %v", chips)
	}
}

// TestInboxCorruptFileDoesNotWedge pins that a corrupt inbox degrades to empty
// rather than taking the hive down — the same posture the upgrade-pause state
// file takes.
func TestInboxCorruptFileDoesNotWedge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inbox.json")
	if err := writeFileHelper(path, "{not json at all"); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}

	inbox := NewInbox(path)
	if got := inbox.Count(); got != 0 {
		t.Fatalf("corrupt inbox reported %d items, want 0", got)
	}
	// And it still accepts new work, overwriting the corrupt file.
	if _, err := inbox.Enqueue(pendingRequest(1), operatorVerdict()); err != nil {
		t.Fatalf("Enqueue after corrupt read: %v", err)
	}
	if got := NewInbox(path).Count(); got != 1 {
		t.Errorf("after recovery, count = %d, want 1", got)
	}
}

// TestDeskAutoApproveNeverReachesInboxViaResolve is a belt-and-braces check on
// the full desk→inbox path used by the real producer.
func TestDeskAutoApproveNeverReachesInboxViaResolve(t *testing.T) {
	inbox := NewInbox(filepath.Join(t.TempDir(), "inbox.json"))
	desk := NewDesk(nil, passingScanner{})

	req := Request{
		Kind:  KindAgentTool,
		Tool:  ToolRequest{Tool: "grep", Arguments: map[string]any{"pattern": "x"}},
		Agent: AgentIdentity{Name: "reader"},
	}
	v := desk.Resolve(context.Background(), req, 6)
	if v.Decision != DecisionAutoApprove {
		t.Fatalf("read-only tool resolved to %s", v.Decision)
	}
	if _, err := inbox.Enqueue(req, v); err == nil {
		t.Fatal("auto-approve verdict entered the inbox")
	}
}
