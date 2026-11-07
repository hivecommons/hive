package toolapprove

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestToolRequestApplyPatchPaths(t *testing.T) {
	multi := "*** Begin Patch\n*** Update File: src/a.go\n@@\n-x\n+y\n*** Add File:  OWNERS \n+a\n*** Delete File: policies/p.yaml\n*** Update File: src/a.go\n*** Move to: hive.yaml\n\n*** End Patch"
	tests := []struct {
		name      string
		req       ToolRequest
		wantFirst string
		wantAll   []string
	}{
		{"input key", ToolRequest{Tool: "apply_patch", Arguments: map[string]any{"input": multi}}, "src/a.go", []string{"src/a.go", "OWNERS", "policies/p.yaml", "hive.yaml"}},
		{"patch key", ToolRequest{Tool: "apply_patch", Arguments: map[string]any{"patch": "*** Begin Patch\n*** Update File: .github/workflows/ci.yml\n*** End Patch"}}, ".github/workflows/ci.yml", []string{".github/workflows/ci.yml"}},
		{"unknown tool with patch text", ToolRequest{Tool: "editor", Arguments: map[string]any{"input": "*** Begin Patch\n*** Add File: OWNERS\n*** End Patch"}}, "OWNERS", []string{"OWNERS"}},
		{"path argument wins", ToolRequest{Tool: "apply_patch", Arguments: map[string]any{"path": "x.go", "input": multi}}, "x.go", []string{"x.go", "src/a.go", "OWNERS", "policies/p.yaml", "hive.yaml"}},
		{"non-patch text ignored", ToolRequest{Tool: "write", Arguments: map[string]any{"input": "*** Update File: OWNERS"}}, "", nil},
		{"no arguments", ToolRequest{Tool: "apply_patch"}, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.req.GetFilePath(); got != tt.wantFirst {
				t.Errorf("GetFilePath() = %q, want %q", got, tt.wantFirst)
			}
			if got := tt.req.GetFilePaths(); !reflect.DeepEqual(got, tt.wantAll) {
				t.Errorf("GetFilePaths() = %v, want %v", got, tt.wantAll)
			}
		})
	}
}

// TestSecurityScanner_ApplyPatchGuardrailAnyPosition drives Scan with a
// multi-file apply_patch so a guardrail path that is not the first header
// still fails the scan (#16979).
func TestSecurityScanner_ApplyPatchGuardrailAnyPosition(t *testing.T) {
	scanner := NewDefaultSecurityScanner()
	tests := []struct {
		name       string
		patch      string
		wantPassed bool
		wantPath   string
	}{
		{"guardrail path second", "*** Begin Patch\n*** Update File: src/a.go\n@@\n-x\n+y\n*** Add File: OWNERS\n+a\n*** End Patch", false, `"OWNERS"`},
		{"guardrail path last via move", "*** Begin Patch\n*** Update File: src/a.go\n*** Update File: src/b.go\n*** Move to: policies/p.yaml\n*** End Patch", false, `"policies/p.yaml"`},
		{"guardrail path first", "*** Begin Patch\n*** Update File: hive.yaml\n*** Update File: src/a.go\n*** End Patch", false, `"hive.yaml"`},
		{"no guardrail paths", "*** Begin Patch\n*** Update File: src/a.go\n*** Add File: src/b.go\n*** End Patch", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := ToolRequest{Tool: "apply_patch", Arguments: map[string]any{"input": tt.patch}}
			res, err := scanner.Scan(context.Background(), req)
			if err != nil {
				t.Fatalf("scanner error: %v", err)
			}
			if res.Passed != tt.wantPassed {
				t.Fatalf("Passed = %v, want %v (violations: %v)", res.Passed, tt.wantPassed, res.Violations)
			}
			if tt.wantPath != "" && !strings.Contains(res.Details, tt.wantPath) {
				t.Errorf("Details %q does not name %s", res.Details, tt.wantPath)
			}
		})
	}
}
