package toolapprove

// Config → desk wiring, the UI accessors, and the error paths.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestDeskFromConfigDisabledByDefault is the additive guarantee: an absent or
// disabled tool_approval block yields no desk, which is every producer's signal
// to keep using its legacy gate.
func TestDeskFromConfigDisabledByDefault(t *testing.T) {
	for name, cfg := range map[string]*config.Config{
		"nil config":   nil,
		"absent block": {},
		"explicit off": {ToolApproval: config.ToolApprovalConfig{Enabled: false}},
	} {
		desk, inbox, err := DeskFromConfig(cfg, nil, nil)
		if err != nil {
			t.Errorf("%s: unexpected error %v", name, err)
		}
		if desk != nil || inbox != nil {
			t.Errorf("%s: expected no desk and no inbox, got desk=%v inbox=%v", name, desk != nil, inbox != nil)
		}
	}
}

// TestDeskFromConfigEnabledBuildsBoth is the positive control.
func TestDeskFromConfigEnabledBuildsBoth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.json")
	cfg := &config.Config{
		ToolApproval: config.ToolApprovalConfig{
			Enabled:   true,
			InboxPath: path,
			Rules: []config.ToolApprovalRule{
				{Name: "hold-workflows", Expr: `request.file_path.startsWith(".github/workflows/")`, Action: "operator-approve", Priority: 10},
				{Name: "green-deps", Expr: `request.checks_green && request.author == "dependabot[bot]"`, Action: "auto-approve"},
			},
		},
	}

	desk, inbox, err := DeskFromConfig(cfg, nil, nil)
	if err != nil {
		t.Fatalf("DeskFromConfig: %v", err)
	}
	if desk == nil || inbox == nil {
		t.Fatal("enabled config produced no desk/inbox")
	}

	names := desk.RuleNames()
	if len(names) != 2 || names[0] != "hold-workflows" {
		t.Errorf("RuleNames = %v, want hold-workflows first (priority 10)", names)
	}

	// The inbox must honor the configured path, not the package default.
	if _, err := inbox.Enqueue(pendingRequest(1), operatorVerdict()); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("inbox did not persist at the configured path %s: %v", path, err)
	}
}

// TestDeskFromConfigRejectsMalformedRules pins the fail-closed load posture:
// a bad rule is an error and NO desk, so the operator fixes config rather than
// running a fleet with a half-applied rule set.
func TestDeskFromConfigRejectsMalformedRules(t *testing.T) {
	cfg := &config.Config{
		ToolApproval: config.ToolApprovalConfig{
			Enabled: true,
			Rules:   []config.ToolApprovalRule{{Name: "bad", Expr: "request.nope == 1", Action: "auto-approve"}},
		},
	}
	desk, inbox, err := DeskFromConfig(cfg, nil, nil)
	if err == nil {
		t.Fatal("a malformed rule was accepted at config load")
	}
	if desk != nil || inbox != nil {
		t.Error("a failed compile still produced a desk/inbox — it must fail closed")
	}
}

// TestACMMLevelOfFailsClosed pins that an unset level reads as the most
// restrictive value rather than defaulting to something permissive.
func TestACMMLevelOfFailsClosed(t *testing.T) {
	if got := ACMMLevelOf(nil); got != 0 {
		t.Errorf("ACMMLevelOf(nil) = %d, want 0", got)
	}
	if got := ACMMLevelOf(&config.Config{}); got != 0 {
		t.Errorf("ACMMLevelOf(unset level) = %d, want 0 (fail-closed)", got)
	}
	six := 6
	if got := ACMMLevelOf(&config.Config{ACMMLevel: &six}); got != 6 {
		t.Errorf("ACMMLevelOf(6) = %d, want 6", got)
	}
}

// TestWouldMatchRuleDrivesThePanel pins the accessor the Approvals panel uses to
// show, per row, which rule would resolve an item.
func TestWouldMatchRuleDrivesThePanel(t *testing.T) {
	eng, err := CompileRules([]Rule{{
		Name: "self-merge-holds", Expr: `request.kind == "self-merge"`, Action: RuleActionOperatorApprove,
	}}, nil)
	if err != nil {
		t.Fatalf("CompileRules: %v", err)
	}
	desk := NewDesk(eng, passingScanner{})

	m, ok := desk.WouldMatchRule(Request{Kind: KindSelfMerge}, 4)
	if !ok || m.Name != "self-merge-holds" || m.Action != RuleActionOperatorApprove {
		t.Errorf("WouldMatchRule = (%+v, %v), want the self-merge-holds rule", m, ok)
	}

	if _, ok := desk.WouldMatchRule(Request{Kind: KindAgentTool}, 4); ok {
		t.Error("WouldMatchRule matched a request no rule covers")
	}

	// A desk with no rules, and a nil desk, must both answer safely rather than
	// panicking — the panel calls this on every row.
	if _, ok := NewDesk(nil, nil).WouldMatchRule(Request{Kind: KindSelfMerge}, 4); ok {
		t.Error("a rule-less desk reported a match")
	}
	var nilDesk *Desk
	if _, ok := nilDesk.WouldMatchRule(Request{}, 0); ok {
		t.Error("a nil desk reported a match")
	}
	if names := nilDesk.RuleNames(); names != nil {
		t.Errorf("nil desk RuleNames = %v, want nil", names)
	}
}

// TestRuleEngineNilSafety pins the nil-receiver paths the desk relies on.
func TestRuleEngineNilSafety(t *testing.T) {
	var e *RuleEngine
	if e.Len() != 0 {
		t.Error("nil engine Len != 0")
	}
	if e.Names() != nil {
		t.Error("nil engine Names != nil")
	}
	if _, ok := e.Match(Request{}, 6); ok {
		t.Error("nil engine matched")
	}
	e.warn("should not panic")
}

// TestScannerErrorFailsClosed pins that a scanner that errors DENIES rather
// than waving the request through.
func TestScannerErrorFailsClosed(t *testing.T) {
	desk := NewDesk(nil, erroringScanner{})
	v := desk.Resolve(context.Background(), sideEffectfulRequest(), 5)
	if v.Decision != DecisionDeny {
		t.Errorf("a scanner error resolved to %s, want deny (fail-closed)", v.Decision)
	}
}

// TestScanViolationDenies pins the failed-scan path.
func TestScanViolationDenies(t *testing.T) {
	desk := NewDesk(nil, failingScanner{})
	v := desk.Resolve(context.Background(), sideEffectfulRequest(), 6)
	if v.Decision != DecisionDeny {
		t.Errorf("a failed scan resolved to %s, want deny", v.Decision)
	}
	if v.ScanResult == nil || v.ScanResult.Passed {
		t.Error("the verdict did not carry the failing scan result")
	}
}

// TestGreenScanBelowScanCeilingStillNeedsOperator pins that a green scan cannot
// lift a low-level hive out of the operator lane.
func TestGreenScanBelowScanCeilingStillNeedsOperator(t *testing.T) {
	if AutoApproveOnGreenScan(2) {
		t.Error("L2 must not auto-approve on a green scan")
	}
	if !AutoApproveOnGreenScan(3) {
		t.Error("L3 should auto-approve on a green scan")
	}
}

// TestNewDeskInstallsDefaultScanner pins that a nil scanner does not mean
// "no scanning".
func TestNewDeskInstallsDefaultScanner(t *testing.T) {
	desk := NewDesk(nil, nil)
	if desk.scanner == nil {
		t.Fatal("NewDesk(nil, nil) left the scanner nil — scan-lane requests would have no scanner")
	}
	// A destructive command must be caught by the default scanner.
	req := Request{
		Kind:  KindAgentTool,
		Tool:  ToolRequest{Tool: "bash", Arguments: map[string]any{"command": "rm -rf /"}},
		Agent: AgentIdentity{Name: "x"},
	}
	if v := desk.Resolve(context.Background(), req, 6); v.Decision != DecisionDeny {
		t.Errorf("destructive command resolved to %s, want deny", v.Decision)
	}
}

// TestNewInboxDefaultsPath pins that an empty path falls back to the package
// default rather than writing to the process working directory.
func TestNewInboxDefaultsPath(t *testing.T) {
	if got := NewInbox("").path; got != DefaultInboxPath {
		t.Errorf("NewInbox(\"\").path = %q, want %q", got, DefaultInboxPath)
	}
}

// TestMarkExecutedUnknownID pins the not-found path.
func TestMarkExecutedUnknownID(t *testing.T) {
	inbox := NewInbox(filepath.Join(t.TempDir(), "inbox.json"))
	if err := inbox.MarkExecuted("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkExecuted(unknown) = %v, want ErrNotFound", err)
	}
}

// TestMarkExecutedIsIdempotent pins that marking twice is safe — a producer
// retrying after a partial failure must not error.
func TestMarkExecutedIsIdempotent(t *testing.T) {
	inbox := NewInbox(filepath.Join(t.TempDir(), "inbox.json"))
	id, _ := inbox.Enqueue(pendingRequest(1), operatorVerdict())
	if _, err := inbox.Resolve(id, true, "op", ""); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := inbox.MarkExecuted(id); err != nil {
		t.Fatalf("first MarkExecuted: %v", err)
	}
	if err := inbox.MarkExecuted(id); err != nil {
		t.Errorf("second MarkExecuted: %v, want nil (idempotent)", err)
	}
}

// TestPersistFailureIsReported pins that an unwritable inbox surfaces an error
// rather than silently dropping a pending approval on the floor.
func TestPersistFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	// A FILE where the inbox wants a directory: MkdirAll must fail.
	blocker := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	inbox := NewInbox(filepath.Join(blocker, "inbox.json"))

	if _, err := inbox.Enqueue(pendingRequest(1), operatorVerdict()); err == nil {
		t.Error("Enqueue reported success although the inbox could not be persisted — " +
			"a pending approval that is not durable must not look durable")
	}
}

// TestGetMissingID pins the miss path of Get.
func TestGetMissingID(t *testing.T) {
	inbox := NewInbox(filepath.Join(t.TempDir(), "inbox.json"))
	if _, ok := inbox.Get("nope"); ok {
		t.Error("Get returned ok for an unknown ID")
	}
	if _, ok := inbox.ResolvedRecordFor("nope"); ok {
		t.Error("ResolvedRecordFor returned ok for an unknown ID")
	}
}

// TestVerdictWordBothBranches keeps the journal messages honest.
func TestVerdictWordBothBranches(t *testing.T) {
	if verdictWord(true) != "granted" || verdictWord(false) != "rejected" {
		t.Error("verdictWord produced the wrong label")
	}
}

// TestLegacyBaseDecisionQueuedMerge pins the queued-merge lane mapping.
func TestLegacyBaseDecisionQueuedMerge(t *testing.T) {
	v := LegacyBaseDecision(Request{Kind: KindQueuedMerge}, 6)
	if v.Decision != DecisionOperatorApprove {
		t.Errorf("queued merge base decision = %s, want operator-approve", v.Decision)
	}
}

// erroringScanner always errors.
type erroringScanner struct{}

func (erroringScanner) Scan(context.Context, ToolRequest) (ScanResult, error) {
	return ScanResult{}, errors.New("scanner exploded")
}

// failingScanner always returns a failed scan.
type failingScanner struct{}

func (failingScanner) Scan(context.Context, ToolRequest) (ScanResult, error) {
	return ScanResult{Passed: false, Violations: []string{"test violation"}}, nil
}
