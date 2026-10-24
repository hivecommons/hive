package toolapprove

// Bulk == N individual decisions.
//
// RFC #4000 is explicit that a bulk approve must never become a parallel
// decision path: "a bulk approve is N individual evaluations through the same
// decision function every agent request goes through". These tests pin that
// equivalence rather than merely testing that the bulk path returns something.

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// mixedRequests spans every lane, so an equivalence that only held for one
// decision would show up here.
func mixedRequests() []Request {
	return []Request{
		// read-only → auto-approve at every level
		{Kind: KindAgentTool, Tool: ToolRequest{Tool: "read", Arguments: map[string]any{"file_path": "a.go"}}, Agent: AgentIdentity{Name: "r"}},
		// side-effectful write
		{Kind: KindAgentTool, Tool: ToolRequest{Tool: "write", Arguments: map[string]any{"file_path": "b.go", "content": "x"}}, Agent: AgentIdentity{Name: "w"}},
		// hard-denied guardrail
		{Kind: KindAgentTool, Tool: ToolRequest{Tool: "mcp__github__create_pull_request"}, Agent: AgentIdentity{Name: "p"}},
		// self-merge, green
		{Kind: KindSelfMerge, Repo: "hivecommons/hive", Number: 1, Author: "dependabot[bot]", ChecksGreen: true, Tool: ToolRequest{Tool: "hive-merge"}},
		// self-merge, red
		{Kind: KindSelfMerge, Repo: "hivecommons/hive", Number: 2, Author: "someone", ChecksGreen: false, Tool: ToolRequest{Tool: "hive-merge"}},
		// plan approval
		{Kind: KindPlanApproval, Tool: ToolRequest{Tool: "plan"}, Agent: AgentIdentity{Name: "architect"}},
		// bash command
		{Kind: KindAgentTool, Tool: ToolRequest{Tool: "bash", Arguments: map[string]any{"command": "go test ./..."}}, Agent: AgentIdentity{Name: "t"}},
	}
}

// TestResolveBatchEqualsNIndividualResolves is the equivalence assertion, run
// across every ACMM level so a level-specific divergence cannot hide.
func TestResolveBatchEqualsNIndividualResolves(t *testing.T) {
	eng, err := CompileRules([]Rule{{
		Name:   "green-dependabot",
		Expr:   `request.kind == "self-merge" && request.checks_green && request.author == "dependabot[bot]"`,
		Action: RuleActionAutoApprove,
	}, {
		Name:     "red-goes-to-operator",
		Expr:     `request.kind == "self-merge" && !request.checks_green`,
		Action:   RuleActionOperatorApprove,
		Priority: 10,
	}}, nil)
	if err != nil {
		t.Fatalf("CompileRules: %v", err)
	}
	desk := NewDesk(eng, passingScanner{})
	reqs := mixedRequests()

	for level := 0; level <= 7; level++ {
		batch := desk.ResolveBatch(context.Background(), reqs, level)

		individual := make([]Verdict, 0, len(reqs))
		for _, r := range reqs {
			individual = append(individual, desk.Resolve(context.Background(), r, level))
		}

		if len(batch) != len(individual) {
			t.Fatalf("L%d: batch returned %d verdicts, individual returned %d", level, len(batch), len(individual))
		}
		for i := range batch {
			if !reflect.DeepEqual(batch[i], individual[i]) {
				t.Errorf("L%d item %d (%s/%s): bulk verdict differs from the individual verdict\n bulk:       %+v\n individual: %+v\n"+
					"a bulk approve MUST be N individual evaluations through the same decision function",
					level, i, reqs[i].Kind, reqs[i].Tool.Tool, batch[i], individual[i])
			}
		}
	}
}

// TestResolveBatchPreservesOrder pins that results line up with inputs — a
// per-item result list is only useful if item i's verdict is at index i.
func TestResolveBatchPreservesOrder(t *testing.T) {
	desk := NewDesk(nil, passingScanner{})
	reqs := mixedRequests()
	got := desk.ResolveBatch(context.Background(), reqs, 4)

	if len(got) != len(reqs) {
		t.Fatalf("got %d verdicts for %d requests", len(got), len(reqs))
	}
	for i, v := range got {
		if v.Kind != reqs[i].Kind {
			t.Errorf("index %d: verdict kind %q does not match request kind %q — results are misaligned",
				i, v.Kind, reqs[i].Kind)
		}
	}
}

// TestResolveManyEqualsNIndividualResolutions is the same equivalence for the
// INBOX's bulk path, which is what the API's /bulk endpoint calls.
func TestResolveManyEqualsNIndividualResolutions(t *testing.T) {
	// Two inboxes seeded identically: one resolved in bulk, one item by item.
	bulkInbox := NewInbox(filepath.Join(t.TempDir(), "bulk.json"))
	oneInbox := NewInbox(filepath.Join(t.TempDir(), "one.json"))

	var ids []string
	for n := 1; n <= 5; n++ {
		id, err := bulkInbox.Enqueue(pendingRequest(n), operatorVerdict())
		if err != nil {
			t.Fatalf("seed bulk: %v", err)
		}
		if _, err := oneInbox.Enqueue(pendingRequest(n), operatorVerdict()); err != nil {
			t.Fatalf("seed individual: %v", err)
		}
		ids = append(ids, id)
	}

	bulkResults := bulkInbox.ResolveMany(ids, true, "operator", "batch approve")

	for i, id := range ids {
		_, err := oneInbox.Resolve(id, true, "operator", "batch approve")
		ok := err == nil
		if bulkResults[i].Ok != ok {
			t.Errorf("item %d: bulk ok=%v, individual ok=%v", i, bulkResults[i].Ok, ok)
		}
	}

	if bulkInbox.Count() != oneInbox.Count() {
		t.Errorf("after resolution: bulk inbox has %d pending, individual has %d",
			bulkInbox.Count(), oneInbox.Count())
	}
	for _, id := range ids {
		b, okB := bulkInbox.ResolvedRecordFor(id)
		o, okO := oneInbox.ResolvedRecordFor(id)
		if okB != okO || b.Approved != o.Approved {
			t.Errorf("journal divergence for %s: bulk=%+v individual=%+v", id, b, o)
		}
	}
}

// TestResolveManyReportsPartialFailure pins that partial failure is per-item
// rather than aborting the batch — the normal case when another operator
// resolved an item between the list and the click.
func TestResolveManyReportsPartialFailure(t *testing.T) {
	inbox := NewInbox(filepath.Join(t.TempDir(), "inbox.json"))

	good1, _ := inbox.Enqueue(pendingRequest(1), operatorVerdict())
	good2, _ := inbox.Enqueue(pendingRequest(2), operatorVerdict())

	results := inbox.ResolveMany([]string{good1, "does-not-exist", good2}, true, "operator", "")
	if len(results) != 3 {
		t.Fatalf("got %d results for 3 ids", len(results))
	}
	if !results[0].Ok || !results[2].Ok {
		t.Error("valid items failed alongside the invalid one — a bad ID must not abort the batch")
	}
	if results[1].Ok {
		t.Error("a nonexistent ID reported success")
	}
	if results[1].Error == "" {
		t.Error("a failed item carries no error message")
	}
	if got := inbox.Count(); got != 0 {
		t.Errorf("both valid items should be resolved; %d still pending", got)
	}
}

// TestResolveManyIsIdempotent pins that re-submitting a bulk approve does not
// double-execute — the bulk path inherits the single path's journal.
func TestResolveManyIsIdempotent(t *testing.T) {
	inbox := NewInbox(filepath.Join(t.TempDir(), "inbox.json"))
	var ids []string
	for n := 1; n <= 3; n++ {
		id, _ := inbox.Enqueue(pendingRequest(n), operatorVerdict())
		ids = append(ids, id)
	}

	first := inbox.ResolveMany(ids, true, "operator", "")
	for i, r := range first {
		if !r.Ok {
			t.Fatalf("first bulk resolve item %d failed: %s", i, r.Error)
		}
	}

	// Re-submit the identical batch (a double-clicked button, a retried request).
	second := inbox.ResolveMany(ids, true, "operator", "")
	for i, r := range second {
		if r.Ok {
			t.Errorf("re-submitted bulk item %d succeeded again — that is a double execution", i)
		}
		if !strings.Contains(r.Error, ErrAlreadyResolved.Error()) {
			t.Errorf("item %d: expected an already-resolved error, got %q", i, r.Error)
		}
	}
}
