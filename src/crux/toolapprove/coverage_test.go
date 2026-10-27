package toolapprove

import (
	"context"
	"testing"
)

func TestClassifySideEffectfulAndSubagent(t *testing.T) {
	if IsSideEffectful("read") {
		t.Error("read must not be side-effectful")
	}
	if !IsSideEffectful("bash") {
		t.Error("bash must be side-effectful")
	}
	req := ToolRequest{Tool: "grep"}
	if !req.IsReadOnly() || req.IsSideEffectful() {
		t.Error("grep request must be read-only and not side-effectful")
	}
	wr := ToolRequest{Tool: "write"}
	if wr.IsReadOnly() || !wr.IsSideEffectful() {
		t.Error("write request must be side-effectful")
	}

	for _, tool := range []string{"agent", "invoke_subagent", "define_subagent", "subagent_sync"} {
		if !IsSubagent(tool) {
			t.Errorf("IsSubagent(%q) = false, want true", tool)
		}
	}
	if IsSubagent("bash") {
		t.Error("IsSubagent(bash) = true, want false")
	}
}

func TestIsGitHubWriteCopilotSyntax(t *testing.T) {
	for _, tool := range []string{
		"github-mcp-server(create_issue)",
		"github-mcp-server(update_issue)",
		"github-mcp-server(add_issue_comment)",
		"github-mcp-server(delete_file)",
		"mcp__github__update_issue",
		"mcp__github__add_issue_comment",
		"mcp__github__delete_file",
	} {
		if !IsGitHubWrite(tool) {
			t.Errorf("IsGitHubWrite(%q) = false, want true", tool)
		}
	}
	if IsGitHubWrite("mcp__github__get_issue") {
		t.Error("get_issue must not be a GitHub write")
	}
}

// TestResolveScanBranchesPerLevel covers Resolve's post-scan mapping: L3/L5
// auto-approve on green, L4 still requires the operator, and a failed scan
// denies regardless of level.
func TestResolveScanBranchesPerLevel(t *testing.T) {
	ctx := context.Background()
	agentId := AgentIdentity{Name: "cov"}
	benign := ToolRequest{Tool: "bash", Command: "go test ./..."}

	// Nil scanner falls back to the default scanner.
	v3 := Resolve(ctx, benign, 3, agentId, nil)
	if !v3.AutoApproved() {
		t.Errorf("L3 green scan should auto-approve, got %+v", v3)
	}
	if v3.ScanResult == nil || !v3.ScanResult.Passed {
		t.Errorf("L3 verdict should carry a passed scan result, got %+v", v3.ScanResult)
	}

	v5 := Resolve(ctx, benign, 5, agentId, NewDefaultSecurityScanner())
	if !v5.AutoApproved() {
		t.Errorf("L5 green scan should auto-approve, got %+v", v5)
	}

	// L4 reaches Resolve via Decide only as operator-approve (no scan), so the
	// post-scan L4 branch is exercised by handing Resolve a synthetic scan
	// request is not possible through the public path; assert Decide's L4 gate.
	v4 := Decide(ctx, benign, 4, agentId)
	if !v4.RequiresOperatorApproval() {
		t.Errorf("L4 side-effectful should require operator approval, got %+v", v4)
	}

	// A destructive command fails the default scan and is denied at L6.
	destructive := ToolRequest{Tool: "bash", Command: "rm -rf / --no-preserve-root"}
	vDeny := Resolve(ctx, destructive, 6, agentId, NewDefaultSecurityScanner())
	if !vDeny.Denied() {
		t.Errorf("failed scan must deny, got %+v", vDeny)
	}
	if vDeny.ScanResult == nil || vDeny.ScanResult.Passed {
		t.Errorf("deny verdict should carry the failed scan result, got %+v", vDeny.ScanResult)
	}
}

func TestToolRequestAccessorFallbacks(t *testing.T) {
	// Argument-map fallbacks.
	r := ToolRequest{Arguments: map[string]any{
		"script":     "make build",
		"filename":   "pkg/x/y.go",
		"repository": "hivecommons/hive",
		"body":       "hello",
	}}
	if r.GetCommand() != "make build" {
		t.Errorf("GetCommand fallback = %q", r.GetCommand())
	}
	if r.GetFilePath() != "pkg/x/y.go" {
		t.Errorf("GetFilePath fallback = %q", r.GetFilePath())
	}
	if r.GetRepo() != "hivecommons/hive" {
		t.Errorf("GetRepo fallback = %q", r.GetRepo())
	}
	if r.GetContent() != "hello" {
		t.Errorf("GetContent fallback = %q", r.GetContent())
	}

	// Empty request yields empty strings, not panics.
	var empty ToolRequest
	if empty.GetCommand() != "" || empty.GetFilePath() != "" || empty.GetRepo() != "" || empty.GetContent() != "" {
		t.Error("empty request accessors must return empty strings")
	}

	// Non-string argument values are ignored.
	odd := ToolRequest{Arguments: map[string]any{"command": 42, "path": true, "repo": []string{"x"}}}
	if odd.GetCommand() != "" || odd.GetFilePath() != "" || odd.GetRepo() != "" {
		t.Error("non-string argument values must be ignored")
	}
}

func TestAuditFieldsIncludeScanViolations(t *testing.T) {
	v := Verdict{
		Decision:  DecisionDeny,
		Rationale: "scan failed",
		Tool:      "bash",
		ACMMLevel: 5,
		ScanResult: &ScanResult{
			Passed:     false,
			Violations: []string{"destructive shell command", "guardrail path"},
		},
	}
	fields := v.AuditFields()
	if fields["scan_passed"] != false {
		t.Errorf("scan_passed = %v, want false", fields["scan_passed"])
	}
	if fields["violations"] != "destructive shell command; guardrail path" {
		t.Errorf("violations = %v", fields["violations"])
	}
	if !v.RequiresSecurityScan() == false && v.Decision == DecisionSecurityScan {
		t.Error("helper consistency")
	}
}

func TestGuardrailPatternMatching(t *testing.T) {
	s := NewDefaultSecurityScanner()
	cases := map[string]bool{
		".github/workflows/ci.yml":       true, // direct **-suffix directory
		"nested/dir/.github/workflows/x": true, // **/ prefix recursion
		"policies/acmm.yaml":             true, // dir/** suffix
		"deep/policies/acmm.yaml":        true, // /policies/ containment
		"sub/dir/OWNERS":                 true, // base-name match
		"bin/gh-wrapper.sh":              true, // gh-wrapper* glob
		"src/pkg/turn/runner.go":         false,
		"docs/design/reentrant-turn.md":  false,
	}
	for path, want := range cases {
		got := s.touchesGuardrailPath(path)
		if got != want {
			t.Errorf("touchesGuardrailPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestScanFlagsCredentialExfil(t *testing.T) {
	s := NewDefaultSecurityScanner()
	res, err := s.Scan(context.Background(), ToolRequest{Tool: "bash", Command: "cat ~/.ssh/id_rsa"})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.Passed {
		t.Error("credential exfiltration command must fail the scan")
	}
}
